package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
	pb "github.com/TheOutdoorProgrammer/crate/proto/provider"
)

// albumEdition is one release inside an album's release-group — surfaced to
// the UI as the edition picker.
type albumEdition struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status,omitempty"`
	Date           string `json:"date,omitempty"`
	Country        string `json:"country,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	TrackCount     int    `json:"track_count,omitempty"`
}

// handleGetAlbumEditions lists the editions of an album's release-group so the
// user can pin a specific pressing (country/format variants share the same
// songs under different MusicBrainz release ids).
func (s *Server) handleGetAlbumEditions(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	album, err := s.queries.GetAlbum(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "album not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get album")
		return
	}
	if album.Provider == "" || album.Provider == provider.LocalProvider {
		writeJSON(w, http.StatusOK, map[string]any{"editions": []albumEdition{}, "current": nil})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	detail, err := s.providers.GetAlbum(ctx, album.Provider, album.ProviderID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider fetch failed")
		return
	}
	var editions []albumEdition
	if raw := detail.Metadata["releases"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &editions)
	}
	hasCounts := false
	for _, e := range editions {
		if e.TrackCount > 0 {
			hasCounts = true
			break
		}
	}
	if !hasCounts && album.Provider == "musicbrainz" {
		// Album detail cached before editions/track-count metadata existed —
		// the picker would hide until the TTL expired. Evict and refetch once.
		s.cache.Delete(album.Provider + ":album:" + album.ProviderID)
		if fresh, ferr := s.providers.GetAlbum(ctx, album.Provider, album.ProviderID); ferr == nil {
			if raw := fresh.Metadata["releases"]; raw != "" {
				_ = json.Unmarshal([]byte(raw), &editions)
			}
		}
	}
	if editions == nil {
		editions = []albumEdition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"editions": editions,
		"current":  album.ReleaseID,
	})
}

// handleRefreshAlbum re-pulls one album's tracklist from its provider —
// honoring a pinned edition — re-folds tracks (recording-id merge included),
// and re-resolves cover art. Runs in the background; progress is published on
// album.sync via GET /albums/{id}, so every device sees the same phases.
func (s *Server) handleRefreshAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	album, err := s.queries.GetAlbum(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "album not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get album")
		return
	}
	if album.Provider == "" || album.Provider == provider.LocalProvider {
		writeError(w, http.StatusBadRequest, "album is not linked to a provider")
		return
	}
	if v, ok := s.albumSync.Load(id); ok && v.(*models.SyncInfo).Active {
		writeError(w, http.StatusConflict, "album refresh already running")
		return
	}

	providerName, albumID, title := album.Provider, album.ID, album.Title
	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		defer s.finishAlbumSync(albumID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		s.setAlbumSync(albumID, &models.SyncInfo{Active: true, Phase: "Fetching tracklist from provider"})
		detail, err := s.providers.GetAlbum(ctx, providerName, album.TracklistID())
		if err != nil {
			slog.Error("refresh: tracklist fetch failed", "album", title, "provider", providerName, "error", err)
			return
		}

		trackStatus := models.TrackStatusWanted
		if album.Status == models.AlbumStatusIgnored {
			trackStatus = models.TrackStatusIgnored
		}
		s.setAlbumSync(albumID, &models.SyncInfo{Active: true, Phase: "Syncing tracks", Total: len(detail.Tracks)})
		added, matched, merged, _ := s.foldAlbumTracks(providerName, albumID, title, trackStatus, detail, false)

		s.setAlbumSync(albumID, &models.SyncInfo{Active: true, Phase: "Resolving cover art"})
		s.enrichAlbumCover(ctx, albumID)

		slog.Info("refresh: album refreshed", "album", title, "provider", providerName,
			"tracks", len(detail.Tracks), "added", added, "matched", matched, "merged", merged)
		s.activityLog.Record("album_refresh", "album", albumID, fmt.Sprintf(
			"Refreshed %s from %s — %d track(s), %d added", title, providerName, len(detail.Tracks), added))
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleSetAlbumEdition pins an album to a specific provider release (or back
// to the release-group default with null), then folds the album's tracks onto
// that edition's tracklist. Owned tracks re-anchor by recording id where
// possible, so a different pressing of the same songs keeps files owned; rows
// unique to the old edition are pruned unless they hold a file.
func (s *Server) handleSetAlbumEdition(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		ReleaseID *string `json:"release_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	album, err := s.queries.GetAlbum(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "album not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get album")
		return
	}
	if album.Provider == "" || album.Provider == provider.LocalProvider {
		writeError(w, http.StatusBadRequest, "album is not linked to a provider")
		return
	}

	if err := s.queries.SetAlbumReleaseID(album.ID, req.ReleaseID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save edition")
		return
	}

	// Fetch the chosen edition's tracklist (or the default pick when cleared)
	// and fold it into the album's rows.
	trackStatus := models.TrackStatusWanted
	if album.Status == models.AlbumStatusIgnored {
		trackStatus = models.TrackStatusIgnored
	}
	album.ReleaseID = req.ReleaseID
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	detail, err := s.providers.GetAlbum(ctx, album.Provider, album.TracklistID())
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider fetch failed")
		return
	}
	added, matched, merged, pruned := s.foldAlbumTracks(album.Provider, album.ID, album.Title, trackStatus, detail, true)
	s.enrichAlbumCover(ctx, album.ID) // pinned edition gets its own cover art

	slog.Info("edition: switched", "album", album.Title, "release_id", req.ReleaseID,
		"matched", matched, "added", added, "merged", merged, "pruned", pruned)
	s.activityLog.Record("album_edition", "album", album.ID, fmt.Sprintf(
		"Switched %s to a different release — %d track(s) matched, %d added, %d removed", album.Title, matched+merged, added, pruned))

	writeJSON(w, http.StatusOK, map[string]any{
		"release_id": req.ReleaseID,
		"matched":    matched + merged,
		"added":      added,
		"pruned":     pruned,
	})
}

