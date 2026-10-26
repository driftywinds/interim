package config

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func withBaseEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PUBLIC_BASE_URL", "https://files.example.test")
	t.Setenv("DATA_DIR", dir)
	t.Setenv("S3_BUCKET", "interim-test")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("COOKIE_SECURE", "auto")
	t.Setenv("MAX_STORAGE", "20GB")
	t.Setenv("MAX_FILE_SIZE", "5GB")
	t.Setenv("RETENTION", "7d")
}

func TestLoad(t *testing.T) {
	withBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.PublicBaseURL != "https://files.example.test" {
		t.Errorf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
	if !cfg.CookieSecure {
		t.Error("CookieSecure should be true for an https base URL")
	}
	if !cfg.S3.PathStyle {
		t.Error("S3 path-style addressing should default to true")
	}
	if cfg.Retention != 7*24*time.Hour {
		t.Errorf("retention = %v", cfg.Retention)
	}
	if cfg.UploadURLTTL != 30*time.Minute || cfg.UploadIntentTTL != 45*time.Minute {
		t.Errorf("upload TTLs = %v / %v", cfg.UploadURLTTL, cfg.UploadIntentTTL)
	}
	if cfg.MaxFileSize != MaxSinglePutSize {
		t.Errorf("max file size = %d", cfg.MaxFileSize)
	}
	if !cfg.PublicUploadsEnabled {
		t.Error("public uploads should default to enabled")
	}
	if len(cfg.SetupToken) < 32 || cfg.SetupTokenPath == "" {
		t.Errorf("setup token was not generated: token=%d path=%q", len(cfg.SetupToken), cfg.SetupTokenPath)
	}
	if info, err := os.Stat(cfg.SetupTokenPath); err != nil {
		t.Errorf("stat setup token: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("setup token mode = %o", info.Mode().Perm())
	}
	if cfg.MaxFiles != 100000 {
		t.Errorf("max files = %d", cfg.MaxFiles)
	}
	if cfg.MaxPendingUploadsPerClient != 2 {
		t.Errorf("max pending uploads = %d", cfg.MaxPendingUploadsPerClient)
	}
	if cfg.MaxReservedBytesPerClient != cfg.MaxFileSize {
		t.Errorf("max reserved bytes = %d", cfg.MaxReservedBytesPerClient)
	}
	if cfg.MultipartEnabled {
		t.Error("multipart uploads should default to disabled")
	}
	if cfg.MultipartThreshold != 64<<20 || cfg.MultipartPartSize != 16<<20 || cfg.MultipartConcurrency != 3 || !cfg.MultipartSignPartLength {
		t.Errorf("multipart defaults = %d / %d / %d / %v", cfg.MultipartThreshold, cfg.MultipartPartSize, cfg.MultipartConcurrency, cfg.MultipartSignPartLength)
	}
}

func TestMultipartConfig(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("MULTIPART_UPLOAD_ENABLED", "true")
	t.Setenv("MAX_FILE_SIZE", "6GB")
	t.Setenv("MULTIPART_THRESHOLD", "128MB")
	t.Setenv("MULTIPART_PART_SIZE", "8MB")
	t.Setenv("MULTIPART_CONCURRENCY", "4")
	t.Setenv("S3_PUT_ACCESS_KEY", "put-access")
	t.Setenv("S3_PUT_SECRET_KEY", "put-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MultipartEnabled || cfg.MaxFileSize != 6<<30 || cfg.MultipartThreshold != 128<<20 || cfg.MultipartPartSize != 8<<20 || cfg.MultipartConcurrency != 4 || !cfg.MultipartSignPartLength {
		t.Fatalf("multipart config = %+v", cfg)
	}
}

func TestMultipartConfigRejectsUnsafeValues(t *testing.T) {
	tests := map[string]string{
		"MULTIPART_PART_SIZE": "1MB",
		"MULTIPART_THRESHOLD": "6GB",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			withBaseEnv(t)
			t.Setenv("MULTIPART_UPLOAD_ENABLED", "true")
			t.Setenv("S3_PUT_ACCESS_KEY", "put-access")
			t.Setenv("S3_PUT_SECRET_KEY", "put-secret")
			t.Setenv(name, value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s=%s to fail", name, value)
			}
		})
	}
	t.Run("large file requires multipart", func(t *testing.T) {
		withBaseEnv(t)
		t.Setenv("MAX_FILE_SIZE", "6GB")
		if _, err := Load(); err == nil {
			t.Fatal("expected >5GB to fail with multipart disabled")
		}
	})
}

