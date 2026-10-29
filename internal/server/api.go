package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/driftywinds/interim/internal/store"
)

type apiErrorEnvelope struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

type fileDTO struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	ContentType  string    `json:"content_type"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	ExpiresIn    int64     `json:"expires_in_seconds"`
	NeverExpires bool      `json:"never_expires"`
	PageURL      string    `json:"page_url"`
	APIURL       string    `json:"api_url"`
	Owner        string    `json:"owner"`
}

type keyDTO struct {
	ID               string    `json:"id"`
	UserHash         string    `json:"user_hash"`
	Prefix           string    `json:"key_prefix"`
	Name             string    `json:"name,omitempty"`
	Approved         bool      `json:"approved"`
	CreatedAt        time.Time `json:"created_at"`
	MaxFileSizeBytes int64     `json:"max_file_size_bytes"`
	RetentionSeconds int64     `json:"retention_seconds"`
	Warning          string    `json:"warning"`
}

type keyInfoDTO struct {
	ID               string     `json:"id"`
	KeyPrefix        string     `json:"key_prefix"`
	Name             string     `json:"name,omitempty"`
	Approved         bool       `json:"approved"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	Files            int64      `json:"files"`
	Bytes            int64      `json:"bytes"`
	MaxFileSizeBytes int64      `json:"max_file_size_bytes"`
	RetentionSeconds int64      `json:"retention_seconds"`
}

