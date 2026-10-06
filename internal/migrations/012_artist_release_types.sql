-- +goose Up
ALTER TABLE artists ADD COLUMN watch_release_types TEXT;

-- +goose Down
ALTER TABLE artists DROP COLUMN watch_release_types;
