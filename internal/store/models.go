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
	TokenHash []byte
	AdminID   int64
	Username  string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type APIKey struct {
	ID         string
	KeyHash    []byte
	Prefix     string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Disabled   bool
	FileCount  int64
	FileBytes  int64
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

type UploadIntent struct {
	ID              string
	ObjectKey       string
	APIKeyID        *string
	OriginalName    string
	ContentType     string
	DeclaredSize    int64
	UploadTokenHash []byte
	DeleteTokenHash []byte
	State           string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	CompletedAt     *time.Time
	FileID          *string
}
