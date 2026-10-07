package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/library"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/naming"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
	"github.com/TheOutdoorProgrammer/crate/internal/services/reject"
	pb "github.com/TheOutdoorProgrammer/crate/proto/provider"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// Status

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	artists, _ := s.queries.ListArtists()
	downloads, _ := s.queries.ListDownloads("")

	pending := 0
	active := 0
	for _, d := range downloads {
		switch d.Status {
		case "pending":
			pending++
		case "searching", "downloading":
			active++
		}
	}

	totalTracks := 0
	ownedTracks := 0
	for _, a := range artists {
		totalTracks += a.TotalTracks
		ownedTracks += a.OwnedTracks
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"version":           s.version,
		"artists_count":     len(artists),
		"total_tracks":      totalTracks,
		"owned_tracks":      ownedTracks,
		"pending_downloads": pending,
		"active_downloads":  active,
	})
}

// Search

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}

	var limit, offset int32 = 25, 0
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		fmt.Sscanf(o, "%d", &offset)
	}

	providerName := r.URL.Query().Get("provider")
	var result *provider.SearchResult
	var err error
	if providerName != "" {
		result, err = s.providers.SearchWithProvider(r.Context(), providerName, query, limit, offset)
	} else {
		result, err = s.providers.Search(r.Context(), query, limit, offset)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleLibrarySearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	results, err := s.queries.SearchLibrary(query, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "library search failed")
		return
	}
	if results == nil {
		results = []db.LibrarySearchResult{}
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handleBrowseArtistTrackSearch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"tracks": []any{}})
		return
	}
	providerName := r.URL.Query().Get("provider")
	if providerName == "" {
		providerName = s.providers.Primary()
	}
	result, err := s.providers.SearchArtistTracks(r.Context(), providerName, id, query, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "track search failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Browse

func (s *Server) handleBrowseArtist(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	primary := r.URL.Query().Get("provider")
	if primary == "" {
		primary = s.providers.Primary()
	}

	artist, err := s.providers.GetArtist(r.Context(), primary, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to browse artist")
		return
	}

	albums, err := s.providers.GetArtistAlbums(r.Context(), primary, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get artist albums")
		return
	}

	watchedAlbumIDs := []string{}
	existing, _ := s.queries.FindArtistByProvider(primary, id)
	if existing != nil {
		dbAlbums, _ := s.queries.ListAlbumsByArtist(existing.ID)
		for _, a := range dbAlbums {
			watchedAlbumIDs = append(watchedAlbumIDs, a.ProviderID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                artist.Id,
		"name":              artist.Name,
		"image_url":         artist.ImageUrl,
		"metadata":          artist.Metadata,
		"album_count":       len(albums.Albums),
		"albums":            albums.Albums,
		"watched_album_ids": watchedAlbumIDs,
		"artist_watched":    existing != nil && existing.Status == models.ArtistStatusWatched,
	})
}

func (s *Server) handleBrowseAlbum(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	primary := r.URL.Query().Get("provider")
	if primary == "" {
		primary = s.providers.Primary()
	}

	album, err := s.providers.GetAlbum(r.Context(), primary, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to browse album")
		return
	}

	existingAlbum, _ := s.queries.FindAlbumByProvider(primary, id)
	watchedTrackIDs := []string{}
	if existingAlbum != nil {
		tracks, _ := s.queries.ListTracksByAlbum(existingAlbum.ID)
		for _, t := range tracks {
			watchedTrackIDs = append(watchedTrackIDs, t.ProviderID)
		}
	}

	albumWatched := existingAlbum != nil && len(album.Tracks) > 0 && len(watchedTrackIDs) >= len(album.Tracks)

	var libraryID *int64
	if existingAlbum != nil {
		libraryID = &existingAlbum.ID
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                album.Id,
		"title":             album.Title,
		"artist_name":       album.ArtistName,
		"year":              album.Year,
		"cover_url":         album.CoverUrl,
		"tracks":            album.Tracks,
		"metadata":          album.Metadata,
		"album_watched":     albumWatched,
		"watched_track_ids": watchedTrackIDs,
		"library_id":        libraryID,
	})
}

// Watch

func (s *Server) handleWatchArtist(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "id")

	var req struct {
		WatchNewReleases bool   `json:"watch_new_releases"`
		Provider         string `json:"provider,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	primary := req.Provider
	if primary == "" {
		primary = s.providers.Primary()
	}

	existing, _ := s.queries.FindArtistByProvider(primary, providerID)

	artistDetail, err := s.providers.GetArtist(r.Context(), primary, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch artist")
		return
	}

	albumList, err := s.providers.GetArtistAlbums(r.Context(), primary, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch artist albums")
		return
	}

	if existing != nil {
		if err := s.queries.UpdateArtistStatus(existing.ID, models.ArtistStatusWatched); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update artist status")
			return
		}
		if req.WatchNewReleases {
			if err := s.queries.SetArtistWatchNewReleases(existing.ID, true); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to update new releases setting")
				return
			}
		}

		albums := albumList.Albums
		s.bgWork.Add(1)
		go func() {
			defer s.bgWork.Done()
			s.enrichArtistImage(context.Background(), existing.ID)
			s.saveAlbumsFromProvider(primary, existing.ID, albums)
		}()

		writeJSON(w, http.StatusOK, existing)
		return
	}

	imgURL := artistDetail.ImageUrl
	artist := &models.Artist{
		Name:       artistDetail.Name,
		Provider:   primary,
		ProviderID: providerID,
		ImageURL:   strPtrOrNil(imgURL),
		Status:     models.ArtistStatusWatched,
	}
	if req.WatchNewReleases {
		artist.WatchNewReleases = true
		ts := time.Now().UTC().Format(time.RFC3339)
		artist.WatchNewReleasesSince = &ts
	}
	if err := s.queries.CreateArtist(artist); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save artist")
		return
	}

	albums := albumList.Albums
	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		s.enrichArtistImage(context.Background(), artist.ID)
		s.saveAlbumsFromProvider(primary, artist.ID, albums)
	}()

	writeJSON(w, http.StatusCreated, artist)
}

func (s *Server) saveAlbumsFromProvider(providerName string, artistID int64, albums []*pb.AlbumSummary) {
	ctx := context.Background()
	name := fmt.Sprintf("artist %d", artistID)
	var artist *models.Artist
	if a, err := s.queries.GetArtist(artistID); err == nil && a != nil {
		artist = a
		name = a.Name
	}
	watch := s.newReleaseWatchFor(artist)
	slog.Info("sync: saving watched discography", "artist", name, "provider", providerName, "releases", len(albums))
	added, ignored := 0, 0
	for _, pa := range albums {
		if existingAlbum, _ := s.queries.FindAlbumByProvider(providerName, pa.Id); existingAlbum != nil {
			if d := pa.Metadata["release_date"]; d != "" {
				s.queries.BackfillAlbumReleaseDate(existingAlbum.ID, d)
			}
			// A release reclassified upstream (e.g. gained a secondary type)
			// re-sorts under its new section on the next sync.
			if pa.RecordType != "" && existingAlbum.RecordType != pa.RecordType {
				s.queries.SetAlbumRecordType(existingAlbum.ID, pa.RecordType)
			}
			s.syncAlbumTracks(ctx, providerName, existingAlbum)
			s.enrichAlbumCover(ctx, existingAlbum.ID)
			s.demoteUnownedAlbum(existingAlbum)
			continue
		}
		st := s.saveAlbumFromProvider(ctx, providerName, artistID, pa, watch)
		if st == "" {
			continue
		}
		added++
		if st == models.AlbumStatusIgnored {
			ignored++
		}
	}
	slog.Info("sync: discography saved", "artist", name, "provider", providerName, "added", added, "ignored", ignored)
}

func (s *Server) syncAlbumTracks(ctx context.Context, providerName string, album *models.Album) {
	// A pinned edition overrides the release-group's default tracklist.
	albumDetail, err := s.providers.GetAlbum(ctx, providerName, album.TracklistID())
	if err != nil {
		slog.Error("sync: failed to fetch release tracklist", "album", album.Title, "provider", providerName, "error", err)
		return
	}
	// Tracks created under an ignored album stay ignored — a wanted track
	// there would still get auto-queued.
	trackStatus := models.TrackStatusWanted
	if album.Status == models.AlbumStatusIgnored {
		trackStatus = models.TrackStatusIgnored
	}
	added := 0
	for _, pt := range albumDetail.Tracks {
		if t, err := s.queries.FindTrackByProvider(providerName, pt.Id); err == nil {
			// Backfill the recording id onto rows created before providers
			// exposed it.
			if recID := pt.Metadata["recording_id"]; recID != "" {
				_ = s.queries.SetTrackMBRecordingID(t.ID, recID)
			}
			continue
		}
		s.queries.CreateTrack(&models.Track{
			AlbumID:       album.ID,
			Title:         pt.Title,
			TrackNumber:   int(pt.TrackNumber),
			DiscNumber:    int(pt.DiscNumber),
			DurationMs:    int(pt.DurationMs),
			Provider:      providerName,
			ProviderID:    pt.Id,
			MBRecordingID: strPtrOrNil(pt.Metadata["recording_id"]),
			Status:        trackStatus,
		})
		added++
	}
	if added > 0 {
		slog.Info("sync: release gained new tracks", "album", album.Title, "provider", providerName, "tracks_added", added)
	}
}

// saveAlbumFromProvider creates a provider release. Newly discovered albums
// default to ignored — only releases qualifying as watched new releases (the
// same gate the scheduler's detectNewReleases applies) land wanted. Returns
// the created album's status, or "" when creation failed.
func (s *Server) saveAlbumFromProvider(ctx context.Context, providerName string, artistID int64, pa *pb.AlbumSummary, watch *newReleaseWatch) models.AlbumStatus {
	year := intPtrOrNil(int(pa.Year))
	cover := pa.CoverUrl
	albumStatus := models.AlbumStatusIgnored
	trackStatus := models.TrackStatusIgnored
	if watch != nil && watch.qualifies(pa) {
		albumStatus = models.AlbumStatusWatched
		trackStatus = models.TrackStatusWanted
	}
	album := &models.Album{
		ArtistID:    artistID,
		Title:       pa.Title,
		Year:        year,
		Provider:    providerName,
		ProviderID:  pa.Id,
		CoverURL:    strPtrOrNil(cover),
		RecordType:  pa.RecordType,
		ReleaseDate: strPtrOrNil(pa.Metadata["release_date"]),
		Status:      albumStatus,
	}
	if err := s.queries.CreateAlbum(album); err != nil {
		slog.Error("sync: failed to create release", "album", pa.Title, "provider", providerName, "error", err)
		return ""
	}

	albumDetail, err := s.providers.GetAlbum(ctx, providerName, pa.Id)
	if err != nil {
		slog.Error("sync: failed to fetch tracklist, release saved without tracks", "album", pa.Title, "provider", providerName, "error", err)
		return albumStatus
	}
	tracks := 0
	for _, pt := range albumDetail.Tracks {
		if err := s.queries.CreateTrack(&models.Track{
			AlbumID:       album.ID,
			Title:         pt.Title,
			TrackNumber:   int(pt.TrackNumber),
			DiscNumber:    int(pt.DiscNumber),
			DurationMs:    int(pt.DurationMs),
			Provider:      providerName,
			ProviderID:    pt.Id,
			MBRecordingID: strPtrOrNil(pt.Metadata["recording_id"]),
			Status:        trackStatus,
		}); err == nil {
			tracks++
		}
	}
	s.enrichAlbumCover(ctx, album.ID)
	slog.Info("sync: added release", "album", pa.Title, "type", pa.RecordType, "provider", providerName, "tracks", tracks, "status", albumStatus)
	return albumStatus
}

func (s *Server) handleWatchAlbum(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "id")

	var req struct {
		ArtistProviderID string `json:"artist_provider_id"`
		ArtistName       string `json:"artist_name"`
		ArtistImageURL   string `json:"artist_image_url"`
		Provider         string `json:"provider,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	primary := req.Provider
	if primary == "" {
		primary = s.providers.Primary()
	}

	artist, err := s.queries.FindArtistByProvider(primary, req.ArtistProviderID)
	if errors.Is(err, sql.ErrNoRows) || artist == nil {
		artist = &models.Artist{
			Name:       req.ArtistName,
			Provider:   primary,
			ProviderID: req.ArtistProviderID,
			ImageURL:   strPtrOrNil(req.ArtistImageURL),
			Status:     models.ArtistStatusPartial,
		}
		if err := s.queries.CreateArtist(artist); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save artist")
			return
		}
	}

	existingAlbum, _ := s.queries.FindAlbumByProvider(primary, providerID)
	if existingAlbum != nil {
		writeJSON(w, http.StatusOK, existingAlbum)
		return
	}

	albumDetail, err := s.providers.GetAlbum(r.Context(), primary, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch album")
		return
	}

	year := intPtrOrNil(int(albumDetail.Year))
	cover := albumDetail.CoverUrl
	album := &models.Album{
		ArtistID:   artist.ID,
		Title:      albumDetail.Title,
		Year:       year,
		Provider:   primary,
		ProviderID: albumDetail.Id,
		CoverURL:   strPtrOrNil(cover),
		RecordType: "album",
		Status:     models.AlbumStatusWatched,
	}
	if err := s.queries.CreateAlbum(album); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save album")
		return
	}

	for _, pt := range albumDetail.Tracks {
		s.queries.CreateTrack(&models.Track{
			AlbumID:     album.ID,
			Title:       pt.Title,
			TrackNumber: int(pt.TrackNumber),
			DiscNumber:  int(pt.DiscNumber),
			DurationMs:  int(pt.DurationMs),
			Provider:    primary,
			ProviderID:  pt.Id,
			Status:      models.TrackStatusWanted,
		})
	}

	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		s.enrichAlbumCover(context.Background(), album.ID)
	}()

	writeJSON(w, http.StatusCreated, album)
}

