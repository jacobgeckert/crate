-- +goose Up
CREATE TABLE IF NOT EXISTS discover_dismissals (
    key TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- +goose Down
DROP TABLE IF EXISTS discover_dismissals;
