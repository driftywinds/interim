package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) AdminCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin").Scan(&count); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return count, nil
}

func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	count, err := s.AdminCount(ctx)
	return count > 0, err
}

func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string, now time.Time) (Admin, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO admin (id, username, password_hash, created_at)
		 SELECT 1, ?, ?, ?
		 WHERE NOT EXISTS (SELECT 1 FROM admin)`,
		username, passwordHash, toUnix(now),
	)
	if err != nil {
		return Admin{}, fmt.Errorf("create admin: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Admin{}, fmt.Errorf("create admin rows: %w", err)
	}
	if affected == 0 {
		return Admin{}, ErrAlreadySetup
	}
	return Admin{ID: 1, Username: username, PasswordHash: passwordHash, CreatedAt: now.UTC()}, nil
}

func (s *Store) AdminByUsername(ctx context.Context, username string) (Admin, error) {
	var admin Admin
	var created int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, created_at FROM admin WHERE username = ?",
		username,
	).Scan(&admin.ID, &admin.Username, &admin.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	if err != nil {
		return Admin{}, fmt.Errorf("get admin: %w", err)
	}
	admin.CreatedAt = scanTime(created)
	return admin, nil
}

// UpdateAdminPassword replaces the password hash and revokes every existing
// session in the same transaction.
func (s *Store) UpdateAdminPassword(ctx context.Context, id int64, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin password update: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		"UPDATE admin SET password_hash = ? WHERE id = ?", passwordHash, id,
	)
	if err != nil {
		return fmt.Errorf("update admin password: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update admin password rows: %w", err)
	}
	if affected != 1 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE admin_id = ?", id); err != nil {
		return fmt.Errorf("revoke sessions after password change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit password update: %w", err)
	}
	return nil
}

func (s *Store) AdminByID(ctx context.Context, id int64) (Admin, error) {
	var admin Admin
	var created int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, created_at FROM admin WHERE id = ?",
		id,
	).Scan(&admin.ID, &admin.Username, &admin.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	if err != nil {
		return Admin{}, fmt.Errorf("get admin by id: %w", err)
	}
	admin.CreatedAt = scanTime(created)
	return admin, nil
}
