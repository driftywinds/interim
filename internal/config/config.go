// Package config loads interim's runtime configuration from the environment,
// with an optional .env file for local development.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"interim/internal/units"
)

const (
	// DefaultRetention is the default file lifetime. Operators can override it
	// with RETENTION in the environment.
	DefaultRetention = 7 * 24 * time.Hour

	// MaxSinglePutSize is the S3 single-PUT object limit. interim uses direct
	// browser PUTs and does not implement multipart uploads.
	MaxSinglePutSize int64 = 5 << 30
)

type Config struct {
	PublicBaseURL string
	ListenAddr    string
	DataDir       string
	DatabasePath  string
	SessionSecret []byte
	SessionTTL    time.Duration
	CookieSecure  bool
	TrustProxy    bool
	LogLevel      string

	Retention       time.Duration
	MaxStorage      int64
	MaxFileSize     int64
	DownloadTTL     time.Duration
	UploadURLTTL    time.Duration
	UploadIntentTTL time.Duration
	S3              S3Config
}

type S3Config struct {
	Endpoint       string
	PublicEndpoint string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	Prefix         string
	PathStyle      bool
	AutoCreate     bool
}

func Load() (Config, error) {
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(); err != nil {
			return Config{}, fmt.Errorf("load .env: %w", err)
		}
	}

	publicBaseURL, base, err := parsePublicBaseURL(os.Getenv("PUBLIC_BASE_URL"))
	if err != nil {
		return Config{}, err
	}

	dataDir := envOr("DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return Config{}, fmt.Errorf("create data directory: %w", err)
	}

	databasePath := envOr("DATABASE_PATH", filepath.Join(dataDir, "interim.db"))
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o750); err != nil {
		return Config{}, fmt.Errorf("create database directory: %w", err)
	}

	secret, err := loadSessionSecret(dataDir)
	if err != nil {
		return Config{}, err
	}

	maxStorage, err := units.ParseBytes(envOr("MAX_STORAGE", "20GB"))
	if err != nil {
		return Config{}, fmt.Errorf("MAX_STORAGE: %w", err)
	}
	maxFileSize, err := units.ParseBytes(envOr("MAX_FILE_SIZE", "5GB"))
	if err != nil {
		return Config{}, fmt.Errorf("MAX_FILE_SIZE: %w", err)
	}
	if maxFileSize > maxStorage {
		return Config{}, errors.New("MAX_FILE_SIZE cannot be larger than MAX_STORAGE")
	}
	if maxFileSize > MaxSinglePutSize {
		return Config{}, fmt.Errorf("MAX_FILE_SIZE cannot exceed %s because direct uploads use a single S3 PUT", units.FormatBytes(MaxSinglePutSize))
	}

	retention, err := parseRetention(envOr("RETENTION", "7d"))
	if err != nil {
		return Config{}, fmt.Errorf("RETENTION: %w", err)
	}
	if retention < time.Hour || retention > 10*365*24*time.Hour {
		return Config{}, errors.New("RETENTION must be between 1h and 3650d")
	}

	downloadTTL, err := parseDuration("DOWNLOAD_URL_TTL", "5m")
	if err != nil {
		return Config{}, err
	}
	if downloadTTL < 30*time.Second || downloadTTL > time.Hour {
		return Config{}, errors.New("DOWNLOAD_URL_TTL must be between 30s and 1h")
	}

	uploadURLTTL, err := parseDuration("UPLOAD_URL_TTL", "30m")
	if err != nil {
		return Config{}, err
	}
	if uploadURLTTL < 5*time.Minute || uploadURLTTL > 2*time.Hour {
		return Config{}, errors.New("UPLOAD_URL_TTL must be between 5m and 2h")
	}

	uploadIntentTTL, err := parseDuration("UPLOAD_INTENT_TTL", "45m")
	if err != nil {
		return Config{}, err
	}
	if uploadIntentTTL < uploadURLTTL || uploadIntentTTL > 6*time.Hour {
		return Config{}, errors.New("UPLOAD_INTENT_TTL must be at least UPLOAD_URL_TTL and no more than 6h")
	}

	sessionTTL, err := parseDuration("ADMIN_SESSION_TTL", "12h")
	if err != nil {
		return Config{}, err
	}
	if sessionTTL < 15*time.Minute || sessionTTL > 7*24*time.Hour {
		return Config{}, errors.New("ADMIN_SESSION_TTL must be between 15m and 168h")
	}

	cookieSecure, err := parseCookieSecure(os.Getenv("COOKIE_SECURE"), base)
	if err != nil {
		return Config{}, err
	}

	trustProxy, err := parseBool("TRUST_PROXY", false)
	if err != nil {
		return Config{}, err
	}

	s3, err := loadS3()
	if err != nil {
		return Config{}, err
	}

	logLevel := strings.ToLower(envOr("LOG_LEVEL", "info"))
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}

	return Config{
		PublicBaseURL:   publicBaseURL,
		ListenAddr:      envOr("LISTEN_ADDR", ":8080"),
		DataDir:         dataDir,
		DatabasePath:    databasePath,
		SessionSecret:   secret,
		SessionTTL:      sessionTTL,
		CookieSecure:    cookieSecure,
		TrustProxy:      trustProxy,
		LogLevel:        logLevel,
		Retention:       retention,
		MaxStorage:      maxStorage,
		MaxFileSize:     maxFileSize,
		DownloadTTL:     downloadTTL,
		UploadURLTTL:    uploadURLTTL,
		UploadIntentTTL: uploadIntentTTL,
		S3:              s3,
	}, nil
}

