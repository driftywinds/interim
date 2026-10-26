package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"interim/internal/store"
)

const (
	adminPageSize = 50
	keyFlashTTL   = 2 * time.Minute
)

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

	flash, hasKeyFlash := s.takeKeyFlash(w, r)

	page := s.basePage(r, "Admin", "admin")
	page.IsAdmin = true
	page.AdminName = session.Username
	page.CSRF = session.CSRF
	page.Wide = true
	page.NoIndex = true

	view := AdminView{
		PageData:      page,
		Usage:         usage,
		Percent:       percent,
		Unlimited:     s.cfg.MaxStorage == 0,
		Files:         files,
		Keys:          keys,
		Total:         total,
		Page:          pageNumber,
		Pages:         pages,
		Query:         query,
		PrevURL:       prevURL,
		NextURL:       nextURL,
		Now:           s.now(),
		PublicUploads: s.cfg.PublicUploadsEnabled,
	}
	if hasKeyFlash {
		view.NewKey = flash.Key
		view.NewKeyName = flash.Name
	}
	if raw := r.URL.Query().Get("deleted"); raw != "" {
		view.Deleted, _ = strconv.Atoi(raw)
	}
	if raw := r.URL.Query().Get("failed"); raw != "" {
		view.Failed, _ = strconv.Atoi(raw)
	}
	switch r.URL.Query().Get("keys") {
	case "created":
		page.Flash = "User hash generated."
	case "revoked":
		page.Flash = "User hash revoked."
	case "restored":
		page.Flash = "User hash restored."
	case "approved":
		page.Flash = "User hash approved for uploads."
	case "unapproved":
		page.Flash = "User hash approval removed."
	case "invalid":
		page.Flash = "That user hash could not be found."
	case "error":
		page.Flash = "Could not update the user hash."
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

func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	session, ok := s.adminSession(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if !s.parseAdminForm(w, r) || !s.adminCSRFValid(r, session) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The form expired. Reload the admin page.")
		return
	}
	name := cleanLabel(r.PostFormValue("name"))
	key, rawHash, err := s.newUserHash(r.Context(), name, true)
	if err != nil {
		s.logger.Error("admin create user hash", "admin", session.Username, "error", err)
		s.redirectAdmin(w, r, "keys=error")
		return
	}
	s.setKeyFlash(w, rawHash, key.Name)
	s.logger.Info("admin created user hash", "admin", session.Username, "key_id", key.ID, "name", key.Name)
	s.redirectAdmin(w, r, "keys=created")
}

func (s *Server) handleAdminUpdateKey(w http.ResponseWriter, r *http.Request) {
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
	if id == "" {
		s.redirectAdmin(w, r, "keys=invalid")
		return
	}

	var err error
	var flash string
	switch action {
	case "approve":
		err = s.store.SetAPIKeyApproved(r.Context(), id, true)
		flash = "approved"
	case "unapprove":
		err = s.store.SetAPIKeyApproved(r.Context(), id, false)
		flash = "unapproved"
	case "revoke":
		err = s.store.SetAPIKeyDisabled(r.Context(), id, true)
		flash = "revoked"
	case "restore":
		err = s.store.SetAPIKeyDisabled(r.Context(), id, false)
		flash = "restored"
	default:
		s.redirectAdmin(w, r, "keys=invalid")
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.redirectAdmin(w, r, "keys=invalid")
			return
		}
		s.logger.Error("update api key", "key_id", id, "action", action, "error", err)
		s.redirectAdmin(w, r, "keys=error")
		return
	}
	s.logger.Info("admin updated user hash", "admin", session.Username, "key_id", id, "action", action)
	s.redirectAdmin(w, r, "keys="+flash)
}

type adminKeyFlash struct {
	Key     string
	Name    string
	Expires time.Time
}

func (s *Server) setKeyFlash(w http.ResponseWriter, key, name string) {
	flashID, err := randomToken(24)
	if err != nil {
		s.logger.Error("generate key flash id", "error", err)
		return
	}
	now := s.now().UTC()
	s.keyFlashMu.Lock()
	if s.keyFlashes == nil {
		s.keyFlashes = make(map[string]adminKeyFlash)
	}
	for id, flash := range s.keyFlashes {
		if !flash.Expires.After(now) {
			delete(s.keyFlashes, id)
		}
	}
	s.keyFlashes[flashID] = adminKeyFlash{
		Key:     key,
		Name:    name,
		Expires: now.Add(keyFlashTTL),
	}
	s.keyFlashMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(keyFlashCookie),
		Value:    flashID,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(keyFlashTTL.Seconds()),
		Expires:  now.Add(keyFlashTTL),
	})
}

func (s *Server) takeKeyFlash(w http.ResponseWriter, r *http.Request) (adminKeyFlash, bool) {
	cookie, err := r.Cookie(s.cookieName(keyFlashCookie))
	if err != nil || cookie.Value == "" {
		return adminKeyFlash{}, false
	}
	s.clearKeyFlash(w)

	now := s.now().UTC()
	s.keyFlashMu.Lock()
	flash, ok := s.keyFlashes[cookie.Value]
	delete(s.keyFlashes, cookie.Value)
	for id, entry := range s.keyFlashes {
		if !entry.Expires.After(now) {
			delete(s.keyFlashes, id)
		}
	}
	s.keyFlashMu.Unlock()
	if !ok || flash.Key == "" || !flash.Expires.After(now) {
		return adminKeyFlash{}, false
	}
	return flash, true
}

func (s *Server) clearKeyFlash(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(keyFlashCookie),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (s *Server) redirectAdmin(w http.ResponseWriter, r *http.Request, query string) {
	target := "/admin"
	if query != "" {
		target += "?" + query
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}