// handleSplitTracks moves a user-selected set of tracks off this album into
// an album keyed by a specific release id — for users who own multiple
// editions (e.g. standard + instrumental) as distinct library entries. The
// target album is created when needed and keyed by the release id (not the
// release-group), so release-group syncs never merge it back; the release's
// remaining tracklist is folded in so the target is complete.
func (s *Server) handleSplitTracks(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		ReleaseID string  `json:"release_id"`
		TrackIDs  []int64 `json:"track_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ReleaseID == "" || len(req.TrackIDs) == 0 {
		writeError(w, http.StatusBadRequest, "release_id and track_ids are required")
		return
	}
	album, err := s.queries.GetAlbum(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "album not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get album")
		return
	}
	if album.Provider == "" || album.Provider == provider.LocalProvider {
		writeError(w, http.StatusBadRequest, "album is not linked to a provider")
		return
	}
	if album.ProviderID == req.ReleaseID {
		writeError(w, http.StatusConflict, "album is already that release")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	detail, err := s.providers.GetAlbum(ctx, album.Provider, req.ReleaseID)
	if err != nil || detail == nil {
		writeError(w, http.StatusBadGateway, "provider fetch failed")
		return
	}

	target, err := s.queries.FindAlbumByProvider(album.Provider, req.ReleaseID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to look up release album")
		return
	}
	if target == nil {
		title := detail.Title
		if title == "" {
			title = album.Title
		}
		year := int(detail.Year)
		if year == 0 && album.Year != nil {
			year = *album.Year
		}
		target = &models.Album{
			ArtistID:   album.ArtistID,
			Title:      title,
			Year:       intPtrOrNil(year),
			Provider:   album.Provider,
			ProviderID: req.ReleaseID,
			ReleaseID:  &req.ReleaseID,
			CoverURL:   strPtrOrNil(detail.CoverUrl),
			RecordType: album.RecordType,
			Status:     album.Status,
		}
		if err := s.queries.CreateAlbum(target); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create album")
			return
		}
	}

	// Move only the selected rows; relink each to the matching release-track
	// entry (provider id → recording id → guarded title) so later folds and
	// edition refreshes stay anchored.
	tracks, err := s.queries.ListTracksByAlbum(album.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tracks")
		return
	}
	want := make(map[int64]bool, len(req.TrackIDs))
	for _, tid := range req.TrackIDs {
		want[tid] = true
	}
	moved, relocated := 0, 0
	for i := range tracks {
		t := &tracks[i]
		if !want[t.ID] {
			continue
		}
		pt := matchProviderTrack(t, detail.Tracks)
		if pt != nil {
			if t.Provider != album.Provider || t.ProviderID != pt.Id {
				if err := s.queries.RelinkTrack(t.ID, album.Provider, pt.Id); err != nil {
					slog.Error("split: relink track failed", "album", target.Title, "track", t.Title, "error", err)
				}
			}
			// Adopt the release's listing so the row — and the file naming
			// below — matches the edition it now lives on.
			if err := s.queries.UpdateTrackListing(t.ID, pt.Title, int(pt.TrackNumber), int(pt.DiscNumber)); err != nil {
				slog.Error("split: relist track failed", "album", target.Title, "track", t.Title, "error", err)
			}
			if recID := pt.Metadata["recording_id"]; recID != "" {
				_ = s.queries.SetTrackMBRecordingID(t.ID, recID)
			}
			t.Title = pt.Title
			t.TrackNumber = int(pt.TrackNumber)
			t.DiscNumber = int(pt.DiscNumber)
			t.Provider = album.Provider
			t.ProviderID = pt.Id
		} else if t.Provider != provider.LocalProvider {
			// No entry on the target release — keeping the old release-track
			// id would block the source from restoring its own tracklist
			// (provider+id is globally unique). Rekey as a local row; the UI's
			// link affordance can anchor it later.
			if err := s.queries.RelinkTrack(t.ID, provider.LocalProvider, fmt.Sprintf("loc-split-%d", t.ID)); err != nil {
				slog.Error("split: local rekey failed", "album", target.Title, "track", t.Title, "error", err)
			} else {
				t.Provider = provider.LocalProvider
				t.ProviderID = fmt.Sprintf("loc-split-%d", t.ID)
			}
		}
		if err := s.queries.UpdateTrackAlbum(t.ID, target.ID); err != nil {
			slog.Error("split: move track failed", "album", target.Title, "track", t.Title, "error", err)
			continue
		}
		t.AlbumID = target.ID
		// The bytes follow the row: re-home the file under the target album
		// via the naming template and retag its album identity.
		if t.FilePath != nil && *t.FilePath != "" && s.organizer != nil {
			if err := s.organizer.Relocate(t); err != nil {
				slog.Warn("split: file relocation failed", "track", t.Title, "error", err)
			} else {
				relocated++
			}
		}
		moved++
	}

	// Fill out the rest of the release tracklist so the target album is a
	// complete edition — moved rows anchor by their release-track ids.
	trackStatus := models.TrackStatusWanted
	if target.Status == models.AlbumStatusIgnored {
		trackStatus = models.TrackStatusIgnored
	}
	added, _, _, _ := s.foldAlbumTracks(album.Provider, target.ID, target.Title, trackStatus, detail, false)
	s.enrichAlbumCover(ctx, target.ID)

	// Re-fold the source album's own tracklist so positions vacated by moved
	// rows are recreated — the source edition keeps its full listing.
	restored := 0
	if srcDetail, err := s.providers.GetAlbum(ctx, album.Provider, album.TracklistID()); err == nil && srcDetail != nil {
		srcStatus := models.TrackStatusWanted
		if album.Status == models.AlbumStatusIgnored {
			srcStatus = models.TrackStatusIgnored
		}
		restored, _, _, _ = s.foldAlbumTracks(album.Provider, album.ID, album.Title, srcStatus, srcDetail, false)
	} else if err != nil {
		slog.Warn("split: source tracklist refresh failed", "album", album.Title, "error", err)
	}

	slog.Info("split: moved tracks to release album", "album", album.Title, "release_id", req.ReleaseID,
		"target", target.ID, "moved", moved, "relocated", relocated, "added", added, "restored", restored)
	s.activityLog.Record("album_split", "album", target.ID, fmt.Sprintf(
		"Split %d track(s) from %s into %s — %d file(s) relocated, %d added, %d restored",
		moved, album.Title, target.Title, relocated, added, restored))

	writeJSON(w, http.StatusOK, map[string]any{
		"album_id":  target.ID,
		"moved":     moved,
		"relocated": relocated,
		"added":     added,
		"restored":  restored,
	})
}

// matchProviderTrack finds a track's entry in a release tracklist: provider
// track id first, then recording id, then title+position — the last only when
// the row's recording id can't prove it belongs to a different edition.
func matchProviderTrack(t *models.Track, pts []*pb.TrackInfo) *pb.TrackInfo {
	for _, pt := range pts {
		if t.ProviderID == pt.Id {
			return pt
		}
	}
	if t.MBRecordingID != nil && *t.MBRecordingID != "" {
		for _, pt := range pts {
			if pt.Metadata["recording_id"] == *t.MBRecordingID {
				return pt
			}
		}
	}
	key := fmt.Sprintf("%d|%d|%s", t.DiscNumber, t.TrackNumber, strings.ToLower(strings.TrimSpace(t.Title)))
	for _, pt := range pts {
		if key != fmt.Sprintf("%d|%d|%s", int(pt.DiscNumber), int(pt.TrackNumber), strings.ToLower(strings.TrimSpace(pt.Title))) {
			continue
		}
		recID := pt.Metadata["recording_id"]
		if recID == "" || t.MBRecordingID == nil || *t.MBRecordingID == "" || *t.MBRecordingID == recID {
			return pt
		}
	}
	return nil
}

var mbIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseMusicBrainzLink extracts a release or release-group MBID from a
// MusicBrainz URL. Bare MBIDs are accepted too — the provider tries the id as
// a release-group first, then as a release.
func parseMusicBrainzLink(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if mbIDRe.MatchString(raw) {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Host != "musicbrainz.org" && u.Host != "www.musicbrainz.org") {
		return "", errors.New("expected a musicbrainz.org release or release-group link")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || (parts[0] != "release" && parts[0] != "release-group") || !mbIDRe.MatchString(parts[1]) {
		return "", errors.New("expected a musicbrainz.org/release/… or /release-group/… link")
	}
	return parts[1], nil
}

// handleAddArtistRelease manually adds one release to an artist from a
// MusicBrainz link — covering releases the artist's discography doesn't list
// (VA appearances, wrong-artist-grouped entries, orphaned editions). A
// release-group link lands as a normal album; a release link lands
// release-keyed like a split target, so it stays a distinct edition even when
// its release-group is already in the library. Explicitly added releases are
// watched regardless of type — the user asked for them.
func (s *Server) handleAddArtistRelease(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	artist, err := s.queries.GetArtist(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "artist not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get artist")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	mbid, err := parseMusicBrainzLink(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	const providerName = "musicbrainz"
	if !s.providers.IsHealthy(providerName) {
		writeError(w, http.StatusServiceUnavailable, "musicbrainz provider is not available")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	detail, err := s.providers.GetAlbum(ctx, providerName, mbid)
	if err != nil || detail == nil {
		writeError(w, http.StatusBadGateway, "release not found on MusicBrainz")
		return
	}

	if existing, _ := s.queries.FindAlbumByProvider(providerName, detail.Id); existing != nil {
		writeJSON(w, http.StatusOK, map[string]any{"album_id": existing.ID, "existed": true})
		return
	}

	recordType := detail.Metadata["record_type"]
	if recordType == "" {
		recordType = "album"
	}
	album := &models.Album{
		ArtistID:   artist.ID,
		Title:      detail.Title,
		Year:       intPtrOrNil(int(detail.Year)),
		Provider:   providerName,
		ProviderID: detail.Id,
		CoverURL:   strPtrOrNil(detail.CoverUrl),
		RecordType: recordType,
		Status:     models.AlbumStatusWatched,
	}
	// A release link is keyed by the release id itself (like a split target)
	// so it can coexist with the release-group's own album.
	if relID := detail.Metadata["release_id"]; relID != "" {
		album.ReleaseID = &relID
	}
	if err := s.queries.CreateAlbum(album); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save album")
		return
	}

	added, _, _, _ := s.foldAlbumTracks(providerName, album.ID, album.Title, models.TrackStatusWanted, detail, false)

	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		// Detached from the request: this work must outlive the response.
		s.enrichAlbumCover(context.WithoutCancel(r.Context()), album.ID)
	}()

	slog.Info("release added manually", "artist", artist.Name, "album", album.Title, "mbid", mbid, "tracks", added)
	s.activityLog.Record("release_add", "album", album.ID, fmt.Sprintf(
		"Added %s to %s from a MusicBrainz link — %d track(s)", album.Title, artist.Name, added))

	writeJSON(w, http.StatusCreated, map[string]any{"album_id": album.ID})
}
