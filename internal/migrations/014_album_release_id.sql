-- +goose Up
-- User-pinned edition: when set, tracklists for this album come from this
-- provider release id instead of the provider's default pick inside the
-- release-group (MusicBrainz releases within a release-group).
ALTER TABLE albums ADD COLUMN release_id TEXT;

-- +goose Down
ALTER TABLE albums DROP COLUMN release_id;
