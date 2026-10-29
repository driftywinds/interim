package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/driftywinds/interim/internal/store"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

const (
	maxAdminFormBytes = 64 << 10
	loginRateLimit    = 10
	loginRateWindow   = 15 * time.Minute
)

func (s *Server) adminSession(r *http.Request) (store.Session, bool) {
	cookie, err := r.Cookie(s.cookieName(adminCookie))
	if err != nil || cookie.Value == "" {
		return store.Session{}, false
	}
	now := s.now()
	session, err := s.store.SessionByTokenHash(r.Context(), tokenHash(cookie.Value), now)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("admin session lookup failed", "error", err)
		}
		return store.Session{}, false
	}
	if s.cfg.AdminIdleTimeout > 0 && now.Sub(session.LastSeenAt) > s.cfg.AdminIdleTimeout {
		if err := s.store.DeleteSession(r.Context(), session.TokenHash); err != nil {
			s.logger.Warn("delete idle admin session", "error", err)
		}
		return store.Session{}, false
	}
	if err := s.store.TouchSession(r.Context(), session.TokenHash, now); err != nil {
		s.logger.Warn("touch admin session", "error", err)
	}
	session.LastSeenAt = now
	return session, true
}

func (s *Server) createAdminSession(ctx context.Context, w http.ResponseWriter, adminID int64) error {
	rawToken, err := randomToken(32)
	if err != nil {
		return err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return err
	}
	now := s.now()
	expires := now.Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(ctx, tokenHash(rawToken), adminID, csrf, now, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(adminCookie),
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	return nil
}

func (s *Server) clearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(adminCookie),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (s *Server) preauthCSRF(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(s.cookieName(preauthCookie)); err == nil && s.validPreauthCSRF(cookie.Value) {
		return cookie.Value
	}
	raw, err := randomToken(32)
	if err != nil {
		s.logger.Error("generate csrf token", "error", err)
		return ""
	}
	token := raw + "." + s.csrfSignature(raw)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(preauthCookie),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
		Expires:  s.now().Add(10 * time.Minute),
	})
	return token
}

func (s *Server) csrfSignature(raw string) string {
	mac := hmac.New(sha256.New, s.cfg.SessionSecret)
	_, _ = mac.Write([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validPreauthCSRF(value string) bool {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	return hmac.Equal([]byte(s.csrfSignature(parts[0])), []byte(parts[1]))
}

func (s *Server) validatePreauthCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(s.cookieName(preauthCookie))
	if err != nil || cookie.Value == "" || !s.validPreauthCSRF(cookie.Value) {
		return false
	}
	formToken := r.PostFormValue("csrf_token")
	return constantTimeEqualString(cookie.Value, formToken)
}

func (s *Server) clearPreauthCSRF(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(preauthCookie),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) parseAdminForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminFormBytes)
	if err := r.ParseForm(); err != nil {
		return false
	}
	return true
}