type storageDTO struct {
	Files          int64   `json:"files"`
	UsedBytes      int64   `json:"used_bytes"`
	ReservedBytes  int64   `json:"reserved_bytes"`
	LimitBytes     int64   `json:"limit_bytes"`
	AvailableBytes int64   `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
	Unlimited      bool    `json:"unlimited"`
}

type statusDTO struct {
	AcceptingUploads          bool       `json:"accepting_uploads"`
	PublicUploads             bool       `json:"public_uploads"`
	Storage                   storageDTO `json:"storage"`
	RetentionSeconds          int64      `json:"retention_seconds"`
	RetentionHuman            string     `json:"retention_human"`
	RetentionExtendOnDownload bool       `json:"retention_extend_on_download"`
	MaxFileSize               int64      `json:"max_file_size_bytes"`
	DownloadURLTTL            int64      `json:"download_url_ttl_seconds"`
	ServerTime                time.Time  `json:"server_time"`
}

type downloadDTO struct {
	URL       string    `json:"url"`
	Filename  string    `json:"filename"`
	ExpiresAt time.Time `json:"expires_at"`
}

type paginationDTO struct {
	Total   int64 `json:"total"`
	Limit   int   `json:"limit"`
	Offset  int   `json:"offset"`
	HasMore bool  `json:"has_more"`
}

type listFilesDTO struct {
	Files      []fileDTO     `json:"files"`
	Pagination paginationDTO `json:"pagination"`
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func (s *Server) writeAPIError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.writeJSON(w, status, apiErrorEnvelope{
		Error: apiErrorDetail{
			Code:      code,
			Message:   message,
			RequestID: requestIDFromContext(r.Context()),
		},
	})
}

func decodeJSONStrict(r io.Reader, dst any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body contains trailing data")
	}
	return nil
}

func (s *Server) rawAPIKey(r *http.Request) (string, error) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			return "", errUnauthorized
		}
		return strings.TrimSpace(parts[1]), nil
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key, nil
	}
	return "", nil
}

func (s *Server) optionalAPIKey(r *http.Request) (*store.APIKey, error) {
	raw, err := s.rawAPIKey(r)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 256 {
		return nil, errUnauthorized
	}
	key, err := s.store.APIKeyByHash(r.Context(), tokenHash(raw))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errUnauthorized
		}
		return nil, err
	}
	if key.Disabled {
		return nil, errUnauthorized
	}
	if err := s.store.TouchAPIKey(r.Context(), key.ID, s.now()); err != nil {
		s.logger.Warn("update api key last used", "error", err)
	}
	return &key, nil
}

func (s *Server) requireAPIKey(w http.ResponseWriter, r *http.Request) (*store.APIKey, bool) {
	key, err := s.optionalAPIKey(r)
	if err != nil {
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "A valid user hash is required.")
		return nil, false
	}
	if key == nil {
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "A valid user hash is required.")
		return nil, false
	}
	return key, true
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PublicUploadsEnabled {
		s.writeAPIError(w, r, http.StatusForbidden, "key_creation_disabled", "Public user hash creation is disabled. Ask the admin for an approved user hash.")
		return
	}

	ip := s.clientIP(r)
	if allowed, retry := s.limiter.allow("keycreate:"+ip, 8, time.Hour); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many user hashes created. Try again later.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request struct {
		Name string `json:"name"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSONStrict(r.Body, &request); err != nil && !errors.Is(err, io.EOF) {
			s.writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
	}
	name := cleanLabel(request.Name)
	maxFileSize, retention := s.keyLimits(nil)
	key, rawHash, err := s.newUserHash(r.Context(), name, false, maxFileSize, int64(retention/time.Second))
	if err != nil {
		s.logger.Error("create api key", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create a user hash.")
		return
	}
	s.writeJSON(w, http.StatusCreated, keyDTO{
		ID:               key.ID,
		UserHash:         rawHash,
		Prefix:           key.Prefix,
		Name:             key.Name,
		Approved:         key.Approved,
		CreatedAt:        key.CreatedAt,
		MaxFileSizeBytes: key.MaxFileSize,
		RetentionSeconds: key.RetentionSeconds,
		Warning:          "Save this user hash now. It is shown once and cannot be recovered.",
	})
}

// newUserHash creates and persists a bearer credential. Admin-created hashes
// start approved; self-service hashes stay unapproved so enabling the upload
// allowlist cannot silently authorize credentials created earlier.
func (s *Server) newUserHash(ctx context.Context, name string, approved bool, maxFileSize, retentionSeconds int64) (store.APIKey, string, error) {
	rawHash, err := randomToken(32)
	if err != nil {
		return store.APIKey{}, "", err
	}
	rawHash = "interim_" + rawHash
	id, err := randomToken(12)
	if err != nil {
		return store.APIKey{}, "", err
	}
	key := store.APIKey{
		ID:               id,
		KeyHash:          tokenHash(rawHash),
		Prefix:           rawHash[:min(16, len(rawHash))],
		Name:             name,
		Approved:         approved,
		CreatedAt:        s.now().UTC(),
		MaxFileSize:      maxFileSize,
		RetentionSeconds: retentionSeconds,
	}
	if err := s.store.CreateAPIKey(ctx, key); err != nil {
		return store.APIKey{}, "", err
	}
	return key, rawHash, nil
}

func (s *Server) handleKeyInfo(w http.ResponseWriter, r *http.Request) {
	key, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	usage, err := s.store.APIKeyUsage(r.Context(), key.ID)
	if err != nil {
		s.logger.Error("api key usage", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read user hash usage.")
		return
	}
	maxFileSize, retention := s.keyLimits(key)
	s.writeJSON(w, http.StatusOK, keyInfoDTO{
		ID:               key.ID,
		KeyPrefix:        key.Prefix,
		Name:             key.Name,
		Approved:         key.Approved,
		CreatedAt:        key.CreatedAt,
		LastUsedAt:       key.LastUsedAt,
		Files:            usage.Files,
		Bytes:            usage.Bytes,
		MaxFileSizeBytes: maxFileSize,
		RetentionSeconds: int64(retention / time.Second),
	})
}

func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"name":                         "interim",
		"version":                      "v1",
		"docs":                         s.cfg.PublicBaseURL + "/api",
		"openapi":                      s.cfg.PublicBaseURL + "/api/openapi.json",
		"retention_seconds":            int64(s.cfg.Retention.Seconds()),
		"retention_human":              retentionLabel(s.cfg.Retention),
		"retention_extend_on_download": s.cfg.ExtendRetentionOnDownload,
		"public_uploads":               s.cfg.PublicUploadsEnabled,
		"endpoints": map[string]string{
			"status":             "/api/v1/status",
			"create_user_hash":   "/api/v1/keys",
			"key_info":           "/api/v1/keys/me",
			"create_upload":      "/api/v1/uploads",
			"upload_status":      "/api/v1/uploads/{id}",
			"upload_parts":       "/api/v1/uploads/{id}/parts",
			"report_upload_part": "/api/v1/uploads/{id}/parts/{n}",
			"complete_upload":    "/api/v1/uploads/{id}/complete",
			"abort_upload":       "/api/v1/uploads/{id}",
			"list":               "/api/v1/files",
			"metadata":           "/api/v1/files/{id}",
			"download":           "/api/v1/files/{id}/download",
			"delete":             "/api/v1/files/{id}",
		},
	})
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if allowed, retry := s.limiter.allow("status:"+s.clientIP(r), 60, time.Minute); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many status requests. Try again shortly.")
		return
	}
	status, err := s.status(r.Context())
	if err != nil {
		s.logger.Error("api status", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read storage status.")
		return
	}
	s.writeJSON(w, http.StatusOK, statusDTO{
		AcceptingUploads: status.Accepting,
		PublicUploads:    status.PublicUploads,
		Storage: storageDTO{
			Files:          status.Files,
			UsedBytes:      status.Used,
			ReservedBytes:  status.Reserved,
			LimitBytes:     status.Limit,
			AvailableBytes: status.Available,
			UsedPercent:    status.UsedPercent,
			Unlimited:      status.Unlimited,
		},
		RetentionSeconds:          int64(s.cfg.Retention.Seconds()),
		RetentionHuman:            retentionLabel(s.cfg.Retention),
		RetentionExtendOnDownload: s.cfg.ExtendRetentionOnDownload,
		MaxFileSize:               s.cfg.MaxFileSize,
		DownloadURLTTL:            int64(s.cfg.DownloadTTL.Seconds()),
		ServerTime:                s.now().UTC(),
	})
}

