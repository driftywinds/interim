package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"interim/internal/config"
	"interim/internal/objects"
	"interim/internal/store"
	"interim/internal/units"
	webassets "interim/web"
)

const (
	adminCookie    = "interim_admin"
	preauthCookie  = "interim_csrf"
	maxReservation = 6 * time.Hour
)

var (
	errUnauthorized = errors.New("unauthorized")
	uploadTooLarge  = errors.New("upload exceeds declared size")
)

type Server struct {
	cfg       config.Config
	store     *store.Store
	objects   objects.Store
	logger    *slog.Logger
	templates map[string]*template.Template
	static    http.Handler
	limiter   *rateLimiter
	dummyHash []byte
	now       func() time.Time
}

func New(cfg config.Config, st *store.Store, obj objects.Store, logger *slog.Logger) (*Server, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}

	staticFS, err := fsSub(webassets.FS, "static")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		store:     st,
		objects:   obj,
		logger:    logger,
		templates: templates,
		static:    http.FileServerFS(staticFS),
		limiter:   newRateLimiter(),
		now:       time.Now,
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("interim-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("generate dummy password hash: %w", err)
	}
	s.dummyHash = dummy
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(s.static)))
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /f/{id}", s.handleFilePage)
	mux.HandleFunc("GET /f/{id}/download", s.handlePageDownload)

	mux.HandleFunc("GET /api", s.handleAPIDocs)
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/" {
			http.Redirect(w, r, "/api", http.StatusMovedPermanently)
			return
		}
		s.writeAPIError(w, r, http.StatusNotFound, "not_found", "The requested API endpoint does not exist.")
	})
	mux.HandleFunc("GET /api/openapi.json", s.handleOpenAPIJSON)
	mux.HandleFunc("GET /api/openapi.yaml", s.handleOpenAPIYAML)
	mux.HandleFunc("GET /api/v1", s.handleAPIIndex)
	mux.HandleFunc("GET /api/v1/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /api/v1/status", s.handleAPIStatus)
	mux.HandleFunc("POST /api/v1/keys", s.handleCreateKey)
	mux.HandleFunc("GET /api/v1/keys/me", s.handleKeyInfo)
	mux.HandleFunc("POST /api/v1/files", s.handleUpload)
	mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	mux.HandleFunc("GET /api/v1/files/{id}", s.handleFileMetadata)
	mux.HandleFunc("POST /api/v1/files/{id}/download", s.handleAPIDownload)
	mux.HandleFunc("DELETE /api/v1/files/{id}", s.handleDeleteFile)

	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /admin/login", s.handleLoginPage)
	mux.HandleFunc("POST /admin/login", s.handleLoginSubmit)
	mux.HandleFunc("POST /admin/logout", s.handleLogout)
	mux.HandleFunc("GET /admin", s.handleAdmin)
	mux.HandleFunc("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	})
	mux.HandleFunc("POST /admin/files/delete", s.handleAdminDeleteFiles)
	mux.HandleFunc("POST /admin/keys/revoke", s.handleAdminRevokeKey)

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /", s.handleNotFound)

	handler := s.requestIDMiddleware(s.accessLogMiddleware(mux))
	handler = s.recoverMiddleware(handler)
	handler = s.securityHeadersMiddleware(handler)
	return handler
}

func (s *Server) basePage(r *http.Request, title, active string) PageData {
	return PageData{
		Title:          title,
		Active:         active,
		BaseURL:        s.cfg.PublicBaseURL,
		RetentionLabel: retentionLabel(s.cfg.Retention),
		MaxFileSize:    s.cfg.MaxFileSize,
		MaxStorage:     s.cfg.MaxStorage,
		DownloadTTL:    s.cfg.DownloadTTL,
		CurrentPath:    r.URL.Path,
	}
}

func (s *Server) pageURL(id string) string {
	return s.cfg.PublicBaseURL + "/f/" + url.PathEscape(id)
}

func (s *Server) apiFileURL(id string) string {
	return s.cfg.PublicBaseURL + "/api/v1/files/" + url.PathEscape(id)
}

func (s *Server) status(ctx context.Context) (StatusView, error) {
	usage, err := s.store.StorageUsage(ctx, s.now())
	if err != nil {
		return StatusView{}, err
	}
	available := s.cfg.MaxStorage - usage.Used - usage.Reserved
	if available < 0 {
		available = 0
	}
	percent := 0.0
	if s.cfg.MaxStorage > 0 {
		percent = float64(usage.Used) / float64(s.cfg.MaxStorage) * 100
		if percent > 100 {
			percent = 100
		}
	}
	return StatusView{
		Files:       usage.Files,
		Used:        usage.Used,
		Reserved:    usage.Reserved,
		Limit:       s.cfg.MaxStorage,
		Available:   available,
		UsedPercent: percent,
		Accepting:   available > 0,
		MaxFileSize: s.cfg.MaxFileSize,
	}, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	tmpl, ok := s.templates[name]
	if !ok {
		s.logger.Error("template not found", "template", name)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.logger.Error("render template", "template", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, buf.String())
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	view := ErrorView{
		PageData:   s.basePage(r, heading, ""),
		StatusCode: status,
		Heading:    heading,
		Message:    message,
	}
	s.render(w, r, status, "error", view)
}

func loadTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"bytes": units.FormatBytes,
		"date": func(t time.Time) string {
			return t.UTC().Format("January 2, 2006")
		},
		"datetime": func(t time.Time) string {
			return t.UTC().Format("Jan 2, 2006 15:04 UTC")
		},
		"rfc3339": func(t time.Time) string {
			return t.UTC().Format(time.RFC3339)
		},
		"pct": func(v float64) string {
			return fmt.Sprintf("%.1f", v)
		},
		"duration": humanDuration,
		"maskPrefix": func(s string) string {
			if s == "" {
				return "anonymous"
			}
			return s + "…"
		},
		"lower": strings.ToLower,
	}

	pages := []string{"home", "file", "api", "admin", "auth", "error"}
	templates := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		tmpl, err := template.New(page).Funcs(funcs).ParseFS(
			webassets.FS,
			"templates/layout.html",
			"templates/"+page+".html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", page, err)
		}
		templates[page] = tmpl
	}
	return templates, nil
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	days := int(d.Hours()) / 24
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

func retentionLabel(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	d -= time.Duration(minutes) * time.Minute
	seconds := int(d / time.Second)

	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, durationPart(days, "day"))
	}
	if hours > 0 {
		parts = append(parts, durationPart(hours, "hour"))
	}
	if minutes > 0 {
		parts = append(parts, durationPart(minutes, "minute"))
	}
	if seconds > 0 && len(parts) == 0 {
		parts = append(parts, durationPart(seconds, "second"))
	}
	if len(parts) == 0 {
		return "0 seconds"
	}
	return strings.Join(parts, " ")
}

func durationPart(value int, unit string) string {
	if value == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", value, unit)
}

func expiresIn(t time.Time) string {
	remaining := time.Until(t)
	if remaining <= 0 {
		return "expired"
	}
	days := int(remaining.Hours()) / 24
	hours := int(remaining.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	minutes := int(remaining.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "file"
	}
	if len(name) > 180 {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		cut := 180 - len(ext)
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut] + ext
	}
	return name
}

func randomToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func constantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func requestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}

func parseContentType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "application/octet-stream"
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "application/octet-stream"
	}
	return mediaType
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
			if net.ParseIP(first) != nil {
				return first
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			if net.ParseIP(realIP) != nil {
				return realIP
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parsePositiveInt64(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	var n int64
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int64(r-'0')
		if n > 1<<53 {
			return 0, false
		}
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}
