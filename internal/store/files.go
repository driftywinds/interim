package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrReservationMissing = errors.New("upload reservation is missing or expired")
)

const fileColumns = `f.id, f.object_key, f.api_key_id, f.original_name, f.content_type,
	f.size, f.delete_token_hash, f.created_at, f.expires_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFile(scanner rowScanner) (File, error) {
	var file File
	var apiKeyID sql.NullString
	var created, expires int64
	err := scanner.Scan(
		&file.ID, &file.ObjectKey, &apiKeyID, &file.OriginalName, &file.ContentType,
		&file.Size, &file.DeleteTokenHash, &created, &expires,
	)
	if err != nil {
		return File{}, err
	}
	if apiKeyID.Valid {
		file.APIKeyID = &apiKeyID.String
	}
	file.CreatedAt = scanTime(created)
	file.ExpiresAt = scanTime(expires)
	return file, nil
}

// ReserveStorage atomically reserves capacity. A single INSERT ... SELECT keeps
// concurrent uploads from overshooting the configured maximum. A non-positive
// limit disables the global cap and reserves unconditionally.
func (s *Store) ReserveStorage(ctx context.Context, id string, bytes, limit int64, now, expiresAt time.Time) (bool, error) {
	if limit <= 0 {
		result, err := s.db.ExecContext(ctx,
			`INSERT INTO storage_reservations (id, bytes, created_at, expires_at)
			 VALUES (?, ?, ?, ?)`,
			id, bytes, toUnix(now), toUnix(expiresAt),
		)
		if err != nil {
			return false, fmt.Errorf("reserve storage: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("reserve storage rows: %w", err)
		}
		return affected == 1, nil
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO storage_reservations (id, bytes, created_at, expires_at)
		 SELECT ?, ?, ?, ?
		 WHERE (
		     COALESCE((SELECT SUM(size) FROM files), 0) +
		     COALESCE((SELECT SUM(bytes) FROM storage_reservations WHERE expires_at > ?), 0) +
		     ?
		 ) <= ?`,
		id, bytes, toUnix(now), toUnix(expiresAt), toUnix(now), bytes, limit,
	)
	if err != nil {
		return false, fmt.Errorf("reserve storage: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reserve storage rows: %w", err)
	}
	return affected == 1, nil
}

func (s *Store) ReleaseReservation(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM storage_reservations WHERE id = ?", id); err != nil {
		return fmt.Errorf("release storage reservation: %w", err)
	}
	return nil
}

func (s *Store) ClearExpiredReservations(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM storage_reservations WHERE expires_at <= ?", toUnix(now),
	)
	if err != nil {
		return 0, fmt.Errorf("clear expired reservations: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) FileByID(ctx context.Context, id string) (File, error) {
	file, err := scanFile(s.db.QueryRowContext(ctx,
		"SELECT "+fileColumns+" FROM files f WHERE f.id = ?", id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("get file: %w", err)
	}
	return file, nil
}

func (s *Store) DeleteFileRow(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM files WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("delete file row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete file row count: %w", err)
	}
	return affected == 1, nil
}

func (s *Store) DeleteExpiredFileRow(ctx context.Context, id string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM files WHERE id = ? AND expires_at <= ?", id, toUnix(now),
	)
	if err != nil {
		return false, fmt.Errorf("delete expired file row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete expired file row count: %w", err)
	}
	return affected == 1, nil
}

func (s *Store) ExpiredFiles(ctx context.Context, now time.Time, limit int) ([]File, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+fileColumns+" FROM files f WHERE f.expires_at <= ? ORDER BY f.expires_at ASC LIMIT ?",
		toUnix(now), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query expired files: %w", err)
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired file: %w", err)
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired files: %w", err)
	}
	return files, nil
}

func (s *Store) StorageUsage(ctx context.Context, now time.Time) (Usage, error) {
	var usage Usage
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files",
	).Scan(&usage.Files, &usage.Used); err != nil {
		return Usage{}, fmt.Errorf("files storage usage: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(bytes), 0) FROM storage_reservations WHERE expires_at > ?",
		toUnix(now),
	).Scan(&usage.Reserved); err != nil {
		return Usage{}, fmt.Errorf("reserved storage usage: %w", err)
	}
	return usage, nil
}

func (s *Store) ListFilesAdmin(ctx context.Context, filter FileFilter) ([]AdminFile, int64, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	search := strings.TrimSpace(filter.Search)
	where := `WHERE (? = '' OR instr(lower(f.original_name), lower(?)) > 0 OR instr(lower(f.id), lower(?)) > 0)`

	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files f "+where,
		search, search, search,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admin files: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+`, COALESCE(k.key_prefix, ''), COALESCE(k.name, '')
		 FROM files f
		 LEFT JOIN api_keys k ON k.id = f.api_key_id
		 `+where+`
		 ORDER BY f.created_at DESC
		 LIMIT ? OFFSET ?`,
		search, search, search, filter.Limit, filter.Offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin files: %w", err)
	}
	defer rows.Close()

	var files []AdminFile
	for rows.Next() {
		var adminFile AdminFile
		var apiKeyID sql.NullString
		var created, expires int64
		if err := rows.Scan(
			&adminFile.ID, &adminFile.ObjectKey, &apiKeyID, &adminFile.OriginalName,
			&adminFile.ContentType, &adminFile.Size, &adminFile.DeleteTokenHash,
			&created, &expires, &adminFile.KeyPrefix, &adminFile.KeyName,
		); err != nil {
			return nil, 0, fmt.Errorf("scan admin file: %w", err)
		}
		if apiKeyID.Valid {
			adminFile.APIKeyID = &apiKeyID.String
		}
		adminFile.CreatedAt = scanTime(created)
		adminFile.ExpiresAt = scanTime(expires)
		files = append(files, adminFile)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate admin files: %w", err)
	}
	return files, total, nil
}

func (s *Store) FilesByAPIKey(ctx context.Context, keyID string, now time.Time, limit, offset int) ([]File, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files WHERE api_key_id = ? AND expires_at > ?",
		keyID, toUnix(now),
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count api files: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+fileColumns+" FROM files f WHERE f.api_key_id = ? AND f.expires_at > ? ORDER BY f.created_at DESC LIMIT ? OFFSET ?",
		keyID, toUnix(now), limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list api files: %w", err)
	}
	defer rows.Close()

	files := make([]File, 0, limit)
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan api file: %w", err)
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate api files: %w", err)
	}
	return files, total, nil
}