func TestSetupTokenCanBeSuppliedByEnv(t *testing.T) {
	withBaseEnv(t)
	token := strings.Repeat("s", 32)
	t.Setenv("SETUP_TOKEN", token)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SetupToken != token || cfg.SetupTokenPath != "" {
		t.Fatalf("setup token = %q path = %q", cfg.SetupToken, cfg.SetupTokenPath)
	}
}

func TestTrustedProxyConfig(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("TRUST_PROXY", "true")
	if _, err := Load(); err == nil {
		t.Fatal("expected TRUST_PROXY without TRUSTED_PROXY_CIDRS to fail")
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.1/32, ::1, 10.0.0.0/8")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxyCIDRs) != 3 {
		t.Fatalf("trusted proxies = %v", cfg.TrustedProxyCIDRs)
	}
	if cfg.TrustedProxyCIDRs[1].String() != "::1/128" {
		t.Errorf("bare IPv6 was not normalized: %v", cfg.TrustedProxyCIDRs[1])
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid trusted proxy CIDR to fail")
	}
}

func TestMultipartRequiresUploadIdentity(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("MULTIPART_UPLOAD_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("expected multipart without upload identity or copy-block acknowledgment to fail")
	}
	t.Setenv("S3_PUBLIC_ENDPOINT_BLOCKS_COPY", "true")
	if _, err := Load(); err != nil {
		t.Fatalf("copy-block acknowledgment should allow multipart: %v", err)
	}
}

func TestS3PutCredentials(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("S3_PUT_ACCESS_KEY", "put-access")
	t.Setenv("S3_PUT_SECRET_KEY", "put-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.PutAccessKey != "put-access" || cfg.S3.PutSecretKey != "put-secret" {
		t.Fatalf("put credentials = %q / %q", cfg.S3.PutAccessKey, cfg.S3.PutSecretKey)
	}

	t.Setenv("S3_PUT_SECRET_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected incomplete put credentials to fail")
	}
}

func TestPublicUploadsCanBeDisabled(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("PUBLIC_UPLOADS_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicUploadsEnabled {
		t.Fatal("public uploads should be disabled")
	}
}

func TestMaxStorageZeroDisablesCap(t *testing.T) {
	for _, value := range []string{"0", "0B", "unlimited"} {
		t.Run(value, func(t *testing.T) {
			withBaseEnv(t)
			t.Setenv("MAX_STORAGE", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with MAX_STORAGE=%q: %v", value, err)
			}
			if cfg.MaxStorage != 0 {
				t.Fatalf("MaxStorage = %d, want 0", cfg.MaxStorage)
			}
		})
	}
}

func TestDownloadWorkerConfig(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("DOWNLOAD_WORKER_ENABLED", "true")
	t.Setenv("DOWNLOAD_WORKER_URLS", "https://dl1.example.test, https://dl2.example.test/")
	t.Setenv("DOWNLOAD_WORKER_SECRET", "0123456789abcdef0123456789abcdef")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DownloadWorkerEnabled {
		t.Fatal("download worker should be enabled")
	}
	if len(cfg.DownloadWorkerURLs) != 2 || cfg.DownloadWorkerURLs[0] != "https://dl1.example.test" || cfg.DownloadWorkerURLs[1] != "https://dl2.example.test" {
		t.Fatalf("worker URLs = %v", cfg.DownloadWorkerURLs)
	}
	if cfg.DownloadWorkerSecret == "" {
		t.Fatal("download worker secret is empty")
	}
}

