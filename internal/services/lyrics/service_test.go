package lyrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

const lrcPayload = "[00:01.00] hello\n[00:02.00] world\n"

func newTestEnv(t *testing.T, handler http.HandlerFunc) (*Service, *db.Queries, string) {
	t.Helper()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.NewQueries(database)

	libDir := t.TempDir()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)

	s := NewService(queries, libDir)
	s.http = fake.Client()
	s.baseURL = fake.URL
	return s, queries, libDir
}

func seed(t *testing.T, q *db.Queries) *models.Album {
	t.Helper()
	artist := &models.Artist{Name: "Test Artist", Provider: "test", ProviderID: "a1", Status: models.ArtistStatusWatched}
	if err := q.CreateArtist(artist); err != nil {
		t.Fatal(err)
	}
	album := &models.Album{ArtistID: artist.ID, Title: "Test Album", Provider: "test", ProviderID: "al1", Status: models.AlbumStatusWatched}
	if err := q.CreateAlbum(album); err != nil {
		t.Fatal(err)
	}
	return album
}

func addTrack(t *testing.T, q *db.Queries, albumID int64, title, relPath string, status models.TrackStatus) *models.Track {
	t.Helper()
	fp := relPath
	tr := &models.Track{
		AlbumID:   albumID,
		Title:     title,
		DurationMs: 201000,
		Provider:  "test", ProviderID: "tr-" + title,
		Status:   status,
		FilePath: &fp,
	}
	if err := q.CreateImportedTrack(tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestFetchAlbumWritesLRC(t *testing.T) {
	s, q, libDir := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"syncedLyrics": "[00:01.00] hello\n[00:02.00] world\n", "duration": 201.0}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	album := seed(t, q)

	rel := "Test Artist/Test Album/01 - Song.flac"
	writeFile(t, filepath.Join(libDir, rel))
	addTrack(t, q, album.ID, "Song", rel, models.TrackStatusOwned)
	addTrack(t, q, album.ID, "Wanted", "", models.TrackStatusWanted)
	// Owned but outside the library — never written.
	outside := filepath.Join(t.TempDir(), "outside.flac")
	writeFile(t, outside)
	addTrack(t, q, album.ID, "Outside", outside, models.TrackStatusOwned)

	rep, err := s.FetchAlbum(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 1 || rep.Skipped != 2 || rep.Missing != 0 {
		t.Fatalf("report = %+v, want fetched=1 skipped=2 missing=0", rep)
	}

	lrcPath := filepath.Join(libDir, "Test Artist/Test Album/01 - Song.lrc")
	content, err := os.ReadFile(lrcPath)
	if err != nil {
		t.Fatalf("expected .lrc sidecar: %v", err)
	}
	if string(content) != lrcPayload {
		t.Errorf("lrc content = %q, want %q", content, lrcPayload)
	}
	if _, err := os.Stat(outside[:len(outside)-len(filepath.Ext(outside))] + ".lrc"); !os.IsNotExist(err) {
		t.Error("sidecar written outside the library")
	}

	// Second run: the existing sidecar is not clobbered.
	rep, err = s.FetchAlbum(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 0 || rep.Skipped != 3 {
		t.Fatalf("second run = %+v, want fetched=0 skipped=3", rep)
	}
}

func TestFetchAlbumSearchFallback(t *testing.T) {
	s, q, libDir := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			w.WriteHeader(http.StatusNotFound)
		case "/api/search":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[
				{"syncedLyrics": "far off", "duration": 999.5},
				{"syncedLyrics": "[00:01.00] hello\n[00:02.00] world\n", "duration": 202.0}
			]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	album := seed(t, q)
	rel := "Test Artist/Test Album/01 - Song.flac"
	writeFile(t, filepath.Join(libDir, rel))
	addTrack(t, q, album.ID, "Song", rel, models.TrackStatusOwned)

	rep, err := s.FetchAlbum(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 1 {
		t.Fatalf("report = %+v, want fetched=1", rep)
	}
	content, _ := os.ReadFile(filepath.Join(libDir, "Test Artist/Test Album/01 - Song.lrc"))
	if string(content) != lrcPayload {
		t.Errorf("picked wrong-duration candidate: %q", content)
	}
}

func TestFetchAlbumNoMatch(t *testing.T) {
	s, q, libDir := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	album := seed(t, q)
	rel := "Test Artist/Test Album/01 - Song.flac"
	writeFile(t, filepath.Join(libDir, rel))
	addTrack(t, q, album.ID, "Song", rel, models.TrackStatusOwned)

	rep, err := s.FetchAlbum(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Missing != 1 || rep.Fetched != 0 {
		t.Fatalf("report = %+v, want missing=1 fetched=0", rep)
	}
}

func TestMarkTracks(t *testing.T) {
	s, q, libDir := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	album := seed(t, q)
	rel := "Test Artist/Test Album/01 - Song.flac"
	writeFile(t, filepath.Join(libDir, rel))
	writeFile(t, filepath.Join(libDir, "Test Artist/Test Album/01 - Song.lrc"))
	addTrack(t, q, album.ID, "Song", rel, models.TrackStatusOwned)
	addTrack(t, q, album.ID, "Other", "Test Artist/Test Album/02 - Other.flac", models.TrackStatusOwned)
	writeFile(t, filepath.Join(libDir, "Test Artist/Test Album/02 - Other.flac"))
	outside := filepath.Join(t.TempDir(), "out.flac")
	writeFile(t, outside)
	writeFile(t, strings.TrimSuffix(outside, ".flac")+".lrc") // exists but outside the library
	addTrack(t, q, album.ID, "Outside", outside, models.TrackStatusOwned)

	tracks, err := q.ListTracksByAlbum(album.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.MarkTracks(tracks)

	want := map[string]bool{"Song": true, "Other": false, "Outside": false}
	for _, tr := range tracks {
		if tr.HasLyrics != want[tr.Title] {
			t.Errorf("%s: has_lyrics = %v, want %v", tr.Title, tr.HasLyrics, want[tr.Title])
		}
	}
}

func TestFetchAlbumPlainFallback(t *testing.T) {
	s, q, libDir := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"plainLyrics": "just words, no timing", "duration": 201.0}`))
		case "/api/search":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	album := seed(t, q)
	rel := "Test Artist/Test Album/01 - Song.flac"
	writeFile(t, filepath.Join(libDir, rel))
	addTrack(t, q, album.ID, "Song", rel, models.TrackStatusOwned)

	rep, err := s.FetchAlbum(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Plain != 1 || rep.Fetched != 0 {
		t.Fatalf("report = %+v, want plain=1", rep)
	}
	content, err := os.ReadFile(filepath.Join(libDir, "Test Artist/Test Album/01 - Song.txt"))
	if err != nil {
		t.Fatal("no .txt sidecar written")
	}
	if string(content) != "just words, no timing" {
		t.Errorf("txt content = %q", content)
	}
}

func TestFetchAlbumPlainUpgradesToSynced(t *testing.T) {
	s, q, libDir := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/get" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"syncedLyrics": "[00:01.00] hello\n", "duration": 201.0}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	album := seed(t, q)
	rel := "Test Artist/Test Album/01 - Song.flac"
	writeFile(t, filepath.Join(libDir, rel))
	writeFile(t, filepath.Join(libDir, "Test Artist/Test Album/01 - Song.txt"))
	addTrack(t, q, album.ID, "Song", rel, models.TrackStatusOwned)

	rep, err := s.FetchAlbum(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 1 {
		t.Fatalf("report = %+v, want fetched=1", rep)
	}
	if _, err := os.Stat(filepath.Join(libDir, "Test Artist/Test Album/01 - Song.lrc")); err != nil {
		t.Fatal("no .lrc sidecar written")
	}
	if _, err := os.Stat(filepath.Join(libDir, "Test Artist/Test Album/01 - Song.txt")); !os.IsNotExist(err) {
		t.Fatal("stale .txt not swept after synced upgrade")
	}
}
