package db

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

type Queries struct {
	db *sql.DB
}

func NewQueries(db *sql.DB) *Queries {
	return &Queries{db: db}
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// Artists

func (q *Queries) CreateArtist(a *models.Artist) error {
	ts := now()
	result, err := q.db.Exec(
		`INSERT INTO artists (name, provider, provider_id, image_url, status, watch_new_releases, watch_new_releases_since, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.Provider, a.ProviderID, a.ImageURL, a.Status, a.WatchNewReleases, a.WatchNewReleasesSince, ts, ts,
	)
	if err != nil {
		return err
	}
	a.ID, _ = result.LastInsertId()
	a.CreatedAt = ts
	a.UpdatedAt = ts
	return nil
}

func (q *Queries) GetArtist(id int64) (*models.Artist, error) {
	a := &models.Artist{}
	var typesJSON sql.NullString
	err := q.db.QueryRow(
		`SELECT id, name, provider, provider_id, image_url, status, watch_new_releases, watch_new_releases_since, watch_release_types, created_at, updated_at
		 FROM artists WHERE id = ?`, id,
	).Scan(&a.ID, &a.Name, &a.Provider, &a.ProviderID, &a.ImageURL, &a.Status, &a.WatchNewReleases, &a.WatchNewReleasesSince, &typesJSON, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if typesJSON.Valid {
		var m map[string]bool
		if json.Unmarshal([]byte(typesJSON.String), &m) == nil {
			a.WatchReleaseTypes = m
		}
	}
	return a, nil
}

// GetArtistWatchReleaseTypes returns the artist's per-type watch override, or
// nil when the artist inherits the global new_release_types setting.
func (q *Queries) GetArtistWatchReleaseTypes(id int64) (map[string]bool, error) {
	var v sql.NullString
	err := q.db.QueryRow(`SELECT watch_release_types FROM artists WHERE id = ?`, id).Scan(&v)
	if err != nil || !v.Valid {
		return nil, err
	}
	var m map[string]bool
	if err := json.Unmarshal([]byte(v.String), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// SetArtistWatchReleaseTypes stores a per-artist watch-type override; nil
// clears it so the artist inherits the global new_release_types setting again.
func (q *Queries) SetArtistWatchReleaseTypes(id int64, types map[string]bool) error {
	var v any
	if types != nil {
		b, err := json.Marshal(types)
		if err != nil {
			return err
		}
		v = string(b)
	}
	_, err := q.db.Exec(`UPDATE artists SET watch_release_types = ?, updated_at = ? WHERE id = ?`, v, now(), id)
	return err
}

func (q *Queries) ListArtists() ([]models.Artist, error) {
	rows, err := q.db.Query(
		`SELECT a.id, a.name, a.provider, a.provider_id, a.image_url, a.status, a.watch_new_releases, a.watch_new_releases_since, a.created_at, a.updated_at,
		        COUNT(t.id) as total_tracks,
		        COALESCE(SUM(CASE WHEN t.status = 'owned' THEN 1 ELSE 0 END), 0) as owned_tracks
		 FROM artists a
		 LEFT JOIN albums al ON al.artist_id = a.id AND al.status != 'ignored'
		 LEFT JOIN tracks t ON t.album_id = al.id AND t.status != 'ignored'
		 GROUP BY a.id
		 ORDER BY a.name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artists []models.Artist
	for rows.Next() {
		var a models.Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.Provider, &a.ProviderID, &a.ImageURL, &a.Status,
			&a.WatchNewReleases, &a.WatchNewReleasesSince, &a.CreatedAt, &a.UpdatedAt, &a.TotalTracks, &a.OwnedTracks); err != nil {
			return nil, err
		}
		artists = append(artists, a)
	}
	return artists, rows.Err()
}

func (q *Queries) UpdateArtistStatus(id int64, status models.ArtistStatus) error {
	_, err := q.db.Exec(`UPDATE artists SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

func (q *Queries) SetAlbumCoverURL(id int64, url string) error {
	_, err := q.db.Exec(`UPDATE albums SET cover_url = ?, updated_at = ? WHERE id = ?`, url, now(), id)
	return err
}

func (q *Queries) SetArtistImageURL(id int64, url string) error {
	_, err := q.db.Exec(`UPDATE artists SET image_url = ?, updated_at = ? WHERE id = ?`, url, now(), id)
	return err
}

func (q *Queries) SetArtistWatchNewReleases(id int64, enabled bool) error {
	ts := now()
	var since *string
	if enabled {
		since = &ts
	}
	_, err := q.db.Exec(
		`UPDATE artists SET watch_new_releases = ?, watch_new_releases_since = COALESCE(?, watch_new_releases_since), updated_at = ? WHERE id = ?`,
		enabled, since, ts, id,
	)
	return err
}

func (q *Queries) DeleteArtist(id int64) error {
	_, err := q.db.Exec(`DELETE FROM artists WHERE id = ?`, id)
	return err
}

func (q *Queries) FindArtistByProvider(provider, providerID string) (*models.Artist, error) {
	a := &models.Artist{}
	err := q.db.QueryRow(
		`SELECT id, name, provider, provider_id, image_url, status, watch_new_releases, watch_new_releases_since, created_at, updated_at
		 FROM artists WHERE provider = ? AND provider_id = ?`, provider, providerID,
	).Scan(&a.ID, &a.Name, &a.Provider, &a.ProviderID, &a.ImageURL, &a.Status, &a.WatchNewReleases, &a.WatchNewReleasesSince, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// FindArtistByNameFold matches an artist by name case-insensitively. The
// importer uses it so an imported "radiohead" attaches to an already-watched
// "Radiohead" instead of creating a duplicate.
func (q *Queries) FindArtistByNameFold(name string) (*models.Artist, error) {
	a := &models.Artist{}
	err := q.db.QueryRow(
		`SELECT id, name, provider, provider_id, image_url, status, watch_new_releases, watch_new_releases_since, created_at, updated_at
		 FROM artists WHERE LOWER(name) = LOWER(?) LIMIT 1`, name,
	).Scan(&a.ID, &a.Name, &a.Provider, &a.ProviderID, &a.ImageURL, &a.Status, &a.WatchNewReleases, &a.WatchNewReleasesSince, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (q *Queries) ListWatchedArtists() ([]models.Artist, error) {
	rows, err := q.db.Query(
		`SELECT id, name, provider, provider_id, image_url, status, watch_new_releases, watch_new_releases_since, created_at, updated_at
		 FROM artists WHERE watch_new_releases = 1 ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artists []models.Artist
	for rows.Next() {
		var a models.Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.Provider, &a.ProviderID, &a.ImageURL, &a.Status,
			&a.WatchNewReleases, &a.WatchNewReleasesSince, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		artists = append(artists, a)
	}
	return artists, rows.Err()
}

func (q *Queries) RelinkArtist(id int64, provider, providerID string) error {
	_, err := q.db.Exec(`UPDATE artists SET provider = ?, provider_id = ?, updated_at = ? WHERE id = ?`,
		provider, providerID, now(), id)
	return err
}

func (q *Queries) RelinkAlbum(id int64, provider, providerID string) error {
	_, err := q.db.Exec(`UPDATE albums SET provider = ?, provider_id = ?, updated_at = ? WHERE id = ?`,
		provider, providerID, now(), id)
	return err
}

// AbsorbTrack moves a duplicate row's ownership (file, status, download
// provenance, recording id) onto the canonical provider row dstID, then
// deletes the duplicate srcID. Used when a file was imported under a different
// MusicBrainz release's track id than the provider's canonical tracklist.
func (q *Queries) AbsorbTrack(dstID, srcID int64) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	var filePath, dlFrom, dlFile, dlFmt, mbRec sql.NullString
	var dlBitrate sql.NullInt64
	var dur int64
	err = tx.QueryRow(
		`SELECT status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate, duration_ms, mb_recording_id
		 FROM tracks WHERE id = ?`, srcID,
	).Scan(&status, &filePath, &dlFrom, &dlFile, &dlFmt, &dlBitrate, &dur, &mbRec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE tracks SET
			status = CASE WHEN ? = 'owned' THEN 'owned' ELSE status END,
			file_path = COALESCE(?, file_path),
			downloaded_from = COALESCE(?, downloaded_from),
			downloaded_filename = COALESCE(?, downloaded_filename),
			download_format = COALESCE(?, download_format),
			download_bitrate = COALESCE(?, download_bitrate),
			duration_ms = CASE WHEN duration_ms > 0 THEN duration_ms ELSE ? END,
			mb_recording_id = COALESCE(mb_recording_id, ?),
			updated_at = ?
		 WHERE id = ?`,
		status, filePath, dlFrom, dlFile, dlFmt, dlBitrate, dur, mbRec, now(), dstID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tracks WHERE id = ?`, srcID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTrackMBRecordingID fills in a missing MusicBrainz recording id — rows
// created before the provider exposed it get stamped on the next sync.
func (q *Queries) SetTrackMBRecordingID(id int64, recordingID string) error {
	_, err := q.db.Exec(
		`UPDATE tracks SET mb_recording_id = COALESCE(mb_recording_id, ?), updated_at = ? WHERE id = ?`,
		recordingID, now(), id)
	return err
}

func (q *Queries) RelinkTrack(id int64, provider, providerID string) error {
	_, err := q.db.Exec(`UPDATE tracks SET provider = ?, provider_id = ?, updated_at = ? WHERE id = ?`,
		provider, providerID, now(), id)
	return err
}

// Albums

func (q *Queries) CreateAlbum(a *models.Album) error {
	ts := now()
	result, err := q.db.Exec(
		`INSERT INTO albums (artist_id, title, year, provider, provider_id, cover_url, record_type, release_date, release_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ArtistID, a.Title, a.Year, a.Provider, a.ProviderID, a.CoverURL, a.RecordType, a.ReleaseDate, a.ReleaseID, a.Status, ts, ts,
	)
	if err != nil {
		return err
	}
	a.ID, _ = result.LastInsertId()
	return nil
}

// ListUpcomingReleases returns non-ignored albums whose release_date is today
// or later, soonest first, for the Upcoming page.
func (q *Queries) ListUpcomingReleases(today string) ([]models.Album, error) {
	rows, err := q.db.Query(
		`SELECT a.id, a.artist_id, a.title, a.year, a.provider, a.provider_id, a.cover_url, a.record_type, a.release_date, a.status, a.created_at, a.updated_at, ar.name
		 FROM albums a JOIN artists ar ON ar.id = a.artist_id
		 WHERE a.release_date IS NOT NULL AND a.release_date != '' AND a.release_date >= ? AND a.status != 'ignored'
		 ORDER BY a.release_date ASC
		 LIMIT 200`, today,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var albums []models.Album
	for rows.Next() {
		var a models.Album
		if err := rows.Scan(&a.ID, &a.ArtistID, &a.Title, &a.Year, &a.Provider, &a.ProviderID, &a.CoverURL, &a.RecordType, &a.ReleaseDate, &a.Status, &a.CreatedAt, &a.UpdatedAt, &a.ArtistName); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, rows.Err()
}

// BackfillAlbumReleaseDate stamps a release date on a row that lacks one —
// used when a provider sync sees a date for an album created before dates
// were tracked.
func (q *Queries) BackfillAlbumReleaseDate(id int64, date string) error {
	_, err := q.db.Exec(`UPDATE albums SET release_date = ? WHERE id = ? AND (release_date IS NULL OR release_date = '')`, date, id)
	return err
}

// SetAlbumReleaseID pins an album to a specific provider release (edition);
// nil clears the pin so tracklists come from the provider's default pick.
func (q *Queries) SetAlbumReleaseID(id int64, releaseID *string) error {
	_, err := q.db.Exec(`UPDATE albums SET release_id = ?, updated_at = ? WHERE id = ?`, releaseID, now(), id)
	return err
}

func (q *Queries) GetAlbum(id int64) (*models.Album, error) {
	a := &models.Album{}
	err := q.db.QueryRow(
		`SELECT al.id, al.artist_id, al.title, al.year, al.provider, al.provider_id, al.cover_url, al.record_type, al.release_id, al.status,
		        al.created_at, al.updated_at, ar.name
		 FROM albums al JOIN artists ar ON ar.id = al.artist_id
		 WHERE al.id = ?`, id,
	).Scan(&a.ID, &a.ArtistID, &a.Title, &a.Year, &a.Provider, &a.ProviderID, &a.CoverURL, &a.RecordType, &a.ReleaseID, &a.Status,
		&a.CreatedAt, &a.UpdatedAt, &a.ArtistName)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (q *Queries) ListAlbumsByArtist(artistID int64) ([]models.Album, error) {
	rows, err := q.db.Query(
		`SELECT id, artist_id, title, year, provider, provider_id, cover_url, record_type, release_id, status, created_at, updated_at
		 FROM albums WHERE artist_id = ? ORDER BY year, title`, artistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var albums []models.Album
	for rows.Next() {
		var a models.Album
		if err := rows.Scan(&a.ID, &a.ArtistID, &a.Title, &a.Year, &a.Provider, &a.ProviderID, &a.CoverURL, &a.RecordType,
			&a.ReleaseID, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, rows.Err()
}

// SetAlbumRecordType refreshes an album's record_type when the provider now
// reports a different classification — e.g. a release-group that gained a
// secondary type since the album was first saved.
func (q *Queries) SetAlbumRecordType(id int64, recordType string) error {
	_, err := q.db.Exec(`UPDATE albums SET record_type = ?, updated_at = ? WHERE id = ?`, recordType, now(), id)
	return err
}

func (q *Queries) UpdateAlbumStatus(id int64, status models.AlbumStatus) error {
	_, err := q.db.Exec(`UPDATE albums SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

// AlbumHasOwnedOrActive reports whether an album has any owned or in-flight
// tracks — used by sync to decide if a non-owned album may default to ignored
// without disturbing downloads in progress.
func (q *Queries) AlbumHasOwnedOrActive(albumID int64) (bool, error) {
	var has bool
	err := q.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM tracks WHERE album_id = ? AND status IN ('owned', 'downloading'))
		      OR EXISTS(SELECT 1 FROM download_queue d JOIN tracks t ON t.id = d.track_id
		                WHERE t.album_id = ? AND d.status IN ('pending', 'searching', 'downloading', 'organizing'))`,
		albumID, albumID).Scan(&has)
	return has, err
}

// ListWantedTrackIDsByAlbum returns ids of an album's wanted tracks — used to
// enqueue them when an album is un-ignored.
func (q *Queries) ListWantedTrackIDsByAlbum(albumID int64) ([]int64, error) {
	rows, err := q.db.Query(`SELECT id FROM tracks WHERE album_id = ? AND status = 'wanted'`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (q *Queries) FindAlbumByProvider(provider, providerID string) (*models.Album, error) {
	a := &models.Album{}
	err := q.db.QueryRow(
		`SELECT id, artist_id, title, year, provider, provider_id, cover_url, record_type, release_id, status, created_at, updated_at
		 FROM albums WHERE provider = ? AND provider_id = ?`, provider, providerID,
	).Scan(&a.ID, &a.ArtistID, &a.Title, &a.Year, &a.Provider, &a.ProviderID, &a.CoverURL, &a.RecordType, &a.ReleaseID, &a.Status,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// FindAlbumByArtistTitleFold matches an album under an artist by title,
// case-insensitively. The importer uses it so imported files attach to an
// already-watched album instead of creating a duplicate.
func (q *Queries) FindAlbumByArtistTitleFold(artistID int64, title string) (*models.Album, error) {
	a := &models.Album{}
	err := q.db.QueryRow(
		`SELECT id, artist_id, title, year, provider, provider_id, cover_url, record_type, release_id, status, created_at, updated_at
		 FROM albums WHERE artist_id = ? AND LOWER(title) = LOWER(?) LIMIT 1`, artistID, title,
	).Scan(&a.ID, &a.ArtistID, &a.Title, &a.Year, &a.Provider, &a.ProviderID, &a.CoverURL, &a.RecordType, &a.ReleaseID, &a.Status,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (q *Queries) DeleteAlbum(id int64) error {
	_, err := q.db.Exec(`DELETE FROM albums WHERE id = ?`, id)
	return err
}

func (q *Queries) UpdateTrackStatusByAlbum(albumID int64, from, to models.TrackStatus) error {
	_, err := q.db.Exec(`UPDATE tracks SET status = ?, updated_at = ? WHERE album_id = ? AND status = ?`,
		to, now(), albumID, from)
	return err
}

// Tracks

func (q *Queries) CreateTrack(t *models.Track) error {
	ts := now()
	result, err := q.db.Exec(
		`INSERT INTO tracks (album_id, title, track_number, disc_number, duration_ms, provider, provider_id, status, mb_recording_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.AlbumID, t.Title, t.TrackNumber, t.DiscNumber, t.DurationMs, t.Provider, t.ProviderID, t.Status, t.MBRecordingID, ts, ts,
	)
	if err != nil {
		return err
	}
	t.ID, _ = result.LastInsertId()
	return nil
}

// CreateImportedTrack inserts a track the importer found on disk: already
// owned, with its file path and audio quality recorded.
func (q *Queries) CreateImportedTrack(t *models.Track) error {
	ts := now()
	result, err := q.db.Exec(
		`INSERT INTO tracks (album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		                     status, file_path, download_format, download_bitrate, mb_recording_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.AlbumID, t.Title, t.TrackNumber, t.DiscNumber, t.DurationMs, t.Provider, t.ProviderID,
		t.Status, t.FilePath, t.DownloadFormat, t.DownloadBitrate, t.MBRecordingID, ts, ts,
	)
	if err != nil {
		return err
	}
	t.ID, _ = result.LastInsertId()
	return nil
}

// ClaimTrackFile marks an existing track as owned by a file found on disk —
// e.g. a wanted track the user already has. Duration is only filled in when
// the row doesn't have one from its provider.
func (q *Queries) ClaimTrackFile(id int64, filePath, format string, bitrate, durationMs int, mbRecordingID *string) error {
	_, err := q.db.Exec(
		`UPDATE tracks SET status = 'owned', file_path = ?, download_format = ?, download_bitrate = ?,
		        duration_ms = CASE WHEN duration_ms > 0 THEN duration_ms ELSE ? END,
		        mb_recording_id = COALESCE(?, mb_recording_id), updated_at = ?
		 WHERE id = ?`,
		filePath, format, bitrate, durationMs, mbRecordingID, now(), id)
	return err
}

// FindTrackByAlbumTitleFold matches a track in an album by title,
// case-insensitively, used by the importer to claim watched-but-wanted tracks.
func (q *Queries) FindTrackByAlbumTitleFold(albumID int64, title string) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		        status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate, created_at, updated_at
		 FROM tracks WHERE album_id = ? AND LOWER(title) = LOWER(?) LIMIT 1`, albumID, title,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (q *Queries) FindTrackByProvider(provider, providerID string) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		        status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate, created_at, updated_at
		 FROM tracks WHERE provider = ? AND provider_id = ?`, provider, providerID,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// FindTrackByAlbumRecordingID matches a track within an album by MusicBrainz
// recording id — the fingerprint-verified identity AcoustID / Music Assistant
// writes. Scoped to the album so a recording shared across releases can't claim
// a file into the wrong one; highest-confidence import match.
func (q *Queries) FindTrackByAlbumRecordingID(albumID int64, mbRecordingID string) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		        status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate, created_at, updated_at
		 FROM tracks WHERE album_id = ? AND mb_recording_id = ? LIMIT 1`, albumID, mbRecordingID,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// FindTrackByPath finds an owned track by its stored file path — used by the
// Music Assistant reject watcher to map an MA filesystem track (whose provider
// item_id is the same library-relative path Crate stores) back to a track.
func (q *Queries) FindTrackByPath(filePath string) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		        status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate, created_at, updated_at
		 FROM tracks WHERE file_path = ? AND status = 'owned' LIMIT 1`, filePath,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (q *Queries) ListTracksByAlbum(albumID int64) ([]models.Track, error) {
	rows, err := q.db.Query(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		        status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate, mb_recording_id, created_at, updated_at
		 FROM tracks WHERE album_id = ? ORDER BY disc_number, track_number`, albumID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
			&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
			&t.DownloadFormat, &t.DownloadBitrate, &t.MBRecordingID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (q *Queries) UpdateTrackStatus(id int64, status models.TrackStatus) error {
	_, err := q.db.Exec(`UPDATE tracks SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

func (q *Queries) DeleteTrack(id int64) error {
	_, err := q.db.Exec(`DELETE FROM tracks WHERE id = ?`, id)
	return err
}

func (q *Queries) UpdateTrackFilePath(id int64, filePath string) error {
	_, err := q.db.Exec(`UPDATE tracks SET file_path = ?, status = 'owned', updated_at = ? WHERE id = ?`,
		filePath, now(), id)
	return err
}

func (q *Queries) UpdateTrackDownloadedFrom(id int64, username string) error {
	_, err := q.db.Exec(`UPDATE tracks SET downloaded_from = ?, updated_at = ? WHERE id = ?`,
		username, now(), id)
	return err
}

// GetTrack loads one track by id, including its mb_recording_id — used by the
// manual "this local file is that track" claim so the recording id carries over.
func (q *Queries) GetTrack(id int64) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms, provider, provider_id,
		        status, file_path, downloaded_from, downloaded_filename, download_format, download_bitrate,
		        mb_recording_id, created_at, updated_at
		 FROM tracks WHERE id = ?`, id,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.MBRecordingID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// UpdateTrackAlbum reparents a track to a different album — used when merging a
// local album into a linked one moves over a track the provider doesn't list.
func (q *Queries) UpdateTrackAlbum(id, albumID int64) error {
	_, err := q.db.Exec(`UPDATE tracks SET album_id = ?, updated_at = ? WHERE id = ?`, albumID, now(), id)
	return err
}

// UpdateTrackListing rewrites a row's display metadata to match a release's
// tracklist entry — used when a split moves a row onto another edition so
// the title/numbering reflect that release rather than the old one.
func (q *Queries) UpdateTrackListing(id int64, title string, trackNumber, discNumber int) error {
	_, err := q.db.Exec(
		`UPDATE tracks SET title = ?, track_number = ?, disc_number = ?, updated_at = ? WHERE id = ?`,
		title, trackNumber, discNumber, now(), id)
	return err
}

func (q *Queries) UpdateTrackDownloadedFilename(id int64, filename string) error {
	_, err := q.db.Exec(`UPDATE tracks SET downloaded_filename = ?, updated_at = ? WHERE id = ?`,
		filename, now(), id)
	return err
}

func (q *Queries) RejectTrack(id int64) error {
	_, err := q.db.Exec(
		`UPDATE tracks SET status = 'wanted', file_path = NULL, downloaded_from = NULL,
		        downloaded_filename = NULL, download_format = NULL, download_bitrate = NULL,
		        updated_at = ? WHERE id = ?`,
		now(), id)
	return err
}

// ListAlbumDownloadSources returns the distinct slskd users that have served
// (or are serving) tracks on an album — used to bias scoring toward keeping an
// album on one source.
func (q *Queries) ListAlbumDownloadSources(albumID int64) ([]string, error) {
	rows, err := q.db.Query(
		`SELECT DISTINCT downloaded_from FROM tracks WHERE album_id = ? AND downloaded_from IS NOT NULL AND downloaded_from != ''`, albumID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (q *Queries) GetTrackWithMeta(id int64) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
		        t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
		        t.download_format, t.download_bitrate, t.mb_recording_id, t.created_at, t.updated_at,
		        al.title, ar.name
		 FROM tracks t
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE t.id = ?`, id,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.MBRecordingID, &t.CreatedAt, &t.UpdatedAt,
		&t.AlbumTitle, &t.ArtistName)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (q *Queries) ListWantedTracks() ([]models.Track, error) {
	return q.ListWantedTracksLimited(0)
}

// wantedReleaseClause excludes tracks on albums whose release date hasn't
// arrived yet — unreleased music can't exist on peers, so searching for it is
// futile. Partial dates compare lexicographically ("2026" > today ⇒ upcoming).
const wantedReleaseClause = ` AND (al.release_date IS NULL OR al.release_date <= date('now'))`

// wantedSingleClause suppresses a single's track when the same-titled track
// exists on an album or EP by the same artist — pre-release singles are
// album-bound, so the album copy wins and the single isn't fetched twice.
// Only applies to auto-queue paths; an explicit per-album search still works,
// and an ignored album/EP (or ignored twin track) releases the suppression.
const wantedSingleClause = ` AND (COALESCE(al.record_type, '') <> 'single' OR NOT EXISTS (
	    SELECT 1 FROM tracks t2 JOIN albums al2 ON al2.id = t2.album_id
	    WHERE al2.artist_id = al.artist_id
	      AND COALESCE(al2.record_type, '') IN ('album', 'ep')
	      AND al2.status <> 'ignored'
	      AND t2.status <> 'ignored'
	      AND lower(t2.title) = lower(t.title)))`

func (q *Queries) ListWantedTracksLimited(limit int) ([]models.Track, error) {
	query := `SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
	                 t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
	                 t.download_format, t.download_bitrate, t.created_at, t.updated_at,
	                 al.title, ar.name
	          FROM tracks t
	          JOIN albums al ON al.id = t.album_id
	          JOIN artists ar ON ar.id = al.artist_id
	          WHERE t.status = 'wanted'` + wantedReleaseClause + wantedSingleClause + `
	          ORDER BY ar.name, al.year, t.disc_number, t.track_number`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracks(rows)
}

func (q *Queries) ListWantedTracksWithCooldown(cooldownCutoff string, limit int) ([]models.Track, error) {
	query := `SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
	                 t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
	                 t.download_format, t.download_bitrate, t.created_at, t.updated_at,
	                 al.title, ar.name
	          FROM tracks t
	          JOIN albums al ON al.id = t.album_id
	          JOIN artists ar ON ar.id = al.artist_id
	          WHERE t.status = 'wanted'` + wantedReleaseClause + wantedSingleClause + `
	            AND NOT EXISTS (
	              SELECT 1 FROM download_queue d
	              WHERE d.track_id = t.id AND d.status = 'failed'
	                AND d.last_attempt > ?
	            )
	          ORDER BY ar.name, al.year, t.disc_number, t.track_number`
	args := []any{cooldownCutoff}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracks(rows)
}

func scanTracks(rows *sql.Rows) ([]models.Track, error) {
	var tracks []models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
			&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
			&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt,
			&t.AlbumTitle, &t.ArtistName); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (q *Queries) ListWantedTracksByArtist(artistID int64) ([]models.Track, error) {
	rows, err := q.db.Query(
		`SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
		        t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
		        t.download_format, t.download_bitrate, t.created_at, t.updated_at,
		        al.title, ar.name
		 FROM tracks t
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE t.status = 'wanted' AND al.artist_id = ?` + wantedReleaseClause + wantedSingleClause + `
		 ORDER BY al.year, t.disc_number, t.track_number`, artistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
			&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
			&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt,
			&t.AlbumTitle, &t.ArtistName); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (q *Queries) ListWantedTracksByAlbum(albumID int64) ([]models.Track, error) {
	rows, err := q.db.Query(
		`SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
		        t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
		        t.download_format, t.download_bitrate, t.created_at, t.updated_at,
		        al.title, ar.name
		 FROM tracks t
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE t.status = 'wanted' AND t.album_id = ?
		 ORDER BY t.disc_number, t.track_number`, albumID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
			&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
			&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt,
			&t.AlbumTitle, &t.ArtistName); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (q *Queries) ListOwnedTracksWithPaths() ([]models.Track, error) {
	rows, err := q.db.Query(
		`SELECT id, album_id, title, track_number, disc_number, duration_ms,
		        provider, provider_id, status, file_path, downloaded_from, downloaded_filename,
		        download_format, download_bitrate, created_at, updated_at
		 FROM tracks WHERE status = 'owned' AND file_path IS NOT NULL`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
			&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
			&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (q *Queries) UpdateTrackQuality(id int64, format string, bitrate int) error {
	_, err := q.db.Exec(`UPDATE tracks SET download_format = ?, download_bitrate = ?, updated_at = ? WHERE id = ?`,
		format, bitrate, now(), id)
	return err
}

func (q *Queries) ListOwnedTracksByArtist(artistID int64) ([]models.Track, error) {
	rows, err := q.db.Query(
		`SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
		        t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
		        t.download_format, t.download_bitrate, t.created_at, t.updated_at,
		        al.title, ar.name
		 FROM tracks t
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE t.status = 'owned' AND al.artist_id = ?
		 ORDER BY al.year, t.disc_number, t.track_number`, artistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
			&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
			&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt,
			&t.AlbumTitle, &t.ArtistName); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (q *Queries) NextArtistForUpgrade(lastID int64) (*models.Artist, error) {
	a := &models.Artist{}
	err := q.db.QueryRow(
		`SELECT id, name, provider, provider_id, image_url, status, watch_new_releases, watch_new_releases_since, created_at, updated_at
		 FROM artists WHERE id > ? ORDER BY id LIMIT 1`, lastID,
	).Scan(&a.ID, &a.Name, &a.Provider, &a.ProviderID, &a.ImageURL, &a.Status, &a.WatchNewReleases, &a.WatchNewReleasesSince, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Download Queue

func (q *Queries) EnqueueDownload(trackID int64) error {
	_, err := q.db.Exec(
		`INSERT INTO download_queue (track_id, status, source, created_at)
		 VALUES (?, 'pending', 'user', ?)
		 ON CONFLICT (track_id) WHERE status IN ('pending', 'searching', 'downloading', 'organizing') DO NOTHING`,
		trackID, now(),
	)
	return err
}

func (q *Queries) ReenqueueDownload(trackID int64) error {
	q.db.Exec(
		`DELETE FROM download_queue WHERE track_id = ? AND status IN ('failed', 'complete')`,
		trackID,
	)
	_, err := q.db.Exec(
		`INSERT INTO download_queue (track_id, status, source, created_at)
		 VALUES (?, 'pending', 'scheduler', ?)
		 ON CONFLICT (track_id) WHERE status IN ('pending', 'searching', 'downloading', 'organizing') DO NOTHING`,
		trackID, now(),
	)
	return err
}

// EnqueueDownloadReturningID inserts a pending row and returns its id, or 0
// when the track already has an in-flight row (conflict). Callers must check
// for 0 — LastInsertId would otherwise surface a stale id.
func (q *Queries) EnqueueDownloadReturningID(trackID int64) (int64, error) {
	result, err := q.db.Exec(
		`INSERT INTO download_queue (track_id, status, source, created_at)
		 VALUES (?, 'pending', 'user', ?)
		 ON CONFLICT (track_id) WHERE status IN ('pending', 'searching', 'downloading', 'organizing') DO NOTHING`,
		trackID, now(),
	)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return 0, nil
	}
	id, _ := result.LastInsertId()
	return id, nil
}

func (q *Queries) ListDownloads(status string) ([]models.DownloadQueueItem, error) {
	query := `SELECT d.id, d.track_id, d.slskd_search_id, d.status, d.attempts, d.last_attempt, d.error, d.next_retry_at, d.source, d.last_progress_bytes, d.created_at
		 FROM download_queue d`
	var args []any
	if status != "" {
		query += " WHERE d.status = ?"
		args = append(args, status)
	}
	query += " ORDER BY d.created_at DESC"

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.DownloadQueueItem
	for rows.Next() {
		var d models.DownloadQueueItem
		if err := rows.Scan(&d.ID, &d.TrackID, &d.SlskdSearchID, &d.Status, &d.Attempts,
			&d.LastAttempt, &d.Error, &d.NextRetryAt, &d.Source, &d.LastProgressBytes, &d.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

func (q *Queries) ListDownloadsWithTrack(status string) ([]models.DownloadQueueItem, error) {
	query := `SELECT d.id, d.track_id, d.slskd_search_id, d.status, d.attempts, d.last_attempt, d.error, d.next_retry_at, d.source, d.last_progress_bytes, d.created_at,
		        t.title, ar.name, al.title, t.album_id, t.disc_number, t.track_number, al.cover_url
		 FROM download_queue d
		 JOIN tracks t ON t.id = d.track_id
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id`
	var args []any
	if status != "" {
		query += " WHERE d.status = ?"
		args = append(args, status)
	}
	query += " ORDER BY d.created_at DESC"

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.DownloadQueueItem
	for rows.Next() {
		var d models.DownloadQueueItem
		var trackTitle, artistName, albumTitle string
		var albumID int64
		var discNum, trackNum int
		var coverURL *string
		if err := rows.Scan(&d.ID, &d.TrackID, &d.SlskdSearchID, &d.Status, &d.Attempts,
			&d.LastAttempt, &d.Error, &d.NextRetryAt, &d.Source, &d.LastProgressBytes, &d.CreatedAt,
			&trackTitle, &artistName, &albumTitle, &albumID, &discNum, &trackNum, &coverURL); err != nil {
			return nil, err
		}
		d.Track = &models.Track{
			ID: d.TrackID, AlbumID: albumID, Title: trackTitle,
			ArtistName: artistName, AlbumTitle: albumTitle, AlbumCoverURL: coverURL,
			DiscNumber: discNum, TrackNumber: trackNum,
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

func (q *Queries) EnqueueDownloadBatch(trackIDs []int64) (int, error) {
	if len(trackIDs) == 0 {
		return 0, nil
	}
	tx, err := q.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO download_queue (track_id, status, source, created_at)
		 VALUES (?, 'pending', 'user', ?)
		 ON CONFLICT (track_id) WHERE status IN ('pending', 'searching', 'downloading', 'organizing') DO NOTHING`,
	)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	ts := now()
	queued := 0
	for _, id := range trackIDs {
		result, err := stmt.Exec(id, ts)
		if err != nil {
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			queued++
		}
	}
	return queued, tx.Commit()
}

func (q *Queries) CountActiveDownloads() (int, error) {
	var count int
	err := q.db.QueryRow(
		`SELECT COUNT(*) FROM download_queue WHERE status IN ('searching', 'downloading')`,
	).Scan(&count)
	return count, err
}

func (q *Queries) GetDownload(id int64) (*models.DownloadQueueItem, error) {
	var d models.DownloadQueueItem
	err := q.db.QueryRow(
		`SELECT id, track_id, slskd_search_id, status, attempts, last_attempt, error, next_retry_at, source, last_progress_bytes, created_at
		 FROM download_queue WHERE id = ?`, id,
	).Scan(&d.ID, &d.TrackID, &d.SlskdSearchID, &d.Status, &d.Attempts, &d.LastAttempt, &d.Error, &d.NextRetryAt, &d.Source, &d.LastProgressBytes, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (q *Queries) DeleteDownload(id int64) error {
	_, err := q.db.Exec(`DELETE FROM download_queue WHERE id = ?`, id)
	return err
}

func (q *Queries) DeleteDownloadsByStatus(status string) (int64, error) {
	result, err := q.db.Exec(`DELETE FROM download_queue WHERE status = ?`, status)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (q *Queries) ResetTrackStatusForDownloads(downloadStatus string) {
	q.db.Exec(
		`UPDATE tracks SET status = 'wanted' WHERE id IN (
			SELECT track_id FROM download_queue WHERE status = ?
		) AND status = 'downloading'`, downloadStatus,
	)
}

func (q *Queries) UpdateDownloadStatus(id int64, status models.DownloadStatus, searchID *string, err *string) error {
	_, e := q.db.Exec(
		`UPDATE download_queue SET status = ?, slskd_search_id = COALESCE(?, slskd_search_id),
		 error = ?, last_attempt = ?, next_retry_at = NULL WHERE id = ?`,
		status, searchID, err, now(), id,
	)
	return e
}

func (q *Queries) UpdateDownloadProgress(id int64, bytesTransferred int64) error {
	_, err := q.db.Exec(
		`UPDATE download_queue SET last_progress_bytes = ?, last_attempt = ? WHERE id = ?`,
		bytesTransferred, now(), id,
	)
	return err
}

func (q *Queries) ScheduleRetry(id int64, retryAt string, errMsg string) error {
	_, err := q.db.Exec(
		`UPDATE download_queue SET status = 'failed', error = ?, next_retry_at = ?,
		 attempts = attempts + 1, last_attempt = ? WHERE id = ?`,
		errMsg, retryAt, now(), id,
	)
	return err
}

func (q *Queries) ListRetryableDownloads() ([]models.DownloadQueueItem, error) {
	rows, err := q.db.Query(
		`SELECT id, track_id, slskd_search_id, status, attempts, last_attempt, error, next_retry_at, source, last_progress_bytes, created_at
		 FROM download_queue
		 WHERE status = 'failed' AND next_retry_at IS NOT NULL AND next_retry_at <= ?
		 ORDER BY next_retry_at`, now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.DownloadQueueItem
	for rows.Next() {
		var d models.DownloadQueueItem
		if err := rows.Scan(&d.ID, &d.TrackID, &d.SlskdSearchID, &d.Status, &d.Attempts,
			&d.LastAttempt, &d.Error, &d.NextRetryAt, &d.Source, &d.LastProgressBytes, &d.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

// Blacklist

func (q *Queries) BlacklistFile(username, filename, reason string) error {
	_, err := q.db.Exec(
		`INSERT INTO slskd_blacklist (username, filename, reason, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(username, filename) DO NOTHING`,
		username, filename, reason, now(),
	)
	return err
}

func (q *Queries) IsBlacklisted(username, filename string) bool {
	var count int
	q.db.QueryRow(
		`SELECT COUNT(*) FROM slskd_blacklist WHERE username = ? AND filename = ?`,
		username, filename,
	).Scan(&count)
	return count > 0
}

func (q *Queries) ListBlacklist() ([]models.BlacklistEntry, error) {
	rows, err := q.db.Query(`SELECT rowid, username, filename, reason, created_at FROM slskd_blacklist ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []models.BlacklistEntry
	for rows.Next() {
		var e models.BlacklistEntry
		if err := rows.Scan(&e.ID, &e.Username, &e.Filename, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

func (q *Queries) DeleteBlacklistEntry(id int64) error {
	_, err := q.db.Exec(`DELETE FROM slskd_blacklist WHERE rowid = ?`, id)
	return err
}

func (q *Queries) ClearBlacklist() error {
	_, err := q.db.Exec(`DELETE FROM slskd_blacklist`)
	return err
}

// Cooldowns (shadow banning)

func (q *Queries) CooldownUser(username, reason string, duration time.Duration) error {
	expiresAt := time.Now().UTC().Add(duration).Format(time.RFC3339)
	_, err := q.db.Exec(
		`INSERT INTO user_cooldowns (username, reason, expires_at, created_at)
		 VALUES (?, ?, ?, ?)`,
		username, reason, expiresAt, now(),
	)
	return err
}

func (q *Queries) IsUserCooledDown(username string) bool {
	var count int
	q.db.QueryRow(
		`SELECT COUNT(*) FROM user_cooldowns WHERE username = ? AND expires_at > ?`,
		username, now(),
	).Scan(&count)
	return count > 0
}

func (q *Queries) ListActiveCooldowns() ([]models.UserCooldown, error) {
	rows, err := q.db.Query(
		`SELECT id, username, reason, expires_at, created_at FROM user_cooldowns WHERE expires_at > ? ORDER BY expires_at`,
		now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []models.UserCooldown
	for rows.Next() {
		var c models.UserCooldown
		if err := rows.Scan(&c.ID, &c.Username, &c.Reason, &c.ExpiresAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func (q *Queries) DeleteCooldown(id int64) error {
	_, err := q.db.Exec(`DELETE FROM user_cooldowns WHERE id = ?`, id)
	return err
}

func (q *Queries) ClearCooldowns() error {
	_, err := q.db.Exec(`DELETE FROM user_cooldowns`)
	return err
}

func (q *Queries) PurgeExpiredCooldowns() {
	q.db.Exec(`DELETE FROM user_cooldowns WHERE expires_at <= ?`, now())
}

// Library Search

type LibrarySearchResult struct {
	ArtistID   int64  `json:"artist_id"`
	ArtistName string `json:"artist_name"`
	AlbumID    int64  `json:"album_id"`
	AlbumTitle string `json:"album_title"`
	TrackID    int64  `json:"track_id"`
	TrackTitle string `json:"track_title"`
}

func (q *Queries) FindOwnedTrackByName(artist, title string) (*models.Track, error) {
	t := &models.Track{}
	err := q.db.QueryRow(
		`SELECT t.id, t.album_id, t.title, t.track_number, t.disc_number, t.duration_ms,
		        t.provider, t.provider_id, t.status, t.file_path, t.downloaded_from, t.downloaded_filename,
		        t.download_format, t.download_bitrate, t.created_at, t.updated_at,
		        al.title, ar.name
		 FROM tracks t
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE ar.name = ? COLLATE NOCASE AND t.title = ? COLLATE NOCASE AND t.status = 'owned'
		 LIMIT 1`, artist, title,
	).Scan(&t.ID, &t.AlbumID, &t.Title, &t.TrackNumber, &t.DiscNumber, &t.DurationMs,
		&t.Provider, &t.ProviderID, &t.Status, &t.FilePath, &t.DownloadedFrom, &t.DownloadedFilename,
		&t.DownloadFormat, &t.DownloadBitrate, &t.CreatedAt, &t.UpdatedAt,
		&t.AlbumTitle, &t.ArtistName)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (q *Queries) SearchLibrary(query string, limit int) ([]LibrarySearchResult, error) {
	if limit <= 0 {
		limit = 50
	}
	pattern := "%" + query + "%"
	rows, err := q.db.Query(
		`SELECT ar.id, ar.name, al.id, al.title, t.id, t.title
		 FROM tracks t
		 JOIN albums al ON al.id = t.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE t.title LIKE ? COLLATE NOCASE
		 ORDER BY ar.name, al.title, t.track_number
		 LIMIT ?`,
		pattern, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []LibrarySearchResult
	for rows.Next() {
		var r LibrarySearchResult
		if err := rows.Scan(&r.ArtistID, &r.ArtistName, &r.AlbumID, &r.AlbumTitle, &r.TrackID, &r.TrackTitle); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// Settings

func (q *Queries) GetSetting(key string) (string, error) {
	var value string
	err := q.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	return value, err
}

func (q *Queries) SetSetting(key, value string) error {
	_, err := q.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?`,
		key, value, value,
	)
	return err
}

func (q *Queries) AllSettings() (map[string]string, error) {
	rows, err := q.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	return settings, rows.Err()
}
