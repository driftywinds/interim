package server

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

func fsSub(fsys fs.FS, dir string) (fs.FS, error) {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("open embedded %s: %w", dir, err)
	}
	return sub, nil
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		connectSource := "'self'"
		if endpoint := s.cfg.S3.PublicEndpoint; endpoint != "" {
			if parsed, err := url.Parse(endpoint); err == nil && parsed.Scheme != "" && parsed.Host != "" {
				connectSource += " " + parsed.Scheme + "://" + parsed.Host
				if !s.cfg.S3.PathStyle {
					// Virtual-host addressing prepends the bucket name to the
					// configured endpoint host.
					if hostname := parsed.Hostname(); hostname != "" {
						port := ""
						if parsed.Port() != "" {
							port = ":" + parsed.Port()
						}
						connectSource += " " + parsed.Scheme + "://*." + hostname + port
					}
				}
			}
		} else {
			// AWS with the default endpoint: allow only the regional S3 hosts
			// rather than every HTTPS origin on the internet.
			region := s.cfg.S3.Region
			if region == "" {
				region = "us-east-1"
			}
			host := "s3." + region + ".amazonaws.com"
			if s.cfg.S3.Bucket != "" && !s.cfg.S3.PathStyle {
				host = s.cfg.S3.Bucket + "." + host
			}
			connectSource += " https://" + host
			if region == "us-east-1" {
				connectSource += " https://s3.amazonaws.com"
				if s.cfg.S3.Bucket != "" && !s.cfg.S3.PathStyle {
					connectSource += " https://" + s.cfg.S3.Bucket + ".s3.amazonaws.com"
				}
			}
		}
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; "+
				"img-src 'self' data:; font-src 'self'; style-src 'self'; script-src 'self'; connect-src "+connectSource)
		if s.cfg.CookieSecure || strings.HasPrefix(s.cfg.PublicBaseURL, "https://") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID()
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("request",
			"method", r.Method,
			"path", redactPath(r.URL.Path),
			"status", status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", requestIDFromContext(r.Context()),
			"ip", s.clientIP(r),
		)
	})
}

// redactPath removes capability-bearing IDs from access logs. File and upload
// IDs are the only secret protecting public pages and API calls, so logging the
// raw path lets anyone with log access open every file.
func redactPath(value string) string {
	segments := strings.Split(value, "/")
	if len(segments) >= 3 && segments[1] == "f" {
		segments[2] = "[id]"
	}
	if len(segments) >= 5 && segments[1] == "api" && segments[2] == "v1" &&
		(segments[3] == "files" || segments[3] == "uploads") {
		segments[4] = "[id]"
	}
	return strings.Join(segments, "/")
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic serving request",
					"error", fmt.Sprint(recovered),
					"request_id", requestIDFromContext(r.Context()),
					"stack", string(debug.Stack()),
				)
				if strings.HasPrefix(r.URL.Path, "/api/") {
					s.writeAPIError(w, r, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
					return
				}
				s.renderError(w, r, http.StatusInternalServerError, "Something broke", "The server hit an unexpected error. Please try again.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
