package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, adminID int64, csrf string, createdAt, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, admin_id, csrf_token, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		tokenHash, adminID, csrf, toUnix(createdAt), toUnix(expiresAt),
	)
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	return nil
}

func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (Session, error) {
	var session Session
	var created, expires int64
	err := s.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.admin_id, a.username, s.csrf_token, s.created_at, s.expires_at
		 FROM sessions s
		 JOIN admin a ON a.id = s.admin_id
		 WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, toUnix(now),
	).Scan(&session.TokenHash, &session.AdminID, &session.Username, &session.CSRF, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get admin session: %w", err)
	}
	session.CreatedAt = scanTime(created)
	session.ExpiresAt = scanTime(expires)
	return session, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash); err != nil {
		return fmt.Errorf("delete admin session: %w", err)
	}
	return nil
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", toUnix(now))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return result.RowsAffected()
}
