package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"interim/internal/store"
)

const adminPageSize = 50

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Admin unavailable", "Could not read the admin account.")
		return
	}
	if !hasAdmin {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	session, ok := s.adminSession(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}

	pageNumber := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed < 100000 {
			pageNumber = parsed
		}
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 200 {
		query = query[:200]
	}
	offset := (pageNumber - 1) * adminPageSize

	files, total, err := s.store.ListFilesAdmin(r.Context(), store.FileFilter{
		Search: query,
		Limit:  adminPageSize,
		Offset: offset,
	})
	if err != nil {
		s.logger.Error("list admin files", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Admin unavailable", "Could not list files.")
		return
	}
	usage, err := s.store.StorageUsage(r.Context(), s.now())
	if err != nil {
		s.logger.Error("admin storage usage", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Admin unavailable", "Could not read storage usage.")
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		s.logger.Error("list admin keys", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Admin unavailable", "Could not list user hashes.")
		return
	}

	pages := int((total + adminPageSize - 1) / adminPageSize)
	if pages < 1 {
		pages = 1
	}
	if pageNumber > pages {
		pageNumber = pages
	}
	prevURL, nextURL := s.adminPaginationURLs(query, pageNumber, pages)
	percent := 0.0
	if s.cfg.MaxStorage > 0 {
		percent = float64(usage.Used) / float64(s.cfg.MaxStorage) * 100
		if percent > 100 {
			percent = 100
		}
	}

	page := s.basePage(r, "Admin", "admin")
	page.IsAdmin = true
	page.AdminName = session.Username
	page.CSRF = session.CSRF
	page.Wide = true
	page.NoIndex = true

	view := AdminView{
		PageData: page,
		Usage:    usage,
		Percent:  percent,
		Files:    files,
		Keys:     keys,
		Total:    total,
		Page:     pageNumber,
		Pages:    pages,
		Query:    query,
		PrevURL:  prevURL,
		NextURL:  nextURL,
		Now:      s.now(),
	}
	if raw := r.URL.Query().Get("deleted"); raw != "" {
		view.Deleted, _ = strconv.Atoi(raw)
	}
	if raw := r.URL.Query().Get("failed"); raw != "" {
		view.Failed, _ = strconv.Atoi(raw)
	}
	switch r.URL.Query().Get("keys") {
	case "revoked":
		page.Flash = "User hash revoked."
	case "restored":
		page.Flash = "User hash restored."
	}
	if view.Deleted > 0 || view.Failed > 0 {
		page.Flash = fmt.Sprintf("Deleted %d file(s). %d failed.", view.Deleted, view.Failed)
	}
	view.PageData = page

	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "admin", view)
}

func (s *Server) adminPaginationURLs(query string, page, pages int) (string, string) {
	makeURL := func(target int) string {
		values := url.Values{}
		if query != "" {
			values.Set("q", query)
		}
		if target > 1 {
			values.Set("page", strconv.Itoa(target))
		}
		if encoded := values.Encode(); encoded != "" {
			return "/admin?" + encoded
		}
		return "/admin"
	}
	var prev, next string
	if page > 1 {
		prev = makeURL(page - 1)
	}
	if page < pages {
		next = makeURL(page + 1)
	}
	return prev, next
}

func (s *Server) handleAdminDeleteFiles(w http.ResponseWriter, r *http.Request) {
	session, ok := s.adminSession(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if !s.parseAdminForm(w, r) || !s.adminCSRFValid(r, session) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The delete form expired. Reload the admin page.")
		return
	}
	ids := r.PostForm["ids"]
	if len(ids) == 0 {
		s.redirectAdmin(w, r, "deleted=0&failed=0")
		return
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}

	deleted := 0
	failed := 0
	for _, id := range ids {
		file, err := s.store.FileByID(r.Context(), id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.logger.Warn("admin delete lookup", "file_id", id, "error", err)
			}
			failed++
			continue
		}
		if err := s.objects.Delete(r.Context(), file.ObjectKey); err != nil {
			s.logger.Error("admin delete object", "file_id", file.ID, "error", err)
			failed++
			continue
		}
		removed, err := s.store.DeleteFileRow(r.Context(), file.ID)
		if err != nil || !removed {
			s.logger.Error("admin delete row", "file_id", file.ID, "error", err)
			failed++
			continue
		}
		deleted++
	}
	s.logger.Info("admin deleted files", "admin", session.Username, "deleted", deleted, "failed", failed)
	s.redirectAdmin(w, r, fmt.Sprintf("deleted=%d&failed=%d", deleted, failed))
}

func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	session, ok := s.adminSession(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if !s.parseAdminForm(w, r) || !s.adminCSRFValid(r, session) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The form expired. Reload the admin page.")
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	action := r.PostFormValue("action")
	disabled := action == "revoke"
	if id == "" || (action != "revoke" && action != "restore") {
		s.redirectAdmin(w, r, "keys=invalid")
		return
	}
	if err := s.store.SetAPIKeyDisabled(r.Context(), id, disabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.redirectAdmin(w, r, "keys=invalid")
			return
		}
		s.logger.Error("revoke api key", "key_id", id, "error", err)
		s.redirectAdmin(w, r, "keys=error")
		return
	}
	if disabled {
		s.redirectAdmin(w, r, "keys=revoked")
	} else {
		s.redirectAdmin(w, r, "keys=restored")
	}
}

func (s *Server) redirectAdmin(w http.ResponseWriter, r *http.Request, query string) {
	target := "/admin"
	if query != "" {
		target += "?" + query
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}
