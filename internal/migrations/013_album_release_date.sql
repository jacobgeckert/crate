-- +goose Up
ALTER TABLE albums ADD COLUMN release_date TEXT;
CREATE INDEX IF NOT EXISTS idx_albums_release_date ON albums(release_date);

-- +goose Down
ALTER TABLE albums DROP COLUMN release_date;