func (s *Server) handleWatchTrack(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "id")

	var req struct {
		ArtistProviderID string `json:"artist_provider_id"`
		ArtistName       string `json:"artist_name"`
		ArtistImageURL   string `json:"artist_image_url"`
		AlbumProviderID  string `json:"album_provider_id"`
		AlbumTitle       string `json:"album_title"`
		AlbumCoverURL    string `json:"album_cover_url"`
		AlbumYear        *int   `json:"album_year"`
		Title            string `json:"title"`
		TrackNumber      int    `json:"track_number"`
		DiscNumber       int    `json:"disc_number"`
		DurationMs       int    `json:"duration_ms"`
		Provider         string `json:"provider,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	primary := req.Provider
	if primary == "" {
		primary = s.providers.Primary()
	}

	artist, err := s.queries.FindArtistByProvider(primary, req.ArtistProviderID)
	if errors.Is(err, sql.ErrNoRows) || artist == nil {
		artist = &models.Artist{
			Name:       req.ArtistName,
			Provider:   primary,
			ProviderID: req.ArtistProviderID,
			ImageURL:   strPtrOrNil(req.ArtistImageURL),
			Status:     models.ArtistStatusPartial,
		}
		if err := s.queries.CreateArtist(artist); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save artist")
			return
		}
	}

	album, err := s.queries.FindAlbumByProvider(primary, req.AlbumProviderID)
	if errors.Is(err, sql.ErrNoRows) || album == nil {
		album = &models.Album{
			ArtistID:   artist.ID,
			Title:      req.AlbumTitle,
			Year:       req.AlbumYear,
			Provider:   primary,
			ProviderID: req.AlbumProviderID,
			CoverURL:   strPtrOrNil(req.AlbumCoverURL),
			RecordType: "album",
			Status:     models.AlbumStatusWatched,
		}
		if err := s.queries.CreateAlbum(album); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save album")
			return
		}
	}

	track := &models.Track{
		AlbumID:     album.ID,
		Title:       req.Title,
		TrackNumber: req.TrackNumber,
		DiscNumber:  req.DiscNumber,
		DurationMs:  req.DurationMs,
		Provider:    primary,
		ProviderID:  providerID,
		Status:      models.TrackStatusWanted,
	}
	if err := s.queries.CreateTrack(track); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save track")
		return
	}

	writeJSON(w, http.StatusCreated, track)
}

// Library

func (s *Server) handleListArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := s.queries.ListArtists()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list artists")
		return
	}
	if artists == nil {
		writeJSON(w, http.StatusOK, []struct{}{})
		return
	}

	healthCache := make(map[string]bool)
	for i := range artists {
		p := artists[i].Provider
		if _, ok := healthCache[p]; !ok {
			healthCache[p] = s.providers.IsHealthy(p)
		}
		artists[i].Orphaned = !healthCache[p]
	}

	writeJSON(w, http.StatusOK, artists)
}

func (s *Server) handleGetArtist(w http.ResponseWriter, r *http.Request) {
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

	albums, _ := s.queries.ListAlbumsByArtist(id)
	for i := range albums {
		tracks, _ := s.queries.ListTracksByAlbum(albums[i].ID)
		albums[i].Tracks = tracks
	}
	artist.Albums = albums
	artist.Orphaned = !s.providers.IsHealthy(artist.Provider)
	if v, ok := s.syncStatus.Load(id); ok {
		artist.Sync = v.(*models.SyncInfo)
	}

	writeJSON(w, http.StatusOK, artist)
}

func (s *Server) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
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

	tracks, _ := s.queries.ListTracksByAlbum(id)
	album.Tracks = tracks
	if v, ok := s.albumSync.Load(id); ok {
		album.Sync = v.(*models.SyncInfo)
	}

	writeJSON(w, http.StatusOK, album)
}

// Unwatch

func (s *Server) handleToggleNewReleases(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.queries.SetArtistWatchNewReleases(id, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update artist")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"watch_new_releases": req.Enabled})
}

// handleBulkNewReleases applies the new-release watch toggle to many artists
// at once, using the same per-artist semantics as handleToggleNewReleases.
func (s *Server) handleBulkNewReleases(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs     []int64 `json:"ids"`
		Enabled bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 500 {
		writeError(w, http.StatusBadRequest, "ids must contain 1-500 entries")
		return
	}

	updated := 0
	for _, id := range req.IDs {
		if err := s.queries.SetArtistWatchNewReleases(id, req.Enabled); err == nil {
			updated++
		}
	}

	writeJSON(w, http.StatusOK, map[string]int{"updated": updated})
}

// handleSetArtistReleaseTypes stores a per-artist override of which release
// types the new-release watcher tracks. {"types": {...}} sets the override;
// {"types": null} clears it so the artist inherits the global setting.
func (s *Server) handleSetArtistReleaseTypes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req struct {
		Types map[string]bool `json:"types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if _, err := s.queries.GetArtist(id); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "artist not found")
		return
	}
	if err := s.queries.SetArtistWatchReleaseTypes(id, req.Types); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update release types")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpcomingReleases lists releases dated today or later across all
