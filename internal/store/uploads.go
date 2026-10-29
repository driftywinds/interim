package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrUploadIntentState = errors.New("upload intent is not pending")

const uploadIntentColumns = `id, object_key, api_key_id, original_name, content_type, declared_size,
	upload_token_hash, delete_token_hash, multipart_upload_id, part_size, part_count,
	state, created_at, expires_at, completed_at, file_id, claim, claim_at, staging_hold_until, url_expires_at`

func scanUploadIntent(scanner rowScanner) (UploadIntent, error) {
	var intent UploadIntent
	var apiKeyID sql.NullString
	var fileID sql.NullString
	var created, expires, claimAt, holdUntil, urlExpires int64
	var completed sql.NullInt64
	err := scanner.Scan(
		&intent.ID, &intent.ObjectKey, &apiKeyID, &intent.OriginalName, &intent.ContentType,
		&intent.DeclaredSize, &intent.UploadTokenHash, &intent.DeleteTokenHash,
		&intent.MultipartUploadID, &intent.PartSize, &intent.PartCount, &intent.State,
		&created, &expires, &completed, &fileID, &intent.Claim, &claimAt, &holdUntil, &urlExpires,
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
	if claimAt != 0 {
		intent.ClaimAt = scanTime(claimAt)
	}
	if holdUntil != 0 {
		intent.StagingHoldUntil = scanTime(holdUntil)
	}
	if urlExpires != 0 {
		intent.URLEExpiresAt = scanTime(urlExpires)
	}
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
		  state, created_at, expires_at, url_expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		intent.ID, intent.ObjectKey, intent.APIKeyID, intent.OriginalName, intent.ContentType,
		intent.DeclaredSize, intent.UploadTokenHash, intent.DeleteTokenHash,
		intent.MultipartUploadID, intent.PartSize, intent.PartCount,
		toUnix(intent.CreatedAt), toUnix(intent.ExpiresAt), toUnix(intent.URLEExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("create upload intent: %w", err)
	}
	return nil
}

func (s *Store) UploadIntentByID(ctx context.Context, id string) (UploadIntent, error) {
	intent, err := scanUploadIntent(s.db.QueryRowContext(ctx,
		"SELECT "+uploadIntentColumns+" FROM upload_intents WHERE id = ?", id,
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
		"SELECT "+uploadIntentColumns+` FROM upload_intents
		 WHERE state = 'pending' AND claim = '' AND expires_at <= ?
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
		"UPDATE upload_intents SET multipart_upload_id = ? WHERE id = ? AND state = 'pending' AND claim = ''",
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

// ClaimUploadIntent reserves the pending intent for one completing, aborting,
// or expiring operation. The conditional UPDATE is the race guard: a second
// operation cannot delete an object after a completion has claimed the intent.
func (s *Store) ClaimUploadIntent(ctx context.Context, id, claim string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE upload_intents SET claim = ?, claim_at = ?
		 WHERE id = ? AND state = 'pending' AND claim = ''`,
		claim, toUnix(now), id,
	)
	if err != nil {
		return false, fmt.Errorf("claim upload intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim upload intent rows: %w", err)
	}
	return affected == 1, nil
}

func (s *Store) ReleaseUploadClaim(ctx context.Context, id, claim string) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE upload_intents SET claim = '', claim_at = 0 WHERE id = ? AND state = 'pending' AND claim = ?",
		id, claim,
	); err != nil {
		return fmt.Errorf("release upload claim: %w", err)
	}
	return nil
}

func (s *Store) StaleUploadClaims(ctx context.Context, before time.Time, limit int) ([]UploadIntent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+uploadIntentColumns+` FROM upload_intents
		 WHERE state = 'pending' AND claim != '' AND claim_at <= ?
		 ORDER BY claim_at ASC LIMIT ?`,
		toUnix(before), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale upload claims: %w", err)
	}
	defer rows.Close()
	var intents []UploadIntent
	for rows.Next() {
		intent, err := scanUploadIntent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan stale upload claim: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale upload claims: %w", err)
	}
	return intents, nil
}

