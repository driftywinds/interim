package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrUploadIntentState = errors.New("upload intent is not pending")

func scanUploadIntent(scanner rowScanner) (UploadIntent, error) {
	var intent UploadIntent
	var apiKeyID sql.NullString
	var fileID sql.NullString
	var created, expires int64
	var completed sql.NullInt64
	err := scanner.Scan(
		&intent.ID, &intent.ObjectKey, &apiKeyID, &intent.OriginalName, &intent.ContentType,
		&intent.DeclaredSize, &intent.UploadTokenHash, &intent.DeleteTokenHash,
		&intent.MultipartUploadID, &intent.PartSize, &intent.PartCount, &intent.State,
		&created, &expires, &completed, &fileID,
	)
	if err != nil {
		return UploadIntent{}, err
	}
	if apiKeyID.Valid {
		intent.APIKeyID = &apiKeyID.String
	}
	if fileID.Valid {
		intent.FileID = &fileID.String
	}
	intent.CreatedAt = scanTime(created)
	intent.ExpiresAt = scanTime(expires)
	if completed.Valid {
		value := scanTime(completed.Int64)
		intent.CompletedAt = &value
	}
	return intent, nil
}

func (s *Store) CreateUploadIntent(ctx context.Context, intent UploadIntent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upload_intents
		 (id, object_key, api_key_id, original_name, content_type, declared_size,
		  upload_token_hash, delete_token_hash, multipart_upload_id, part_size, part_count,
		  state, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		intent.ID, intent.ObjectKey, intent.APIKeyID, intent.OriginalName, intent.ContentType,
		intent.DeclaredSize, intent.UploadTokenHash, intent.DeleteTokenHash,
		intent.MultipartUploadID, intent.PartSize, intent.PartCount,
		toUnix(intent.CreatedAt), toUnix(intent.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("create upload intent: %w", err)
	}
	return nil
}

func (s *Store) UploadIntentByID(ctx context.Context, id string) (UploadIntent, error) {
	intent, err := scanUploadIntent(s.db.QueryRowContext(ctx,
		`SELECT id, object_key, api_key_id, original_name, content_type, declared_size,
		        upload_token_hash, delete_token_hash, multipart_upload_id, part_size, part_count,
		        state, created_at, expires_at, completed_at, file_id
		 FROM upload_intents WHERE id = ?`,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return UploadIntent{}, ErrNotFound
	}
	if err != nil {
		return UploadIntent{}, fmt.Errorf("get upload intent: %w", err)
	}
	return intent, nil
}

func (s *Store) ExpiredPendingUploadIntents(ctx context.Context, now time.Time, limit int) ([]UploadIntent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, object_key, api_key_id, original_name, content_type, declared_size,
		        upload_token_hash, delete_token_hash, multipart_upload_id, part_size, part_count,
		        state, created_at, expires_at, completed_at, file_id
		 FROM upload_intents
		 WHERE state = 'pending' AND expires_at <= ?
		 ORDER BY expires_at ASC
		 LIMIT ?`,
		toUnix(now), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired upload intents: %w", err)
	}
	defer rows.Close()

	var intents []UploadIntent
	for rows.Next() {
		intent, err := scanUploadIntent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan upload intent: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upload intents: %w", err)
	}
	return intents, nil
}

func (s *Store) SetUploadMultipartID(ctx context.Context, id, multipartID string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE upload_intents SET multipart_upload_id = ? WHERE id = ? AND state = 'pending'",
		multipartID, id,
	)
	if err != nil {
		return fmt.Errorf("set multipart upload id: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set multipart upload id rows: %w", err)
	}
	if affected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SaveUploadPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upload_parts (upload_id, part_number, etag, size, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(upload_id, part_number) DO UPDATE SET
		     etag = excluded.etag,
		     size = excluded.size,
		     created_at = excluded.created_at`,
		uploadID, partNumber, etag, size, toUnix(now),
	)
	if err != nil {
		return fmt.Errorf("save upload part: %w", err)
	}
	return nil
}

func (s *Store) UploadParts(ctx context.Context, uploadID string) ([]UploadPart, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, part_number, etag, size, created_at
		 FROM upload_parts WHERE upload_id = ? ORDER BY part_number ASC`,
		uploadID,
	)
	if err != nil {
		return nil, fmt.Errorf("list upload parts: %w", err)
	}
	defer rows.Close()

	var parts []UploadPart
	for rows.Next() {
		var part UploadPart
		var created int64
		if err := rows.Scan(&part.UploadID, &part.PartNumber, &part.ETag, &part.Size, &created); err != nil {
			return nil, fmt.Errorf("scan upload part: %w", err)
		}
		part.CreatedAt = scanTime(created)
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upload parts: %w", err)
	}
	return parts, nil
}

// CommitUploadIntent atomically converts a pending intent into a committed
// file, consumes its reservation, and marks the intent complete.
func (s *Store) CommitUploadIntent(ctx context.Context, intentID string, file File, now time.Time) error {
	if file.ID != intentID {
		return errors.New("file ID must match upload intent ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upload commit: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx,
		`UPDATE upload_intents
		 SET state = 'committed', completed_at = ?, file_id = ?
		 WHERE id = ? AND state = 'pending'`,
		toUnix(now), file.ID, intentID,
	)
	if err != nil {
		return fmt.Errorf("mark upload committed: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark upload committed rows: %w", err)
	}
	if affected != 1 {
		return ErrUploadIntentState
	}

	result, err = tx.ExecContext(ctx, "DELETE FROM storage_reservations WHERE id = ?", intentID)
	if err != nil {
		return fmt.Errorf("consume upload reservation: %w", err)
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("consume upload reservation rows: %w", err)
	}
	if affected != 1 {
		return ErrReservationMissing
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO files
		 (id, object_key, api_key_id, original_name, content_type, size, delete_token_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		file.ID, file.ObjectKey, file.APIKeyID, file.OriginalName, file.ContentType,
		file.Size, file.DeleteTokenHash, toUnix(file.CreatedAt), toUnix(file.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("insert uploaded file: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM upload_parts WHERE upload_id = ?", intentID); err != nil {
		return fmt.Errorf("delete upload parts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upload: %w", err)
	}
	return nil
}

// CloseUploadIntent releases the reservation and moves a pending intent to a
// terminal state. It is idempotent for already-terminal intents.
func (s *Store) CloseUploadIntent(ctx context.Context, id, state string, now time.Time) error {
	switch state {
	case "aborted", "expired", "failed":
	default:
		return fmt.Errorf("invalid terminal intent state %q", state)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin close upload intent: %w", err)
	}
	defer tx.Rollback()

	var current string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM upload_intents WHERE id = ?", id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read upload intent state: %w", err)
	}
	if current == "pending" {
		if _, err := tx.ExecContext(ctx,
			"UPDATE upload_intents SET state = ?, completed_at = ? WHERE id = ?",
			state, toUnix(now), id,
		); err != nil {
			return fmt.Errorf("close upload intent: %w", err)
		}
	}
	// Also clean a lingering reservation for an already-terminal intent. This is
	// a no-op for committed intents because their reservation was consumed.
	if _, err := tx.ExecContext(ctx, "DELETE FROM storage_reservations WHERE id = ?", id); err != nil {
		return fmt.Errorf("release upload reservation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM upload_parts WHERE upload_id = ?", id); err != nil {
		return fmt.Errorf("delete upload parts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit close upload intent: %w", err)
	}
	return nil
}

func (s *Store) DeleteTerminalUploadIntents(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM upload_intents
		 WHERE id IN (
		     SELECT id FROM upload_intents
		     WHERE state != 'pending' AND expires_at <= ?
		     ORDER BY expires_at ASC
		     LIMIT ?
		 )`,
		toUnix(before), limit,
	)
	if err != nil {
		return 0, fmt.Errorf("delete terminal upload intents: %w", err)
	}
	return result.RowsAffected()
}

