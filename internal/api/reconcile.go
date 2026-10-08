package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/library"
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
	// Report queued immediately so the sync-status poll sees every selected
	// artist before the lock below admits it — otherwise the UI's all-done
	// check could fire in the gap between two serialized syncs.
	s.setSync(artistID, &models.SyncInfo{Active: true, Phase: "queued"})
	// One discography sync at a time. The timeout applies only after the lock
	// is held — a sync's budget covers its own provider calls, not time spent
	// queued behind other artists' work in the provider's rate limiter.
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	// A wedged provider must not park a background worker forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	name := fmt.Sprintf("artist %d", artistID)
	var artist *models.Artist
	if a, err := s.queries.GetArtist(artistID); err == nil && a != nil {
		artist = a
		name = a.Name
	}
	watch := s.newReleaseWatchFor(artist)
	slog.Info("sync: starting discography sync", "artist", name, "provider", providerName)

	s.setSync(artistID, &models.SyncInfo{Active: true, Phase: "contacting provider"})
	defer s.finishSync(artistID)

	albumList, err := s.providers.GetArtistAlbums(ctx, providerName, artistProviderID)
	if err != nil {
		slog.Error("sync: failed to fetch discography from provider", "artist", name, "provider", providerName, "error", err)
		s.activityLog.Record("sync_failed", "artist", artistID, fmt.Sprintf(
			"Discography sync failed for %s (%s): %v", name, providerName, err))
		return
	}
	slog.Info("sync: provider returned releases", "artist", name, "provider", providerName, "releases", len(albumList.Albums))

	s.enrichArtistImage(ctx, artistID)

	existing, err := s.queries.ListAlbumsByArtist(artistID)
	if err != nil {
		slog.Error("sync: list albums", "artist_id", artistID, "error", err)
		s.activityLog.Record("sync_failed", "artist", artistID, fmt.Sprintf(
			"Discography sync failed for %s: %v", name, err))
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
	total := len(albumList.Albums)

	for i, pa := range albumList.Albums {
		s.setSync(artistID, &models.SyncInfo{Active: true, Phase: "syncing releases", Total: total, Done: i, Current: pa.Title})
		if a, ok := linked[pa.Id]; ok {
			if d := pa.Metadata["release_date"]; d != "" {
				s.queries.BackfillAlbumReleaseDate(a.ID, d)
			}
			if pa.RecordType != "" && a.RecordType != pa.RecordType {
				s.queries.SetAlbumRecordType(a.ID, pa.RecordType)
			}
			// Already ours on this provider — fill any tracks it's newly listing.
			s.reconcileAlbumTracks(ctx, providerName, a.ID, pa.Id)
			s.enrichAlbumCover(ctx, a.ID)
			s.demoteUnownedAlbum(a)
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
			s.enrichAlbumCover(ctx, match.ID)
			s.demoteUnownedAlbum(match)
			continue
		}
		// A genuine gap: create the album — wanted when it qualifies as a
		// watched new release, ignored otherwise.
		if s.saveAlbumFromProvider(ctx, providerName, artistID, pa, watch) != "" {
			added++
		}
	}
	s.setSync(artistID, &models.SyncInfo{Active: true, Phase: "syncing releases", Total: total, Done: total})

	slog.Info("sync: discography sync complete", "artist", name, "provider", providerName,
		"provider_releases", len(albumList.Albums), "added", added, "local_matched", len(used))
	s.activityLog.Record("discography_sync", "artist", artistID, fmt.Sprintf(
		"Synced %s from %s — %d release(s) added, %d local album(s) matched", name, providerName, added, len(used)))
}

// reconcileAlbumTracks reconciles one album's tracks against the provider's
// track list: local tracks matching a provider track by title (album-scoped,
// disc/track number as tiebreak) are relinked in place so the owned file is
// kept; provider tracks with no local match are created as wanted; local tracks
// matching nothing stay owned + local; provider rows matching nothing are
// pruned — leftovers from a different edition or stale listing. With no local
// tracks present it degenerates to "create the missing wanted tracks",
// matching the watch path.
func (s *Server) reconcileAlbumTracks(ctx context.Context, providerName string, albumID int64, albumProviderID string) {
	title := fmt.Sprintf("album %d", albumID)
	// Tracks created under an ignored album stay ignored — a wanted track under
	// an ignored album would still get auto-queued.
	trackStatus := models.TrackStatusWanted
	fetchID := albumProviderID
	if album, err := s.queries.GetAlbum(albumID); err == nil && album != nil {
		title = album.Title
		if album.Status == models.AlbumStatusIgnored {
			trackStatus = models.TrackStatusIgnored
		}
		// A pinned edition overrides the release-group's default tracklist.
		fetchID = album.TracklistID()
	}

	detail, err := s.providers.GetAlbum(ctx, providerName, fetchID)
	if err != nil {
		slog.Error("sync: failed to fetch release tracklist", "album", title, "provider", providerName, "provider_id", fetchID, "error", err)
		return
	}
	s.foldAlbumTracks(providerName, albumID, title, trackStatus, detail, true)
}