// CommitUploadIntent atomically converts a claimed intent into a committed
// file. A reservation is consumed for multipart uploads; for single PUTs it is
// retained until the already-issued PUT URL has expired.
func (s *Store) CommitUploadIntent(ctx context.Context, intentID string, file File, now, holdUntil time.Time) error {
	if file.ID != intentID {
		return errors.New("file ID must match upload intent ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upload commit: %w", err)
	}
	defer tx.Rollback()

	holdUnix := int64(0)
	if !holdUntil.IsZero() {
		holdUnix = toUnix(holdUntil)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE upload_intents
		 SET state = 'committed', completed_at = ?, file_id = ?,
		     claim = '', claim_at = 0, staging_hold_until = ?
		 WHERE id = ? AND state = 'pending' AND claim = 'completing'`,
		toUnix(now), file.ID, holdUnix, intentID,
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

	if holdUntil.IsZero() {
		result, err = tx.ExecContext(ctx, "DELETE FROM storage_reservations WHERE id = ?", intentID)
	} else {
		result, err = tx.ExecContext(ctx,
			"UPDATE storage_reservations SET expires_at = ? WHERE id = ?", holdUnix, intentID,
		)
	}
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
		 (id, object_key, api_key_id, original_name, content_type, size, delete_token_hash, created_at, expires_at, retention_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		file.ID, file.ObjectKey, file.APIKeyID, file.OriginalName, file.ContentType,
		file.Size, file.DeleteTokenHash, toUnix(file.CreatedAt), toUnix(file.ExpiresAt), file.RetentionSeconds,
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

// CloseUploadIntent releases a claimed pending intent into a terminal state. It
// returns false without touching the reservation if another operation already
// moved the intent. A non-zero holdUntil keeps the reservation while a
// single-PUT URL may still recreate the staging object.
func (s *Store) CloseUploadIntent(ctx context.Context, id, claim, state string, now, holdUntil time.Time) (bool, error) {
	switch state {
	case "aborted", "expired", "failed":
	default:
		return false, fmt.Errorf("invalid terminal intent state %q", state)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin close upload intent: %w", err)
	}
	defer tx.Rollback()

	holdUnix := int64(0)
	if !holdUntil.IsZero() {
		holdUnix = toUnix(holdUntil)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE upload_intents
		 SET state = ?, completed_at = ?, claim = '', claim_at = 0, staging_hold_until = ?
		 WHERE id = ? AND state = 'pending' AND claim = ?`,
		state, toUnix(now), holdUnix, id, claim,
	)
	if err != nil {
		return false, fmt.Errorf("close upload intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("close upload intent rows: %w", err)
	}
	if affected != 1 {
		return false, nil
	}

	if holdUntil.IsZero() {
		if _, err := tx.ExecContext(ctx, "DELETE FROM storage_reservations WHERE id = ?", id); err != nil {
			return false, fmt.Errorf("release upload reservation: %w", err)
		}
	} else {
		result, err = tx.ExecContext(ctx,
			"UPDATE storage_reservations SET expires_at = ? WHERE id = ?", holdUnix, id,
		)
		if err != nil {
			return false, fmt.Errorf("hold upload reservation: %w", err)
		}
		held, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("hold upload reservation rows: %w", err)
		}
		if held != 1 {
			return false, ErrReservationMissing
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM upload_parts WHERE upload_id = ?", id); err != nil {
		return false, fmt.Errorf("delete upload parts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit close upload intent: %w", err)
	}
	return true, nil
}

func (s *Store) ExpiredStagingHolds(ctx context.Context, now time.Time, limit int) ([]UploadIntent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+uploadIntentColumns+` FROM upload_intents
		 WHERE staging_hold_until > 0 AND staging_hold_until <= ?
		 ORDER BY staging_hold_until ASC LIMIT ?`,
		toUnix(now), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired staging holds: %w", err)
	}
	defer rows.Close()
	var intents []UploadIntent
	for rows.Next() {
		intent, err := scanUploadIntent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired staging hold: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired staging holds: %w", err)
	}
	return intents, nil
}

func (s *Store) FinishStagingHold(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finish staging hold: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM storage_reservations WHERE id = ?", id); err != nil {
		return fmt.Errorf("release staging hold reservation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE upload_intents SET staging_hold_until = 0 WHERE id = ?", id,
	); err != nil {
		return fmt.Errorf("clear staging hold: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit staging hold: %w", err)
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
		     WHERE state != 'pending' AND claim = '' AND staging_hold_until = 0 AND expires_at <= ?
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
		`SELECT object_key FROM upload_intents
		 WHERE (state = 'pending' AND expires_at > ?) OR staging_hold_until > 0`,
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
		 WHERE id NOT IN (
		     SELECT id FROM upload_intents
		     WHERE state = 'pending' OR staging_hold_until > 0
		 )`,
	)
	if err != nil {
		return 0, fmt.Errorf("clean orphan reservations: %w", err)
	}
	return result.RowsAffected()
}
