PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS admin (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash  BLOB PRIMARY KEY,
    admin_id    INTEGER NOT NULL REFERENCES admin(id) ON DELETE CASCADE,
    csrf_token  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS api_keys (
    id           TEXT PRIMARY KEY,
    key_hash     BLOB NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    disabled     INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1))
);

CREATE TABLE IF NOT EXISTS files (
    id                TEXT PRIMARY KEY,
    object_key        TEXT NOT NULL UNIQUE,
    api_key_id        TEXT REFERENCES api_keys(id) ON DELETE SET NULL,
    original_name     TEXT NOT NULL,
    content_type      TEXT NOT NULL,
    size              INTEGER NOT NULL CHECK (size > 0),
    delete_token_hash BLOB NOT NULL,
    created_at        INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS files_expires_at ON files(expires_at);
CREATE INDEX IF NOT EXISTS files_api_key_id ON files(api_key_id);
CREATE INDEX IF NOT EXISTS files_created_at ON files(created_at DESC);

CREATE TABLE IF NOT EXISTS storage_reservations (
    id         TEXT PRIMARY KEY,
    bytes      INTEGER NOT NULL CHECK (bytes > 0),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS storage_reservations_expires_at ON storage_reservations(expires_at);
