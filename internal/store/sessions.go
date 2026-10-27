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
		`INSERT INTO sessions (token_hash, admin_id, csrf_token, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash, adminID, csrf, toUnix(createdAt), toUnix(expiresAt), toUnix(createdAt),
	)
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	return nil
}

func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (Session, error) {
	var session Session
	var created, expires, lastSeen int64
	err := s.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.admin_id, a.username, s.csrf_token, s.created_at, s.expires_at, s.last_seen_at
		 FROM sessions s
		 JOIN admin a ON a.id = s.admin_id
		 WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, toUnix(now),
	).Scan(&session.TokenHash, &session.AdminID, &session.Username, &session.CSRF, &created, &expires, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get admin session: %w", err)
	}
	session.CreatedAt = scanTime(created)
	session.ExpiresAt = scanTime(expires)
	session.LastSeenAt = scanTime(lastSeen)
	if lastSeen == 0 {
		session.LastSeenAt = session.CreatedAt
	}
	return session, nil
}

func (s *Store) TouchSession(ctx context.Context, tokenHash []byte, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?", toUnix(now), tokenHash,
	); err != nil {
		return fmt.Errorf("touch admin session: %w", err)
	}
	return nil
}

func (s *Store) DeleteSessionsForAdmin(ctx context.Context, adminID int64) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE admin_id = ?", adminID); err != nil {
		return fmt.Errorf("delete admin sessions: %w", err)
	}
	return nil
}

func (s *Store) DeleteIdleSessions(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM sessions WHERE last_seen_at > 0 AND last_seen_at <= ?", toUnix(before),
	)
	if err != nil {
		return 0, fmt.Errorf("delete idle sessions: %w", err)
	}
	return result.RowsAffected()
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