// foldAlbumTracks folds a fetched provider tracklist into an album's rows.
// When prune is set, provider-linked rows that match nothing in the new
// listing and aren't owned or in-flight are deleted — they're leftovers from
// a different edition or an older listing. Pruning is skipped entirely when
// the fetch returned no tracks, so a truncated provider response can't wipe
// the album.
func (s *Server) foldAlbumTracks(providerName string, albumID int64, title string, trackStatus models.TrackStatus, detail *pb.AlbumDetail, prune bool) (added, matched, merged, pruned int) {
	existing, err := s.queries.ListTracksByAlbum(albumID)
	if err != nil {
		slog.Error("sync: list tracks", "album", title, "error", err)
		return
	}

	byID := make(map[string]*models.Track)
	byRecording := make(map[string][]*models.Track)
	var locals []*models.Track
	for i := range existing {
		t := &existing[i]
		switch t.Provider {
		case providerName:
			byID[t.ProviderID] = t
		case provider.LocalProvider:
			locals = append(locals, t)
		}
		if t.MBRecordingID != nil && *t.MBRecordingID != "" {
			byRecording[*t.MBRecordingID] = append(byRecording[*t.MBRecordingID], t)
		}
	}
	used := make(map[int64]bool)

	for _, pt := range detail.Tracks {
		if canon := byID[pt.Id]; canon != nil {
			used[canon.ID] = true
			if recID := pt.Metadata["recording_id"]; recID != "" {
				// Stamp the recording id if the row predates us storing it.
				_ = s.queries.SetTrackMBRecordingID(canon.ID, recID)
				// A stale duplicate may sit under another release's track id —
				// the release-independent recording id folds it in.
				if dup := matchByRecording(byRecording[recID], used, canon.ID, pt); dup != nil {
					used[dup.ID] = true
					if err := s.queries.AbsorbTrack(canon.ID, dup.ID); err != nil {
						slog.Error("sync: merge duplicate track", "album", title, "track", pt.Title, "error", err)
					} else {
						merged++
					}
				}
			}
			continue
		}
		// Same recording under a different release's track id — the file was
		// tagged against another edition. Higher confidence than title fold.
		if recID := pt.Metadata["recording_id"]; recID != "" {
			if match := matchByRecording(byRecording[recID], used, 0, pt); match != nil {
				used[match.ID] = true
				if err := s.queries.RelinkTrack(match.ID, providerName, pt.Id); err != nil {
					slog.Error("sync: relink track", "album", title, "track", pt.Title, "error", err)
				} else {
					matched++
				}
				continue
			}
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
			AlbumID:       albumID,
			Title:         pt.Title,
			TrackNumber:   int(pt.TrackNumber),
			DiscNumber:    int(pt.DiscNumber),
			DurationMs:    int(pt.DurationMs),
			Provider:      providerName,
			ProviderID:    pt.Id,
			MBRecordingID: strPtrOrNil(pt.Metadata["recording_id"]),
			Status:        trackStatus,
		}); err != nil {
			// A row on another album may be squatting this release-track id —
			// e.g. it was moved before splits rekeyed unmatched rows. This
			// album's own tracklist is authoritative for the id, so the
			// squatter goes local and the create retries once.
			if squatter, ferr := s.queries.FindTrackByProvider(providerName, pt.Id); ferr == nil && squatter != nil && squatter.AlbumID != albumID {
				if rerr := s.queries.RelinkTrack(squatter.ID, provider.LocalProvider, fmt.Sprintf("loc-split-%d", squatter.ID)); rerr == nil {
					slog.Info("sync: rekeyed squatted track to local", "album", title, "track", squatter.Title, "other_album", squatter.AlbumID)
					if err := s.queries.CreateTrack(&models.Track{
						AlbumID:       albumID,
						Title:         pt.Title,
						TrackNumber:   int(pt.TrackNumber),
						DiscNumber:    int(pt.DiscNumber),
						DurationMs:    int(pt.DurationMs),
						Provider:      providerName,
						ProviderID:    pt.Id,
						MBRecordingID: strPtrOrNil(pt.Metadata["recording_id"]),
						Status:        trackStatus,
					}); err == nil {
						added++
						continue
					}
				}
			}
			slog.Error("sync: create track", "album", title, "track", pt.Title, "error", err)
		} else {
			added++
		}
	}

	if prune && len(detail.Tracks) > 0 {
		// Provider rows that matched nothing in the new listing are leftovers
		// from a different edition or an outdated tracklist. Owned/in-flight
		// ones are kept — the file still exists — everything else is removed
		// (queue rows cascade).
		for pid, t := range byID {
			if used[t.ID] || t.Status == models.TrackStatusOwned || t.Status == models.TrackStatusDownloading {
				continue
			}
			if err := s.queries.DeleteTrack(t.ID); err != nil {
				slog.Error("sync: prune stale track", "album", title, "track", t.Title, "provider_id", pid, "error", err)
			} else {
				pruned++
			}
		}
	}

	reverted := s.verifyAlbumFiles(albumID)

	if added > 0 || matched > 0 || merged > 0 || pruned > 0 || reverted > 0 {
		slog.Info("sync: release synced", "album", title, "provider", providerName, "tracks_added", added, "local_matched", matched, "dupes_merged", merged, "stale_pruned", pruned, "files_reverted", reverted)
	}
	return
}

