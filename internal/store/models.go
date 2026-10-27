package store

import (
	"time"
)

type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

type Session struct {
	TokenHash  []byte
	AdminID    int64
	Username   string
	CSRF       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

type APIKey struct {
	ID         string
	KeyHash    []byte
	Prefix     string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Disabled   bool
	Approved   bool
	// MaxFileSize and RetentionSeconds are per-hash overrides. -1 inherits the
	// instance default; 0 means unlimited; a positive value is the limit.
	MaxFileSize      int64
	RetentionSeconds int64
	FileCount        int64
	FileBytes        int64
}

type File struct {
	ID              string
	ObjectKey       string
	APIKeyID        *string
	OriginalName    string
	ContentType     string
	Size            int64
	DeleteTokenHash []byte
	CreatedAt       time.Time
	ExpiresAt       time.Time
	// RetentionSeconds is the per-file retention captured at upload. Nil means
	// the file predates per-key retention and follows the instance default.
	RetentionSeconds *int64
	DeletingAt       *time.Time
}

type AdminFile struct {
	File
	KeyPrefix string
	KeyName   string
}

type Usage struct {
	Files    int64
	Used     int64
	Reserved int64
}

type FileFilter struct {
	Search string
	Limit  int
	Offset int
}

type APIKeyUsage struct {
	Files int64
	Bytes int64
}

type ReservationLimits struct {
	TotalBytes    int64
	TotalFiles    int64
	ClientBytes   int64
	ClientPending int
}

type UploadIntent struct {
	ID                string
	ObjectKey         string
	APIKeyID          *string
	OriginalName      string
	ContentType       string
	DeclaredSize      int64
	UploadTokenHash   []byte
	DeleteTokenHash   []byte
	MultipartUploadID string
	PartSize          int64
	PartCount         int
	State             string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	CompletedAt       *time.Time
	FileID            *string
}

type UploadPart struct {
	UploadID   string
	PartNumber int
	ETag       string
	Size       int64
	CreatedAt  time.Time
}
