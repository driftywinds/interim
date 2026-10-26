package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"interim/internal/config"
	"interim/internal/objects"
	"interim/internal/store"
	"interim/internal/units"
)

const uploadTokenHeader = "X-Upload-Token"

type uploadIntentDTO struct {
	UploadID          string            `json:"upload_id"`
	URL               string            `json:"url"`
	Method            string            `json:"method"`
	Headers           map[string]string `json:"headers"`
	ExpiresAt         time.Time         `json:"expires_at"`
	IntentExpiresAt   time.Time         `json:"intent_expires_at"`
	UploadToken       string            `json:"upload_token"`
	CompleteURL       string            `json:"complete_url"`
	AbortURL          string            `json:"abort_url"`
	DeleteToken       string            `json:"delete_token"`
	DeleteTokenHeader string            `json:"delete_token_header"`
	Warning           string            `json:"warning"`
}

type completeUploadDTO struct {
	UploadID string  `json:"upload_id"`
	File     fileDTO `json:"file"`
}

func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	key, err := s.optionalAPIKey(r)
	if err != nil {
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "The supplied user hash is invalid or revoked.")
		return
	}
	if !s.cfg.PublicUploadsEnabled {
		if key == nil {
			s.writeAPIError(w, r, http.StatusUnauthorized, "uploads_restricted", "Uploads are restricted to approved user hashes. Ask the admin for a user hash.")
			return
		}
		if !key.Approved {
			s.writeAPIError(w, r, http.StatusForbidden, "upload_not_allowed", "This user hash is valid but is not approved for uploads.")
			return
		}
	}
	ip := s.clientIP(r)
	if allowed, retry := s.limiter.allow("uploadintent:"+ip, 120, 10*time.Minute); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many uploads requested. Try again shortly.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request struct {
		Name        string `json:"name"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			s.writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "A JSON body with name and size is required.")
			return
		}
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
		return
	}

	name := sanitizeFilename(request.Name)
	if request.Size <= 0 {
		s.writeAPIError(w, r, http.StatusBadRequest, "empty_file", "The file size must be greater than zero.")
		return
	}
	if request.Size > s.cfg.MaxFileSize {
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", fmt.Sprintf("The file exceeds the %s per-file limit.", units.FormatBytes(s.cfg.MaxFileSize)))
		return
	}
	if request.Size > config.MaxSinglePutSize {
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "Direct uploads support at most 5 GB per file.")
		return
	}
	contentType := objects.SanitizeContentType(request.ContentType)

	now := s.now().UTC()
	uploadID, err := randomToken(12)
	if err != nil {
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create an upload.")
		return
	}
	uploadToken, err := randomToken(32)
	if err != nil {
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create an upload.")
		return
	}
	deleteToken, err := randomToken(32)
	if err != nil {
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create an upload.")
		return
	}

	objectKey := s.cfg.S3.Prefix + "uploads/" + uploadID
	intentExpires := now.Add(s.cfg.UploadIntentTTL)
	reserved, err := s.store.ReserveStorage(r.Context(), uploadID, request.Size, s.reservationOwner(key, ip), store.ReservationLimits{
		TotalBytes:    s.cfg.MaxStorage,
		TotalFiles:    s.cfg.MaxFiles,
		ClientBytes:   s.cfg.MaxReservedBytesPerClient,
		ClientPending: s.cfg.MaxPendingUploadsPerClient,
	}, now, intentExpires)
	if err != nil {
		s.logger.Error("reserve upload storage", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not reserve storage.")
		return
	}
	if !reserved {
		s.writeAPIError(w, r, http.StatusInsufficientStorage, "storage_full", "Storage maxed out or too many pending uploads — please try again later.")
		return
	}

	intent := store.UploadIntent{
		ID:              uploadID,
		ObjectKey:       objectKey,
		APIKeyID:        apiKeyID(key),
		OriginalName:    name,
		ContentType:     contentType,
		DeclaredSize:    request.Size,
		UploadTokenHash: tokenHash(uploadToken),
		DeleteTokenHash: tokenHash(deleteToken),
		State:           "pending",
		CreatedAt:       now,
		ExpiresAt:       intentExpires,
	}
	if err := s.store.CreateUploadIntent(r.Context(), intent); err != nil {
		s.releaseReservationDetached(uploadID)
		s.logger.Error("create upload intent", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create an upload.")
		return
	}

	disposition := objects.AttachmentDisposition(name)
	url, expiresAt, err := s.objects.PresignPut(r.Context(), objectKey, contentType, disposition, s.cfg.UploadURLTTL)
	if err != nil {
		s.logger.Error("presign upload", "upload_id", uploadID, "error", err)
		if closeErr := s.store.CloseUploadIntent(r.Context(), uploadID, "failed", s.now()); closeErr != nil {
			s.logger.Error("close failed upload intent", "upload_id", uploadID, "error", closeErr)
		}
		s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not create an upload URL.")
		return
	}

	response := uploadIntentDTO{
		UploadID: uploadID,
		URL:      url,
		Method:   http.MethodPut,
		Headers: map[string]string{
			"Content-Type":        contentType,
			"Content-Disposition": disposition,
		},
		ExpiresAt:         expiresAt,
		IntentExpiresAt:   intentExpires,
		UploadToken:       uploadToken,
		CompleteURL:       s.cfg.PublicBaseURL + "/api/v1/uploads/" + uploadID + "/complete",
		AbortURL:          s.cfg.PublicBaseURL + "/api/v1/uploads/" + uploadID,
		DeleteToken:       deleteToken,
		DeleteTokenHeader: "X-Delete-Token",
		Warning:           "Upload directly to the signed URL, then call complete_url with the upload token. Keep delete_token private.",
	}
	w.Header().Set("Location", response.CompleteURL)
	s.writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	intent, ok := s.authenticateUploadIntent(w, r)
	if !ok {
		return
	}

	switch intent.State {
	case "committed":
		if intent.FileID == nil {
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "The upload was committed but its file link is missing.")
			return
		}
		file, err := s.store.FileByID(r.Context(), *intent.FileID)
		if err != nil {
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "The upload was committed but its file record is unavailable.")
			return
		}
		s.writeJSON(w, http.StatusOK, completeUploadDTO{UploadID: intent.ID, File: s.fileDTO(file)})
		return
	case "aborted", "expired", "failed":
		s.writeAPIError(w, r, http.StatusGone, "upload_closed", "This upload is no longer pending.")
		return
	}

	now := s.now().UTC()
	if !intent.ExpiresAt.After(now) {
		s.expireUploadIntent(r.Context(), intent)
		s.writeAPIError(w, r, http.StatusGone, "upload_expired", "The upload window expired. The temporary object has been deleted.")
		return
	}

	info, err := s.objects.Head(r.Context(), intent.ObjectKey)
	if err != nil {
		if errors.Is(err, objects.ErrNotFound) {
			s.writeAPIError(w, r, http.StatusConflict, "upload_missing", "The object has not reached storage yet. Retry completion after the PUT finishes.")
			return
		}
		s.logger.Error("head uploaded object", "upload_id", intent.ID, "error", err)
		s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not verify the uploaded object.")
		return
	}

	switch {
	case info.Size <= 0:
		s.failUploadIntent(r.Context(), intent, "Uploaded object is empty.")
		s.writeAPIError(w, r, http.StatusBadRequest, "empty_file", "The uploaded object is empty.")
		return
	case info.Size > intent.DeclaredSize:
		s.failUploadIntent(r.Context(), intent, "Uploaded object exceeds declared size.")
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "The uploaded object is larger than the declared size.")
		return
	case info.Size > s.cfg.MaxFileSize:
		s.failUploadIntent(r.Context(), intent, "Uploaded object exceeds per-file cap.")
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "The uploaded object exceeds the per-file limit.")
		return
	}

	liveKey, err := s.newLiveObjectKey()
	if err != nil {
		s.logger.Error("generate live object key", "upload_id", intent.ID, "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not finalize the uploaded file.")
		return
	}
	if err := s.objects.Copy(
		r.Context(),
		intent.ObjectKey,
		liveKey,
		intent.ContentType,
		objects.AttachmentDisposition(intent.OriginalName),
	); err != nil {
		// Another completion may have committed the intent while this one was
		// copying. Return the committed file instead of failing the retry.
		if committed, ok := s.committedFile(r.Context(), intent.ID); ok {
			s.writeJSON(w, http.StatusOK, completeUploadDTO{UploadID: intent.ID, File: s.fileDTO(committed)})
			return
		}
		s.logger.Error("copy uploaded object", "upload_id", intent.ID, "error", err)
		s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not finalize the uploaded object. Retry completion.")
		return
	}

	completedAt := s.now().UTC()
	file := store.File{
		ID:              intent.ID,
		ObjectKey:       liveKey,
		APIKeyID:        intent.APIKeyID,
		OriginalName:    intent.OriginalName,
		ContentType:     intent.ContentType,
		Size:            info.Size,
		DeleteTokenHash: intent.DeleteTokenHash,
		CreatedAt:       completedAt,
		ExpiresAt:       completedAt.Add(s.cfg.Retention),
	}
	if err := s.store.CommitUploadIntent(r.Context(), intent.ID, file, completedAt); err != nil {
		// A commit error can be ambiguous on an embedded database. If the row
		// is present, keep the live object and return it instead of deleting it.
		if committed, ok := s.committedFile(r.Context(), intent.ID); ok {
			if committed.ObjectKey != liveKey {
				s.deleteObjectBestEffort(liveKey, "delete duplicate live object")
			}
			s.writeJSON(w, http.StatusOK, completeUploadDTO{UploadID: intent.ID, File: s.fileDTO(committed)})
			return
		}
		s.deleteObjectBestEffort(liveKey, "delete uncommitted live object")
		switch {
		case errors.Is(err, store.ErrUploadIntentState):
			s.writeAPIError(w, r, http.StatusConflict, "upload_closed", "The upload is no longer pending.")
		case errors.Is(err, store.ErrReservationMissing):
			s.deleteObjectBestEffort(intent.ObjectKey, "delete staging object after missing reservation")
			_ = s.store.CloseUploadIntent(context.Background(), intent.ID, "failed", s.now())
			s.logger.Warn("upload reservation missing at completion", "upload_id", intent.ID)
			s.writeAPIError(w, r, http.StatusGone, "upload_expired", "The upload reservation is no longer valid.")
		default:
			s.logger.Error("commit upload intent", "upload_id", intent.ID, "error", err)
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not commit the uploaded file.")
		}
		return
	}

	s.deleteObjectBestEffort(intent.ObjectKey, "delete staging object after commit")
	s.writeJSON(w, http.StatusCreated, completeUploadDTO{UploadID: intent.ID, File: s.fileDTO(file)})
}

func (s *Server) newLiveObjectKey() (string, error) {
	token, err := randomToken(16)
	if err != nil {
		return "", err
	}
	return s.cfg.S3.Prefix + "objects/" + token, nil
}

func (s *Server) committedFile(ctx context.Context, intentID string) (store.File, bool) {
	reloaded, err := s.store.UploadIntentByID(ctx, intentID)
	if err != nil || reloaded.State != "committed" || reloaded.FileID == nil {
		return store.File{}, false
	}
	file, err := s.store.FileByID(ctx, *reloaded.FileID)
	if err != nil {
		return store.File{}, false
	}
	return file, true
}

func (s *Server) deleteObjectBestEffort(key, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.objects.Delete(ctx, key); err != nil {
		s.logger.Warn(reason, "key", key, "error", err)
	}
}

func (s *Server) reservationOwner(key *store.APIKey, ip string) string {
	if key != nil {
		return "key:" + key.ID
	}
	mac := hmac.New(sha256.New, s.cfg.SessionSecret)
	_, _ = mac.Write([]byte(ip))
	return "ip:" + hex.EncodeToString(mac.Sum(nil)[:16])
}

func (s *Server) handleAbortUpload(w http.ResponseWriter, r *http.Request) {
	intent, ok := s.authenticateUploadIntent(w, r)
	if !ok {
		return
	}
	switch intent.State {
	case "committed":
		s.writeAPIError(w, r, http.StatusConflict, "already_committed", "This upload has already been committed.")
		return
	case "aborted", "expired", "failed":
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.objects.Delete(ctx, intent.ObjectKey); err != nil {
		s.logger.Error("delete aborted upload object", "upload_id", intent.ID, "error", err)
		s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not remove the uploaded object. The janitor will retry.")
		return
	}
	if err := s.store.CloseUploadIntent(r.Context(), intent.ID, "aborted", s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("close aborted upload", "upload_id", intent.ID, "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not close the upload.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticateUploadIntent(w http.ResponseWriter, r *http.Request) (store.UploadIntent, bool) {
	token := r.Header.Get(uploadTokenHeader)
	if token == "" || len(token) > 256 {
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "The upload token is required.")
		return store.UploadIntent{}, false
	}
	intent, err := s.store.UploadIntentByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAPIError(w, r, http.StatusNotFound, "not_found", "Upload not found.")
			return store.UploadIntent{}, false
		}
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read the upload.")
		return store.UploadIntent{}, false
	}
	if !hashEquals(tokenHash(token), intent.UploadTokenHash) {
		s.writeAPIError(w, r, http.StatusForbidden, "forbidden", "The upload token is invalid.")
		return store.UploadIntent{}, false
	}
	return intent, true
}

func (s *Server) expireUploadIntent(ctx context.Context, intent store.UploadIntent) {
	if err := s.objects.Delete(ctx, intent.ObjectKey); err != nil {
		s.logger.Error("delete expired upload object", "upload_id", intent.ID, "error", err)
	}
	if err := s.store.CloseUploadIntent(ctx, intent.ID, "expired", s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("close expired upload", "upload_id", intent.ID, "error", err)
	}
}

func (s *Server) failUploadIntent(ctx context.Context, intent store.UploadIntent, reason string) {
	s.logger.Warn("upload failed verification", "upload_id", intent.ID, "reason", reason)
	if err := s.objects.Delete(ctx, intent.ObjectKey); err != nil {
		s.logger.Error("delete failed upload object", "upload_id", intent.ID, "error", err)
	}
	if err := s.store.CloseUploadIntent(ctx, intent.ID, "failed", s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("close failed upload", "upload_id", intent.ID, "error", err)
	}
}

func apiKeyID(key *store.APIKey) *string {
	if key == nil {
		return nil
	}
	id := key.ID
	return &id
}