// KnownObjectKeys returns every object that must be protected from the orphan
// sweeper: committed file keys and keys of pending, unexpired intents.
func (s *Store) KnownObjectKeys(ctx context.Context, now time.Time) (map[string]struct{}, error) {
	known := make(map[string]struct{})
	rows, err := s.db.QueryContext(ctx, "SELECT object_key FROM files")
	if err != nil {
		return nil, fmt.Errorf("list known file objects: %w", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan known file object: %w", err)
		}
		known[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate known file objects: %w", err)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx,
		"SELECT object_key FROM upload_intents WHERE state = 'pending' AND expires_at > ?",
		toUnix(now),
	)
	if err != nil {
		return nil, fmt.Errorf("list pending upload objects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan pending upload object: %w", err)
		}
		known[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending upload objects: %w", err)
	}
	return known, nil
}

func (s *Store) KnownMultipartUploadIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT multipart_upload_id FROM upload_intents WHERE multipart_upload_id != ''")
	if err != nil {
		return nil, fmt.Errorf("list multipart upload ids: %w", err)
	}
	defer rows.Close()
	known := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan multipart upload id: %w", err)
		}
		known[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multipart upload ids: %w", err)
	}
	return known, nil
}

func (s *Store) CleanOrphanReservations(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM storage_reservations
		 WHERE id NOT IN (SELECT id FROM upload_intents WHERE state = 'pending')`,
	)
	if err != nil {
		return 0, fmt.Errorf("clean orphan reservations: %w", err)
	}
	return result.RowsAffected()
}
