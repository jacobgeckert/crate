package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
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

// handleSplitAlbumEdition creates a separate album for one release inside the
// album's release-group and moves matching track rows over — for users who
// own multiple editions (e.g. standard + instrumental) as distinct library
// entries. The new album is keyed by the release id (not the release-group),
// so release-group syncs never merge it back.
func (s *Server) handleSplitAlbumEdition(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		ReleaseID string `json:"release_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ReleaseID == "" {
		writeError(w, http.StatusBadRequest, "release_id is required")
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
	if (album.ReleaseID != nil && *album.ReleaseID == req.ReleaseID) || album.ProviderID == req.ReleaseID {
		writeError(w, http.StatusConflict, "album is already pinned to that release")
		return
	}
	if existing, _ := s.queries.FindAlbumByProvider(album.Provider, req.ReleaseID); existing != nil {
		writeError(w, http.StatusConflict, "that release already exists as an album")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	detail, err := s.providers.GetAlbum(ctx, album.Provider, req.ReleaseID)
	if err != nil || detail == nil {
		writeError(w, http.StatusBadGateway, "provider fetch failed")
		return
	}

	trackStatus := models.TrackStatusWanted
	if album.Status == models.AlbumStatusIgnored {
		trackStatus = models.TrackStatusIgnored
	}
	title := detail.Title
	if title == "" {
		title = album.Title
	}
	year := int(detail.Year)
	if year == 0 && album.Year != nil {
		year = *album.Year
	}
	newAlbum := &models.Album{
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
	if err := s.queries.CreateAlbum(newAlbum); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create album")
		return
	}

	// Move matching track rows off the source album: provider track id, then
	// recording id, then title+position (only when a recording id can't
	// prove it belongs to a different edition).
	tracks, err := s.queries.ListTracksByAlbum(album.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tracks")
		return
	}
	byProvider := make(map[string]*models.Track)
	byRecording := make(map[string]*models.Track)
	byTitle := make(map[string]*models.Track)
	for i := range tracks {
		t := &tracks[i]
		if t.Provider == album.Provider {
			byProvider[t.ProviderID] = t
		}
		if t.MBRecordingID != nil && *t.MBRecordingID != "" {
			byRecording[*t.MBRecordingID] = t
		}
		key := fmt.Sprintf("%d|%d|%s", t.DiscNumber, t.TrackNumber, strings.ToLower(strings.TrimSpace(t.Title)))
		if _, ok := byTitle[key]; !ok {
			byTitle[key] = t
		}
	}
	used := make(map[int64]bool)
	moved, added := 0, 0
	for _, pt := range detail.Tracks {
		recID := pt.Metadata["recording_id"]
		var match *models.Track
		if t := byProvider[pt.Id]; t != nil && !used[t.ID] {
			match = t
		}
		if match == nil && recID != "" {
			if t := byRecording[recID]; t != nil && !used[t.ID] {
				match = t
			}
		}
		if match == nil {
			key := fmt.Sprintf("%d|%d|%s", int(pt.DiscNumber), int(pt.TrackNumber), strings.ToLower(strings.TrimSpace(pt.Title)))
			if t := byTitle[key]; t != nil && !used[t.ID] {
				// Only when the row's recording id can't prove it belongs to
				// a different edition.
				if recID == "" || t.MBRecordingID == nil || *t.MBRecordingID == "" || *t.MBRecordingID == recID {
					match = t
				}
			}
		}
		if match != nil {
			used[match.ID] = true
			if match.Provider != album.Provider || match.ProviderID != pt.Id {
				if err := s.queries.RelinkTrack(match.ID, album.Provider, pt.Id); err != nil {
					slog.Error("split: relink track failed", "album", title, "track", pt.Title, "error", err)
				}
			}
			if err := s.queries.UpdateTrackAlbum(match.ID, newAlbum.ID); err != nil {
				slog.Error("split: move track failed", "album", title, "track", pt.Title, "error", err)
			} else {
				moved++
			}
			continue
		}
		if err := s.queries.CreateTrack(&models.Track{
			AlbumID:       newAlbum.ID,
			Title:         pt.Title,
			TrackNumber:   int(pt.TrackNumber),
			DiscNumber:    int(pt.DiscNumber),
			DurationMs:    int(pt.DurationMs),
			Provider:      album.Provider,
			ProviderID:    pt.Id,
			MBRecordingID: strPtrOrNil(recID),
			Status:        trackStatus,
		}); err != nil {
			slog.Error("split: create track failed", "album", title, "track", pt.Title, "error", err)
		} else {
			added++
		}
	}
	s.enrichAlbumCover(ctx, newAlbum.ID)

	slog.Info("edition: split into separate album", "album", album.Title, "release_id", req.ReleaseID,
		"new_album", newAlbum.ID, "moved", moved, "added", added)
	s.activityLog.Record("album_split", "album", newAlbum.ID, fmt.Sprintf(
		"Split %s into a separate album — %d track(s) moved, %d added", title, moved, added))

	writeJSON(w, http.StatusOK, map[string]any{
		"album_id": newAlbum.ID,
		"moved":    moved,
		"added":    added,
	})
}
