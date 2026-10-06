package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
	pb "github.com/TheOutdoorProgrammer/crate/proto/provider"
)

// Reconciling a promoted local import. See docs/adr/0007-reconcile-local-import.md.
//
// When an imported (local-provider) artist is relinked to a real provider, we
// fetch that provider's discography and fold the artist's existing local
// albums/tracks into it *in place* — matching conservatively, relinking matches
// so owned files are preserved, and creating only the genuine remainder as
// wanted gaps. Nothing is ever deleted; local children that match nothing are
// left owned + local for the user to link manually. Only local-provider
// entities are ever fuzzed — provider-anchored rows are never touched.

// reconcileLocalArtist reconciles one artist's local albums against the
// provider discography. Safe to re-run: already-linked albums are recognised by
// provider id and only gain newly-listed tracks, so it never duplicates.
func (s *Server) reconcileLocalArtist(providerName string, artistID int64, artistProviderID string) {
	// A wedged provider must not park a background worker forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	name := fmt.Sprintf("artist %d", artistID)
	if artist, err := s.queries.GetArtist(artistID); err == nil && artist != nil {
		name = artist.Name
	}
	slog.Info("sync: starting discography sync", "artist", name, "provider", providerName)

	albumList, err := s.providers.GetArtistAlbums(ctx, providerName, artistProviderID)
	if err != nil {
		slog.Error("sync: failed to fetch discography from provider", "artist", name, "provider", providerName, "error", err)
		return
	}
	slog.Info("sync: provider returned releases", "artist", name, "provider", providerName, "releases", len(albumList.Albums))

	s.enrichArtistImage(ctx, artistID)

	existing, err := s.queries.ListAlbumsByArtist(artistID)
	if err != nil {
		slog.Error("sync: list albums", "artist_id", artistID, "error", err)
		return
	}

	// Partition the artist's current albums: those already on the target
	// provider (recognised by id, skip-or-sync) and the local ones we may fold in.
	linked := make(map[string]*models.Album)
	var locals []*models.Album
	for i := range existing {
		a := &existing[i]
		switch a.Provider {
		case providerName:
			linked[a.ProviderID] = a
		case provider.LocalProvider:
			locals = append(locals, a)
		}
	}
	used := make(map[int64]bool)
	added := 0

	for _, pa := range albumList.Albums {
		if a, ok := linked[pa.Id]; ok {
			if d := pa.Metadata["release_date"]; d != "" {
				s.queries.BackfillAlbumReleaseDate(a.ID, d)
			}
			// Already ours on this provider — fill any tracks it's newly listing.
			s.reconcileAlbumTracks(ctx, providerName, a.ID, pa.Id)
			continue
		}
		if match := matchLocalAlbum(locals, used, pa); match != nil {
			used[match.ID] = true
			slog.Info("sync: matched existing album to provider release", "artist", name, "album", match.Title, "type", pa.RecordType)
			if err := s.queries.RelinkAlbum(match.ID, providerName, pa.Id); err != nil {
				slog.Error("sync: relink album", "album_id", match.ID, "error", err)
				continue
			}
			if d := pa.Metadata["release_date"]; d != "" {
				s.queries.BackfillAlbumReleaseDate(match.ID, d)
			}
			s.reconcileAlbumTracks(ctx, providerName, match.ID, pa.Id)
			continue
		}
		// A genuine gap: create the album with every track wanted.
		s.saveAlbumFromProvider(ctx, providerName, artistID, pa)
		added++
	}

	slog.Info("sync: discography sync complete", "artist", name, "provider", providerName,
		"provider_releases", len(albumList.Albums), "added", added, "local_matched", len(used))
	s.activityLog.Record("discography_sync", "artist", artistID, fmt.Sprintf(
		"Synced %s from %s — %d release(s) added, %d local album(s) matched", name, providerName, added, len(used)))
}

