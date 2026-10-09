// Package lyrics fetches time-synced lyrics from LRCLIB and writes them as
// .lrc sidecar files next to the audio file. Navidrome, Jellyfin, and most
// players read <track>.lrc automatically — no player-side setup needed.
//
// https://lrclib.net/docs — free, no API key.
package lyrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/library"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

const (
	defaultBaseURL = "https://lrclib.net"
	// How far a search-result duration may drift from the track's own before
	// we reject it — /api/get already enforces a match, this guards the
	// looser /api/search fallback.
	durationToleranceSec = 5
	// Polite pacing between LRCLIB calls — the API is free and unauthenticated.
	requestDelay = 150 * time.Millisecond
)

type Service struct {
	queries    *db.Queries
	libraryDir string
	http       *http.Client
	baseURL    string // overridable for tests
}

type Report struct {
	Fetched int `json:"fetched"` // synced .lrc written
	Plain   int `json:"plain"`   // plain-text .txt written (no synced lyrics available)
	Skipped int `json:"skipped"` // sidecar exists, not owned, or file outside the library
	Missing int `json:"missing"` // LRCLIB had no lyrics at all
}

func NewService(queries *db.Queries, libraryDir string) *Service {
	return &Service{
		queries:    queries,
		libraryDir: libraryDir,
		http:       &http.Client{Timeout: 15 * time.Second},
		baseURL:    defaultBaseURL,
	}
}

// AlbumDownloaded implements the downloader's album-settled hook — fires
// once an album's download batch finishes (the last track just completed or
// the stragglers exhausted their attempts). Best-effort: failures are
// logged, never surfaced.
func (s *Service) AlbumDownloaded(ctx context.Context, albumID int64) {
	album, err := s.queries.GetAlbum(albumID)
	if err != nil || album == nil {
		slog.Warn("lyrics: album-done fetch skipped", "album_id", albumID, "error", err)
		return
	}
	rep, err := s.FetchAlbum(ctx, album)
	if err != nil {
		slog.Warn("lyrics: album-done fetch failed", "album", album.Title, "error", err)
		return
	}
	if rep.Fetched+rep.Plain > 0 {
		slog.Info("lyrics: fetched on download complete", "album", album.Title,
			"synced", rep.Fetched, "plain", rep.Plain, "missing", rep.Missing)
	}
}

// FetchAlbum pulls synced lyrics for every owned track on the album.
func (s *Service) FetchAlbum(ctx context.Context, album *models.Album) (*Report, error) {
	artist, err := s.queries.GetArtist(album.ArtistID)
	if err != nil || artist == nil {
		return nil, fmt.Errorf("get artist: %w", err)
	}
	tracks, err := s.queries.ListTracksByAlbum(album.ID)
	if err != nil {
		return nil, fmt.Errorf("list tracks: %w", err)
	}

	rep := &Report{}
	for i := range tracks {
		res := s.fetchTrack(ctx, &tracks[i], artist.Name, album.Title)
		switch res {
		case resultFetched:
			rep.Fetched++
		case resultPlain:
			rep.Plain++
		case resultSkipped:
			rep.Skipped++
		case resultMissing:
			rep.Missing++
		}
	}
	return rep, nil
}

type result int

const (
	resultFetched result = iota
	resultPlain
	resultSkipped
	resultMissing
)

// MarkTracks sets HasLyrics on each track whose .lrc sidecar already exists
// inside the library — the API calls this before serializing track lists.
func (s *Service) MarkTracks(tracks []models.Track) {
	for i := range tracks {
		tracks[i].HasLyrics = s.hasSidecar(&tracks[i])
	}
}

