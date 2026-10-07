package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