func (s *Server) adminCSRFValid(r *http.Request, session store.Session) bool {
	return constantTimeEqualString(session.CSRF, r.PostFormValue("csrf_token"))
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) authPageData(r *http.Request, mode string) PageData {
	page := s.basePage(r, "Admin", "admin")
	page.NoIndex = true
	page.Wide = false
	if mode == "setup" {
		page.Title = "Set up interim"
	}
	return page
}

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.logger.Error("check admin", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Setup unavailable", "Could not read the admin account.")
		return
	}
	if hasAdmin {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}

	page := s.authPageData(r, "setup")
	page.CSRF = s.preauthCSRF(w, r)
	view := AuthView{
		PageData: page,
		Mode:     "setup",
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "auth", view)
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Setup unavailable", "Could not read the admin account.")
		return
	}
	if hasAdmin {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	rateKey := "setup:" + s.clientIP(r)
	if allowed, retry := s.authLimiter.allow(rateKey, 5, 15*time.Minute); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		s.renderSetupError(w, r, http.StatusTooManyRequests, "Too many setup attempts. Try again later.")
		return
	}
	if !s.parseAdminForm(w, r) || !s.validatePreauthCSRF(r) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The setup form expired. Reload and try again.")
		return
	}
	if !constantTimeEqualString(s.cfg.SetupToken, strings.TrimSpace(r.PostFormValue("setup_token"))) {
		s.renderSetupError(w, r, http.StatusForbidden, "Incorrect setup token. Read it from DATA_DIR/setup_token or SETUP_TOKEN.")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")
	errorMessage := ""
	switch {
	case !usernamePattern.MatchString(username):
		errorMessage = "Username must be 3–32 characters: letters, numbers, dots, underscores, or dashes."
	case len(password) < 12:
		errorMessage = "Password must be at least 12 characters."
	case len([]byte(password)) > 72:
		errorMessage = "Password must be at most 72 bytes."
	case password != confirm:
		errorMessage = "Passwords do not match."
	}
	if errorMessage != "" {
		page := s.authPageData(r, "setup")
		page.CSRF = s.preauthCSRF(w, r)
		view := AuthView{
			PageData: page,
			Mode:     "setup",
			Username: username,
			Error:    errorMessage,
		}
		s.render(w, r, http.StatusUnprocessableEntity, "auth", view)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		s.logger.Error("hash admin password", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Setup failed", "Could not secure the password.")
		return
	}
	admin, err := s.store.CreateAdmin(r.Context(), username, string(hash), s.now())
	if errors.Is(err, store.ErrAlreadySetup) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.logger.Error("create admin", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Setup failed", "Could not create the admin account.")
		return
	}
	if err := s.createAdminSession(r.Context(), w, admin.ID); err != nil {
		s.logger.Error("create admin session", "error", err)
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	s.authLimiter.reset(rateKey)
	s.clearPreauthCSRF(w)
	if s.cfg.SetupTokenPath != "" {
		if err := os.Remove(s.cfg.SetupTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("remove setup token after setup", "error", err)
		}
	}
	s.logger.Info("admin account created", "username", admin.Username)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) renderSetupError(w http.ResponseWriter, r *http.Request, status int, message string) {
	page := s.authPageData(r, "setup")
	page.CSRF = s.preauthCSRF(w, r)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, status, "auth", AuthView{
		PageData: page,
		Mode:     "setup",
		Error:    message,
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Login unavailable", "Could not read the admin account.")
		return
	}
	if !hasAdmin {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, ok := s.adminSession(r); ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	page := s.authPageData(r, "login")
	page.CSRF = s.preauthCSRF(w, r)
	view := AuthView{
		PageData: page,
		Mode:     "login",
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "auth", view)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.store.HasAdmin(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Login unavailable", "Could not read the admin account.")
		return
	}
	if !hasAdmin {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	rateKey := "login:" + s.clientIP(r)
	if allowed, retry := s.authLimiter.allow(rateKey, loginRateLimit, loginRateWindow); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		w.Header().Set("Cache-Control", "no-store")
		page := s.authPageData(r, "login")
		page.CSRF = s.preauthCSRF(w, r)
		s.render(w, r, http.StatusTooManyRequests, "auth", AuthView{
			PageData: page,
			Mode:     "login",
			Error:    "Too many attempts. Try again in a few minutes.",
		})
		return
	}

	if !s.parseAdminForm(w, r) || !s.validatePreauthCSRF(r) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The login form expired. Reload and try again.")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	// A second, fixed counter (not a map entry) slows a distributed
	// brute-force attempt across rotated IPv4/IPv6 addresses. A flood of
	// arbitrary limiter keys cannot evict it. There is one admin account, so
	// counting all usernames together is equivalent to a per-account lockout.
	if allowed, retry := s.loginAccountLimiter.allow(30, loginRateWindow); !allowed {
		w.Header().Set("Retry-After", retryString(retry))
		w.Header().Set("Cache-Control", "no-store")
		page := s.authPageData(r, "login")
		page.CSRF = s.preauthCSRF(w, r)
		s.render(w, r, http.StatusTooManyRequests, "auth", AuthView{
			PageData: page,
			Mode:     "login",
			Error:    "Too many attempts for this account. Try again later.",
		})
		return
	}

	admin, err := s.store.AdminByUsername(r.Context(), username)
	hash := s.dummyHash
	if err == nil {
		hash = []byte(admin.PasswordHash)
	}
	compareErr := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if err != nil || compareErr != nil {
		s.logger.Warn("admin login failed", "username_hash", usernameLogValue(username), "ip", s.clientIP(r))
		page := s.authPageData(r, "login")
		page.CSRF = s.preauthCSRF(w, r)
		view := AuthView{
			PageData: page,
			Mode:     "login",
			Username: username,
			Error:    "Invalid username or password.",
		}
		s.render(w, r, http.StatusUnauthorized, "auth", view)
		return
	}

	if err := s.createAdminSession(r.Context(), w, admin.ID); err != nil {
		s.logger.Error("create admin session", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Login failed", "Could not create a session.")
		return
	}
	s.authLimiter.reset(rateKey)
	s.loginAccountLimiter.reset()
	s.clearPreauthCSRF(w)
	s.logger.Info("admin login", "username", admin.Username, "ip", s.clientIP(r))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, ok := s.adminSession(r)
	if ok {
		if !s.parseAdminForm(w, r) || !s.adminCSRFValid(r, session) {
			s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The logout form expired. Reload the page.")
			return
		}
		if err := s.store.DeleteSession(r.Context(), session.TokenHash); err != nil {
			s.logger.Warn("delete admin session", "error", err)
		}
	}
	s.clearKeyFlash(w)
	s.clearAdminCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	session, ok := s.adminSession(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if !s.parseAdminForm(w, r) || !s.adminCSRFValid(r, session) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The password form expired. Reload the page.")
		return
	}
	current := r.PostFormValue("current_password")
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")
	if len(password) < 12 {
		s.renderError(w, r, http.StatusUnprocessableEntity, "Password not changed", "The new password must be at least 12 characters.")
		return
	}
	if len([]byte(password)) > 72 {
		s.renderError(w, r, http.StatusUnprocessableEntity, "Password not changed", "The new password must be at most 72 bytes.")
		return
	}
	if password != confirm {
		s.renderError(w, r, http.StatusUnprocessableEntity, "Password not changed", "The new passwords do not match.")
		return
	}

	admin, err := s.store.AdminByID(r.Context(), session.AdminID)
	if err != nil {
		s.logger.Error("read admin for password change", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Password not changed", "Could not read the admin account.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(current)); err != nil {
		s.logger.Warn("admin password change rejected", "admin", admin.Username, "ip", s.clientIP(r))
		s.renderError(w, r, http.StatusForbidden, "Password not changed", "The current password is incorrect.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		s.logger.Error("hash new admin password", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Password not changed", "Could not secure the new password.")
		return
	}
	if err := s.store.UpdateAdminPassword(r.Context(), admin.ID, string(hash)); err != nil {
		s.logger.Error("update admin password", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Password not changed", "Could not update the password.")
		return
	}
	s.clearKeyFlash(w)
	s.clearAdminCookie(w)
	s.logger.Info("admin password changed; all sessions revoked", "admin", admin.Username, "ip", s.clientIP(r))
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	session, ok := s.adminSession(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if !s.parseAdminForm(w, r) || !s.adminCSRFValid(r, session) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid form", "The logout form expired. Reload the page.")
		return
	}
	if err := s.store.DeleteSessionsForAdmin(r.Context(), session.AdminID); err != nil {
		s.logger.Error("delete all admin sessions", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Logout failed", "Could not revoke all sessions.")
		return
	}
	s.clearKeyFlash(w)
	s.clearAdminCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeAPIError(w, r, http.StatusNotFound, "not_found", "The requested API endpoint does not exist.")
		return
	}
	s.renderError(w, r, http.StatusNotFound, "Not found", "That page does not exist.")
}

func retryString(d time.Duration) string {
	return strconv.Itoa(retrySeconds(d))
}

// usernameLogValue keeps failed-login logs correlation-friendly without
// recording whatever was typed. Users sometimes paste a password into the
// username field.
func usernameLogValue(username string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}
