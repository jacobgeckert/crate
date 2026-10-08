// Package concerts finds upcoming Ticketmaster shows for watched artists.
//
// One Discovery API call fetches every music event near the configured postal
// code; event attractions are then name-matched against the watchlist. The raw
// event feed is cached (not the matched shows) so newly watched artists match
// immediately without another API call.
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
	"strings"
	"time"
	"unicode"

	"github.com/TheOutdoorProgrammer/crate/internal/cache"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
)

const (
	SettingAPIKey     = "ticketmaster_api_key"
	SettingPostalCode = "concerts_postal_code"
	SettingRadius     = "concerts_radius"
	SettingUnit       = "concerts_unit"

	defaultBaseURL = "https://app.ticketmaster.com/discovery/v2"
	defaultRadius  = "50"
	cacheTTL       = 12 * time.Hour
	maxPages       = 3 // 200/page — 600 events covers even dense metros
)

var ErrNotConfigured = errors.New("concerts not configured: set ticketmaster_api_key and concerts_postal_code in settings")

type Show struct {
	EventID    string `json:"event_id"`
	EventName  string `json:"event_name"`
	ArtistID   *int64 `json:"artist_id,omitempty"`
	ArtistName string `json:"artist_name"` // attraction name as billed
	Date       string `json:"date"`        // YYYY-MM-DD local to the venue
	Time       string `json:"time,omitempty"`
	Venue      string `json:"venue"`
	City       string `json:"city"`
	State      string `json:"state,omitempty"`
	URL        string `json:"url"`
	Image      string `json:"image,omitempty"`
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
		baseURL: defaultBaseURL,
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
func (s *Service) Shows(ctx context.Context, refresh bool) ([]Show, error) {
	apiKey, postal, radius, unit, ok := s.config()
	if !ok {
		return nil, ErrNotConfigured
	}

	events, err := s.events(ctx, apiKey, postal, radius, unit, refresh)
	if err != nil {
		return nil, err
	}

	artists, err := s.queries.ListArtists()
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", err)
	}
	byName := make(map[string]int64, len(artists))
	for _, a := range artists {
		byName[normalizeName(a.Name)] = a.ID
	}

	var shows []Show
	seen := make(map[string]bool)
	for _, ev := range events {
		for _, attr := range ev.Embedded.Attractions {
			artistID, matched := byName[normalizeName(attr.Name)]
			if !matched {
				continue
			}
			key := ev.ID + ":" + attr.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			shows = append(shows, toShow(ev, attr.Name, artistID))
		}
	}
	return shows, nil
}

// events returns the raw event feed, from cache when fresh.
func (s *Service) events(ctx context.Context, apiKey, postal, radius, unit string, refresh bool) ([]tmEvent, error) {
	key := fmt.Sprintf("concerts:events:%s:%s:%s", strings.ToLower(postal), radius, unit)
	if !refresh && s.cache != nil {
		if raw, hit := s.cache.Get(key); hit {
			var events []tmEvent
			if err := json.Unmarshal(raw, &events); err == nil {
				return events, nil
			}
		}
	}

	events, err := s.fetchEvents(ctx, apiKey, postal, radius, unit)
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

func (s *Service) fetchEvents(ctx context.Context, apiKey, postal, radius, unit string) ([]tmEvent, error) {
	var all []tmEvent
	for page := 0; page < maxPages; page++ {
		u := fmt.Sprintf("%s/events.json?apikey=%s&classificationName=music&postalCode=%s&radius=%s&unit=%s&size=200&sort=date,asc&startDateTime=%s&page=%d",
			s.baseURL,
			url.QueryEscape(apiKey), url.QueryEscape(postal), url.QueryEscape(radius), unit,
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

func toShow(ev tmEvent, artistName string, artistID int64) Show {
	show := Show{
		EventID:    ev.ID,
		EventName:  ev.Name,
		ArtistID:   &artistID,
		ArtistName: artistName,
		Date:       ev.Dates.Start.LocalDate,
		Time:       ev.Dates.Start.LocalTime,
		URL:        ev.URL,
		Image:      pickImage(ev.Images),
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
			Name  string `json:"name"`
			City  struct {
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
