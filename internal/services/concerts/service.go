// Package concerts finds upcoming Ticketmaster shows for watched artists.
//
// One Discovery keyword query per artist fetches that artist's events near the
// configured postal code; event attractions are then name-verified against the
// artist so fuzzy keyword matches can't leak in. Per-artist feeds are cached so
// repeat page loads are free and a refresh only re-queries stale artists.
package concerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TheOutdoorProgrammer/crate/internal/cache"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

const (
	SettingAPIKey     = "ticketmaster_api_key"
	SettingPostalCode = "concerts_postal_code"
	SettingRadius     = "concerts_radius"
	SettingUnit       = "concerts_unit"

	defaultBaseURL = "https://app.ticketmaster.com/discovery/v2"
	defaultGeoURL  = "https://api.zippopotam.us" // postal→latlong, no key needed
	defaultRadius  = "50"
	cacheTTL       = 12 * time.Hour
	geoTTL         = 30 * 24 * time.Hour    // postal codes don't move
	maxPages       = 2                      // per artist — own-shows feeds are tiny
	requestPace    = 210 * time.Millisecond // TM free tier allows ~5 req/s
)

var ErrNotConfigured = errors.New("concerts not configured: set ticketmaster_api_key and concerts_postal_code in settings")

// ShowArtist is one watched artist billed on a show — several can share an
// event (headliner + support), so a Show carries the whole list.
type ShowArtist struct {
	ID   int64  `json:"id"`
	Name string `json:"name"` // attraction name as billed
}

type Show struct {
	EventID   string       `json:"event_id"`
	EventName string       `json:"event_name"`
	Artists   []ShowArtist `json:"artists"` // watched artists on the bill
	Date      string       `json:"date"`    // YYYY-MM-DD local to the venue
	Time      string       `json:"time,omitempty"`
	Venue     string       `json:"venue"`
	City      string       `json:"city"`
	State     string       `json:"state,omitempty"`
	URL       string       `json:"url"`
	Image     string       `json:"image,omitempty"`
}

type Service struct {
	queries *db.Queries
	cache   *cache.Cache
	http    *http.Client
	baseURL string // overridable for tests
	geoURL  string // overridable for tests
}

func NewService(queries *db.Queries, c *cache.Cache) *Service {
	return &Service{
		queries: queries,
		cache:   c,
		http:    &http.Client{Timeout: 15 * time.Second},
		baseURL: defaultBaseURL,
		geoURL:  defaultGeoURL,
	}
}

func (s *Service) config() (apiKey, postal, radius, unit string, ok bool) {
	apiKey, _ = s.queries.GetSetting(SettingAPIKey)
	postal, _ = s.queries.GetSetting(SettingPostalCode)
	radius, _ = s.queries.GetSetting(SettingRadius)
	if radius == "" {
		radius = defaultRadius
	}
	unit, _ = s.queries.GetSetting(SettingUnit)
	if unit != "km" {
		unit = "miles"
	} else {
		unit = "kms" // Discovery API spells it "kms"
	}
	ok = apiKey != "" && postal != ""
	return
}

// Configured reports whether the minimum settings are present.
func (s *Service) Configured() bool {
	_, _, _, _, ok := s.config()
	return ok
}

