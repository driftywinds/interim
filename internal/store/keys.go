package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) CreateAPIKey(ctx context.Context, key APIKey) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys
		 (id, key_hash, key_prefix, name, created_at, disabled, approved, max_file_size, retention_seconds)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		key.ID, key.KeyHash, key.Prefix, key.Name, toUnix(key.CreatedAt), boolInt(key.Approved),
		key.MaxFileSize, key.RetentionSeconds,
	)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

func (s *Store) APIKeyByHash(ctx context.Context, hash []byte) (APIKey, error) {
	var key APIKey
	var created int64
	var lastUsed sql.NullInt64
	var disabled, approved int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, key_hash, key_prefix, name, created_at, last_used_at, disabled, approved,
		        max_file_size, retention_seconds
		 FROM api_keys WHERE key_hash = ?`,
		hash,
	).Scan(&key.ID, &key.KeyHash, &key.Prefix, &key.Name, &created, &lastUsed, &disabled, &approved,
		&key.MaxFileSize, &key.RetentionSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("get api key: %w", err)
	}
	key.CreatedAt = scanTime(created)
	key.LastUsedAt = nullableUnix(lastUsed)
	key.Disabled = disabled == 1
	key.Approved = approved == 1
	return key, nil
}

func (s *Store) APIKeyByID(ctx context.Context, id string) (APIKey, error) {
	var key APIKey
	var created int64
	var lastUsed sql.NullInt64
	var disabled, approved int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, key_hash, key_prefix, name, created_at, last_used_at, disabled, approved,
		        max_file_size, retention_seconds
		 FROM api_keys WHERE id = ?`,
		id,
	).Scan(&key.ID, &key.KeyHash, &key.Prefix, &key.Name, &created, &lastUsed, &disabled, &approved,
		&key.MaxFileSize, &key.RetentionSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("get api key by id: %w", err)
	}
	key.CreatedAt = scanTime(created)
	key.LastUsedAt = nullableUnix(lastUsed)
	key.Disabled = disabled == 1
	key.Approved = approved == 1
	return key, nil
}

func (s *Store) TouchAPIKey(ctx context.Context, id string, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET last_used_at = ? WHERE id = ?", toUnix(now), id,
	); err != nil {
		return fmt.Errorf("touch api key: %w", err)
	}
	return nil
}

func (s *Store) APIKeyUsage(ctx context.Context, id string) (APIKeyUsage, error) {
	var usage APIKeyUsage
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files WHERE api_key_id = ?`,
		id,
	).Scan(&usage.Files, &usage.Bytes)
	if err != nil {
		return APIKeyUsage{}, fmt.Errorf("api key usage: %w", err)
	}
	return usage, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT k.id, k.key_hash, k.key_prefix, k.name, k.created_at, k.last_used_at, k.disabled, k.approved,
		        k.max_file_size, k.retention_seconds, COUNT(f.id), COALESCE(SUM(f.size), 0)
		 FROM api_keys k
		 LEFT JOIN files f ON f.api_key_id = k.id
		 GROUP BY k.id
		 ORDER BY k.created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var key APIKey
		var created int64
		var lastUsed sql.NullInt64
		var disabled, approved int
		if err := rows.Scan(
			&key.ID, &key.KeyHash, &key.Prefix, &key.Name, &created, &lastUsed, &disabled, &approved,
			&key.MaxFileSize, &key.RetentionSeconds, &key.FileCount, &key.FileBytes,
		); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		key.CreatedAt = scanTime(created)
		key.LastUsedAt = nullableUnix(lastUsed)
		key.Disabled = disabled == 1
		key.Approved = approved == 1
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api keys: %w", err)
	}
	return keys, nil
}

func (s *Store) SetAPIKeyLimits(ctx context.Context, id string, maxFileSize, retentionSeconds int64) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET max_file_size = ?, retention_seconds = ? WHERE id = ?",
		maxFileSize, retentionSeconds, id,
	)
	if err != nil {
		return fmt.Errorf("update api key limits: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update api key limits rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetAPIKeyDisabled(ctx context.Context, id string, disabled bool) error {
	return s.updateAPIKeyFlag(ctx, id, "disabled", disabled)
}

func (s *Store) SetAPIKeyApproved(ctx context.Context, id string, approved bool) error {
	return s.updateAPIKeyFlag(ctx, id, "approved", approved)
}

func (s *Store) updateAPIKeyFlag(ctx context.Context, id, column string, value bool) error {
	// The column name is internal and never comes from a request.
	result, err := s.db.ExecContext(ctx, "UPDATE api_keys SET "+column+" = ? WHERE id = ?", boolInt(value), id)
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update api key rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
