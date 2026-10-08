// Package upload implements the staged upload pipeline (ADR-0008): files are
// dropped into a staging dir outside the library, identified against provider
// metadata or existing library rows (reusing the importer's tag readers),
// reviewed by the user, then committed through the organizer + tagger like a
// normal download.
package upload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheOutdoorProgrammer/crate/internal/activity"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
	"github.com/TheOutdoorProgrammer/crate/internal/services/downloader"
	"github.com/TheOutdoorProgrammer/crate/internal/services/importer"
	"github.com/TheOutdoorProgrammer/crate/internal/services/organizer"
	pb "github.com/TheOutdoorProgrammer/crate/proto/provider"
)

const musicbrainzProvider = "musicbrainz"

// AlbumRef identifies the album an uploaded file belongs to — an existing
// library album (ID set) or a provider album not yet in the library (New).
type AlbumRef struct {
	ID         int64  `json:"id,omitempty"`
	Provider   string `json:"provider"`
	ProviderID string `json:"provider_id"`
	Title      string `json:"title"`
	// ReleaseID pins a specific edition inside the release-group (a
	// MusicBrainz release id); empty means the provider's default release.
	ReleaseID string `json:"release_id,omitempty"`
	New       bool   `json:"new"`
}

// AlbumEdition is one release inside a release-group, as surfaced by the
// provider's album metadata — the upload page's edition picker.
type AlbumEdition struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status,omitempty"`
	Date           string `json:"date,omitempty"`
	Country        string `json:"country,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	TrackCount     int    `json:"track_count,omitempty"`
}

// Match is the proposed destination for one staged file.
type Match struct {
	TrackID         int64     `json:"track_id,omitempty"`
	TrackTitle      string    `json:"track_title,omitempty"`
	ProviderTrackID string    `json:"provider_track_id,omitempty"`
	Album           *AlbumRef `json:"album,omitempty"`
	Confidence      string    `json:"confidence"`
	Reason          string    `json:"reason"`
	Duplicate       bool      `json:"duplicate"`
}

// FileMetaView is the subset of embedded tags surfaced to the review UI.
type FileMetaView struct {
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Title    string `json:"title"`
	Track    int    `json:"track"`
	Disc     int    `json:"disc"`
	Year     int    `json:"year"`
	Duration int    `json:"duration_ms"`
	Format   string `json:"format"`
	Bitrate  int    `json:"bitrate"`
	MBTagged bool   `json:"mb_tagged"`
}

// FileView is one staged file as returned by the API.
type FileView struct {
	ID       int64         `json:"id"`
	Filename string        `json:"filename"`
	Size     int64         `json:"size"`
	State    string        `json:"state"`
	Skip     bool          `json:"skip"`
	Error    *string       `json:"error,omitempty"`
	Meta     *FileMetaView `json:"meta,omitempty"`
	Match    *Match        `json:"match,omitempty"`
}

// BatchView is the review payload for one upload batch.
type BatchView struct {
	BatchID      string     `json:"batch_id"`
	Files        []FileView `json:"files"`
	Total        int        `json:"total"`
	Identified   int        `json:"identified"`
	Unidentified int        `json:"unidentified"`
}

// CommitResult reports the outcome of committing a batch.
type CommitResult struct {
	Committed []CommittedFile `json:"committed"`
	Skipped   []int64         `json:"skipped"`
	Failed    []FailedFile    `json:"failed"`
	// Cleared is true when the commit left nothing to review — the batch was
	// discarded automatically.
	Cleared bool `json:"cleared"`
}

type CommittedFile struct {
	FileID  int64  `json:"file_id"`
	TrackID int64  `json:"track_id"`
	Path    string `json:"path"`
}

type FailedFile struct {
	FileID int64  `json:"file_id"`
	Error  string `json:"error"`
}

type Service struct {
	queries   *db.Queries
	providers *provider.Manager
	uploadDir string
	org       *organizer.Service
	actLog    *activity.Log
	dl        *downloader.Service // lazily queried for notifiers at commit time
}

func NewService(queries *db.Queries, providers *provider.Manager, uploadDir, libraryDir string, actLog *activity.Log, dl *downloader.Service) *Service {
	return &Service{
		queries:   queries,
		providers: providers,
		uploadDir: uploadDir,
		org:       organizer.NewService(queries, uploadDir, libraryDir),
		actLog:    actLog,
		dl:        dl,
	}
}

func validBatchID(batchID string) bool {
	if batchID == "" || len(batchID) > 64 {
		return false
	}
	for _, r := range batchID {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// Stage writes one uploaded file under uploadDir/batchID/ and records a row.
func (s *Service) Stage(batchID, filename string, src io.Reader, size int64) (*models.UploadFile, error) {
	if !validBatchID(batchID) {
		return nil, fmt.Errorf("invalid batch id")
	}
	name := filepath.Base(filename)
	if name == "" || name == "." || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("invalid filename")
	}
	dir := filepath.Join(s.uploadDir, batchID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	// Avoid clobbering a same-named file already in the batch.
	staged := filepath.Join(dir, name)
	if _, err := os.Stat(staged); err == nil {
		staged = filepath.Join(dir, fmt.Sprintf("%d-%s", len(name), name))
	}
	dst, err := os.Create(staged)
	if err != nil {
		return nil, err
	}
	defer dst.Close()
	written, err := io.Copy(dst, src)
	if err != nil {
		os.Remove(staged)
		return nil, err
	}
	if size <= 0 {
		size = written
	}
	f := &models.UploadFile{
		BatchID:    batchID,
		Filename:   name,
		StagedPath: staged,
		Size:       size,
		State:      models.UploadStateUploaded,
	}
	if err := s.queries.CreateUploadFile(f); err != nil {
		os.Remove(staged)
		return nil, err
	}
	return f, nil
}

// Identify runs tag reading + matching over every uploaded/unidentified file in
// the batch. It is synchronous — batches are small and provider lookups are
// cached.
func (s *Service) Identify(ctx context.Context, batchID string) error {
	if !validBatchID(batchID) {
		return fmt.Errorf("invalid batch id")
	}
	files, err := s.queries.ListUploadFiles(batchID)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.State != models.UploadStateUploaded && f.State != models.UploadStateUnidentified {
			continue
		}
		s.identifyFile(ctx, &f)
	}
	return nil
}

func (s *Service) identifyFile(ctx context.Context, f *models.UploadFile) {
	fail := func(state, reason string) {
		r := reason
		meta := s.readMeta(f.StagedPath)
		_ = s.queries.UpdateUploadFileResult(f.ID, state, meta, nil, &r)
	}

	fm, err := importer.ReadTags(f.StagedPath)
	if err != nil {
		fail(models.UploadStateUnidentified, "could not read tags: "+err.Error())
		return
	}
	if fm.Artist == "" || fm.Title == "" {
		fail(models.UploadStateUnidentified, "missing artist or title tags")
		return
	}

	match := s.matchFile(ctx, fm)
	if match == nil {
		fail(models.UploadStateUnidentified, "no matching album in library or provider")
		return
	}
	metaJSON := s.readMeta(f.StagedPath)
	matchJSON, _ := json.Marshal(match)
	mStr := string(matchJSON)
	_ = s.queries.UpdateUploadFileResult(f.ID, models.UploadStateIdentified, metaJSON, &mStr, nil)
}

func (s *Service) readMeta(path string) *string {
	fm, err := importer.ReadTags(path)
	if err != nil {
		return nil
	}
	v := FileMetaView{
		Artist:   fm.Artist,
		Album:    fm.Album,
		Title:    fm.Title,
		Track:    fm.Track,
		Disc:     fm.Disc,
		Year:     fm.Year,
		Duration: fm.DurationMs,
		Format:   fm.Format,
		Bitrate:  fm.Bitrate,
		MBTagged: fm.MBReleaseGroupID != "",
	}
	b, _ := json.Marshal(v)
	str := string(b)
	return &str
}

// matchFile resolves the file to an existing library track, or a provider album
// the library doesn't have yet (proposed for creation at commit).
func (s *Service) matchFile(ctx context.Context, fm *importer.FileMeta) *Match {
	var album *models.Album
	// A release-keyed album (split edition, e.g. an instrumental release)
	// beats the release-group album when the file's tags name it directly.
	if fm.MBReleaseID != "" {
		album, _ = s.queries.FindAlbumByProvider(musicbrainzProvider, fm.MBReleaseID)
	}
	if album == nil && fm.MBReleaseGroupID != "" {
		album, _ = s.queries.FindAlbumByProvider(musicbrainzProvider, fm.MBReleaseGroupID)
	}
	if album == nil && fm.Album != "" {
		if artist, _ := s.queries.FindArtistByNameFold(fm.Artist); artist != nil {
			album, _ = s.queries.FindAlbumByArtistTitleFold(artist.ID, fm.Album)
		}
	}
	if album != nil {
		return s.matchInAlbum(album, fm)
	}
	if fm.MBReleaseGroupID != "" {
		return s.proposeNewAlbum(ctx, fm)
	}
	return nil
}

func trackMatch(t *models.Track, album *models.Album, confidence, reason string) *Match {
	m := &Match{
		TrackID:    t.ID,
		TrackTitle: t.Title,
		Album:      &AlbumRef{ID: album.ID, Provider: album.Provider, ProviderID: album.ProviderID, Title: album.Title, ReleaseID: deref(album.ReleaseID)},
		Confidence: confidence,
		Reason:     reason,
	}
	if t.Status == models.TrackStatusOwned && t.FilePath != nil && *t.FilePath != "" {
		m.Duplicate = true
	}
	return m
}

// matchInAlbum applies the importer's ladder — recording id → release-track id
// → title fold — scoped to one album.
func (s *Service) matchInAlbum(album *models.Album, fm *importer.FileMeta) *Match {
	if fm.MBRecordingID != "" {
		if t, _ := s.queries.FindTrackByAlbumRecordingID(album.ID, fm.MBRecordingID); t != nil {
			return trackMatch(t, album, "high", "musicbrainz recording id")
		}
	}
	if fm.MBTrackID != "" && album.Provider == musicbrainzProvider {
		if t, _ := s.queries.FindTrackByProvider(musicbrainzProvider, fm.MBTrackID); t != nil && t.AlbumID == album.ID {
			return trackMatch(t, album, "high", "musicbrainz track id")
		}
	}
	if t, _ := s.queries.FindTrackByAlbumTitleFold(album.ID, fm.Title); t != nil {
		confidence, reason := "medium", "title match"
		if durationMismatch(t.DurationMs, fm.DurationMs) {
			confidence, reason = "low", "title match, duration differs"
		}
		return trackMatch(t, album, confidence, reason)
	}
	return &Match{
		Album:      &AlbumRef{ID: album.ID, Provider: album.Provider, ProviderID: album.ProviderID, Title: album.Title, ReleaseID: deref(album.ReleaseID)},
		Confidence: "low",
		Reason:     "album matched but no track matched",
	}
}

// Editions lists the releases inside a provider album's release-group — the
// edition picker for albums a batch would create.
func (s *Service) Editions(ctx context.Context, providerName, providerID string) ([]AlbumEdition, error) {
	detail, err := s.providers.GetAlbum(ctx, providerName, providerID)
	if err != nil || detail == nil {
		return nil, fmt.Errorf("provider album lookup failed")
	}
	var editions []AlbumEdition
	if raw := detail.Metadata["releases"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &editions)
	}
	return editions, nil
}

// matchProviderTrack applies the recording-id → release-track-id → title
// ladder against a provider tracklist.
func matchProviderTrack(tracks []*pb.TrackInfo, fm *importer.FileMeta) (id, title, confidence, reason string) {
	if fm.MBRecordingID != "" {
		for _, pt := range tracks {
			if pt.Metadata["recording_id"] == fm.MBRecordingID {
				return pt.Id, pt.Title, "high", "musicbrainz recording id"
			}
		}
	}
	if fm.MBTrackID != "" {
		for _, pt := range tracks {
			if pt.Id == fm.MBTrackID {
				return pt.Id, pt.Title, "high", "musicbrainz track id"
			}
		}
	}
	for _, pt := range tracks {
		if strings.EqualFold(pt.Title, fm.Title) {
			confidence, reason := "medium", "title match"
			if durationMismatch(int(pt.DurationMs), fm.DurationMs) {
				confidence, reason = "low", "title match, duration differs"
			}
			return pt.Id, pt.Title, confidence, reason
		}
	}
	return "", "", "", ""
}

// taggedEdition returns the release id the file's own tags point at when it's
// a member of this release-group — Picard-tagged uploads pin their edition
// automatically instead of defaulting to the group's first release.
func taggedEdition(detail *pb.AlbumDetail, fm *importer.FileMeta) string {
	if fm.MBReleaseID == "" {
		return ""
	}
	if raw := detail.Metadata["releases"]; raw != "" {
		var editions []AlbumEdition
		if json.Unmarshal([]byte(raw), &editions) == nil {
			for _, e := range editions {
				if e.ID == fm.MBReleaseID {
					return fm.MBReleaseID
				}
			}
		}
	}
	return ""
}

// albumDetailFor returns the album detail whose tracklist should drive
// matching/creation — the pinned edition's own release detail when set.
func (s *Service) albumDetailFor(ctx context.Context, providerName, providerID, releaseID string) (*pb.AlbumDetail, error) {
	id := providerID
	if releaseID != "" {
		id = releaseID
	}
	return s.providers.GetAlbum(ctx, providerName, id)
}

// proposeNewAlbum fetches the provider release-group and matches the file to
// one of its tracks. The album row itself is only created at commit time.
func (s *Service) proposeNewAlbum(ctx context.Context, fm *importer.FileMeta) *Match {
	detail, err := s.providers.GetAlbum(ctx, musicbrainzProvider, fm.MBReleaseGroupID)
	if err != nil || detail == nil {
		return nil
	}
	ref := &AlbumRef{Provider: musicbrainzProvider, ProviderID: fm.MBReleaseGroupID, Title: detail.Title, New: true}
	if rel := taggedEdition(detail, fm); rel != "" {
		ref.ReleaseID = rel
		if d, derr := s.albumDetailFor(ctx, musicbrainzProvider, fm.MBReleaseGroupID, rel); derr == nil && d != nil {
			detail = d
		}
	}
	m := &Match{Album: ref, Confidence: "low", Reason: "album not in library; no track matched"}
	if id, title, conf, reason := matchProviderTrack(detail.Tracks, fm); id != "" {
		m.ProviderTrackID, m.TrackTitle = id, title
		m.Confidence, m.Reason = conf, "new album; "+reason
	}
	return m
}

func durationMismatch(expectedMs, actualMs int) bool {
	if expectedMs <= 0 || actualMs <= 0 {
		return false
	}
	return math.Abs(float64(expectedMs-actualMs)) > float64(expectedMs)*0.15
}

// Batch returns the review view for one batch.
func (s *Service) Batch(batchID string) (*BatchView, error) {
	if !validBatchID(batchID) {
		return nil, fmt.Errorf("invalid batch id")
	}
	files, err := s.queries.ListUploadFiles(batchID)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return nil, fmt.Errorf("batch not found")
	}
	view := &BatchView{BatchID: batchID, Files: make([]FileView, 0, len(files))}
	for _, f := range files {
		fv := FileView{
			ID: f.ID, Filename: f.Filename, Size: f.Size,
			State: f.State, Skip: f.Skip, Error: f.Error,
		}
		if f.Meta != nil {
			var meta FileMetaView
			if json.Unmarshal([]byte(*f.Meta), &meta) == nil {
				fv.Meta = &meta
			}
		}
		if f.Match != nil {
			var m Match
			if json.Unmarshal([]byte(*f.Match), &m) == nil {
				fv.Match = &m
			}
		}
		view.Files = append(view.Files, fv)
		switch f.State {
		case models.UploadStateIdentified:
			view.Identified++
		case models.UploadStateUnidentified:
			view.Unidentified++
		}
	}
	view.Total = len(files)
	return view, nil
}

// SetSkip toggles whether a file is committed.
func (s *Service) SetSkip(fileID int64, skip bool) error {
	return s.queries.SetUploadFileSkip(fileID, skip)
}

// SetTrackMatch retargets a file at an existing track (user override from the
// review UI). The track's album is recorded on the match for context.
func (s *Service) SetTrackMatch(fileID, trackID int64) error {
	track, err := s.queries.GetTrack(trackID)
	if err != nil {
		return fmt.Errorf("track not found")
	}
	album, _ := s.queries.GetAlbum(track.AlbumID)
	m := &Match{
		TrackID:    track.ID,
		TrackTitle: track.Title,
		Confidence: "high",
		Reason:     "user selected",
	}
	if album != nil {
		m.Album = &AlbumRef{ID: album.ID, Provider: album.Provider, ProviderID: album.ProviderID, Title: album.Title}
	}
	if track.Status == models.TrackStatusOwned && track.FilePath != nil && *track.FilePath != "" {
		m.Duplicate = true
	}
	b, _ := json.Marshal(m)
	str := string(b)
	return s.queries.UpdateUploadFileMatch(fileID, &str, models.UploadStateIdentified)
}

// SetGroupRelease overrides the release (edition) for every identified file
// in the batch matched to a provider album the library doesn't have yet —
// the upload page's edition picker. Each file re-matches against that
// edition's own tracklist. A nil/empty releaseID returns to the
// release-group default.
func (s *Service) SetGroupRelease(ctx context.Context, batchID, providerName, providerID string, releaseID *string) error {
	if !validBatchID(batchID) {
		return fmt.Errorf("invalid batch id")
	}
	files, err := s.queries.ListUploadFiles(batchID)
	if err != nil {
		return err
	}
	rid := ""
	if releaseID != nil {
		rid = *releaseID
	}
	detail, err := s.albumDetailFor(ctx, providerName, providerID, rid)
	if err != nil || detail == nil {
		return fmt.Errorf("provider album lookup failed")
	}
	for i := range files {
		f := &files[i]
		if f.State != models.UploadStateIdentified || f.Match == nil {
			continue
		}
		var m Match
		if json.Unmarshal([]byte(*f.Match), &m) != nil || m.Album == nil {
			continue
		}
		if !m.Album.New || m.Album.Provider != providerName || m.Album.ProviderID != providerID {
			continue
		}
		m.Album.ReleaseID = rid
		m.ProviderTrackID, m.TrackTitle = "", ""
		m.Confidence, m.Reason = "low", "edition selected; no track matched"
		if fm, terr := importer.ReadTags(f.StagedPath); terr == nil {
			if id, title, conf, reason := matchProviderTrack(detail.Tracks, fm); id != "" {
				m.ProviderTrackID, m.TrackTitle = id, title
				m.Confidence, m.Reason = conf, "edition selected; "+reason
			}
		}
		b, _ := json.Marshal(&m)
		str := string(b)
		_ = s.queries.UpdateUploadFileMatch(f.ID, &str, models.UploadStateIdentified)
	}
	return nil
}

// Commit files every identified, non-skipped file into the library. onDuplicate
// is "skip" (default) or "replace".
func (s *Service) Commit(ctx context.Context, batchID, onDuplicate string) (*CommitResult, error) {
	if !validBatchID(batchID) {
		return nil, fmt.Errorf("invalid batch id")
	}
	files, err := s.queries.ListUploadFiles(batchID)
	if err != nil {
		return nil, err
	}
	res := &CommitResult{Committed: []CommittedFile{}, Skipped: []int64{}, Failed: []FailedFile{}}
	ensured := map[string]bool{}

	for i := range files {
		f := &files[i]
		if f.Skip || f.State != models.UploadStateIdentified {
			continue
		}
		if f.Meta == nil || f.Match == nil {
			continue
		}
		var meta FileMetaView
		var match Match
		if json.Unmarshal([]byte(*f.Meta), &meta) != nil || json.Unmarshal([]byte(*f.Match), &match) != nil {
			continue
		}
		fm := s.fullMeta(f.StagedPath, &meta)

		track, err := s.resolveTrack(ctx, fm, &match, ensured)
		if err != nil {
			res.Failed = append(res.Failed, FailedFile{FileID: f.ID, Error: err.Error()})
			errStr := err.Error()
			_ = s.queries.UpdateUploadFileState(f.ID, models.UploadStateFailed, &errStr)
			continue
		}

		if track.Status == models.TrackStatusOwned && track.FilePath != nil && *track.FilePath != "" && onDuplicate != "replace" {
			res.Skipped = append(res.Skipped, f.ID)
			_ = s.queries.UpdateUploadFileState(f.ID, models.UploadStateSkipped, nil)
			continue
		}

		base := filepath.Base(f.StagedPath)
		track.FilePath = &base
		if err := s.org.Organize(track); err != nil {
			res.Failed = append(res.Failed, FailedFile{FileID: f.ID, Error: "organize: " + err.Error()})
			errStr := err.Error()
			_ = s.queries.UpdateUploadFileState(f.ID, models.UploadStateFailed, &errStr)
			continue
		}

		relPath := ""
		if stored, err := s.queries.GetTrack(track.ID); err == nil && stored.FilePath != nil {
			relPath = *stored.FilePath
		}
		_ = s.queries.ClaimTrackFile(track.ID, relPath, fm.Format, fm.Bitrate, fm.DurationMs, optStr(fm.MBRecordingID))
		_ = s.queries.DeleteQueuedForTrack(track.ID)
		_ = s.queries.UpdateUploadFileState(f.ID, models.UploadStateCommitted, nil)
		res.Committed = append(res.Committed, CommittedFile{FileID: f.ID, TrackID: track.ID, Path: relPath})
		if s.actLog != nil {
			s.actLog.Record("upload_committed", "track", track.ID,
				fmt.Sprintf("Uploaded: %s - %s", fm.Artist, fm.Title))
		}
	}

	if len(res.Committed) > 0 && s.dl != nil {
		for _, n := range s.dl.Notifiers() {
			n.TriggerScan(ctx)
		}
	}
	res.Cleared = s.clearIfResolved(batchID)
	return res, nil
}

// clearIfResolved discards a batch once every file is resolved — committed,
// skipped, or deliberately excluded — with nothing failed or unidentified
// left. Batches holding failed or unidentified files stay listed so the user
// can fix and retry.
func (s *Service) clearIfResolved(batchID string) bool {
	files, err := s.queries.ListUploadFiles(batchID)
	if err != nil {
		return false
	}
	for _, f := range files {
		if f.Skip {
			continue
		}
		if f.State != models.UploadStateCommitted && f.State != models.UploadStateSkipped {
			return false
		}
	}
	if err := s.Discard(batchID); err != nil {
		slog.Warn("upload: auto-clear failed", "batch", batchID, "error", err)
		return false
	}
	return true
}

// fullMeta re-reads tags at commit time so format/bitrate/duration/MBRecordingID
// are always from the file itself, not the earlier JSON snapshot.
func (s *Service) fullMeta(path string, fallback *FileMetaView) *importer.FileMeta {
	if fm, err := importer.ReadTags(path); err == nil {
		return fm
	}
	return &importer.FileMeta{
		Path:       path,
		Artist:     fallback.Artist,
		Album:      fallback.Album,
		Title:      fallback.Title,
		Track:      fallback.Track,
		Disc:       fallback.Disc,
		Year:       fallback.Year,
		DurationMs: fallback.Duration,
		Format:     fallback.Format,
		Bitrate:    fallback.Bitrate,
	}
}

// resolveTrack turns a match into a concrete track row, creating the proposed
// provider album (and its wanted tracks) on first use.
func (s *Service) resolveTrack(ctx context.Context, fm *importer.FileMeta, m *Match, ensured map[string]bool) (*models.Track, error) {
	if m.TrackID != 0 {
		return s.queries.GetTrack(m.TrackID)
	}
	if m.Album == nil {
		return nil, fmt.Errorf("no album match")
	}
	if m.Album.New {
		key := m.Album.Provider + "|" + m.Album.ProviderID
		if !ensured[key] {
			if err := s.ensureAlbum(ctx, fm, m.Album); err != nil {
				return nil, err
			}
			ensured[key] = true
		}
		album, err := s.queries.FindAlbumByProvider(m.Album.Provider, m.Album.ProviderID)
		if err != nil || album == nil {
			return nil, fmt.Errorf("album not created")
		}
		if m.ProviderTrackID != "" {
			if t, _ := s.queries.FindTrackByProvider(m.Album.Provider, m.ProviderTrackID); t != nil && t.AlbumID == album.ID {
				return t, nil
			}
		}
		if t, _ := s.queries.FindTrackByAlbumTitleFold(album.ID, fm.Title); t != nil {
			return t, nil
		}
		return nil, fmt.Errorf("no matching track in new album")
	}
	if m.Album.ID == 0 {
		return nil, fmt.Errorf("no track match")
	}
	if t, _ := s.queries.FindTrackByAlbumTitleFold(m.Album.ID, fm.Title); t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("no matching track in album")
}

// ensureAlbum creates the artist (if needed), the provider album, and its full
// tracklist as wanted — mirroring what watching an album does.
func (s *Service) ensureAlbum(ctx context.Context, fm *importer.FileMeta, ref *AlbumRef) error {
	detail, err := s.albumDetailFor(ctx, ref.Provider, ref.ProviderID, ref.ReleaseID)
	if err != nil || detail == nil {
		return fmt.Errorf("provider album lookup failed")
	}

	var artist *models.Artist
	if fm.MBArtistID != "" {
		artist, _ = s.queries.FindArtistByProvider(ref.Provider, fm.MBArtistID)
	}
	if artist == nil {
		artist, _ = s.queries.FindArtistByNameFold(fm.Artist)
	}
	if artist == nil {
		if fm.MBArtistID == "" {
			return fmt.Errorf("cannot create artist without provider id")
		}
		name := fm.Artist
		if detail.ArtistName != "" {
			name = detail.ArtistName
		}
		artist = &models.Artist{
			Name:       name,
			Provider:   ref.Provider,
			ProviderID: fm.MBArtistID,
			Status:     models.ArtistStatusOwned,
		}
		if err := s.queries.CreateArtist(artist); err != nil {
			return fmt.Errorf("create artist: %w", err)
		}
	}

	album := &models.Album{
		ArtistID:   artist.ID,
		Title:      detail.Title,
		Provider:   ref.Provider,
		ProviderID: ref.ProviderID,
		ReleaseID:  optStr(ref.ReleaseID),
		CoverURL:   optStr(detail.CoverUrl),
		Status:     models.AlbumStatusWatched,
	}
	if detail.Year > 0 {
		y := int(detail.Year)
		album.Year = &y
	}
	if err := s.queries.CreateAlbum(album); err != nil {
		return fmt.Errorf("create album: %w", err)
	}
	for _, pt := range detail.Tracks {
		_ = s.queries.CreateTrack(&models.Track{
			AlbumID:       album.ID,
			Title:         pt.Title,
			TrackNumber:   int(pt.TrackNumber),
			DiscNumber:    int(pt.DiscNumber),
			DurationMs:    int(pt.DurationMs),
			Provider:      ref.Provider,
			ProviderID:    pt.Id,
			Status:        models.TrackStatusWanted,
			MBRecordingID: optStr(pt.Metadata["recording_id"]),
		})
	}
	return nil
}

// Discard removes a batch's staged files and rows.
func (s *Service) Discard(batchID string) error {
	if !validBatchID(batchID) {
		return fmt.Errorf("invalid batch id")
	}
	if err := s.queries.DeleteUploadBatch(batchID); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(s.uploadDir, batchID))
	return nil
}

// Batches lists upload batch summaries for the UI.
func (s *Service) Batches() ([]models.UploadBatchSummary, error) {
	return s.queries.ListUploadBatches()
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