// artists, soonest first — the Upcoming page.
func (s *Server) handleUpcomingReleases(w http.ResponseWriter, r *http.Request) {
	today := time.Now().UTC().Format("2006-01-02")
	albums, err := s.queries.ListUpcomingReleases(today)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list upcoming releases")
		return
	}
	writeJSON(w, http.StatusOK, albums)
}

func (s *Server) handleUnwatchArtist(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.DeleteArtist(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete artist")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnwatchAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.DeleteAlbum(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete album")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleIgnoreAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.UpdateAlbumStatus(id, models.AlbumStatusIgnored); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to ignore album")
		return
	}
	s.queries.UpdateTrackStatusByAlbum(id, models.TrackStatusWanted, models.TrackStatusIgnored)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
}

func (s *Server) handleUnignoreAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.UpdateAlbumStatus(id, models.AlbumStatusWatched); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to unignore album")
		return
	}
	s.queries.UpdateTrackStatusByAlbum(id, models.TrackStatusIgnored, models.TrackStatusWanted)
	// Un-ignoring means "I want this" — download now rather than waiting for
	// the scheduler's next wanted-tracks sweep. Album-first search, per-track
	// fallback for whatever a single source can't cover.
	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		s.downloader.DownloadAlbum(context.Background(), id)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "watched"})
}

