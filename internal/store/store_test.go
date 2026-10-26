package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
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
}
