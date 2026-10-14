// Package discover suggests new artists from Last.fm's artist.getSimilar,
// aggregated across the watched library: an artist similar to many of the
// user's artists ranks above a one-off match.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TheOutdoorProgrammer/crate/internal/cache"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

const (
	SettingAPIKey = "lastfm_api_key" // #nosec G101 -- setting name, not a credential
	baseURL       = "https://ws.audioscrobbler.com/2.0/"

	cacheTTL    = 7 * 24 * time.Hour // similar-artist lists barely move
	maxArtists  = 200                // cap watched artists queried per refresh
	maxResults  = 60                 // suggestions returned
	requestPace = 210 * time.Millisecond
)

var ErrNotConfigured = errors.New("discover not configured: set lastfm_api_key in settings")

// Suggestion is an artist the user doesn't watch, ranked by combined match.
type Suggestion struct {
	Name    string   `json:"name"`
	MBID    string   `json:"mbid,omitempty"`
	Score   float64  `json:"score"`   // summed last.fm match across related artists
	Artists []string `json:"artists"` // watched artists it's similar to — "because you watch…"
}

type Service struct {
	queries *db.Queries
	cache   *cache.Cache
	http    *http.Client
	baseURL string // overridable for tests
}

func NewService(queries *db.Queries, c *cache.Cache) *Service {
	return &Service{
		queries: queries,
		cache:   c,
		http:    &http.Client{Timeout: 15 * time.Second},
		baseURL: baseURL,
	}
}

func (s *Service) apiKey() string {
	k, _ := s.queries.GetSetting(SettingAPIKey)
	return k
}

// Configured reports whether the API key is set.
func (s *Service) Configured() bool {
	return s.apiKey() != ""
}

// Suggestions returns artists similar to watched artists, best matches first.
//
// One artist.getSimilar call per watched artist, cached a week — a single
// artist's similar list is small and stable, so per-artist caching means
// repeat page loads only refetch artists added since the last run.
func (s *Service) Suggestions(ctx context.Context, refresh bool) ([]Suggestion, error) {
	key := s.apiKey()
	if key == "" {
		return nil, ErrNotConfigured
	}

	artists, err := s.queries.ListArtists()
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", err)
	}

	// Already-in-library detection: normalized name or display name, plus
	// provider id — a suggestion carrying the same MBID is the same artist
	// even under a different spelling.
	inLibrary := make(map[string]bool)
	libraryMBIDs := make(map[string]bool)
	for i := range artists {
		inLibrary[normalizeName(artists[i].Name)] = true
		inLibrary[normalizeName(artists[i].EffectiveName())] = true
		if artists[i].ProviderID != "" {
			libraryMBIDs[strings.ToLower(artists[i].ProviderID)] = true
		}
	}

	type artistResult struct {
		artist  *models.Artist
		similar []lfmSimilar
		err     error
	}
	tick := time.NewTicker(requestPace)
	defer tick.Stop()

	jobs := make(chan *models.Artist)
	results := make(chan artistResult, len(artists))
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				sim, err := s.similarArtists(ctx, key, a.EffectiveName(), refresh, tick.C)
				results <- artistResult{a, sim, err}
			}
		}()
	}
	go func() {
		for i := range artists {
			if i >= maxArtists {
				break
			}
			jobs <- &artists[i]
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	type agg struct {
		name  string
		mbid  string
		score float64
		from  map[string]bool
	}
	byName := make(map[string]*agg)
	var failCount int
	for res := range results {
		if res.err != nil {
			failCount++
			slog.Warn("discover: similar fetch failed", "artist", res.artist.Name, "error", res.err)
			continue
		}
		for _, sim := range res.similar {
			if sim.Name == "" || sim.Match <= 0 {
				continue
			}
			norm := normalizeName(sim.Name)
			if norm == "" || inLibrary[norm] {
				continue
			}
			if sim.MBID != "" && libraryMBIDs[strings.ToLower(sim.MBID)] {
				continue
			}
			a, ok := byName[norm]
			if !ok {
				a = &agg{name: sim.Name, mbid: sim.MBID, from: make(map[string]bool)}
				byName[norm] = a
			}
			a.score += sim.Match
			if a.mbid == "" {
				a.mbid = sim.MBID
			}
			a.from[res.artist.EffectiveName()] = true
		}
		slog.Debug("discover: artist similar", "artist", res.artist.Name, "similar", len(res.similar))
	}
	if len(artists) > 0 && failCount == len(artists) {
		return nil, fmt.Errorf("last.fm unreachable: every artist query failed")
	}

	out := make([]Suggestion, 0, len(byName))
	for _, a := range byName {
		from := make([]string, 0, len(a.from))
		for n := range a.from {
			from = append(from, n)
		}
		sort.Strings(from)
		out = append(out, Suggestion{Name: a.name, MBID: a.mbid, Score: a.score, Artists: from})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxResults {
		out = out[:maxResults]
	}
	slog.Info("discover: suggestions built", "artists", len(artists), "failures", failCount, "suggestions", len(out))
	return out, nil
}

type lfmSimilar struct {
	Name  string
	MBID  string
	Match float64
}

// similarArtists fetches one artist's similar list, cached per artist.
func (s *Service) similarArtists(ctx context.Context, apiKey, name string, refresh bool, pace <-chan time.Time) ([]lfmSimilar, error) {
	cacheKey := "discover:similar:" + normalizeName(name)
	if !refresh {
		if raw, hit := s.cache.Get(cacheKey); hit {
			var cached []lfmSimilar
			if json.Unmarshal(raw, &cached) == nil {
				return cached, nil
			}
		}
	}

	<-pace
	u := s.baseURL + "?method=artist.getsimilar&format=json&autocorrect=1" +
		"&api_key=" + url.QueryEscape(apiKey) +
		"&artist=" + url.QueryEscape(name) +
		"&limit=25"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("discover: last.fm returned non-200", "status", resp.StatusCode)
		return nil, fmt.Errorf("last.fm returned status %d", resp.StatusCode)
	}

	var body struct {
		SimilarArtists struct {
			Artist []struct {
				Name  string `json:"name"`
				MBID  string `json:"mbid"`
				Match string `json:"match"`
			} `json:"artist"`
		} `json:"similarartists"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode last.fm response: %w", err)
	}

	similar := make([]lfmSimilar, 0, len(body.SimilarArtists.Artist))
	for _, a := range body.SimilarArtists.Artist {
		m, _ := strconv.ParseFloat(a.Match, 64)
		similar = append(similar, lfmSimilar{Name: a.Name, MBID: a.MBID, Match: m})
	}
	if raw, err := json.Marshal(similar); err == nil {
		s.cache.Set(cacheKey, raw, cacheTTL)
	}
	return similar, nil
}

// normalizeName mirrors the concerts matcher — letters/digits kept, the rest
// whitespace, "the " prefix dropped — so library dedupe agrees with shows.
func normalizeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.TrimPrefix(strings.Join(strings.Fields(b.String()), " "), "the ")
}