func (s *Server) handleIgnoreTrack(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.UpdateTrackStatus(id, models.TrackStatusIgnored); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to ignore track")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
}

func (s *Server) handleUnignoreTrack(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.UpdateTrackStatus(id, models.TrackStatusWanted); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to unignore track")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "wanted"})
}

func (s *Server) handleQueueTrack(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.EnqueueDownload(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to queue track")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (s *Server) handleQueueArtistTracks(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	tracks, err := s.queries.ListWantedTracksByArtist(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list wanted tracks")
		return
	}
	ids := make([]int64, len(tracks))
	for i, t := range tracks {
		ids[i] = t.ID
	}
	queued, err := s.queries.EnqueueDownloadBatch(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to queue tracks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": queued})
}

func (s *Server) handleQueueAlbumTracks(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	tracks, err := s.queries.ListWantedTracksByAlbum(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list wanted tracks")
		return
	}
	// Album-first: one slskd search for the whole album, single-source what it
	// can, per-track pipeline for the rest. Runs async — the slskd search
	// takes seconds.
	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		s.downloader.DownloadAlbum(context.Background(), id)
	}()
	writeJSON(w, http.StatusOK, map[string]int{"queued": len(tracks)})
}

func (s *Server) handleStartManualSearch(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	searchID, query, err := s.downloader.StartManualSearch(r.Context(), id, req.Query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"search_id": searchID, "track_id": id, "query": query})
}