// reconcileAlbumTracks reconciles one album's tracks against the provider's
// track list: local tracks matching a provider track by title (album-scoped,
// disc/track number as tiebreak) are relinked in place so the owned file is
// kept; provider tracks with no local match are created as wanted; local tracks
// matching nothing stay owned + local. With no local tracks present it
// degenerates to "create the missing wanted tracks", matching the watch path.
func (s *Server) reconcileAlbumTracks(ctx context.Context, providerName string, albumID int64, albumProviderID string) {
	title := fmt.Sprintf("album %d", albumID)
	if album, err := s.queries.GetAlbum(albumID); err == nil && album != nil {
		title = album.Title
	}

	detail, err := s.providers.GetAlbum(ctx, providerName, albumProviderID)
	if err != nil {
		slog.Error("sync: failed to fetch release tracklist", "album", title, "provider", providerName, "provider_id", albumProviderID, "error", err)
		return
	}

	existing, err := s.queries.ListTracksByAlbum(albumID)
	if err != nil {
		slog.Error("sync: list tracks", "album", title, "error", err)
		return
	}

	linked := make(map[string]bool)
	var locals []*models.Track
	for i := range existing {
		t := &existing[i]
		switch t.Provider {
		case providerName:
			linked[t.ProviderID] = true
		case provider.LocalProvider:
			locals = append(locals, t)
		}
	}
	used := make(map[int64]bool)
	matched, added := 0, 0

	for _, pt := range detail.Tracks {
		if linked[pt.Id] {
			continue
		}
		if match := matchLocalTrack(locals, used, pt); match != nil {
			used[match.ID] = true
			if err := s.queries.RelinkTrack(match.ID, providerName, pt.Id); err != nil {
				slog.Error("sync: relink track", "album", title, "track", pt.Title, "error", err)
			} else {
				matched++
			}
			continue
		}
		if err := s.queries.CreateTrack(&models.Track{
			AlbumID:     albumID,
			Title:       pt.Title,
			TrackNumber: int(pt.TrackNumber),
			DiscNumber:  int(pt.DiscNumber),
			DurationMs:  int(pt.DurationMs),
			Provider:    providerName,
			ProviderID:  pt.Id,
			Status:      models.TrackStatusWanted,
		}); err != nil {
			slog.Error("sync: create wanted track", "album", title, "track", pt.Title, "error", err)
		} else {
			added++
		}
	}
	if added > 0 || matched > 0 {
		slog.Info("sync: release synced", "album", title, "provider", providerName, "tracks_added", added, "local_matched", matched)
	}
}

// albumHasLocalTracks reports whether an album still contains any local-provider
// tracks. This is the trigger for reconciling on a manual album relink: it
// covers both promotion (a freshly-linked local album) and correction (an album
// fuzzy-matched to the wrong release-group, whose tracks never matched and so
// stayed local) — and naturally no-ops once everything is linked.
func (s *Server) albumHasLocalTracks(albumID int64) bool {
	tracks, err := s.queries.ListTracksByAlbum(albumID)
	if err != nil {
		return false
	}
	for _, t := range tracks {
		if t.Provider == provider.LocalProvider {
			return true
		}
	}
	return false
}

// matchLocalAlbum returns the first unused local album whose title folds equal
// to pa and whose year is compatible (equal, or unknown on either side). The
// year guard stops a same-titled but distinct release (a re-record, a reissue
// under the same name) from being merged.
func matchLocalAlbum(locals []*models.Album, used map[int64]bool, pa *pb.AlbumSummary) *models.Album {
	for _, a := range locals {
		if used[a.ID] || !foldEqual(a.Title, pa.Title) {
			continue
		}
		if a.Year != nil && pa.Year != 0 && *a.Year != int(pa.Year) {
			continue
		}
		return a
	}
	return nil
}

// matchLocalTrack returns the best unused local track for pt: an exact
// title-fold match, preferring one whose disc/track number also matches when
// several share a title. There is deliberately no number-only fallback — an
// unmatched local track is left owned + local for the user to link manually,
// never force-matched onto a title it doesn't share.
func matchLocalTrack(locals []*models.Track, used map[int64]bool, pt *pb.TrackInfo) *models.Track {
	var titleOnly *models.Track
	for _, t := range locals {
		if used[t.ID] || !foldEqual(t.Title, pt.Title) {
			continue
		}
		if t.TrackNumber == int(pt.TrackNumber) && t.DiscNumber == int(pt.DiscNumber) {
			return t
		}
		if titleOnly == nil {
			titleOnly = t
		}
	}
	return titleOnly
}

func foldEqual(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// enrichArtistImage backfills artist.image_url from Deezer for artists whose
// own provider (notably MusicBrainz) doesn't serve artist photos. Runs inside
// background work — never blocks a request. No-ops when the artist already has
// an image or Deezer isn't healthy.
func (s *Server) enrichArtistImage(ctx context.Context, artistID int64) {
	artist, err := s.queries.GetArtist(artistID)
	if err != nil || artist == nil {
		return
	}
	if artist.ImageURL != nil && *artist.ImageURL != "" {
		return
	}
	const imageProvider = "deezer"
	if !s.providers.IsHealthy(imageProvider) {
		return
	}

	res, err := s.providers.SearchWithProvider(ctx, imageProvider, artist.Name, 5, 0)
	if err != nil {
		slog.Warn("image: artist image lookup failed", "artist", artist.Name, "provider", imageProvider, "error", err)
		return
	}
	var pick *pb.ArtistResult
	for _, a := range res.Artists {
		if a.ImageUrl == "" {
			continue
		}
		if strings.EqualFold(a.Name, artist.Name) {
			pick = a
			break
		}
		if pick == nil {
			pick = a
		}
	}
	if pick == nil {
		return
	}
	if err := s.queries.SetArtistImageURL(artistID, pick.ImageUrl); err != nil {
		slog.Error("image: save artist image", "artist", artist.Name, "error", err)
		return
	}
	slog.Info("image: artist image updated", "artist", artist.Name, "source", imageProvider)
}
