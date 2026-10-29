package server

import (
	"time"

	"github.com/driftywinds/interim/internal/store"
)

type PageData struct {
	Title          string
	Description    string
	Active         string
	IsAdmin        bool
	AdminName      string
	CSRF           string
	Flash          string
	BaseURL        string
	Wide           bool
	NoIndex        bool
	RetentionLabel string
	RetentionRule  string
	// ExtendRetentionOnDownload tells templates to describe sliding retention.
	ExtendRetentionOnDownload bool
	MaxFileSize               int64
	MaxStorage                int64
	DownloadTTL               time.Duration
	CurrentPath               string
	AssetVersion              string
	ContactEmail              string
	DonationURL               string
}

type StatusView struct {
	Files         int64
	Used          int64
	Reserved      int64
	Limit         int64
	Available     int64
	UsedPercent   float64
	Accepting     bool
	PublicUploads bool
	Unlimited     bool
	MaxFileSize   int64
}

type HomeView struct {
	PageData
	Status StatusView
}

type FilePageView struct {
	PageData
	ID                string
	Name              string
	Size              int64
	ContentType       string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ExpiresUnix       int64
	ExpiresIn         string
	NeverExpires      bool
	FileRetentionRule string
	PageURL           string
	DownloadTTL       string
	IsExpired         bool
}

type APIDocsView struct {
	PageData
	APIBase          string
	OpenAPIJSON      string
	OpenAPIYAML      string
	MaxFileSize      string
	MaxStorage       string
	StorageUnlimited bool
	PublicUploads    bool
	MultipartEnabled bool
	DownloadTTL      string
	UploadURLTTL     string
}

type AdminKeyView struct {
	store.APIKey
	FileLimitLabel      string
	MaxFileSizeInput    string
	RetentionLimitLabel string
	RetentionInput      string
}

type AdminView struct {
	PageData
	Usage              store.Usage
	Percent            float64
	Unlimited          bool
	Files              []store.AdminFile
	Keys               []AdminKeyView
	Total              int64
	Page               int
	Pages              int
	Query              string
	PrevURL            string
	NextURL            string
	Deleted            int
	Failed             int
	Now                time.Time
	PublicUploads      bool
	NewKey             string
	NewKeyName         string
	DefaultMaxFileSize string
	DefaultRetention   string
}

type AuthView struct {
	PageData
	Mode     string
	Username string
	Error    string
}

type ErrorView struct {
	PageData
	StatusCode int
	Heading    string
	Message    string
}