func (s *Server) handlePollManualSearch(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	searchID := chi.URLParam(r, "searchId")
	if searchID == "" {
		writeError(w, http.StatusBadRequest, "missing search id")
		return
	}
	resp, err := s.downloader.PollManualSearch(r.Context(), id, searchID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "poll failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteManualSearch(w http.ResponseWriter, r *http.Request) {
	searchID := chi.URLParam(r, "searchId")
	if searchID == "" {
		writeError(w, http.StatusBadRequest, "missing search id")
		return
	}
	s.downloader.CleanupSearch(r.Context(), searchID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleManualDownload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Username string `json:"username"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
		BitRate  int    `json:"bit_rate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.downloader.ManualDownload(r.Context(), id, req.Username, req.Filename, req.Size, req.BitRate); err != nil {
		writeError(w, http.StatusInternalServerError, "download failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "downloading"})
}

func (s *Server) handleUnwatchTrack(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	if r.URL.Query().Get("delete") == "true" {
		track, err := s.queries.GetTrackWithMeta(id)
		if err == nil && track.Status == models.TrackStatusOwned {
			if track.FilePath != nil {
				library.DeleteFile(s.libraryDir, *track.FilePath)
			}
			s.triggerScanAsync()
		}
	}

	if err := s.queries.DeleteTrack(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete track")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Downloads

func (s *Server) handleDownloadProgress(w http.ResponseWriter, r *http.Request) {
	progress, err := s.downloader.GetProgress(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get progress")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	downloads, err := s.queries.ListDownloadsWithTrack(status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list downloads")
		return
	}
	if downloads == nil {
		downloads = []models.DownloadQueueItem{}
	}
	writeJSON(w, http.StatusOK, downloads)
}

func (s *Server) handleQueueDownloads(w http.ResponseWriter, r *http.Request) {
	tracks, err := s.queries.ListWantedTracks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list wanted tracks")
		return
	}
	ids := make([]int64, len(tracks))
	for i, t := range tracks {
		ids[i] = t.ID
	}
	queued, err := s.queries.EnqueueDownloadBatch(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to queue tracks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": queued})
}

func (s *Server) handleDeleteDownload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	dl, err := s.queries.GetDownload(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "download not found")
		return
	}
	s.downloader.CancelTransfer(r.Context(), dl)
	if err := s.queries.DeleteDownload(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete download")
		return
	}
	if dl.Status == models.DownloadStatusDownloading || dl.Status == models.DownloadStatusSearching || dl.Status == models.DownloadStatusPending {
		_ = s.queries.UpdateTrackStatus(dl.TrackID, models.TrackStatusWanted)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearDownloadsByStatus(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		writeError(w, http.StatusBadRequest, "status parameter required")
		return
	}
	allowed := map[string]bool{"failed": true, "complete": true, "pending": true, "downloading": true, "searching": true}
	if !allowed[status] {
		writeError(w, http.StatusBadRequest, "can only clear failed, complete, pending, downloading, or searching downloads")
		return
	}
	resetTrack := status == "downloading" || status == "searching" || status == "pending"
	if resetTrack {
		s.queries.ResetTrackStatusForDownloads(status)
	}
	deleted, err := s.queries.DeleteDownloadsByStatus(status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear downloads")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

func (s *Server) handleRetryDownload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.UpdateDownloadStatus(id, models.DownloadStatusPending, nil, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retry download")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
}

// Providers

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers := s.providers.ListProviders()
	writeJSON(w, http.StatusOK, providers)
}

func (s *Server) handleListActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	var offset int
	if o := r.URL.Query().Get("offset"); o != "" {
		fmt.Sscanf(o, "%d", &offset)
	}
	items, err := s.activityLog.List(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list activity")
		return
	}
	if items == nil {
		items = []models.ActivityLog{}
	}
	total, _ := s.activityLog.Count()
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": total,
	})
}

func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	s.cache.Clear()
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

func (s *Server) handleRelinkEntity(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req struct {
		ProviderID string `json:"provider_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	primary := s.providers.Primary()

	path := r.URL.Path
	var err2 error
	reconciling := false
	switch {
	case strings.Contains(path, "/relink/artist/"):
		// Capture the pre-relink provider: only promoting a local import runs the
		// discography reconcile. A real→real swap stays a plain re-stamp.
		prev, _ := s.queries.GetArtist(id)
		if err2 = s.queries.RelinkArtist(id, primary, req.ProviderID); err2 == nil &&
			prev != nil && prev.Provider == provider.LocalProvider {
			_ = s.queries.UpdateArtistStatus(id, models.ArtistStatusWatched)
			reconciling = true
			providerID := req.ProviderID
			s.bgWork.Add(1)
			go func() {
				defer s.bgWork.Done()
				s.reconcileLocalArtist(primary, id, providerID)
			}()
		}
	case strings.Contains(path, "/relink/album/"):
		if err2 = s.queries.RelinkAlbum(id, primary, req.ProviderID); err2 == nil &&
			s.albumHasLocalTracks(id) {
			reconciling = true
			albumID, providerID := id, req.ProviderID
			s.bgWork.Add(1)
			go func() {
				defer s.bgWork.Done()
				s.reconcileAlbumTracks(context.Background(), primary, albumID, providerID)
			}()
		}
	case strings.Contains(path, "/relink/track/"):
		// A track is a leaf — nothing to reconcile beneath it.
		err2 = s.queries.RelinkTrack(id, primary, req.ProviderID)
	default:
		writeError(w, http.StatusBadRequest, "invalid relink target")
		return
	}

	if err2 != nil {
		writeError(w, http.StatusInternalServerError, "failed to relink")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "relinked", "reconciling": reconciling})
}

// handleRefreshArtist re-syncs a provider-linked artist's discography: the same
// reconcile the local-import link path runs — new releases are created as
// wanted, linked albums gain missing tracks, stray local albums fold in.
// Cached provider responses are dropped first so this always reads fresh data.
func (s *Server) handleRefreshArtist(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	artist, err := s.queries.GetArtist(id)
	if err != nil || artist == nil {
		writeError(w, http.StatusNotFound, "artist not found")
		return
	}
	if artist.Provider == provider.LocalProvider {
		writeError(w, http.StatusConflict, "link the artist to a provider first")
		return
	}
	if !s.providers.IsHealthy(artist.Provider) {
		writeError(w, http.StatusServiceUnavailable, "provider is unreachable")
		return
	}

	s.startArtistRefresh(artist)

	writeJSON(w, http.StatusAccepted, map[string]any{"status": "refreshing", "reconciling": true})
}

// startArtistRefresh drops the artist's cached provider data and kicks off a
// background discography reconcile. Caller has already validated the artist is
// provider-linked and healthy.
func (s *Server) startArtistRefresh(artist *models.Artist) {
	s.cache.Delete(artist.Provider + ":artist:" + artist.ProviderID)
	s.cache.Delete(artist.Provider + ":artist-albums:" + artist.ProviderID)
	if albums, err := s.queries.ListAlbumsByArtist(artist.ID); err == nil {
		for _, a := range albums {
			if a.Provider == artist.Provider {
				s.cache.Delete(artist.Provider + ":album:" + a.ProviderID)
			}
		}
	}

	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		s.reconcileLocalArtist(artist.Provider, artist.ID, artist.ProviderID)
	}()
}

// handleBulkRefreshArtists refreshes the discographies of up to 100 selected
// artists — the same work the artist page's refresh button does, fanned out.
func (s *Server) handleBulkRefreshArtists(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 100 {
		writeError(w, http.StatusBadRequest, "ids must contain 1-100 entries")
		return
	}

	queued := 0
	for _, id := range req.IDs {
		artist, err := s.queries.GetArtist(id)
		if err != nil || artist == nil {
			continue
		}
		if artist.Provider == provider.LocalProvider || !s.providers.IsHealthy(artist.Provider) {
			continue
		}
		s.startArtistRefresh(artist)
		queued++
	}
	slog.Info("sync: bulk refresh queued", "artists", queued)

	writeJSON(w, http.StatusAccepted, map[string]int{"queued": queued})
}

// handleSyncStatus returns every artist's live discography-sync progress — the
// library page's bulk-refresh banner polls this.
func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	type syncItem struct {
		ArtistID int64  `json:"artist_id"`
		Active   bool   `json:"active"`
		Phase    string `json:"phase,omitempty"`
		Total    int    `json:"total"`
		Done     int    `json:"done"`
		Current  string `json:"current,omitempty"`
	}
	items := []syncItem{}
	s.syncStatus.Range(func(k, v any) bool {
		st := v.(*models.SyncInfo)
		items = append(items, syncItem{
			ArtistID: k.(int64),
			Active:   st.Active,
			Phase:    st.Phase,
			Total:    st.Total,
			Done:     st.Done,
			Current:  st.Current,
		})
		return true
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Settings

var sensitiveSettings = map[string]bool{
	"navidrome_password":    true,
	"music_assistant_token": true,
}

var hiddenSettings = map[string]bool{
	"slskd_url":                  true,
	"slskd_api_key":              true,
	"library_path":               true,
	"scan_interval":              true,
	"download_format_preference": true,
	"min_bitrate":                true,
}

const redactedPlaceholder = "••••••••"

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.queries.AllSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	for k := range settings {
		if hiddenSettings[k] {
			delete(settings, k)
		} else if sensitiveSettings[k] {
			settings[k] = redactedPlaceholder
		}
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var settings map[string]string
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Validate everything before saving anything, so a bad value can't leave
	// the settings half-applied.
	if v, ok := settings[naming.SettingKey]; ok && strings.TrimSpace(v) != "" {
		if err := naming.Validate(v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid naming template: "+err.Error())
			return
		}
	}
	for k, v := range settings {
		if hiddenSettings[k] {
			continue
		}
		if sensitiveSettings[k] && v == redactedPlaceholder {
			continue
		}
		if err := s.queries.SetSetting(k, v); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save settings")
			return
		}
	}
	writeJSON(w, http.StatusOK, settings)
}

// namingPreviewMeta is the sample metadata rendered by the settings UI's
// live template preview.
var namingPreviewMeta = naming.Meta{
	Artist: "Radiohead",
	Album:  "OK Computer",
	Year:   1997,
	Track:  6,
	Disc:   1,
	Title:  "Karma Police",
}

func (s *Server) handleNamingPreview(w http.ResponseWriter, r *http.Request) {
	tmpl := r.URL.Query().Get("template")
	if strings.TrimSpace(tmpl) == "" {
		tmpl = naming.DefaultTemplate
	}
	path, err := naming.Render(tmpl, namingPreviewMeta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path + ".flac"})
}

// Library import

func (s *Server) handleStartImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		DryRun bool   `json:"dry_run"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if err := s.importer.Start(req.Path, req.DryRun); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already running") {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.importer.Status())
}

func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.importer.Status())
}

