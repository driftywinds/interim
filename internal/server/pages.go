package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
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
	if s.cfg.PublicUploadsEnabled {
		page.Description = fmt.Sprintf("interim is a temporary file host. Upload anonymously or with a user hash. Every file is permanently deleted after %s.", page.RetentionLabel)
	} else {
		page.Description = fmt.Sprintf("interim is a temporary file host. Uploads require an admin-approved user hash. Every file is permanently deleted after %s.", page.RetentionLabel)
	}
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
	retention := s.fileRetention(file)
	page.RetentionLabel = retentionLabel(retention)
	if retention <= 0 {
		page.RetentionLabel = "never"
		page.Description = fmt.Sprintf("Download %s from interim. This file does not expire.", file.OriginalName)
	} else {
		page.Description = fmt.Sprintf("Download %s from interim. Available for %s.", file.OriginalName, page.RetentionLabel)
	}
	view := FilePageView{
		PageData:          page,
		ID:                file.ID,
		Name:              file.OriginalName,
		Size:              file.Size,
		ContentType:       file.ContentType,
		CreatedAt:         file.CreatedAt,
		ExpiresAt:         file.ExpiresAt,
		ExpiresUnix:       file.ExpiresAt.Unix(),
		ExpiresIn:         expiresIn(file.ExpiresAt),
		NeverExpires:      neverExpiresAt(file.ExpiresAt),
		FileRetentionRule: s.retentionRule(retention),
		PageURL:           s.pageURL(file.ID),
		DownloadTTL:       humanDuration(s.cfg.DownloadTTL),
		IsExpired:         expired,
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
	file = s.retainOnDownload(r.Context(), file)

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
		PageData:         page,
		APIBase:          s.cfg.PublicBaseURL + "/api/v1",
		OpenAPIJSON:      s.cfg.PublicBaseURL + "/api/openapi.json",
		OpenAPIYAML:      s.cfg.PublicBaseURL + "/api/openapi.yaml",
		MaxFileSize:      units.FormatBytes(s.cfg.MaxFileSize),
		MaxStorage:       units.FormatBytes(s.cfg.MaxStorage),
		StorageUnlimited: s.cfg.MaxStorage == 0,
		PublicUploads:    s.cfg.PublicUploadsEnabled,
		MultipartEnabled: s.cfg.MultipartEnabled,
		DownloadTTL:      humanDuration(s.cfg.DownloadTTL),
		UploadURLTTL:     humanDuration(s.cfg.UploadURLTTL),
	}
	s.render(w, r, http.StatusOK, "api", view)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	detail := s.loopbackRequest(r)
	if _, err := s.store.StorageUsage(ctx, s.now()); err != nil {
		body := map[string]string{"status": "error"}
		if detail {
			body["database"] = "unavailable"
		}
		s.writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	if err := s.cachedStorageHealth(ctx); err != nil {
		body := map[string]string{"status": "error"}
		if detail {
			body["storage"] = "unavailable"
		}
		s.writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) loopbackRequest(r *http.Request) bool {
	addr, err := netip.ParseAddr(remoteHost(r.RemoteAddr))
	if err != nil {
		return false
	}
	return addr.Unmap().IsLoopback()
}

// cachedStorageHealth protects the public health endpoint from turning into an
// unbounded HeadBucket amplifier.
func (s *Server) cachedStorageHealth(ctx context.Context) error {
	now := s.now()
	s.healthMu.Lock()
	if !s.healthChecked.IsZero() && now.Sub(s.healthChecked) < 10*time.Second {
		err := s.healthErr
		s.healthMu.Unlock()
		return err
	}
	s.healthMu.Unlock()

	err := s.objects.Health(ctx)
	s.healthMu.Lock()
	s.healthChecked = now
	s.healthErr = err
	s.healthMu.Unlock()
	return err
}

func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /admin\nDisallow: /api/v1/keys\nAllow: /api\n"))
}
