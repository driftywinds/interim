// Package config loads interim's runtime configuration from the environment,
// with an optional .env file for local development.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/joho/godotenv"

	"interim/internal/units"
)

const (
	// DefaultRetention is the default file lifetime. Operators can override it
	// with RETENTION in the environment.
	DefaultRetention = 7 * 24 * time.Hour

	// MaxSinglePutSize is the S3 single-PUT object limit.
	MaxSinglePutSize int64 = 5 << 30

	// MaxMultipartObjectSize is the S3 maximum object size. The effective
	// multipart part size is raised as needed to stay under MaxMultipartParts.
	MaxMultipartObjectSize int64 = 5 << 40
	MinMultipartPartSize   int64 = 5 << 20
	MaxMultipartParts            = 10000
)

type Config struct {
	PublicBaseURL     string
	ListenAddr        string
	DataDir           string
	DatabasePath      string
	SessionSecret     []byte
	SessionTTL        time.Duration
	CookieSecure      bool
	TrustProxy        bool
	TrustedProxyCIDRs []netip.Prefix
	SetupToken        string
	SetupTokenPath    string
	LogLevel          string

	Retention                  time.Duration
	MaxStorage                 int64
	MaxFileSize                int64
	DownloadTTL                time.Duration
	UploadURLTTL               time.Duration
	UploadIntentTTL            time.Duration
	MaxFiles                   int64
	MaxPendingUploadsPerClient int
	MaxReservedBytesPerClient  int64
	MultipartEnabled           bool
	MultipartThreshold         int64
	MultipartPartSize          int64
	MultipartConcurrency       int
	MultipartSignPartLength    bool

	PublicUploadsEnabled bool

	DownloadWorkerEnabled bool
	DownloadWorkerURLs    []string
	DownloadWorkerSecret  string

	S3 S3Config
}