func (s *Server) fileDTO(file store.File) fileDTO {
	owner := "anonymous"
	if file.APIKeyID != nil {
		owner = "user_hash"
	}
	expiresIn := int64(file.ExpiresAt.Sub(s.now()).Seconds())
	neverExpires := neverExpiresAt(file.ExpiresAt)
	if neverExpires {
		expiresIn = 0
	} else if expiresIn < 0 {
		expiresIn = 0
	}
	return fileDTO{
		ID:           file.ID,
		Name:         file.OriginalName,
		Size:         file.Size,
		ContentType:  file.ContentType,
		CreatedAt:    file.CreatedAt,
		ExpiresAt:    file.ExpiresAt,
		ExpiresIn:    expiresIn,
		NeverExpires: neverExpires,
		PageURL:      s.pageURL(file.ID),
		APIURL:       s.apiFileURL(file.ID),
		Owner:        owner,
	}
}

func (s *Server) handleFileMetadata(w http.ResponseWriter, r *http.Request) {
	file, err := s.store.FileByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAPIError(w, r, http.StatusNotFound, "not_found", "File not found.")
			return
		}
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read file metadata.")
		return
	}
	if !file.ExpiresAt.After(s.now()) {
		s.writeAPIError(w, r, http.StatusGone, "expired", "This file has expired and is being deleted.")
		return
	}
	s.writeJSON(w, http.StatusOK, s.fileDTO(file))
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	key, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	limit := 50
	offset := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 1_000_000 {
			offset = n
		}
	}
	files, total, err := s.store.FilesByAPIKey(r.Context(), key.ID, s.now(), limit, offset)
	if err != nil {
		s.logger.Error("list api files", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not list files.")
		return
	}
	items := make([]fileDTO, 0, len(files))
	for _, file := range files {
		items = append(items, s.fileDTO(file))
	}
	s.writeJSON(w, http.StatusOK, listFilesDTO{
		Files: items,
		Pagination: paginationDTO{
			Total:   total,
			Limit:   limit,
			Offset:  offset,
			HasMore: int64(offset+len(items)) < total,
		},
	})
}

