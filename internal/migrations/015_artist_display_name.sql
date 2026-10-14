-- +goose Up
ALTER TABLE artists ADD COLUMN display_name TEXT;

-- +goose Down
ALTER TABLE artists DROP COLUMN display_name;