type S3Config struct {
	Endpoint                 string
	PublicEndpoint           string
	Region                   string
	Bucket                   string
	AccessKey                string
	SecretKey                string
	PutAccessKey             string
	PutSecretKey             string
	Prefix                   string
	PathStyle                bool
	AutoCreate               bool
	PublicEndpointBlocksCopy bool
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

	setupToken, setupTokenPath, err := loadSetupToken(dataDir)
	if err != nil {
		return Config{}, err
	}

	maxStorage, err := parseStorageLimit(envOr("MAX_STORAGE", "20GB"))
	if err != nil {
		return Config{}, fmt.Errorf("MAX_STORAGE: %w", err)
	}
	maxFileSize, err := units.ParseBytes(envOr("MAX_FILE_SIZE", "5GB"))
	if err != nil {
		return Config{}, fmt.Errorf("MAX_FILE_SIZE: %w", err)
	}
	if maxStorage > 0 && maxFileSize > maxStorage {
		return Config{}, errors.New("MAX_FILE_SIZE cannot be larger than MAX_STORAGE")
	}

	maxFiles, err := parseInt64Limit("MAX_FILES", 100000, 0, 1_000_000_000)
	if err != nil {
		return Config{}, err
	}
	maxPending, err := parseIntLimit("MAX_PENDING_UPLOADS_PER_CLIENT", 2, 0, 1000)
	if err != nil {
		return Config{}, err
	}
	maxReservedBytes, err := parseOptionalByteLimit(
		"MAX_RESERVED_BYTES_PER_CLIENT",
		strconv.FormatInt(maxFileSize, 10),
	)
	if err != nil {
		return Config{}, err
	}

	multipartEnabled, err := parseBool("MULTIPART_UPLOAD_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	multipartThreshold, err := parseByteLimitAllowZero("MULTIPART_THRESHOLD", "64MB")
	if err != nil {
		return Config{}, err
	}
	multipartPartSize, err := units.ParseBytes(envOr("MULTIPART_PART_SIZE", "16MB"))
	if err != nil {
		return Config{}, fmt.Errorf("MULTIPART_PART_SIZE: %w", err)
	}
	multipartConcurrency, err := parseIntLimit("MULTIPART_CONCURRENCY", 3, 1, 8)
	if err != nil {
		return Config{}, err
	}
	multipartSignPartLength, err := parseBool("MULTIPART_SIGN_PART_LENGTH", true)
	if err != nil {
		return Config{}, err
	}
	if multipartThreshold > MaxSinglePutSize {
		return Config{}, fmt.Errorf("MULTIPART_THRESHOLD cannot exceed %s", units.FormatBytes(MaxSinglePutSize))
	}
	if multipartPartSize < MinMultipartPartSize {
		return Config{}, fmt.Errorf("MULTIPART_PART_SIZE must be at least %s", units.FormatBytes(MinMultipartPartSize))
	}
	if multipartPartSize > MaxSinglePutSize {
		return Config{}, fmt.Errorf("MULTIPART_PART_SIZE cannot exceed %s", units.FormatBytes(MaxSinglePutSize))
	}
	if multipartEnabled {
		if maxFileSize > MaxMultipartObjectSize {
			return Config{}, fmt.Errorf("MAX_FILE_SIZE cannot exceed %s with multipart uploads", units.FormatBytes(MaxMultipartObjectSize))
		}
	} else if maxFileSize > MaxSinglePutSize {
		return Config{}, fmt.Errorf("MAX_FILE_SIZE cannot exceed %s because single-PUT uploads are enabled", units.FormatBytes(MaxSinglePutSize))
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
	trustedProxyCIDRs, err := parseTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}
	if trustProxy && len(trustedProxyCIDRs) == 0 {
		return Config{}, errors.New("TRUSTED_PROXY_CIDRS is required when TRUST_PROXY=true")
	}

	publicUploadsEnabled, err := parseBool("PUBLIC_UPLOADS_ENABLED", true)
	if err != nil {
		return Config{}, err
	}

	downloadWorkerEnabled, err := parseBool("DOWNLOAD_WORKER_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	downloadWorkerURLs, downloadWorkerSecret, err := loadDownloadWorker(
		downloadWorkerEnabled,
		os.Getenv("DOWNLOAD_WORKER_URLS"),
		os.Getenv("DOWNLOAD_WORKER_SECRET"),
	)
	if err != nil {
		return Config{}, err
	}

	s3, err := loadS3()
	if err != nil {
		return Config{}, err
	}
	if multipartEnabled && s3.PutAccessKey == "" && !s3.PublicEndpointBlocksCopy {
		return Config{}, errors.New("multipart uploads require S3_PUT_ACCESS_KEY/S3_PUT_SECRET_KEY, or S3_PUBLIC_ENDPOINT_BLOCKS_COPY=true when the public endpoint blocks X-Amz-Copy-Source")
	}

	logLevel := strings.ToLower(envOr("LOG_LEVEL", "info"))
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}

	return Config{
		PublicBaseURL:              publicBaseURL,
		ListenAddr:                 envOr("LISTEN_ADDR", ":8080"),
		DataDir:                    dataDir,
		DatabasePath:               databasePath,
		SessionSecret:              secret,
		SessionTTL:                 sessionTTL,
		CookieSecure:               cookieSecure,
		TrustProxy:                 trustProxy,
		TrustedProxyCIDRs:          trustedProxyCIDRs,
		SetupToken:                 setupToken,
		SetupTokenPath:             setupTokenPath,
		LogLevel:                   logLevel,
		Retention:                  retention,
		MaxStorage:                 maxStorage,
		MaxFileSize:                maxFileSize,
		DownloadTTL:                downloadTTL,
		UploadURLTTL:               uploadURLTTL,
		UploadIntentTTL:            uploadIntentTTL,
		MaxFiles:                   maxFiles,
		MaxPendingUploadsPerClient: maxPending,
		MaxReservedBytesPerClient:  maxReservedBytes,
		MultipartEnabled:           multipartEnabled,
		MultipartThreshold:         multipartThreshold,
		MultipartPartSize:          multipartPartSize,
		MultipartConcurrency:       multipartConcurrency,
		MultipartSignPartLength:    multipartSignPartLength,
		PublicUploadsEnabled:       publicUploadsEnabled,
		DownloadWorkerEnabled:      downloadWorkerEnabled,
		DownloadWorkerURLs:         downloadWorkerURLs,
		DownloadWorkerSecret:       downloadWorkerSecret,
		S3:                         s3,
	}, nil
}

