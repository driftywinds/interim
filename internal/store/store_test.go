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
}
