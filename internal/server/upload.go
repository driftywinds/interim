package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"interim/internal/config"
	"interim/internal/store"
	"interim/internal/units"
)

type boundedFileReader struct {
	reader io.Reader
	max    int64
	n      int64
}

func (b *boundedFileReader) Read(p []byte) (int, error) {
	if b.n > b.max {
		return 0, uploadTooLarge
	}
	remaining := b.max + 1 - b.n
	if remaining <= 0 {
		return 0, uploadTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.reader.Read(p)
	b.n += int64(n)
	return n, err
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	key, err := s.optionalAPIKey(r)
	if err != nil {
		s.writeAPIError(w, r, http.StatusUnauthorized, "unauthorized", "The supplied user hash is invalid or revoked.")
		return
	}

	maxRequest := s.cfg.MaxFileSize + config.UploadOverheadAllowance
	if r.ContentLength > maxRequest {
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", fmt.Sprintf("The upload exceeds the %s per-file limit.", unitsLabel(s.cfg.MaxFileSize)))
		return
	}

	declared, hasDeclared := parsePositiveInt64(r.Header.Get("X-File-Size"))
	if hasDeclared && declared > s.cfg.MaxFileSize {
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", fmt.Sprintf("The file exceeds the %s per-file limit.", unitsLabel(s.cfg.MaxFileSize)))
		return
	}

	reserveBytes := declared
	if !hasDeclared {
		if r.ContentLength <= 0 {
			s.writeAPIError(w, r, http.StatusLengthRequired, "length_required", "Content-Length is required unless X-File-Size is supplied.")
			return
		}
		reserveBytes = r.ContentLength
		if reserveBytes > s.cfg.MaxFileSize {
			reserveBytes = s.cfg.MaxFileSize
		}
	}
	if reserveBytes <= 0 {
		s.writeAPIError(w, r, http.StatusBadRequest, "empty_file", "The file is empty.")
		return
	}

	now := s.now().UTC()
	reservationID, err := randomToken(16)
	if err != nil {
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not start the upload.")
		return
	}
	reserved, err := s.store.ReserveStorage(r.Context(), reservationID, reserveBytes, s.cfg.MaxStorage, now, now.Add(maxReservation))
	if err != nil {
		s.logger.Error("reserve storage", "error", err)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not reserve storage.")
		return
	}
	if !reserved {
		s.writeAPIError(w, r, http.StatusInsufficientStorage, "storage_full", "Storage maxed out — please try again later.")
		return
	}

	finalized := false
	defer func() {
		if !finalized {
			s.releaseReservationDetached(reservationID)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	multipartReader, err := r.MultipartReader()
	if err != nil {
		s.writeAPIError(w, r, http.StatusUnsupportedMediaType, "invalid_upload", "Uploads must use multipart/form-data with a file field.")
		return
	}

	var (
		part        *multipart.Part
		fileLimit   = s.cfg.MaxFileSize
		fileName    string
		contentType = "application/octet-stream"
	)
	if hasDeclared {
		fileLimit = declared
	}
	for {
		part, err = multipartReader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.writeAPIError(w, r, http.StatusBadRequest, "invalid_upload", "The multipart upload could not be read.")
			return
		}
		if part.FormName() == "file" {
			fileName = sanitizeFilename(part.FileName())
			contentType = parseContentType(part.Header.Get("Content-Type"))
			break
		}
		_ = part.Close()
	}
	if part == nil {
		s.writeAPIError(w, r, http.StatusBadRequest, "invalid_upload", "A file field is required.")
		return
	}
	defer part.Close()

	fileID, err := randomToken(12)
	if err != nil {
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not allocate a file ID.")
		return
	}
	objectKey := s.cfg.S3.Prefix + fileID
	reader := &boundedFileReader{reader: part, max: fileLimit}

	putErr := s.objects.Put(r.Context(), objectKey, reader, contentType, fileName)
	if putErr != nil {
		var maxBytesErr *http.MaxBytesError
		switch {
		case errors.Is(putErr, uploadTooLarge) || errors.As(putErr, &maxBytesErr):
			s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "The upload exceeds the declared or configured size limit.")
		case errors.Is(putErr, context.Canceled), errors.Is(putErr, context.DeadlineExceeded):
			// The client went away; no response body matters.
			return
		default:
			s.logger.Error("upload object", "error", putErr)
			s.writeAPIError(w, r, http.StatusBadGateway, "storage_error", "Upload failed while writing to storage.")
		}
		return
	}
	if reader.n <= 0 {
		s.deleteObjectDetached(objectKey)
		s.writeAPIError(w, r, http.StatusBadRequest, "empty_file", "The file is empty.")
		return
	}
	if reader.n > fileLimit {
		s.deleteObjectDetached(objectKey)
		s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "The upload exceeds the declared or configured size limit.")
		return
	}

	deleteToken, err := randomToken(32)
	if err != nil {
		s.deleteObjectDetached(objectKey)
		s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not create a delete token.")
		return
	}
	createdAt := s.now().UTC()
	file := store.File{
		ID:              fileID,
		ObjectKey:       objectKey,
		APIKeyID:        apiKeyID(key),
		OriginalName:    fileName,
		ContentType:     contentType,
		Size:            reader.n,
		DeleteTokenHash: tokenHash(deleteToken),
		CreatedAt:       createdAt,
		ExpiresAt:       createdAt.Add(s.cfg.Retention),
	}

	if err := s.store.CreateFileWithReservation(r.Context(), file, reservationID, reserveBytes); err != nil {
		s.deleteObjectDetached(objectKey)
		switch {
		case errors.Is(err, store.ErrReservationMissing):
			s.logger.Warn("reservation expired before upload commit", "reservation", reservationID)
			s.writeAPIError(w, r, http.StatusInsufficientStorage, "storage_full", "Storage reservation expired. Please try again.")
		case errors.Is(err, store.ErrReservationTooSmall):
			s.writeAPIError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "Uploaded data exceeded the reserved capacity.")
		default:
			s.logger.Error("commit uploaded file", "error", err)
			s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "Could not save file metadata.")
		}
		return
	}
	finalized = true

	response := uploadDTO{
		fileDTO:           s.fileDTO(file),
		DeleteToken:       deleteToken,
		DeleteTokenHeader: "X-Delete-Token",
		Warning:           "Keep delete_token private. It is the only way to delete this anonymous upload.",
	}
	w.Header().Set("Location", response.APIURL)
	s.writeJSON(w, http.StatusCreated, response)
}

func apiKeyID(key *store.APIKey) *string {
	if key == nil {
		return nil
	}
	id := key.ID
	return &id
}

func unitsLabel(bytes int64) string {
	return units.FormatBytes(bytes)
}