func (s *Server) handleAPIDownload(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if allowed, retry := s.limiter.allow("download:"+ip, 120, 10*time.Minute); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many download links requested. Try again shortly.")
		return
	}

	file, err := s.store.FileByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAPIError(w, r, http.StatusNotFound, "not_found", "File not found.")
			return
		}
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read file metadata.")
		return
	}
	if !file.ExpiresAt.After(s.now()) {
		s.writeAPIError(w, r, http.StatusGone, "expired", "This file has expired and is being deleted.")
		return
	}
	file = s.retainOnDownload(r.Context(), file)

	url, expires, err := s.downloadURL(r.Context(), file)
	if err != nil {
		s.logger.Error("create download link", "file_id", s.logID(file.ID), "error", err)
		s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not create a download link.")
		return
	}
	s.writeJSON(w, http.StatusOK, downloadDTO{
		URL:       url,
		Filename:  file.OriginalName,
		ExpiresAt: expires,
	})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	if allowed, retry := s.limiter.allow("deletefile:"+s.clientIP(r)+":"+r.PathValue("id"), 30, 10*time.Minute); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many delete attempts. Try again shortly.")
		return
	}
	file, err := s.store.FileByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAPIError(w, r, http.StatusNotFound, "not_found", "File not found.")
			return
		}
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read file metadata.")
		return
	}
	if !file.ExpiresAt.After(s.now()) {
		s.writeAPIError(w, r, http.StatusGone, "expired", "This file has expired and is being deleted.")
		return
	}

	key, authErr := s.optionalAPIKey(r)
	if authErr != nil {
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "A valid user hash is required.")
		return
	}
	authorized := key != nil && file.APIKeyID != nil && *file.APIKeyID == key.ID
	// The delete token remains valid even when the request also carries a
	// non-owner user hash. Otherwise a browser with a saved hash could never
	// delete an anonymous file it uploaded earlier.
	if !authorized {
		token := strings.TrimSpace(r.Header.Get("X-Delete-Token"))
		if token != "" {
			if len(token) > 256 || !hashEquals(tokenHash(token), file.DeleteTokenHash) {
				if key != nil {
					s.writeAPIError(w, r, http.StatusNotFound, "not_found", "File not found.")
					return
				}
				s.writeAPIError(w, r, http.StatusForbidden, "forbidden", "The delete token is invalid.")
				return
			}
			authorized = true
		}
	}
	if !authorized {
		if key != nil {
			s.writeAPIError(w, r, http.StatusNotFound, "not_found", "File not found.")
			return
		}
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "Provide a user hash or the file's delete token.")
		return
	}

	if err := s.objects.Delete(r.Context(), file.ObjectKey); err != nil {
		s.logger.Error("delete object", "file_id", s.logID(file.ID), "error", err)
		s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not delete the stored object.")
		return
	}
	deleted, err := s.store.DeleteFileRow(r.Context(), file.ID)
	if err != nil {
		s.logger.Error("delete file row", "file_id", s.logID(file.ID), "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not delete file metadata.")
		return
	}
	if !deleted {
		s.writeAPIError(w, r, http.StatusNotFound, "not_found", "File not found.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func cleanLabel(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(stripFormattingRune, value)
	if len(value) > 64 {
		cut := 64
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = value[:cut]
	}
	return value
}

func (s *Server) deleteObjectDetached(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.objects.Delete(ctx, key); err != nil {
		s.logger.Error("cleanup object after failed upload", "key", key, "error", err)
	}
}

func (s *Server) releaseReservationDetached(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.ReleaseReservation(ctx, id); err != nil {
		s.logger.Error("release upload reservation", "reservation", id, "error", err)
	}
}

func hashEquals(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
