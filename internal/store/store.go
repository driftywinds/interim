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
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 6"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set schema version: %w", err)
	}

	return &Store{db: db}, nil
}

// migrate adds columns that cannot be introduced with CREATE TABLE IF NOT
// EXISTS. Existing user hashes deliberately start unapproved: enabling the
// upload allowlist must not silently authorize hashes created earlier.
func migrate(ctx context.Context, db *sql.DB) error {
	if err := ensureColumn(ctx, db, "sessions", "last_seen_at",
		`ALTER TABLE sessions ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "api_keys", "approved",
		`ALTER TABLE api_keys ADD COLUMN approved INTEGER NOT NULL DEFAULT 0 CHECK (approved IN (0, 1))`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "storage_reservations", "owner",
		`ALTER TABLE storage_reservations ADD COLUMN owner TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "upload_intents", "multipart_upload_id",
		`ALTER TABLE upload_intents ADD COLUMN multipart_upload_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "upload_intents", "part_size",
		`ALTER TABLE upload_intents ADD COLUMN part_size INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "upload_intents", "part_count",
		`ALTER TABLE upload_intents ADD COLUMN part_count INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS storage_reservations_owner_expires
		 ON storage_reservations(owner, expires_at)`,
	); err != nil {
		return fmt.Errorf("create reservation owner index: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS storage_reservations_owner_expires_bytes
		 ON storage_reservations(owner, expires_at, bytes)`,
	); err != nil {
		return fmt.Errorf("create reservation owner bytes index: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO usage_totals (id, files, file_bytes, reserved_bytes)
		 SELECT 1,
		        (SELECT COUNT(*) FROM files),
		        COALESCE((SELECT SUM(size) FROM files), 0),
		        COALESCE((SELECT SUM(bytes) FROM storage_reservations), 0)`,
	); err != nil {
		return fmt.Errorf("rebuild usage totals: %w", err)
	}
	return nil
}

func ensureColumn(ctx context.Context, db *sql.DB, table, column, alter string) error {
	var count int
	query := `SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name = ?`
	if err := db.QueryRowContext(ctx, query, column).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	if count == 0 {
		if _, err := db.ExecContext(ctx, alter); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, column, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// ResetForStartup removes only state that cannot survive a process restart.
// Pending upload intents and their reservations are persisted because the
// browser may already have uploaded an object; the janitor reconciles them.
func (s *Store) ResetForStartup(ctx context.Context) error {
	if _, err := s.CleanOrphanReservations(ctx); err != nil {
		return err
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