// verifyAlbumFiles reverts owned rows whose file vanished from disk back to
// wanted — the same check the scheduler's daily integrity tick runs library-
// wide, scoped to this album so a refresh reflects deletions immediately.
// The stored file_path is kept so the download history/audit stays intact.
func (s *Server) verifyAlbumFiles(albumID int64) (reverted int) {
	tracks, err := s.queries.ListTracksByAlbum(albumID)
	if err != nil {
		slog.Error("sync: verify files", "album_id", albumID, "error", err)
		return 0
	}
	for _, t := range tracks {
		if t.Status != models.TrackStatusOwned || t.FilePath == nil {
			continue
		}
		if _, err := os.Stat(library.ResolvePath(s.libraryDir, *t.FilePath)); os.IsNotExist(err) {
			if err := s.queries.UpdateTrackStatus(t.ID, models.TrackStatusWanted); err != nil {
				slog.Error("sync: revert missing-file track", "track_id", t.ID, "error", err)
				continue
			}
			reverted++
			slog.Warn("sync: file missing, reverted to wanted", "track_id", t.ID, "path", *t.FilePath)
		}
	}
	return reverted
}

// matchByRecording returns the best unused candidate sharing pt's
// release-independent recording id — preferring a disc/track-number match when
// several rows share it. skipID excludes the canonical row itself.
func matchByRecording(cands []*models.Track, used map[int64]bool, skipID int64, pt *pb.TrackInfo) *models.Track {
	var any *models.Track
	for _, t := range cands {
		if used[t.ID] || t.ID == skipID {
			continue
		}
		if t.TrackNumber == int(pt.TrackNumber) && t.DiscNumber == int(pt.DiscNumber) {
			return t
		}
		if any == nil {
			any = t
		}
	}
	return any
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

// setAlbumSync/finishAlbumSync publish live refresh progress for the album
// detail page's polling banner — shared server-side, so a refresh kicked off
// on one device shows on others.
func (s *Server) setAlbumSync(albumID int64, st *models.SyncInfo) {
	s.albumSync.Store(albumID, st)
}

func (s *Server) finishAlbumSync(albumID int64) {
	if v, ok := s.albumSync.Load(albumID); ok {
		st := *v.(*models.SyncInfo)
		st.Active = false
		s.albumSync.Store(albumID, &st)
	}
}

// setSync/finishSync publish live discography-sync progress for the artist
// detail page's polling banner. finishSync flips Active off but keeps the
// final counts so the UI can show the completed state.
func (s *Server) setSync(artistID int64, st *models.SyncInfo) {
	s.syncStatus.Store(artistID, st)
}

func (s *Server) finishSync(artistID int64) {
	if v, ok := s.syncStatus.Load(artistID); ok {
		st := *v.(*models.SyncInfo)
		st.Active = false
		s.syncStatus.Store(artistID, &st)
	}
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

// enrichAlbumCover resolves real cover art for an album. MusicBrainz provider
// ids are release-groups whose Cover Art Archive URL is only a guess — CAA
// 404s for releases with no submitted art, so the URL is verified with a HEAD
// before trusting it (a pinned edition's release art is preferred over the
// release-group's). When CAA has nothing, the Deezer catalog is searched by
// artist+title for a cover. Albums already carrying non-CAA art (Deezer,
// manual) are left alone. Runs inside background sync work.
func (s *Server) enrichAlbumCover(ctx context.Context, albumID int64) {
	album, err := s.queries.GetAlbum(albumID)
	if err != nil || album == nil {
		return
	}
	cur := ""
	if album.CoverURL != nil {
		cur = *album.CoverURL
	}
	if cur != "" && !strings.Contains(cur, "coverartarchive.org") {
		return
	}

	if album.Provider == "musicbrainz" && album.ProviderID != "" {
		var cands []string
		pinned := ""
		if album.ReleaseID != nil {
			pinned = *album.ReleaseID
		}
		if pinned != "" {
			cands = append(cands, "https://coverartarchive.org/release/"+pinned+"/front-500")
		}
		cands = append(cands, "https://coverartarchive.org/release-group/"+album.ProviderID+"/front-500")
		// Editions share artwork often enough that a sibling pressing's cover
		// beats nothing — try each release in the group, capped to bound the
		// HEADs on release-groups with dozens of editions.
		if detail, derr := s.providers.GetAlbum(ctx, "musicbrainz", album.ProviderID); derr == nil {
			var editions []albumEdition
			if raw := detail.Metadata["releases"]; raw != "" {
				_ = json.Unmarshal([]byte(raw), &editions)
			}
			for _, e := range editions {
				if len(cands) >= 10 {
					break
				}
				if e.ID != "" && e.ID != pinned {
					cands = append(cands, "https://coverartarchive.org/release/"+e.ID+"/front-500")
				}
			}
		}
		for _, u := range cands {
			if !coverArtExists(ctx, u) {
				continue
			}
			if u != cur {
				if err := s.queries.SetAlbumCoverURL(album.ID, u); err != nil {
					slog.Error("image: save album cover", "album", album.Title, "error", err)
				} else {
					slog.Info("image: album cover updated", "album", album.Title, "source", "coverartarchive")
				}
			}
			return // either just stored u, or cur verified still resolving
		}
	}

	if cover := s.deezerAlbumCover(ctx, album); cover != "" {
		if err := s.queries.SetAlbumCoverURL(album.ID, cover); err != nil {
			slog.Error("image: save album cover", "album", album.Title, "error", err)
		} else {
			slog.Info("image: album cover updated", "album", album.Title, "source", "deezer")
		}
	}
}

// deezerAlbumCover finds a Deezer album matching this album's artist+title and
// returns its cover url, or "" when Deezer is unhealthy / has no match.
func (s *Server) deezerAlbumCover(ctx context.Context, album *models.Album) string {
	const imageProvider = "deezer"
	if !s.providers.IsHealthy(imageProvider) {
		return ""
	}
	artist, err := s.queries.GetArtist(album.ArtistID)
	if err != nil || artist == nil {
		return ""
	}
	res, err := s.providers.SearchWithProvider(ctx, imageProvider, artist.Name, 5, 0)
	if err != nil {
		slog.Warn("image: deezer artist search failed", "artist", artist.Name, "error", err)
		return ""
	}
	artistID := ""
	for _, a := range res.Artists {
		if strings.EqualFold(a.Name, artist.Name) {
			artistID = a.Id
			break
		}
	}
	if artistID == "" && len(res.Artists) > 0 {
		artistID = res.Artists[0].Id
	}
	if artistID == "" {
		return ""
	}
	albums, err := s.providers.GetArtistAlbums(ctx, imageProvider, artistID)
	if err != nil {
		slog.Warn("image: deezer album lookup failed", "artist", artist.Name, "error", err)
		return ""
	}
	for _, pa := range albums.Albums {
		if pa.CoverUrl != "" && foldEqual(pa.Title, album.Title) {
			return pa.CoverUrl
		}
	}
	return "" // no title match — a wrong album's cover is worse than none
}

var caaClient = &http.Client{Timeout: 8 * time.Second}

// coverArtExists HEADs a Cover Art Archive URL; CAA 307s to archive.org when
// art exists and 404s when the release/group has no submitted art.
func coverArtExists(ctx context.Context, url string) bool {
	// Only ever CAA — the URL is built from a fixed host plus a provider
	// release/group id, never raw user input. Guard anyway: a compromised
	// or buggy provider must not turn this into an arbitrary fetcher.
	if !strings.HasPrefix(url, "https://coverartarchive.org/") {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil) // #nosec G704 -- host pinned above
	if err != nil {
		return false
	}
	resp, err := caaClient.Do(req) // #nosec G704 -- host pinned above
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// newReleaseWatch captures an artist's effective new-release watch config once
// per sync, so sync-discovered releases can be gated on the same predicate the
// scheduler's detectNewReleases uses: releases that qualify land wanted, the
// rest default to ignored.
type newReleaseWatch struct {
	watching bool
	since    time.Time
	types    map[string]bool
}

func (s *Server) newReleaseWatchFor(artist *models.Artist) *newReleaseWatch {
	w := &newReleaseWatch{types: s.globalNewReleaseTypes()}
	if artist == nil || !artist.WatchNewReleases || artist.WatchNewReleasesSince == nil {
		return w
	}
	w.watching = true
	w.since, _ = time.Parse(time.RFC3339, *artist.WatchNewReleasesSince)
	if override, err := s.queries.GetArtistWatchReleaseTypes(artist.ID); err == nil && override != nil {
		w.types = override
	}
	return w
}

// qualifies mirrors the scheduler's gate: watched type, released on/after the
// watch start. A missing release date doesn't disqualify (can't prove it's
// old), matching detectNewReleases. Secondary-type releases ride the same
// type gate — their record_types default off, so they land ignored unless the
// user enables them.
func (w *newReleaseWatch) qualifies(pa *pb.AlbumSummary) bool {
	if !w.watching {
		return false
	}
	if t := pa.RecordType; t != "" && !w.types[t] {
		return false
	}
	if d := pa.Metadata["release_date"]; d != "" {
		if rd, err := time.Parse("2006-01-02", d); err == nil && rd.Before(w.since) {
			return false
		}
	}
	return true
}

// globalNewReleaseTypes mirrors the scheduler's new_release_types setting —
// missing or malformed means all types watched.
func (s *Server) globalNewReleaseTypes() map[string]bool {
	types := map[string]bool{
		"album": true, "ep": true, "single": true, "compilation": true,
		// Secondary types ("Album + Live" etc.) are collected but ignored
		// unless the user opts in.
		"live": false, "remix": false, "soundtrack": false, "dj-mix": false,
		"mixtape": false, "demo": false, "spokenword": false, "interview": false,
		"audiobook": false, "field-recording": false, "audio-drama": false,
	}
	v, err := s.queries.GetSetting("new_release_types")
	if err != nil || v == "" {
		return types
	}
	var m map[string]bool
	if json.Unmarshal([]byte(v), &m) != nil {
		return types
	}
	for k := range types {
		if b, ok := m[k]; ok {
			types[k] = b
		}
	}
	return types
}

// demoteUnownedAlbum flips a watched album to ignored when sync confirmed it
// holds nothing owned, downloading, or queued — refresh's default for releases
// the user hasn't collected. Owned/in-flight albums are never demoted, and
// already-ignored albums are untouched.
func (s *Server) demoteUnownedAlbum(album *models.Album) {
	if album.Status != models.AlbumStatusWatched {
		return
	}
	active, err := s.queries.AlbumHasOwnedOrActive(album.ID)
	if err != nil {
		slog.Error("sync: album activity check failed", "album", album.Title, "error", err)
		return
	}
	if active {
		return
	}
	if err := s.queries.UpdateAlbumStatus(album.ID, models.AlbumStatusIgnored); err != nil {
		slog.Error("sync: ignore album failed", "album", album.Title, "error", err)
		return
	}
	s.queries.UpdateTrackStatusByAlbum(album.ID, models.TrackStatusWanted, models.TrackStatusIgnored)
	slog.Info("sync: ignored non-owned release", "album", album.Title)
}
