package discover

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

	s := NewService(queries, c)
	if handler != nil {
		fake := httptest.NewServer(handler)
		t.Cleanup(fake.Close)
		s.baseURL = fake.URL + "/"
	}
	return s, queries
}

func configure(t *testing.T, q *db.Queries) {
	t.Helper()
	if err := q.SetSetting(SettingAPIKey, "test-key"); err != nil {
		t.Fatal(err)
	}
}

const lfmFeed = `{
  "similarartists": {
    "artist": [
      {"name": "New Band", "mbid": "mbid-new", "match": "0.8"},
      {"name": "Watched Artist", "mbid": "mbid-self", "match": "0.9"},
      {"name": "Another Band", "mbid": "", "match": "0.5"}
    ]
  }
}`

func TestSuggestionsNotConfigured(t *testing.T) {
	s, _ := newTestService(t, nil)
	_, err := s.Suggestions(context.Background(), false)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	if s.Configured() {
		t.Error("Configured() = true without settings")
	}
}

// Watched artists are excluded (even under a different spelling via mbid),
// scores aggregate across sources, and suggestions sort by score.
func TestSuggestionsAggregates(t *testing.T) {
	var gotQuery string
	s, queries := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(lfmFeed))
	})
	configure(t, queries)

	// Two watched artists → each feed's matches aggregate; "Watched Artist"
	// is excluded by name, "mbid-self" is excluded by provider id.
	if err := queries.CreateArtist(&models.Artist{Name: "Watched Artist", Provider: "test", ProviderID: "mbid-self", Status: models.ArtistStatusWatched}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateArtist(&models.Artist{Name: "Other Watched", Provider: "test", ProviderID: "other-1", Status: models.ArtistStatusWatched}); err != nil {
		t.Fatal(err)
	}

	sugs, err := s.Suggestions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sugs) != 2 {
		t.Fatalf("expected 2 suggestions, got %d: %+v", len(sugs), sugs)
	}
	if sugs[0].Name != "New Band" || sugs[0].MBID != "mbid-new" {
		t.Errorf("unexpected top suggestion: %+v", sugs[0])
	}
	// Both watched artists' feeds contributed → score = 0.8 + 0.8 = 1.6.
	if sugs[0].Score < 1.5 || sugs[0].Score > 1.7 {
		t.Errorf("score not aggregated: %v", sugs[0].Score)
	}
	if len(sugs[0].Artists) != 2 {
		t.Errorf("expected both artists credited, got %+v", sugs[0].Artists)
	}
	if sugs[1].Name != "Another Band" {
		t.Errorf("unexpected second suggestion: %+v", sugs[1])
	}
	if !containsAll(gotQuery, "method=artist.getsimilar", "api_key=test-key", "artist=") {
		t.Errorf("unexpected query: %s", gotQuery)
	}
}

func TestSuggestionsUsesCache(t *testing.T) {
	calls := 0
	s, queries := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(lfmFeed))
	})
	configure(t, queries)
	if err := queries.CreateArtist(&models.Artist{Name: "Some Artist", Provider: "test", ProviderID: "x", Status: models.ArtistStatusWatched}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Suggestions(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Suggestions(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected 1 upstream call with cache, got %d", calls)
	}
	// refresh=true bypasses cache.
	if _, err := s.Suggestions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("expected refresh to bypass cache, got %d calls", calls)
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
