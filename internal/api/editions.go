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
	if editions == nil {
		editions = []albumEdition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"editions": editions,
		"current":  album.ReleaseID,
	})
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
