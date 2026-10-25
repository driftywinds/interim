package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"interim/internal/store"
	"interim/internal/units"
)

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	status, err := s.status(r.Context())
	if err != nil {
		s.logger.Error("home status", "error", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "Storage unavailable", "The storage status could not be read. Please try again.")
		return
	}
	page := s.basePage(r, "Temporary file hosting", "home")
	page.Description = fmt.Sprintf("interim is a temporary file host. Upload anonymously or with a user hash. Every file is permanently deleted after %s.", page.RetentionLabel)
	s.render(w, r, http.StatusOK, "home", HomeView{PageData: page, Status: status})
}

func (s *Server) handleFilePage(w http.ResponseWriter, r *http.Request) {
	file, err := s.store.FileByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Not found", "That file does not exist, or its lifetime is already over.")
			return
		}
		s.logger.Error("file page", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something broke", "Could not read the file details.")
		return
	}

	expired := !file.ExpiresAt.After(s.now())
	page := s.basePage(r, file.OriginalName, "")
	page.Description = fmt.Sprintf("Download %s from interim. Available for %s.", file.OriginalName, page.RetentionLabel)
	view := FilePageView{
		PageData:    page,
		ID:          file.ID,
		Name:        file.OriginalName,
		Size:        file.Size,
		ContentType: file.ContentType,
		CreatedAt:   file.CreatedAt,
		ExpiresAt:   file.ExpiresAt,
		ExpiresUnix: file.ExpiresAt.Unix(),
		ExpiresIn:   expiresIn(file.ExpiresAt),
		PageURL:     s.pageURL(file.ID),
		DownloadTTL: humanDuration(s.cfg.DownloadTTL),
		IsExpired:   expired,
	}
	status := http.StatusOK
	if expired {
		status = http.StatusGone
	}
	s.render(w, r, status, "file", view)
}

func (s *Server) handlePageDownload(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if allowed, retry := s.limiter.allow("download:"+ip, 120, 10*time.Minute); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.renderError(w, r, http.StatusTooManyRequests, "Slow down", "Too many downloads were requested from this address. Try again in a few minutes.")
		return
	}

	file, err := s.store.FileByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Not found", "That file does not exist.")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Something broke", "Could not read the file details.")
		return
	}
	if !file.ExpiresAt.After(s.now()) {
		s.renderError(w, r, http.StatusGone, "Expired", "This file has been permanently deleted.")
		return
	}

	url, _, err := s.downloadURL(r.Context(), file)
	if err != nil {
		s.logger.Error("create page download link", "file_id", file.ID, "error", err)
		s.renderError(w, r, http.StatusBadGateway, "Download unavailable", "The storage provider did not return a download link. Please try again.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (s *Server) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	page := s.basePage(r, "API", "api")
	page.Description = "interim API documentation, endpoints, auth, uploads, downloads, and OpenAPI specification."
	view := APIDocsView{
		PageData:     page,
		APIBase:      s.cfg.PublicBaseURL + "/api/v1",
		OpenAPIJSON:  s.cfg.PublicBaseURL + "/api/openapi.json",
		OpenAPIYAML:  s.cfg.PublicBaseURL + "/api/openapi.yaml",
		MaxFileSize:  units.FormatBytes(s.cfg.MaxFileSize),
		MaxStorage:   units.FormatBytes(s.cfg.MaxStorage),
		DownloadTTL:  humanDuration(s.cfg.DownloadTTL),
		UploadURLTTL: humanDuration(s.cfg.UploadURLTTL),
	}
	s.render(w, r, http.StatusOK, "api", view)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.store.StorageUsage(ctx, s.now()); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "database": "unavailable"})
		return
	}
	if err := s.objects.Health(ctx); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "storage": "unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /admin\nDisallow: /api/v1/keys\nAllow: /api\n"))
}
