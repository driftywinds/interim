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
    expires_at  INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS api_keys (
    id           TEXT PRIMARY KEY,
    key_hash     BLOB NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    disabled     INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    approved     INTEGER NOT NULL DEFAULT 0 CHECK (approved IN (0, 1)),
    max_file_size     INTEGER NOT NULL DEFAULT -1,
    retention_seconds INTEGER NOT NULL DEFAULT -1
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
    expires_at        INTEGER NOT NULL,
    retention_seconds INTEGER,
    deleting_at       INTEGER
);

CREATE INDEX IF NOT EXISTS files_expires_at ON files(expires_at);
CREATE INDEX IF NOT EXISTS files_api_key_id ON files(api_key_id);
CREATE INDEX IF NOT EXISTS files_created_at ON files(created_at DESC);

CREATE TABLE IF NOT EXISTS storage_reservations (
    id         TEXT PRIMARY KEY,
    bytes      INTEGER NOT NULL CHECK (bytes > 0),
    owner      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS storage_reservations_expires_at ON storage_reservations(expires_at);

CREATE TABLE IF NOT EXISTS upload_intents (
    id                 TEXT PRIMARY KEY,
    object_key         TEXT NOT NULL UNIQUE,
    api_key_id         TEXT REFERENCES api_keys(id) ON DELETE SET NULL,
    original_name      TEXT NOT NULL,
    content_type       TEXT NOT NULL,
    declared_size      INTEGER NOT NULL CHECK (declared_size > 0),
    upload_token_hash  BLOB NOT NULL,
    delete_token_hash  BLOB NOT NULL,
    multipart_upload_id TEXT NOT NULL DEFAULT '',
    part_size          INTEGER NOT NULL DEFAULT 0,
    part_count         INTEGER NOT NULL DEFAULT 0,
    state              TEXT NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'committed', 'aborted', 'expired', 'failed')),
    created_at         INTEGER NOT NULL,
    expires_at         INTEGER NOT NULL,
    completed_at       INTEGER,
    file_id            TEXT,
    claim              TEXT NOT NULL DEFAULT '',
    claim_at           INTEGER NOT NULL DEFAULT 0,
    staging_hold_until INTEGER NOT NULL DEFAULT 0,
    url_expires_at     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS upload_intents_state_expires ON upload_intents(state, expires_at);
CREATE INDEX IF NOT EXISTS upload_intents_expires_at ON upload_intents(expires_at);

CREATE TABLE IF NOT EXISTS upload_parts (
    upload_id   TEXT NOT NULL REFERENCES upload_intents(id) ON DELETE CASCADE,
    part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    etag        TEXT NOT NULL,
    size        INTEGER NOT NULL CHECK (size > 0),
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (upload_id, part_number)
);

CREATE INDEX IF NOT EXISTS upload_parts_upload_id ON upload_parts(upload_id);

-- Running totals used by quota checks. Triggers keep them exact so
-- ReserveStorage does not scan 100k file rows on every public request.
CREATE TABLE IF NOT EXISTS usage_totals (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    files          INTEGER NOT NULL DEFAULT 0,
    file_bytes     INTEGER NOT NULL DEFAULT 0,
    reserved_bytes INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO usage_totals (id, files, file_bytes, reserved_bytes)
VALUES (1, 0, 0, 0);

CREATE TRIGGER IF NOT EXISTS files_usage_insert AFTER INSERT ON files
BEGIN
    UPDATE usage_totals
    SET files = files + 1, file_bytes = file_bytes + NEW.size
    WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS files_usage_delete AFTER DELETE ON files
BEGIN
    UPDATE usage_totals
    SET files = files - 1, file_bytes = file_bytes - OLD.size
    WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS reservations_usage_insert AFTER INSERT ON storage_reservations
BEGIN
    UPDATE usage_totals
    SET reserved_bytes = reserved_bytes + NEW.bytes
    WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS reservations_usage_delete AFTER DELETE ON storage_reservations
BEGIN
    UPDATE usage_totals
    SET reserved_bytes = reserved_bytes - OLD.bytes
    WHERE id = 1;
END;
