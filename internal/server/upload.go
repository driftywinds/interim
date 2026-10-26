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
	"strconv"
	"strings"
	"time"

	"interim/internal/config"
	"interim/internal/objects"
	"interim/internal/store"
	"interim/internal/units"
)

const uploadTokenHeader = "X-Upload-Token"

type uploadIntentDTO struct {
	UploadID          string            `json:"upload_id"`
	Mode              string            `json:"mode"`
	URL               string            `json:"url,omitempty"`
	Method            string            `json:"method,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	PartSize          int64             `json:"part_size,omitempty"`
	PartCount         int               `json:"part_count,omitempty"`
	Concurrency       int               `json:"concurrency,omitempty"`
	PartsURL          string            `json:"parts_url,omitempty"`
	StatusURL         string            `json:"status_url,omitempty"`
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

type uploadStatusDTO struct {
	UploadID       string    `json:"upload_id"`
	Mode           string    `json:"mode"`
	State          string    `json:"state"`
	PartSize       int64     `json:"part_size,omitempty"`
	PartCount      int       `json:"part_count,omitempty"`
	Concurrency    int       `json:"concurrency,omitempty"`
	CompletedParts []int     `json:"completed_parts"`
	ExpiresAt      time.Time `json:"expires_at"`
	CompleteURL    string    `json:"complete_url"`
	AbortURL       string    `json:"abort_url"`
	PartsURL       string    `json:"parts_url,omitempty"`
}

type uploadPartURLDTO struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type uploadPartURLsDTO struct {
	Parts []uploadPartURLDTO `json:"parts"`
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
	multipart := s.cfg.MultipartEnabled && request.Size > s.cfg.MultipartThreshold
	if !multipart && request.Size > config.MaxSinglePutSize {
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "Direct uploads support at most 5 GB per file.")
		return
	}
	contentType := objects.SanitizeContentType(request.ContentType)

	var partSize int64
	var partCount int
	if multipart {
		partSize = s.multipartPartSize(request.Size)
		partCount = int((request.Size + partSize - 1) / partSize)
		if partCount < 1 || partCount > config.MaxMultipartParts {
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not choose a multipart part size.")
			return
		}
	}

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
		PartSize:        partSize,
		PartCount:       partCount,
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
	response := uploadIntentDTO{
		UploadID:          uploadID,
		Mode:              "single",
		IntentExpiresAt:   intentExpires,
		ExpiresAt:         intentExpires,
		UploadToken:       uploadToken,
		CompleteURL:       s.cfg.PublicBaseURL + "/api/v1/uploads/" + uploadID + "/complete",
		AbortURL:          s.cfg.PublicBaseURL + "/api/v1/uploads/" + uploadID,
		DeleteToken:       deleteToken,
		DeleteTokenHeader: "X-Delete-Token",
		Warning:           "Upload directly to the signed URL, then call complete_url with the upload token. Keep delete_token private.",
	}
	if multipart {
		multipartID, err := s.objects.CreateMultipart(r.Context(), objectKey, contentType, disposition)
		if err != nil {
			s.logger.Error("create multipart upload", "upload_id", uploadID, "error", err)
			_ = s.store.CloseUploadIntent(r.Context(), uploadID, "failed", s.now())
			s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not create a multipart upload.")
			return
		}
		if err := s.store.SetUploadMultipartID(r.Context(), uploadID, multipartID); err != nil {
			s.abortMultipartObject(objectKey, multipartID)
			_ = s.store.CloseUploadIntent(r.Context(), uploadID, "failed", s.now())
			s.logger.Error("save multipart upload id", "upload_id", uploadID, "error", err)
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create a multipart upload.")
			return
		}
		response.Mode = "multipart"
		response.PartSize = partSize
		response.PartCount = partCount
		response.Concurrency = s.cfg.MultipartConcurrency
		response.PartsURL = s.cfg.PublicBaseURL + "/api/v1/uploads/" + uploadID + "/parts"
		response.StatusURL = s.cfg.PublicBaseURL + "/api/v1/uploads/" + uploadID
		response.Warning = "Request part URLs, PUT each part, report its ETag, then call complete_url with the upload token. Keep delete_token private."
	} else {
		url, expiresAt, err := s.objects.PresignPut(r.Context(), objectKey, contentType, disposition, s.cfg.UploadURLTTL)
		if err != nil {
			s.logger.Error("presign upload", "upload_id", uploadID, "error", err)
			if closeErr := s.store.CloseUploadIntent(r.Context(), uploadID, "failed", s.now()); closeErr != nil {
				s.logger.Error("close failed upload intent", "upload_id", uploadID, "error", closeErr)
			}
			s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not create an upload URL.")
			return
		}
		response.URL = url
		response.Method = http.MethodPut
		response.Headers = map[string]string{
			"Content-Type":        contentType,
			"Content-Disposition": disposition,
		}
		response.ExpiresAt = expiresAt
	}
	w.Header().Set("Location", response.CompleteURL)
	s.writeJSON(w, http.StatusCreated, response)
}

func validMultipartParts(intent store.UploadIntent, parts []store.UploadPart) bool {
	if len(parts) != intent.PartCount || intent.PartCount < 1 || intent.PartSize <= 0 {
		return false
	}
	for i, part := range parts {
		if part.PartNumber != i+1 || part.Size <= 0 || part.Size > intent.PartSize || part.ETag == "" {
			return false
		}
	}
	return true
}

func (s *Server) multipartPartSize(size int64) int64 {
	partSize := s.cfg.MultipartPartSize
	if partSize <= 0 {
		partSize = config.MinMultipartPartSize
	}
	minPartSize := (size + int64(config.MaxMultipartParts) - 1) / int64(config.MaxMultipartParts)
	if partSize < minPartSize {
		partSize = minPartSize
	}
	if partSize < 1 {
		partSize = 1
	}
	return partSize
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

	if intent.MultipartUploadID != "" {
		parts, err := s.store.UploadParts(r.Context(), intent.ID)
		if err != nil {
			s.logger.Error("list upload parts", "upload_id", intent.ID, "error", err)
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read the uploaded parts.")
			return
		}
		if !validMultipartParts(intent, parts) {
			s.writeAPIError(w, r, http.StatusConflict, "upload_parts_incomplete", "Upload all parts from 1 through part_count and report each ETag before completing.")
			return
		}
		completedParts := make([]objects.MultipartPart, 0, len(parts))
		for _, part := range parts {
			completedParts = append(completedParts, objects.MultipartPart{
				PartNumber: int32(part.PartNumber),
				ETag:       part.ETag,
			})
		}
		if _, err := s.objects.Head(r.Context(), intent.ObjectKey); errors.Is(err, objects.ErrNotFound) {
			if err := s.objects.CompleteMultipart(r.Context(), intent.ObjectKey, intent.MultipartUploadID, completedParts); err != nil {
				// A retry can race a completion that already succeeded. If the
				// staging object now exists, continue with the common checks.
				if _, headErr := s.objects.Head(r.Context(), intent.ObjectKey); headErr != nil {
					s.logger.Error("complete multipart upload", "upload_id", intent.ID, "error", err)
					s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not complete the multipart upload. Retry completion.")
					return
				}
			}
		} else if err != nil {
			s.logger.Error("head multipart staging object", "upload_id", intent.ID, "error", err)
			s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not verify the multipart upload.")
			return
		}
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

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	intent, ok := s.authenticateUploadIntent(w, r)
	if !ok {
		return
	}
	if intent.State != "pending" && intent.State != "committed" {
		s.writeAPIError(w, r, http.StatusGone, "upload_closed", "This upload is no longer available.")
		return
	}
	if intent.State == "pending" && !intent.ExpiresAt.After(s.now().UTC()) {
		s.expireUploadIntent(r.Context(), intent)
		s.writeAPIError(w, r, http.StatusGone, "upload_expired", "The upload window expired.")
		return
	}
	completed := make([]int, 0)
	if intent.MultipartUploadID != "" {
		parts, err := s.store.UploadParts(r.Context(), intent.ID)
		if err != nil {
			s.logger.Error("list upload parts for status", "upload_id", intent.ID, "error", err)
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not read upload parts.")
			return
		}
		for _, part := range parts {
			completed = append(completed, part.PartNumber)
		}
	}
	mode := "single"
	partsURL := ""
	if intent.MultipartUploadID != "" {
		mode = "multipart"
		partsURL = s.cfg.PublicBaseURL + "/api/v1/uploads/" + intent.ID + "/parts"
	}
	s.writeJSON(w, http.StatusOK, uploadStatusDTO{
		UploadID:       intent.ID,
		Mode:           mode,
		State:          intent.State,
		PartSize:       intent.PartSize,
		PartCount:      intent.PartCount,
		Concurrency:    s.cfg.MultipartConcurrency,
		CompletedParts: completed,
		ExpiresAt:      intent.ExpiresAt,
		CompleteURL:    s.cfg.PublicBaseURL + "/api/v1/uploads/" + intent.ID + "/complete",
		AbortURL:       s.cfg.PublicBaseURL + "/api/v1/uploads/" + intent.ID,
		PartsURL:       partsURL,
	})
}

func (s *Server) handleUploadPartURLs(w http.ResponseWriter, r *http.Request) {
	intent, ok := s.authenticateUploadIntent(w, r)
	if !ok {
		return
	}
	if intent.State != "pending" {
		s.writeAPIError(w, r, http.StatusGone, "upload_closed", "This upload is no longer pending.")
		return
	}
	if !intent.ExpiresAt.After(s.now().UTC()) {
		s.expireUploadIntent(r.Context(), intent)
		s.writeAPIError(w, r, http.StatusGone, "upload_expired", "The upload window expired.")
		return
	}
	if intent.MultipartUploadID == "" {
		s.writeAPIError(w, r, http.StatusBadRequest, "not_multipart", "This upload does not use multipart parts.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request struct {
		PartNumbers []int `json:"part_numbers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "A JSON body with part_numbers is required.")
		return
	}
	if len(request.PartNumbers) == 0 || len(request.PartNumbers) > 100 {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "Request between 1 and 100 part URLs at a time.")
		return
	}
	seen := make(map[int]struct{}, len(request.PartNumbers))
	response := uploadPartURLsDTO{Parts: make([]uploadPartURLDTO, 0, len(request.PartNumbers))}
	for _, partNumber := range request.PartNumbers {
		if partNumber < 1 || partNumber > intent.PartCount {
			s.writeAPIError(w, r, http.StatusBadRequest, "invalid_part", "Part number is outside this upload.")
			return
		}
		if _, duplicate := seen[partNumber]; duplicate {
			continue
		}
		seen[partNumber] = struct{}{}
		url, expiresAt, err := s.objects.PresignUploadPart(r.Context(), intent.ObjectKey, intent.MultipartUploadID, partNumber, s.cfg.UploadURLTTL)
		if err != nil {
			s.logger.Error("presign upload part", "upload_id", intent.ID, "part", partNumber, "error", err)
			s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Could not create a part URL.")
			return
		}
		response.Parts = append(response.Parts, uploadPartURLDTO{
			PartNumber: partNumber,
			URL:        url,
			ExpiresAt:  expiresAt,
		})
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleUploadPartReport(w http.ResponseWriter, r *http.Request) {
	intent, ok := s.authenticateUploadIntent(w, r)
	if !ok {
		return
	}
	if intent.State != "pending" {
		s.writeAPIError(w, r, http.StatusGone, "upload_closed", "This upload is no longer pending.")
		return
	}
	if !intent.ExpiresAt.After(s.now().UTC()) {
		s.expireUploadIntent(r.Context(), intent)
		s.writeAPIError(w, r, http.StatusGone, "upload_expired", "The upload window expired.")
		return
	}
	if intent.MultipartUploadID == "" {
		s.writeAPIError(w, r, http.StatusBadRequest, "not_multipart", "This upload does not use multipart parts.")
		return
	}
	partNumber, err := strconv.Atoi(r.PathValue("part"))
	if err != nil || partNumber < 1 || partNumber > intent.PartCount {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_part", "Part number is outside this upload.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var request struct {
		ETag string `json:"etag"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_request", "A JSON body with etag and size is required.")
		return
	}
	request.ETag = strings.TrimSpace(request.ETag)
	if !validETag(request.ETag) {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_etag", "The part ETag is invalid.")
		return
	}
	if request.Size <= 0 || request.Size > intent.PartSize {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_part_size", "The part size is invalid.")
		return
	}
	if err := s.store.SaveUploadPart(r.Context(), intent.ID, partNumber, request.ETag, request.Size, s.now()); err != nil {
		s.logger.Error("save upload part", "upload_id", intent.ID, "part", partNumber, "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not record the uploaded part.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func validETag(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) abortMultipartObject(key, uploadID string) {
	if uploadID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.objects.AbortMultipart(ctx, key, uploadID); err != nil {
		s.logger.Warn("abort multipart upload", "key", key, "upload_id", uploadID, "error", err)
	}
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

	s.abortMultipartObject(intent.ObjectKey, intent.MultipartUploadID)
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
	s.abortMultipartObject(intent.ObjectKey, intent.MultipartUploadID)
	if err := s.objects.Delete(ctx, intent.ObjectKey); err != nil {
		s.logger.Error("delete expired upload object", "upload_id", intent.ID, "error", err)
	}
	if err := s.store.CloseUploadIntent(ctx, intent.ID, "expired", s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("close expired upload", "upload_id", intent.ID, "error", err)
	}
}

func (s *Server) failUploadIntent(ctx context.Context, intent store.UploadIntent, reason string) {
	s.logger.Warn("upload failed verification", "upload_id", intent.ID, "reason", reason)
	s.abortMultipartObject(intent.ObjectKey, intent.MultipartUploadID)
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
