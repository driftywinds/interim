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
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"interim/internal/config"
	"interim/internal/download"
	"interim/internal/objects"
	"interim/internal/store"
	"interim/internal/units"
	webassets "interim/web"
)

const (
	adminCookie    = "interim_admin"
	preauthCookie  = "interim_csrf"
	keyFlashCookie = "interim_key_flash"
)

var (
	errUnauthorized = errors.New("unauthorized")
)

type Server struct {
	cfg          config.Config
	store        *store.Store
	objects      objects.Store
	logger       *slog.Logger
	templates    map[string]*template.Template
	static       http.Handler
	assetVersion string
	downloads    *download.Signer
	limiter      *rateLimiter
	dummyHash    []byte
	now          func() time.Time

	keyFlashMu sync.Mutex
	keyFlashes map[string]adminKeyFlash

	healthMu      sync.Mutex
	healthChecked time.Time
	healthErr     error
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

	var downloads *download.Signer
	if cfg.DownloadWorkerEnabled {
		downloads, err = download.NewSigner(cfg.DownloadWorkerSecret, cfg.DownloadWorkerURLs)
		if err != nil {
			return nil, fmt.Errorf("configure download worker: %w", err)
		}
	}

	s := &Server{
		cfg:          cfg,
		store:        st,
		objects:      obj,
		logger:       logger,
		templates:    templates,
		static:       http.FileServerFS(staticFS),
		assetVersion: staticAssetVersion(),
		downloads:    downloads,
		limiter:      newRateLimiter(),
		now:          time.Now,
		keyFlashes:   make(map[string]adminKeyFlash),
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("interim-dummy-password"), 12)
	if err != nil {
		return nil, fmt.Errorf("generate dummy password hash: %w", err)
	}
	s.dummyHash = dummy
	return s, nil
}

func staticAssetVersion() string {
	h := sha256.New()
	for _, name := range []string{"static/app.js", "static/app.css", "static/favicon.svg"} {
		data, err := webassets.FS.ReadFile(name)
		if err != nil {
			continue
		}
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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
	mux.HandleFunc("POST /api/v1/uploads", s.handleCreateUpload)
	mux.HandleFunc("GET /api/v1/uploads/{id}", s.handleUploadStatus)
	mux.HandleFunc("POST /api/v1/uploads/{id}/parts", s.handleUploadPartURLs)
	mux.HandleFunc("PUT /api/v1/uploads/{id}/parts/{part}", s.handleUploadPartReport)
	mux.HandleFunc("POST /api/v1/uploads/{id}/complete", s.handleCompleteUpload)
	mux.HandleFunc("DELETE /api/v1/uploads/{id}", s.handleAbortUpload)
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
	mux.HandleFunc("POST /admin/keys/create", s.handleAdminCreateKey)
	mux.HandleFunc("POST /admin/keys/update", s.handleAdminUpdateKey)

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

func (s *Server) cookieName(base string) string {
	if s.cfg.CookieSecure {
		return "__Host-" + base
	}
	return base
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
		AssetVersion:   s.assetVersion,
	}
}

func (s *Server) downloadURL(ctx context.Context, file store.File) (string, time.Time, error) {
	if s.downloads != nil {
		return s.downloads.URL(file.ObjectKey, file.OriginalName, s.cfg.DownloadTTL, s.now().UTC())
	}
	return s.objects.PresignGet(ctx, file.ObjectKey, file.OriginalName, s.cfg.DownloadTTL)
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
	unlimited := s.cfg.MaxStorage == 0
	available := s.cfg.MaxStorage - usage.Used - usage.Reserved
	if unlimited || available < 0 {
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
		Files:         usage.Files,
		Used:          usage.Used,
		Reserved:      usage.Reserved,
		Limit:         s.cfg.MaxStorage,
		Available:     available,
		UsedPercent:   percent,
		Accepting:     unlimited || available > 0,
		PublicUploads: s.cfg.PublicUploadsEnabled,
		Unlimited:     unlimited,
		MaxFileSize:   s.cfg.MaxFileSize,
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
	name = strings.Map(stripFormattingRune, name)
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

func stripFormattingRune(r rune) rune {
	if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
		return -1
	}
	return r
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

func (s *Server) clientIP(r *http.Request) string {
	remote := remoteHost(r.RemoteAddr)
	remoteAddr, err := netip.ParseAddr(remote)
	if err != nil {
		return remote
	}
	remoteAddr = remoteAddr.Unmap()
	if !s.cfg.TrustProxy || !s.isTrustedProxy(remoteAddr) {
		return remoteAddr.String()
	}

	var chain []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(header, ",") {
			if ip := strings.TrimSpace(part); ip != "" {
				chain = append(chain, ip)
			}
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(chain[i])
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if !s.isTrustedProxy(addr) {
			return addr.String()
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if addr, err := netip.ParseAddr(realIP); err == nil {
			return addr.Unmap().String()
		}
	}
	return remoteAddr.String()
}

func (s *Server) isTrustedProxy(addr netip.Addr) bool {
	for _, prefix := range s.cfg.TrustedProxyCIDRs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