// Shows returns upcoming events whose attractions match watched artists.
//
// One Discovery keyword query per artist — a shared geo feed sorted by date
// gets truncated (TM caps results) before reaching a niche show weeks out in
// dense metros, but a single artist's own feed is always small enough to fit.
// Attraction names are still verified per event so fuzzy keyword matches
// (cover bands, festivals, unrelated events) can't leak in.
func (s *Service) Shows(ctx context.Context, refresh bool) ([]Show, error) {
	apiKey, postal, radius, unit, ok := s.config()
	if !ok {
		return nil, ErrNotConfigured
	}

	artists, err := s.queries.ListArtists()
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", err)
	}

	type artistResult struct {
		artist *models.Artist
		events []tmEvent
		err    error
	}
	// Shared pace gate: one slot every ~210ms no matter how many workers are
	// waiting, keeping us under TM's ~5 req/s free tier. Concurrency overlaps
	// HTTP latency, not quota.
	tick := time.NewTicker(requestPace)
	defer tick.Stop()
	geo := s.geoParam(ctx, postal)

	jobs := make(chan *models.Artist)
	results := make(chan artistResult, len(artists))
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				events, err := s.artistEvents(ctx, apiKey, a.EffectiveName(), geo, postal, radius, unit, refresh, tick.C)
				results <- artistResult{a, events, err}
			}
		}()
	}
	go func() {
		for i := range artists {
			jobs <- &artists[i]
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var shows []*Show
	byEvent := make(map[string]*Show)
	seen := make(map[string]bool)
	var failCount int
	for res := range results {
		if res.err != nil {
			failCount++
			slog.Warn("concerts: artist events fetch failed", "artist", res.artist.Name, "error", res.err)
			continue
		}
		// Match either name — TM may bill the canonical or the overridden
		// display name regardless of which one the keyword query used.
		want := normalizeName(res.artist.EffectiveName())
		wantCanon := normalizeName(res.artist.Name)
		var matched, seenNames int
		var sample []string
		for _, ev := range res.events {
			for _, attr := range ev.Embedded.Attractions {
				seenNames++
				if len(sample) < 6 {
					sample = append(sample, attr.Name)
				}
				if attrName := normalizeName(attr.Name); attrName != want && attrName != wantCanon {
					continue
				}
				matched++
				key := ev.ID + ":" + attr.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				// Same event, another watched artist on the bill — fold into
				// one listing instead of a row per artist.
				sh, ok := byEvent[ev.ID]
				if !ok {
					sh = toShow(ev)
					byEvent[ev.ID] = sh
					shows = append(shows, sh)
				}
				sh.Artists = append(sh.Artists, ShowArtist{ID: res.artist.ID, Name: attr.Name})
			}
		}
		slog.Debug("concerts: artist feed", "artist", res.artist.Name,
			"events", len(res.events), "attractions", seenNames, "matched", matched,
			"attraction_sample", sample)
	}
	if len(artists) > 0 && failCount == len(artists) {
		return nil, fmt.Errorf("ticketmaster unreachable: every artist query failed")
	}
	out := make([]Show, 0, len(shows))
	for _, sh := range shows {
		sort.Slice(sh.Artists, func(i, j int) bool { return sh.Artists[i].Name < sh.Artists[j].Name })
		out = append(out, *sh)
	}
	out = dedupeSameShow(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	slog.Info("concerts: matched shows", "artists", len(artists), "failures", failCount, "shows", len(out))
	return out, nil
}

// dedupeSameShow collapses double-listed events — TM sometimes serves the
// same concert under two event ids with variant venue records ("Riviera
// Theatre" vs "Riviera Theatre-IL"). Same date + normalized venue + city and
// at least one shared watched artist means it's the same show. The artist
// overlap requirement keeps genuinely different events at one venue on one
// night distinct.
func dedupeSameShow(shows []Show) []Show {
	var merged []Show
	slot := make(map[string]int)
	artistIDs := make(map[int]map[int64]bool)
	for _, sh := range shows {
		key := sh.Date + "|" + venueKey(sh.Venue, sh.State) + "|" + normalizeName(sh.City)
		if i, ok := slot[key]; ok && overlapsAny(sh.Artists, artistIDs[i]) {
			ex := &merged[i]
			for _, a := range sh.Artists {
				if !artistIDs[i][a.ID] {
					artistIDs[i][a.ID] = true
					ex.Artists = append(ex.Artists, a)
				}
			}
			sort.Slice(ex.Artists, func(x, y int) bool { return ex.Artists[x].Name < ex.Artists[y].Name })
			// Prefer the cleaner venue variant (the "-IL" style suffix is the
			// dup venue record) and fill any fields the first row lacked.
			if sh.Venue != "" && (ex.Venue == "" || len(sh.Venue) < len(ex.Venue)) {
				ex.Venue = sh.Venue
			}
			if ex.Image == "" {
				ex.Image = sh.Image
			}
			continue
		}
		slot[key] = len(merged)
		ids := make(map[int64]bool, len(sh.Artists))
		for _, a := range sh.Artists {
			ids[a.ID] = true
		}
		artistIDs[len(merged)] = ids
		merged = append(merged, sh)
	}
	return merged
}

func overlapsAny(artists []ShowArtist, ids map[int64]bool) bool {
	for _, a := range artists {
		if ids[a.ID] {
			return true
		}
	}
	return false
}

// venueKey normalizes a venue for duplicate detection, dropping a trailing
// state code so "Riviera Theatre-IL" (state IL) matches "Riviera Theatre".
func venueKey(venue, state string) string {
	v := normalizeName(venue)
	if st := normalizeName(state); st != "" {
		v = strings.TrimSuffix(v, " "+st)
	}
	return v
}

// geoParam resolves the configured postal code into Ticketmaster's latlong
// geo filter. postalCode+radius is unreliable in Discovery queries — TM can
// narrow to the zip itself and return zero events for shows clearly inside
// the radius. latlong+radius is the supported radius search.
func (s *Service) geoParam(ctx context.Context, postal string) string {
	if ll := s.geocode(ctx, postal); ll != "" {
		slog.Debug("concerts: postal geocoded", "postal", postal, "latlong", ll)
		return "latlong=" + url.QueryEscape(ll)
	}
	slog.Debug("concerts: postal not geocoded, using postalCode filter", "postal", postal)
	return "postalCode=" + url.QueryEscape(postal)
}

// geocode converts a US zip to "lat,long" via zippopotam.us. Returns "" for
// non-US codes or lookup failures — the caller falls back to the raw
// postalCode filter. Results cache for a month; zips don't move.
func (s *Service) geocode(ctx context.Context, postal string) string {
	if len(postal) != 5 {
		return ""
	}
	for _, r := range postal {
		if !unicode.IsDigit(r) {
			return ""
		}
	}
	key := "concerts:geo:" + postal
	if s.cache != nil {
		if raw, hit := s.cache.Get(key); hit {
			if ll := strings.TrimSpace(string(raw)); ll != "" {
				return ll
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.geoURL+"/us/"+url.PathEscape(postal), nil)
	if err != nil {
		return ""
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close() // #nosec G104 -- close error is irrelevant on a response body
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	var geo struct {
		Places []struct {
			Lat  string `json:"latitude"`
			Long string `json:"longitude"`
		} `json:"places"`
	}
	if err := json.Unmarshal(body, &geo); err != nil || len(geo.Places) == 0 {
		return ""
	}
	ll := geo.Places[0].Lat + "," + geo.Places[0].Long
	if s.cache != nil {
		s.cache.Set(key, []byte(ll), geoTTL)
	}
	return ll
}

// artistEvents returns an artist's upcoming events near the configured postal
// code, from cache when fresh. gate paces actual API calls against TM's rate
// limit — workers share one ticker.
func (s *Service) artistEvents(ctx context.Context, apiKey, artist, geo, postal, radius, unit string, refresh bool, gate <-chan time.Time) ([]tmEvent, error) {
	key := fmt.Sprintf("concerts:artist:%s:%s:%s:%s", normalizeName(artist), strings.ToLower(postal), radius, unit)
	if !refresh && s.cache != nil {
		if raw, hit := s.cache.Get(key); hit {
			var events []tmEvent
			if err := json.Unmarshal(raw, &events); err == nil {
				slog.Debug("concerts: cache hit", "artist", artist, "events", len(events))
				return events, nil
			}
		}
	}

	<-gate
	slog.Debug("concerts: fetching artist events", "artist", artist, "geo", geo, "radius", radius, "unit", unit)
	events, err := s.fetchArtistEvents(ctx, apiKey, artist, geo, radius, unit)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		if raw, err := json.Marshal(events); err == nil {
			s.cache.Set(key, raw, cacheTTL)
		}
	}
	return events, nil
}

func (s *Service) fetchArtistEvents(ctx context.Context, apiKey, artist, geo, radius, unit string) ([]tmEvent, error) {
	var all []tmEvent
	for page := 0; page < maxPages; page++ {
		u := fmt.Sprintf("%s/events.json?apikey=%s&classificationName=music&keyword=%s&%s&radius=%s&unit=%s&size=200&sort=date,asc&startDateTime=%s&page=%d",
			s.baseURL,
			url.QueryEscape(apiKey), url.QueryEscape(artist), geo, url.QueryEscape(radius), unit,
			time.Now().UTC().Format("2006-01-02T15:04:05Z"), page)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("ticketmaster request: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close() // #nosec G104 -- close error is irrelevant on a response body
		if err != nil {
			return nil, fmt.Errorf("read ticketmaster response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			slog.Warn("concerts: ticketmaster returned non-200", "status", resp.StatusCode)
			return nil, fmt.Errorf("ticketmaster returned status %d", resp.StatusCode)
		}

		var tm tmResponse
		if err := json.Unmarshal(body, &tm); err != nil {
			return nil, fmt.Errorf("decode ticketmaster response: %w", err)
		}
		all = append(all, tm.Embedded.Events...)
		if tm.Page.Number >= tm.Page.TotalPages-1 {
			break
		}
	}
	return all, nil
}

func toShow(ev tmEvent) *Show {
	show := &Show{
		EventID:   ev.ID,
		EventName: ev.Name,
		Date:      ev.Dates.Start.LocalDate,
		Time:      ev.Dates.Start.LocalTime,
		URL:       ev.URL,
		Image:     pickImage(ev.Images),
	}
	if len(ev.Embedded.Venues) > 0 {
		v := ev.Embedded.Venues[0]
		show.Venue = v.Name
		show.City = v.City.Name
		show.State = v.State.StateCode
	}
	return show
}

func pickImage(images []tmImage) string {
	best, fallback := "", ""
	for _, img := range images {
		if fallback == "" {
			fallback = img.URL
		}
		if img.Ratio == "16_9" && img.Width >= 640 {
			best = img.URL
			break
		}
	}
	if best != "" {
		return best
	}
	return fallback
}

// normalizeName folds case, turns punctuation into whitespace, and strips a
// leading "the" so "blink-182" matches "Blink 182" and "The Offspring"
// matches "Offspring".
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

// Ticketmaster Discovery response shapes.

type tmResponse struct {
	Embedded struct {
		Events []tmEvent `json:"events"`
	} `json:"_embedded"`
	Page struct {
		Number     int `json:"number"`
		TotalPages int `json:"totalPages"`
	} `json:"page"`
}

type tmEvent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Dates struct {
		Start struct {
			LocalDate string `json:"localDate"`
			LocalTime string `json:"localTime"`
		} `json:"start"`
	} `json:"dates"`
	Images   []tmImage `json:"images"`
	Embedded struct {
		Venues []struct {
			Name string `json:"name"`
			City struct {
				Name string `json:"name"`
			} `json:"city"`
			State struct {
				StateCode string `json:"stateCode"`
			} `json:"state"`
		} `json:"venues"`
		Attractions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"attractions"`
	} `json:"_embedded"`
}

type tmImage struct {
	URL   string `json:"url"`
	Ratio string `json:"ratio"`
	Width int    `json:"width"`
}