func loadS3() (S3Config, error) {
	endpoint, err := optionalEndpoint("S3_ENDPOINT", os.Getenv("S3_ENDPOINT"))
	if err != nil {
		return S3Config{}, err
	}
	publicEndpoint, err := optionalEndpoint("S3_PUBLIC_ENDPOINT", os.Getenv("S3_PUBLIC_ENDPOINT"))
	if err != nil {
		return S3Config{}, err
	}
	if publicEndpoint == "" {
		publicEndpoint = endpoint
	}

	bucket := strings.TrimSpace(os.Getenv("S3_BUCKET"))
	if bucket == "" {
		return S3Config{}, errors.New("S3_BUCKET is required")
	}
	if strings.ContainsAny(bucket, "/ \t\r\n") {
		return S3Config{}, errors.New("S3_BUCKET must be a bare bucket name")
	}

	accessKey := strings.TrimSpace(os.Getenv("S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("S3_SECRET_KEY"))
	if (accessKey == "") != (secretKey == "") {
		return S3Config{}, errors.New("S3_ACCESS_KEY and S3_SECRET_KEY must be set together (leave both empty for the instance role chain)")
	}

	pathStyle, err := parseBool("S3_FORCE_PATH_STYLE", true)
	if err != nil {
		return S3Config{}, err
	}
	autoCreate, err := parseBool("S3_AUTO_CREATE_BUCKET", false)
	if err != nil {
		return S3Config{}, err
	}

	prefix := strings.Trim(strings.TrimSpace(os.Getenv("S3_OBJECT_PREFIX")), "/")
	if prefix == "" {
		prefix = "files"
	}
	prefix += "/"

	return S3Config{
		Endpoint:       endpoint,
		PublicEndpoint: publicEndpoint,
		Region:         envOr("S3_REGION", "us-east-1"),
		Bucket:         bucket,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		Prefix:         prefix,
		PathStyle:      pathStyle,
		AutoCreate:     autoCreate,
	}, nil
}

func parsePublicBaseURL(value string) (string, *url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, errors.New("PUBLIC_BASE_URL is required (for example https://files.example.com)")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", nil, fmt.Errorf("PUBLIC_BASE_URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", nil, errors.New("PUBLIC_BASE_URL must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil {
		return "", nil, errors.New("PUBLIC_BASE_URL must include a host and no credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("PUBLIC_BASE_URL cannot contain a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", nil, errors.New("PUBLIC_BASE_URL cannot contain a path prefix")
	}
	parsed.Path = ""
	return strings.TrimSuffix(parsed.String(), "/"), parsed, nil
}

func optionalEndpoint(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", name)
	}
	if parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("%s must include a host and no credentials", name)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%s cannot contain a path prefix", name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s cannot contain a query or fragment", name)
	}
	return strings.TrimSuffix(value, "/"), nil
}

func loadSessionSecret(dataDir string) ([]byte, error) {
	if configured := strings.TrimSpace(os.Getenv("SESSION_SECRET")); configured != "" {
		if len(configured) < 32 {
			return nil, errors.New("SESSION_SECRET must be at least 32 characters")
		}
		return []byte(configured), nil
	}

	path := filepath.Join(dataDir, "session_secret")
	data, err := os.ReadFile(path)
	if err == nil {
		if len(strings.TrimSpace(string(data))) < 32 {
			return nil, fmt.Errorf("%s is invalid; remove it or set SESSION_SECRET", path)
		}
		return []byte(strings.TrimSpace(string(data))), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read session secret: %w", err)
	}

	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	secret := []byte(base64.RawURLEncoding.EncodeToString(raw))
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, fmt.Errorf("write session secret: %w", err)
	}
	return secret, nil
}

func parseCookieSecure(value string, base *url.URL) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return base.Scheme == "https", nil
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, errors.New("COOKIE_SECURE must be auto, true, or false")
	}
}

func parseBool(name string, fallback bool) (bool, error) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback, nil
	}
	switch value {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}

func parseDuration(name, fallback string) (time.Duration, error) {
	value := envOr(name, fallback)
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return duration, nil
}

// parseRetention accepts standard Go durations plus day-based values such as
// 7d, 30d, or 1.5d.
func parseRetention(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return DefaultRetention, nil
	}
	suffix := ""
	for _, candidate := range []string{"days", "day", "d"} {
		if strings.HasSuffix(value, candidate) {
			suffix = candidate
			break
		}
	}
	if suffix != "" {
		number := strings.TrimSpace(strings.TrimSuffix(value, suffix))
		days, err := strconv.ParseFloat(number, 64)
		if err != nil || days <= 0 || days > 3650 {
			return 0, fmt.Errorf("invalid day duration %q (examples: 7d, 30d, 1.5d)", value)
		}
		duration := time.Duration(days * 24 * float64(time.Hour))
		if duration <= 0 {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		return duration, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 12h, 7d, 30d)", value)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return duration, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
