-- +goose Up
CREATE TABLE IF NOT EXISTS upload_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id TEXT NOT NULL,
    filename TEXT NOT NULL,
    staged_path TEXT NOT NULL,
    size INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'uploaded',
    meta_json TEXT,
    match_json TEXT,
    skip INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_upload_files_batch ON upload_files(batch_id);

-- +goose Down
DROP TABLE IF EXISTS upload_files;
