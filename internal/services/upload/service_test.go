package upload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	id3v2 "github.com/bogem/id3v2/v2"

	"github.com/TheOutdoorProgrammer/crate/internal/activity"
	"github.com/TheOutdoorProgrammer/crate/internal/cache"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
)

// writeMP3 writes a minimal MPEG1 Layer3 frame with an ID3v2 tag — same
// approach as the importer/tagger test fixtures.
func writeMP3(t *testing.T, path, artist, album, title string, track int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 417)
	frame[0] = 0xFF
	frame[1] = 0xFB
	frame[2] = 0x90
	if err := os.WriteFile(path, frame, 0644); err != nil {
		t.Fatal(err)
	}
	tag, err := id3v2.Open(path, id3v2.Options{Parse: false})
	if err != nil {
		t.Fatal(err)
	}
	defer tag.Close()
	tag.SetDefaultEncoding(id3v2.EncodingUTF8)
	tag.SetTitle(title)
	tag.SetArtist(artist)
	tag.SetAlbum(album)
	if track > 0 {
		tag.AddTextFrame("TRCK", id3v2.EncodingUTF8, fmt.Sprintf("%d", track))
	}
	if err := tag.Save(); err != nil {
		t.Fatal(err)
	}
}

func newTestService(t *testing.T) (*Service, *db.Queries, string, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	c, err := cache.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	actLog, err := activity.NewLog(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { actLog.Close() })

	queries := db.NewQueries(database)
	providers := provider.NewManager(c, queries)
	uploadDir := t.TempDir()
	libraryDir := t.TempDir()
	return NewService(queries, providers, uploadDir, libraryDir, actLog, nil), queries, uploadDir, libraryDir
}

func seedWantedTrack(t *testing.T, q *db.Queries, artistName, albumTitle, trackTitle string) *models.Track {
	t.Helper()
	artist := &models.Artist{Name: artistName, Provider: "musicbrainz", ProviderID: "mb-artist", Status: models.ArtistStatusWatched}
	if err := q.CreateArtist(artist); err != nil {
		t.Fatal(err)
	}
	album := &models.Album{ArtistID: artist.ID, Title: albumTitle, Provider: "musicbrainz", ProviderID: "mb-album", Status: models.AlbumStatusWatched}
	if err := q.CreateAlbum(album); err != nil {
		t.Fatal(err)
	}
	track := &models.Track{AlbumID: album.ID, Title: trackTitle, TrackNumber: 1, Provider: "musicbrainz", ProviderID: "mb-track", Status: models.TrackStatusWanted}
	if err := q.CreateTrack(track); err != nil {
		t.Fatal(err)
	}
	return track
}

func TestUploadIdentifyCommit(t *testing.T) {
	s, q, uploadDir, libraryDir := newTestService(t)
	track := seedWantedTrack(t, q, "Radiohead", "OK Computer", "Paranoid Android")

	// Stage a tagged mp3 into a batch.
	src := filepath.Join(t.TempDir(), "song.mp3")
	writeMP3(t, src, "Radiohead", "OK Computer", "Paranoid Android", 1)

	fh, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()

	staged, err := s.Stage("batch1", "song.mp3", fh, 0)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if staged.State != models.UploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", staged.State)
	}

	if err := s.Identify(context.Background(), "batch1"); err != nil {
		t.Fatalf("identify: %v", err)
	}

	view, err := s.Batch("batch1")
	if err != nil {
		t.Fatalf("batch view: %v", err)
	}
	if view.Identified != 1 {
		t.Fatalf("identified = %d, want 1 (file state %v)", view.Identified, view.Files[0].State)
	}
	m := view.Files[0].Match
	if m == nil || m.TrackID != track.ID {
		t.Fatalf("match = %+v, want track %d", m, track.ID)
	}

	res, err := s.Commit(context.Background(), "batch1", "skip")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(res.Committed) != 1 || len(res.Failed) != 0 {
		t.Fatalf("commit result = %+v", res)
	}

	updated, err := q.GetTrack(track.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != models.TrackStatusOwned {
		t.Fatalf("track status = %q, want owned", updated.Status)
	}
	if updated.FilePath == nil || !strings.HasSuffix(*updated.FilePath, ".mp3") {
		t.Fatalf("file_path = %v", updated.FilePath)
	}
	dest := filepath.Join(libraryDir, *updated.FilePath)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected file at %s: %v", dest, err)
	}
	// Staged file should be gone (moved).
	if _, err := os.Stat(staged.StagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file still exists: %s", staged.StagedPath)
	}
	_ = uploadDir
}

func TestUploadUnidentifiedMissingTags(t *testing.T) {
	s, _, uploadDir, _ := newTestService(t)
	// Untagged file: no artist/title → unidentified.
	path := filepath.Join(uploadDir, "b1", "bare.mp3")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{0xFF, 0xFB, 0x90}, 0644); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := s.Stage("b1", "bare.mp3", fh, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Identify(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}
	view, err := s.Batch("b1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Unidentified != 1 {
		t.Fatalf("unidentified = %d, want 1", view.Unidentified)
	}
}

func TestUploadDuplicateSkip(t *testing.T) {
	s, q, _, libraryDir := newTestService(t)
	track := seedWantedTrack(t, q, "Artist", "Album", "Song")
	// Pretend the track is already owned.
	owned := "Artist/Album/01 - Song.mp3"
	abs := filepath.Join(libraryDir, owned)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatal(err)
	}
	writeMP3(t, abs, "Artist", "Album", "Song", 1)
	if err := q.ClaimTrackFile(track.ID, owned, "mp3", 128, 0, nil); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "song2.mp3")
	writeMP3(t, src, "Artist", "Album", "Song", 1)
	fh, _ := os.Open(src)
	defer fh.Close()
	if _, err := s.Stage("b2", "song2.mp3", fh, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Identify(context.Background(), "b2"); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Batch("b2")
	if view.Files[0].Match == nil || !view.Files[0].Match.Duplicate {
		t.Fatalf("expected duplicate flag, match = %+v", view.Files[0].Match)
	}
	res, err := s.Commit(context.Background(), "b2", "skip")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || len(res.Committed) != 0 {
		t.Fatalf("expected skip, got %+v", res)
	}
}

func TestValidBatchID(t *testing.T) {
	for name, want := range map[string]bool{
		"abc-123":     true,
		"a_b":         true,
		"../etc":      false,
		"a/b":         false,
		"":            false,
		"a b":         false,
		"..":          false,
		"batch\\evil": false,
	} {
		if got := validBatchID(name); got != want {
			t.Errorf("validBatchID(%q) = %v, want %v", name, got, want)
		}
	}
}
