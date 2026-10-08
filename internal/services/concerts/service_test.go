package concerts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TheOutdoorProgrammer/crate/internal/cache"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

func newTestService(t *testing.T, handler http.HandlerFunc) (*Service, *db.Queries) {
	t.Helper()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.NewQueries(database)

	c, err := cache.Open(t.TempDir() + "/cache.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	var fake *httptest.Server
	if handler != nil {
		fake = httptest.NewServer(handler)
		t.Cleanup(fake.Close)
	}

	s := NewService(queries, c)
	if fake != nil {
		s.baseURL = fake.URL
	}
	return s, queries
}

func configure(t *testing.T, q *db.Queries) {
	t.Helper()
	for k, v := range map[string]string{
		SettingAPIKey:     "test-key",
		SettingPostalCode: "90210",
		SettingRadius:     "25",
	} {
		if err := q.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
}

const tmFeed = `{
  "_embedded": {
    "events": [
      {
        "id": "ev1", "name": "Blink 182: Tour", "url": "https://tm.example/ev1",
        "dates": {"start": {"localDate": "2026-08-01", "localTime": "19:30:00"}},
        "images": [{"url": "https://img.example/a.jpg", "ratio": "16_9", "width": 1024}],
        "_embedded": {
          "venues": [{"name": "The Forum", "city": {"name": "Inglewood"}, "state": {"stateCode": "CA"}}],
          "attractions": [{"id": "at1", "name": "blink-182"}, {"id": "at2", "name": "Some Opener"}]
        }
      },
      {
        "id": "ev2", "name": "Offspring Live", "url": "https://tm.example/ev2",
        "dates": {"start": {"localDate": "2026-09-15"}},
        "_embedded": {
          "venues": [{"name": "Club", "city": {"name": "LA"}, "state": {"stateCode": "CA"}}],
          "attractions": [{"id": "at3", "name": "Offspring"}]
        }
      },
      {
        "id": "ev3", "name": "Unwatched Band", "url": "https://tm.example/ev3",
        "dates": {"start": {"localDate": "2026-10-01"}},
        "_embedded": {
          "venues": [{"name": "Hall", "city": {"name": "LA"}, "state": {"stateCode": "CA"}}],
          "attractions": [{"id": "at4", "name": "Unwatched Band"}]
        }
      }
    ]
  },
  "page": {"number": 0, "totalPages": 1}
}`

func TestShowsNotConfigured(t *testing.T) {
	s, _ := newTestService(t, nil)
	_, err := s.Shows(context.Background(), false)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	if s.Configured() {
		t.Error("Configured() = true without settings")
	}
}

func TestShowsMatchesWatchedArtists(t *testing.T) {
	var gotQuery string
	s, queries := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(tmFeed))
	})
	configure(t, queries)

	for _, name := range []string{"blink-182", "The Offspring"} {
		if err := queries.CreateArtist(&models.Artist{Name: name, Provider: "test", ProviderID: name, Status: models.ArtistStatusWatched}); err != nil {
			t.Fatal(err)
		}
	}

	shows, err := s.Shows(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 2 {
		t.Fatalf("expected 2 matched shows, got %d: %+v", len(shows), shows)
	}

	// "blink-182" normalizes to "blink 182" — same as the attraction "blink-182".
	if *shows[0].ArtistID == 0 || shows[0].ArtistName != "blink-182" {
		t.Errorf("unexpected first show: %+v", shows[0])
	}
	if shows[0].Venue != "The Forum" || shows[0].City != "Inglewood" || shows[0].State != "CA" {
		t.Errorf("venue fields not mapped: %+v", shows[0])
	}
	if shows[0].Date != "2026-08-01" || shows[0].Time != "19:30:00" {
		t.Errorf("date fields not mapped: %+v", shows[0])
	}
	if shows[0].Image != "https://img.example/a.jpg" {
		t.Errorf("image not picked: %+v", shows[0])
	}
	// "The Offspring" normalizes to "offspring" — matches attraction "Offspring".
	if shows[1].ArtistName != "Offspring" {
		t.Errorf("expected Offspring match, got %+v", shows[1])
	}
	if gotQuery == "" || !containsAll(gotQuery, "apikey=test-key", "postalCode=90210", "radius=25", "unit=miles", "classificationName=music") {
		t.Errorf("unexpected query: %s", gotQuery)
	}
}

func TestShowsUsesCache(t *testing.T) {
	calls := 0
	s, queries := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(tmFeed))
	})
	configure(t, queries)
	if err := queries.CreateArtist(&models.Artist{Name: "blink-182", Provider: "test", ProviderID: "x", Status: models.ArtistStatusWatched}); err != nil {
		t.Fatal(err)
	}

	first, err := s.Shows(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first call: %d shows", len(first))
	// Watch a new artist — matching must re-run against the cached feed.
	if err := queries.CreateArtist(&models.Artist{Name: "Offspring", Provider: "test", ProviderID: "y", Status: models.ArtistStatusWatched}); err != nil {
		t.Fatal(err)
	}
	shows, err := s.Shows(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected 1 API call (cached), got %d", calls)
	}
	if len(shows) != 2 {
		t.Errorf("expected new artist to match cached feed, got %d shows", len(shows))
	}

	// refresh bypasses the cache.
	if _, err := s.Shows(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("expected refresh to hit the API, got %d calls", calls)
	}
}

func TestShowsUpstreamError(t *testing.T) {
	s, queries := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	configure(t, queries)

	_, err := s.Shows(context.Background(), false)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Blink-182":       "blink 182",
		"The Offspring":   "offspring",
		"  Sigur   Rós  ": "sigur rós",
		"A.C/DC":          "a c dc",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
