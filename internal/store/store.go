// Package store contains the SQLite persistence layer for interim.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound     = errors.New("not found")
	ErrAlreadySetup = errors.New("admin account already exists")
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	dsn := "file:" + url.PathEscape(filepath.ToSlash(abs)) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	// SQLite has a single writer. Keeping one pooled connection avoids
	// SQLITE_BUSY surprises and makes pragma setup deterministic.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set schema version: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// ResetForStartup removes state that cannot survive a process restart. Upload
// reservations from a previous process are orphaned because their HTTP
// request is gone.
func (s *Store) ResetForStartup(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM storage_reservations"); err != nil {
		return fmt.Errorf("clear stale upload reservations: %w", err)
	}
	if _, err := s.DeleteExpiredSessions(ctx, time.Now()); err != nil {
		return err
	}
	return nil
}

func scanTime(value int64) time.Time {
	return time.Unix(value, 0).UTC()
}

func toUnix(t time.Time) int64 {
	return t.UTC().Unix()
}

func nullableUnix(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := scanTime(value.Int64)
	return &t
}