func (s *Service) hasSidecar(t *models.Track) bool {
	if t.FilePath == nil || *t.FilePath == "" {
		return false
	}
	abs := library.ResolvePath(s.libraryDir, *t.FilePath)
	if !library.Contains(s.libraryDir, abs) {
		return false
	}
	base := strings.TrimSuffix(abs, filepath.Ext(abs))
	for _, ext := range []string{".lrc", ".txt"} {
		info, err := os.Stat(base + ext)
		if err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func (s *Service) fetchTrack(ctx context.Context, t *models.Track, artistName, albumTitle string) result {
	if t.Status != models.TrackStatusOwned || t.FilePath == nil || *t.FilePath == "" {
		return resultSkipped
	}
	abs := library.ResolvePath(s.libraryDir, *t.FilePath)
	if !library.Contains(s.libraryDir, abs) {
		return resultSkipped
	}
	base := strings.TrimSuffix(abs, filepath.Ext(abs))
	lrcPath, txtPath := base+".lrc", base+".txt"
	if info, err := os.Stat(lrcPath); err == nil && !info.IsDir() {
		return resultSkipped // synced sidecar is complete — never clobbered
	}

	hit, err := s.lookup(ctx, artistName, t.Title, albumTitle, t.DurationMs/1000)
	if err != nil {
		slog.Warn("lyrics: lrclib lookup failed", "track", t.Title, "error", err)
		return resultMissing
	}
	if hit == nil || hit.text == "" {
		return resultMissing
	}
	if !hit.synced {
		// Plain lyrics go in a .txt sidecar (Navidrome reads these too). A
		// matching existing file means nothing changed — not a fresh fetch.
		if prev, err := os.ReadFile(txtPath); err == nil && string(prev) == hit.text { // #nosec G304 -- path resolved inside the library dir above
			return resultSkipped
		}
		if err := os.WriteFile(txtPath, []byte(hit.text), 0644); err != nil { // #nosec G306 -- media sidecar, world-readable is fine
			slog.Warn("lyrics: write sidecar", "path", txtPath, "error", err)
			return resultMissing
		}
		return resultPlain
	}
	if err := os.WriteFile(lrcPath, []byte(hit.text), 0644); err != nil { // #nosec G306 -- media sidecar, world-readable is fine
		slog.Warn("lyrics: write sidecar", "path", lrcPath, "error", err)
		return resultMissing
	}
	// Synced arrived — the plain-text fallback is superseded.
	if err := os.Remove(txtPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("lyrics: remove superseded txt", "path", txtPath, "error", err)
	}
	return resultFetched
}

// lyricHit is a usable lyrics payload — synced (.lrc) or plain text (.txt)
// when no synced version exists.
type lyricHit struct {
	text   string
	synced bool
}

// lookup tries the exact-match endpoint first, then the search fallback.
// Preference order: synced from /api/get → synced from search → plain from
// /api/get → plain from search. Returns nil when nothing usable exists, or an
// error for transport/server failures.
func (s *Service) lookup(ctx context.Context, artist, title, album string, durationSec int) (*lyricHit, error) {
	time.Sleep(requestDelay)

	q := url.Values{
		"artist_name": {artist},
		"track_name":  {title},
		"album_name":  {album},
	}
	if durationSec > 0 {
		q.Set("duration", fmt.Sprint(durationSec))
	}
	resp, err := s.get(ctx, "/api/get?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.SyncedLyrics != "" {
		return &lyricHit{text: resp.SyncedLyrics, synced: true}, nil
	}
	if resp != nil && resp.Instrumental {
		return nil, nil // instrumentals carry no lyrics by definition
	}
	plainGet := ""
	if resp != nil {
		plainGet = resp.PlainLyrics
	}

	// Fallback: search, then take the closest-duration candidate that has
	// synced lyrics — tracking the closest plain-lyrics candidate too.
	q = url.Values{
		"artist_name": {artist},
		"track_name":  {title},
		"album_name":  {album},
	}
	results, err := s.search(ctx, "/api/search?"+q.Encode())
	if err != nil {
		return nil, err
	}
	best := ""
	bestPlain := ""
	bestDiff := float64(durationToleranceSec + 1)
	bestPlainDiff := float64(durationToleranceSec + 1)
	for _, r := range results {
		if r.Instrumental {
			continue
		}
		diff := float64(durationToleranceSec) // no duration to compare → accept at the edge
		if durationSec > 0 && r.Duration > 0 {
			d := float64(durationSec) - r.Duration
			if d < 0 {
				d = -d
			}
			diff = d
		}
		if r.SyncedLyrics != "" && diff < bestDiff {
			best = r.SyncedLyrics
			bestDiff = diff
		}
		if r.PlainLyrics != "" && diff < bestPlainDiff {
			bestPlain = r.PlainLyrics
			bestPlainDiff = diff
		}
	}
	if best != "" {
		return &lyricHit{text: best, synced: true}, nil
	}
	if plainGet != "" {
		return &lyricHit{text: plainGet}, nil // exact match's plain text wins
	}
	if bestPlain != "" {
		return &lyricHit{text: bestPlain}, nil
	}
	return nil, nil
}

type lrclibResult struct {
	SyncedLyrics string  `json:"syncedLyrics"`
	PlainLyrics  string  `json:"plainLyrics"`
	Instrumental bool    `json:"instrumental"`
	Duration     float64 `json:"duration"` // LRCLIB returns floats (e.g. 220.0, 238.64)
}

// get hits /api/get — returns (nil, nil) on a clean 404 no-match.
func (s *Service) get(ctx context.Context, path string) (*lrclibResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "crate (self-hosted music manager)")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() // #nosec G104 -- response body close error is irrelevant
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lrclib status %d", resp.StatusCode)
	}
	var r lrclibResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Service) search(ctx context.Context, path string) ([]lrclibResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "crate (self-hosted music manager)")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() // #nosec G104 -- response body close error is irrelevant
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lrclib status %d", resp.StatusCode)
	}
	var results []lrclibResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}