func loadDownloadWorker(enabled bool, rawURLs, rawSecret string) ([]string, string, error) {
	if !enabled {
		return nil, "", nil
	}

	secret := strings.TrimSpace(rawSecret)
	if len(secret) < 32 {
		return nil, "", errors.New("DOWNLOAD_WORKER_SECRET must be at least 32 characters when DOWNLOAD_WORKER_ENABLED=true")
	}

	parts := strings.FieldsFunc(rawURLs, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	if len(parts) == 0 {
		return nil, "", errors.New("DOWNLOAD_WORKER_URLS must contain at least one URL when DOWNLOAD_WORKER_ENABLED=true")
	}

	seen := make(map[string]struct{}, len(parts))
	urls := make([]string, 0, len(parts))
	for _, part := range parts {
		parsed, err := url.Parse(strings.TrimSpace(part))
		if err != nil {
			return nil, "", fmt.Errorf("DOWNLOAD_WORKER_URLS: %w", err)
		}
		if parsed.Scheme != "https" {
			return nil, "", errors.New("DOWNLOAD_WORKER_URLS entries must use https")
		}
		if parsed.Host == "" || parsed.User != nil {
			return nil, "", errors.New("DOWNLOAD_WORKER_URLS entries must include a host and no credentials")
		}
		if parsed.Path != "" && parsed.Path != "/" {
			return nil, "", errors.New("DOWNLOAD_WORKER_URLS entries cannot contain a path; the /d/<token> path is appended")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, "", errors.New("DOWNLOAD_WORKER_URLS entries cannot contain a query or fragment")
		}
		base := strings.TrimSuffix(parsed.String(), "/")
		if _, ok := seen[base]; ok {
			continue
		}
		seen[base] = struct{}{}
		urls = append(urls, base)
	}
	if len(urls) == 0 {
		return nil, "", errors.New("DOWNLOAD_WORKER_URLS must contain at least one URL")
	}
	return urls, secret, nil
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
	putAccessKey := strings.TrimSpace(os.Getenv("S3_PUT_ACCESS_KEY"))
	putSecretKey := strings.TrimSpace(os.Getenv("S3_PUT_SECRET_KEY"))
	if (putAccessKey == "") != (putSecretKey == "") {
		return S3Config{}, errors.New("S3_PUT_ACCESS_KEY and S3_PUT_SECRET_KEY must be set together")
	}

	pathStyle, err := parseBool("S3_FORCE_PATH_STYLE", true)
	if err != nil {
		return S3Config{}, err
	}
	autoCreate, err := parseBool("S3_AUTO_CREATE_BUCKET", false)
	if err != nil {
		return S3Config{}, err
	}
	blocksCopy, err := parseBool("S3_PUBLIC_ENDPOINT_BLOCKS_COPY", false)
	if err != nil {
		return S3Config{}, err
	}

	prefix := strings.Trim(strings.TrimSpace(os.Getenv("S3_OBJECT_PREFIX")), "/")
	if prefix == "" {
		prefix = "files"
	}
	prefix += "/"

	return S3Config{
		Endpoint:                 endpoint,
		PublicEndpoint:           publicEndpoint,
		Region:                   envOr("S3_REGION", "us-east-1"),
		Bucket:                   bucket,
		AccessKey:                accessKey,
		SecretKey:                secretKey,
		PutAccessKey:             putAccessKey,
		PutSecretKey:             putSecretKey,
		Prefix:                   prefix,
		PathStyle:                pathStyle,
		AutoCreate:               autoCreate,
		PublicEndpointBlocksCopy: blocksCopy,
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

func loadSetupToken(dataDir string) (string, string, error) {
	if configured := strings.TrimSpace(os.Getenv("SETUP_TOKEN")); configured != "" {
		if len(configured) < 32 {
			return "", "", errors.New("SETUP_TOKEN must be at least 32 characters")
		}
		return configured, "", nil
	}

	path := filepath.Join(dataDir, "setup_token")
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 32 {
			return "", "", fmt.Errorf("%s is invalid; remove it or set SETUP_TOKEN", path)
		}
		return token, path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("read setup token: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate setup token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", "", fmt.Errorf("write setup token: %w", err)
	}
	return token, path, nil
}

func parseInt64Limit(name string, fallback, minValue, maxValue int64) (int64, error) {
	raw := envOr(name, strconv.FormatInt(fallback, 10))
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minValue, maxValue)
	}
	return value, nil
}

func parseIntLimit(name string, fallback, minValue, maxValue int) (int, error) {
	raw := envOr(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minValue, maxValue)
	}
	return value, nil
}

func parseByteLimitAllowZero(name, fallback string) (int64, error) {
	raw := envOr(name, fallback)
	if strings.TrimSpace(raw) == "0" {
		return 0, nil
	}
	value, err := units.ParseBytes(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func parseOptionalByteLimit(name, fallback string) (int64, error) {
	raw := envOr(name, fallback)
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "0b", "unlimited":
		return 0, nil
	}
	value, err := units.ParseBytes(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func parseTrustedProxyCIDRs(raw string) ([]netip.Prefix, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			addr, addrErr := netip.ParseAddr(part)
			if addrErr != nil {
				return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: invalid CIDR or IP %q", part)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
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

// parseStorageLimit accepts a byte size, or zero/unlimited to disable the
// global storage cap. MAX_FILE_SIZE remains enforced independently.
func parseStorageLimit(value string) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "0b", "unlimited":
		return 0, nil
	}
	return units.ParseBytes(value)
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