func TestDownloadWorkerConfigDisabledIgnoresInvalidValues(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("DOWNLOAD_WORKER_ENABLED", "false")
	t.Setenv("DOWNLOAD_WORKER_URLS", "not a url")
	t.Setenv("DOWNLOAD_WORKER_SECRET", "short")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DownloadWorkerEnabled || len(cfg.DownloadWorkerURLs) != 0 || cfg.DownloadWorkerSecret != "" {
		t.Fatalf("disabled worker config = %+v", cfg)
	}
}

func TestDownloadWorkerConfigRequiresURLsAndSecret(t *testing.T) {
	tests := map[string]struct {
		urls   string
		secret string
	}{
		"missing URLs":   {urls: "", secret: "0123456789abcdef0123456789abcdef"},
		"missing secret": {urls: "https://dl.example.test", secret: ""},
		"short secret":   {urls: "https://dl.example.test", secret: "short"},
		"URL with path":  {urls: "https://dl.example.test/base", secret: "0123456789abcdef0123456789abcdef"},
		"plain HTTP URL": {urls: "http://dl.example.test", secret: "0123456789abcdef0123456789abcdef"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			withBaseEnv(t)
			t.Setenv("DOWNLOAD_WORKER_ENABLED", "true")
			t.Setenv("DOWNLOAD_WORKER_URLS", tc.urls)
			t.Setenv("DOWNLOAD_WORKER_SECRET", tc.secret)
			if _, err := Load(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRetentionFormats(t *testing.T) {
	tests := map[string]time.Duration{
		"7d":    7 * 24 * time.Hour,
		"30d":   30 * 24 * time.Hour,
		"1.5d":  36 * time.Hour,
		"12h":   12 * time.Hour,
		"90m":   90 * time.Minute,
		"2days": 48 * time.Hour,
	}
	for input, want := range tests {
		got, err := parseRetention(input)
		if err != nil {
			t.Errorf("parseRetention(%q) error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseRetention(%q) = %v, want %v", input, got, want)
		}
	}
	for _, input := range []string{"", "0d", "-7d", "seven days", "7x"} {
		if input == "" {
			continue
		}
		if _, err := parseRetention(input); err == nil {
			t.Errorf("parseRetention(%q) expected error", input)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := map[string]string{
		"PUBLIC_BASE_URL":                "",
		"MAX_STORAGE":                    "nope",
		"MAX_FILE_SIZE":                  "6GB",
		"S3_BUCKET":                      "",
		"COOKIE_SECURE":                  "sometimes",
		"PUBLIC_UPLOADS_ENABLED":         "sometimes",
		"MAX_FILES":                      "many",
		"MAX_PENDING_UPLOADS_PER_CLIENT": "many",
		"MAX_RESERVED_BYTES_PER_CLIENT":  "nope",
		"SETUP_TOKEN":                    "short",
		"UPLOAD_INTENT_TTL":              "10m",
		"UPLOAD_URL_TTL":                 "3h",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			withBaseEnv(t)
			t.Setenv(name, value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() expected error for %s=%q", name, value)
			}
		})
	}
}

func TestSessionSecretPersisted(t *testing.T) {
	withBaseEnv(t)
	t.Setenv("SESSION_SECRET", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SessionSecret) < 32 {
		t.Fatal("generated session secret is too short")
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "session_secret")); err != nil {
		t.Fatal(err)
	}
}

func TestCookieSecureAutoHTTP(t *testing.T) {
	base, _ := url.Parse("http://localhost:8080")
	secure, err := parseCookieSecure("", base)
	if err != nil || secure {
		t.Fatalf("auto http cookie secure = %v, %v", secure, err)
	}
}