// Blacklist & Cooldowns

func (s *Server) handleListBlacklist(w http.ResponseWriter, r *http.Request) {
	entries, err := s.queries.ListBlacklist()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list blacklist")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleDeleteBlacklistEntry(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.DeleteBlacklistEntry(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete blacklist entry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearBlacklist(w http.ResponseWriter, r *http.Request) {
	if err := s.queries.ClearBlacklist(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear blacklist")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListCooldowns(w http.ResponseWriter, r *http.Request) {
	cooldowns, err := s.queries.ListActiveCooldowns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list cooldowns")
		return
	}
	writeJSON(w, http.StatusOK, cooldowns)
}

func (s *Server) handleDeleteCooldown(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.queries.DeleteCooldown(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete cooldown")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearCooldowns(w http.ResponseWriter, r *http.Request) {
	if err := s.queries.ClearCooldowns(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear cooldowns")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Reject

func (s *Server) handleRejectTrack(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	switch err := s.reject.Reject(id); {
	case errors.Is(err, reject.ErrNotFound):
		writeError(w, http.StatusNotFound, "track not found")
		return
	case errors.Is(err, reject.ErrNotOwned):
		writeError(w, http.StatusBadRequest, "track is not owned")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to reject track")
		return
	}

	s.triggerScanAsync()

	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

func (s *Server) handleRejectTrackByName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Artist string `json:"artist"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Artist == "" || req.Title == "" {
		writeError(w, http.StatusBadRequest, "artist and title are required")
		return
	}

	track, err := s.queries.FindOwnedTrackByName(req.Artist, req.Title)
	if err != nil {
		writeError(w, http.StatusNotFound, "no owned track found matching artist/title")
		return
	}

	if err := s.reject.RejectTrack(track); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reject track")
		return
	}

	s.triggerScanAsync()

	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// Helpers

// triggerScanAsync fans out a library rescan to all post-download notifiers
// (Navidrome, Music Assistant) in the background.
func (s *Server) triggerScanAsync() {
	s.bgWork.Add(1)
	go func() {
		defer s.bgWork.Done()
		for _, n := range s.downloader.Notifiers() {
			n.TriggerScan(context.Background())
		}
	}()
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intPtrOrNil(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
