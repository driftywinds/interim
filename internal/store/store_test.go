package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMigratesLegacyAPIKeysAsUnapproved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE api_keys (
			id           TEXT PRIMARY KEY,
			key_hash     BLOB NOT NULL UNIQUE,
			key_prefix   TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			last_used_at INTEGER,
			disabled     INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1))
		);
		INSERT INTO api_keys (id, key_hash, key_prefix, name, created_at)
		VALUES ('legacy', X'00', 'interim_legacy', 'old', 1);

		CREATE TABLE storage_reservations (
			id         TEXT PRIMARY KEY,
			bytes      INTEGER NOT NULL CHECK (bytes > 0),
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		);
		INSERT INTO storage_reservations (id, bytes, created_at, expires_at)
		VALUES ('legacy-reservation', 1, 1, 2);

		CREATE TABLE upload_intents (
			id                 TEXT PRIMARY KEY,
			object_key         TEXT NOT NULL UNIQUE,
			api_key_id         TEXT,
			original_name      TEXT NOT NULL,
			content_type       TEXT NOT NULL,
			declared_size      INTEGER NOT NULL,
			upload_token_hash  BLOB NOT NULL,
			delete_token_hash  BLOB NOT NULL,
			state              TEXT NOT NULL DEFAULT 'pending',
			created_at         INTEGER NOT NULL,
			expires_at         INTEGER NOT NULL,
			completed_at       INTEGER,
			file_id            TEXT
		);
		INSERT INTO upload_intents
			(id, object_key, original_name, content_type, declared_size,
			 upload_token_hash, delete_token_hash, state, created_at, expires_at)
		VALUES ('legacy-upload', 'files/uploads/legacy', 'old.bin', 'application/octet-stream', 1,
		        X'00', X'01', 'pending', 1, 9999999999);
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	key, err := st.APIKeyByID(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if key.Approved {
		t.Fatal("legacy user hash should be unapproved after migration")
	}
	if err := st.SetAPIKeyApproved(context.Background(), key.ID, true); err != nil {
		t.Fatal(err)
	}
	key, err = st.APIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !key.Approved {
		t.Fatal("approved flag was not persisted")
	}
	if key.CreatedAt.Unix() != 1 {
		t.Fatalf("unexpected created_at: %v", key.CreatedAt)
	}

	var legacyOwner string
	if err := st.db.QueryRow("SELECT owner FROM storage_reservations WHERE id = 'legacy-reservation'").Scan(&legacyOwner); err != nil {
		t.Fatal(err)
	}
	if legacyOwner != "" {
		t.Fatalf("legacy reservation owner = %q", legacyOwner)
	}
	now := time.Now()
	reserved, err := st.ReserveStorage(context.Background(), "owned-reservation", 1, "key:test", ReservationLimits{}, now, now.Add(time.Hour))
	if err != nil || !reserved {
		t.Fatalf("reserve owned storage = %v, %v", reserved, err)
	}
	var owner string
	if err := st.db.QueryRow("SELECT owner FROM storage_reservations WHERE id = 'owned-reservation'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "key:test" {
		t.Fatalf("reservation owner = %q", owner)
	}

	intent, err := st.UploadIntentByID(context.Background(), "legacy-upload")
	if err != nil {
		t.Fatal(err)
	}
	if intent.MultipartUploadID != "" || intent.PartSize != 0 || intent.PartCount != 0 {
		t.Fatalf("legacy upload multipart fields = %+v", intent)
	}
	if err := st.SetUploadMultipartID(context.Background(), intent.ID, "mp-legacy"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveUploadPart(context.Background(), intent.ID, 1, `"etag"`, 1, now); err != nil {
		t.Fatal(err)
	}
	parts, err := st.UploadParts(context.Background(), intent.ID)
	if err != nil || len(parts) != 1 || parts[0].ETag != `"etag"` {
		t.Fatalf("legacy upload parts = %+v, %v", parts, err)
	}
}

func TestFileDeletionClaimBlocksExtension(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(time.Hour)
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO files
		 (id, object_key, original_name, content_type, size, delete_token_hash, created_at, expires_at)
		 VALUES ('claimed', 'files/claimed', 'claimed.bin', 'application/octet-stream', 1, X'00', ?, ?)`,
		toUnix(now), toUnix(expiry),
	); err != nil {
		t.Fatal(err)
	}

	// Claim the row as the janitor would after the deadline.
	claimed, err := st.ClaimExpiredFile(context.Background(), "claimed", expiry.Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("ClaimExpiredFile = %v, %v", claimed, err)
	}
	// Moving the expiry back before the claim deadline must not bypass the
	// deleting_at guard.
	extended, err := st.ExtendFileExpiry(context.Background(), "claimed", now, now.Add(2*time.Hour))
	if err != nil || extended {
		t.Fatalf("ExtendFileExpiry on claimed file = %v, %v", extended, err)
	}
	file, err := st.FileByID(context.Background(), "claimed")
	if err != nil {
		t.Fatal(err)
	}
	if file.DeletingAt == nil {
		t.Fatal("claimed file has no deleting_at marker")
	}

	if err := st.ReleaseFileDeletion(context.Background(), "claimed"); err != nil {
		t.Fatal(err)
	}
	extended, err = st.ExtendFileExpiry(context.Background(), "claimed", now, now.Add(2*time.Hour))
	if err != nil || !extended {
		t.Fatalf("ExtendFileExpiry after release = %v, %v", extended, err)
	}
	if removed, err := st.FinishFileDeletion(context.Background(), "claimed"); err != nil || removed {
		t.Fatalf("FinishFileDeletion without a claim = %v, %v", removed, err)
	}
}
