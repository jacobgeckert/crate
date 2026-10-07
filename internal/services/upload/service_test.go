package upload

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	id3v2 "github.com/bogem/id3v2/v2"
	"google.golang.org/grpc"

	"github.com/TheOutdoorProgrammer/crate/internal/activity"
	"github.com/TheOutdoorProgrammer/crate/internal/cache"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/provider"
	pb "github.com/TheOutdoorProgrammer/crate/proto/provider"
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

// --- Fake gRPC provider (album details only) ---

type fakeAlbumProvider struct {
	pb.UnimplementedMusicProviderServer
	albums map[string]*pb.AlbumDetail
}

func (f *fakeAlbumProvider) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "musicbrainz", DisplayName: "MB", Version: "1"}, nil
}

func (f *fakeAlbumProvider) GetAlbum(_ context.Context, req *pb.EntityRequest) (*pb.AlbumDetail, error) {
	if d, ok := f.albums[req.Id]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("album not found")
}

func startFakeAlbumProvider(t *testing.T, albums map[string]*pb.AlbumDetail) string {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterMusicProviderServer(srv, &fakeAlbumProvider{albums: albums})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// writeMBMP3 writes a tagged MP3 with MusicBrainz TXXX frames.
func writeMBMP3(t *testing.T, path, artist, album, title string, txxx map[string]string) {
	t.Helper()
	writeMP3(t, path, artist, album, title, 1)
	tag, err := id3v2.Open(path, id3v2.Options{Parse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tag.Close()
	for desc, val := range txxx {
		tag.AddUserDefinedTextFrame(id3v2.UserDefinedTextFrame{
			Encoding:    id3v2.EncodingUTF8,
			Description: desc,
			Value:       val,
		})
	}
	if err := tag.Save(); err != nil {
		t.Fatal(err)
	}
}

func editionFixtures() map[string]*pb.AlbumDetail {
	return map[string]*pb.AlbumDetail{
		// Release-group detail — default edition's tracklist + the editions list.
		"rg-1": {
			Id: "rg-1", Title: "Album Two", Year: 2024, ArtistName: "Test Artist",
			Tracks: []*pb.TrackInfo{
				{Id: "3002", Title: "Song One", TrackNumber: 1, Metadata: map[string]string{"recording_id": "rec-1"}},
			},
			Metadata: map[string]string{"releases": `[
				{"id":"rel-std","title":"Album Two","status":"Official","date":"2024-01-01","country":"US","track_count":1},
				{"id":"rel-deluxe","title":"Album Two","status":"Official","date":"2024-06-01","country":"JP","disambiguation":"deluxe","track_count":2}
			]`},
		},
		// The deluxe edition — different release-track ids + a bonus track.
		"rel-deluxe": {
			Id: "rel-deluxe", Title: "Album Two", Year: 2024, ArtistName: "Test Artist",
			Tracks: []*pb.TrackInfo{
				{Id: "4001", Title: "Song One", TrackNumber: 1, Metadata: map[string]string{"recording_id": "rec-1"}},
				{Id: "4002", Title: "Bonus", TrackNumber: 2, Metadata: map[string]string{"recording_id": "rec-2"}},
			},
		},
	}
}

func TestSetGroupRelease(t *testing.T) {
	s, q, _, libraryDir := newTestService(t)
	addr := startFakeAlbumProvider(t, editionFixtures())
	if err := s.providers.RegisterProvider(context.Background(), "musicbrainz", addr); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "song.mp3")
	writeMBMP3(t, src, "Test Artist", "Album Two", "Song One", map[string]string{
		"MusicBrainz album artist id":  "mb-artist-1",
		"MusicBrainz release group id": "rg-1",
	})
	fh, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := s.Stage("b1", "song.mp3", fh, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Identify(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}

	view, err := s.Batch("b1")
	if err != nil {
		t.Fatal(err)
	}
	m := view.Files[0].Match
	if m == nil || m.Album == nil || !m.Album.New || m.Album.ProviderID != "rg-1" {
		t.Fatalf("expected new-album proposal for rg-1, got %+v", m)
	}
	if m.Album.ReleaseID != "" {
		t.Fatalf("unexpected release pin: %q", m.Album.ReleaseID)
	}
	if m.ProviderTrackID != "3002" {
		t.Fatalf("provider track = %q, want 3002 (default edition)", m.ProviderTrackID)
	}

	// Pin the deluxe edition — the file re-matches to its tracklist.
	rel := "rel-deluxe"
	if err := s.SetGroupRelease(context.Background(), "b1", "musicbrainz", "rg-1", &rel); err != nil {
		t.Fatal(err)
	}
	view, _ = s.Batch("b1")
	m = view.Files[0].Match
	if m.Album.ReleaseID != "rel-deluxe" {
		t.Fatalf("release pin = %q, want rel-deluxe", m.Album.ReleaseID)
	}
	if m.ProviderTrackID != "4001" {
		t.Fatalf("provider track = %q, want 4001 (deluxe edition)", m.ProviderTrackID)
	}

	// Commit creates the album pinned to the deluxe edition's tracklist.
	res, err := s.Commit(context.Background(), "b1", "skip")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 1 {
		t.Fatalf("commit result = %+v", res)
	}
	album, err := q.FindAlbumByProvider("musicbrainz", "rg-1")
	if err != nil || album == nil {
		t.Fatal("album not created")
	}
	if album.ReleaseID == nil || *album.ReleaseID != "rel-deluxe" {
		t.Fatalf("album release pin = %v, want rel-deluxe", album.ReleaseID)
	}
	tracks, err := q.ListTracksByAlbum(album.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("track count = %d, want 2 (deluxe edition)", len(tracks))
	}
	var owned *models.Track
	for i := range tracks {
		if tracks[i].Status == models.TrackStatusOwned {
			owned = &tracks[i]
		}
	}
	if owned == nil || owned.FilePath == nil {
		t.Fatalf("expected one owned track with file_path, got %+v", tracks)
	}
	if _, err := os.Stat(filepath.Join(libraryDir, *owned.FilePath)); err != nil {
		t.Fatalf("committed file missing in library: %v", err)
	}
}

func TestSetGroupReleaseSkipsOtherGroups(t *testing.T) {
	s, q, _, _ := newTestService(t)
	addr := startFakeAlbumProvider(t, editionFixtures())
	if err := s.providers.RegisterProvider(context.Background(), "musicbrainz", addr); err != nil {
		t.Fatal(err)
	}

	// File A: new provider album rg-1. File B: existing library album.
	libTrack := seedWantedTrack(t, q, "Artist", "Album", "Song")
	srcA := filepath.Join(t.TempDir(), "a.mp3")
	writeMBMP3(t, srcA, "Test Artist", "Album Two", "Song One", map[string]string{
		"MusicBrainz album artist id":  "mb-artist-1",
		"MusicBrainz release group id": "rg-1",
	})
	srcB := filepath.Join(t.TempDir(), "b.mp3")
	writeMP3(t, srcB, "Artist", "Album", "Song", 1)
	for i, src := range []string{srcA, srcB} {
		fh, err := os.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stage("b2", fmt.Sprintf("f%d.mp3", i), fh, 0); err != nil {
			fh.Close()
			t.Fatal(err)
		}
		fh.Close()
	}
	if err := s.Identify(context.Background(), "b2"); err != nil {
		t.Fatal(err)
	}

	rel := "rel-deluxe"
	if err := s.SetGroupRelease(context.Background(), "b2", "musicbrainz", "rg-1", &rel); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Batch("b2")
	var libMatch, newMatch *Match
	for i := range view.Files {
		m := view.Files[i].Match
		if m != nil && m.TrackID == libTrack.ID {
			libMatch = m
		}
		if m != nil && m.Album != nil && m.Album.New {
			newMatch = m
		}
	}
	if newMatch == nil || newMatch.Album.ReleaseID != "rel-deluxe" {
		t.Fatalf("new group not repinned: %+v", newMatch)
	}
	if libMatch == nil || libMatch.Album.ReleaseID != "" {
		t.Fatalf("library match touched: %+v", libMatch)
	}
}

func TestIdentifyAutoPinsTaggedEdition(t *testing.T) {
	s, _, _, _ := newTestService(t)
	addr := startFakeAlbumProvider(t, editionFixtures())
	if err := s.providers.RegisterProvider(context.Background(), "musicbrainz", addr); err != nil {
		t.Fatal(err)
	}

	// File tagged by Picard against the deluxe edition.
	src := filepath.Join(t.TempDir(), "song.mp3")
	writeMBMP3(t, src, "Test Artist", "Album Two", "Song One", map[string]string{
		"MusicBrainz album artist id":  "mb-artist-1",
		"MusicBrainz release group id": "rg-1",
		"MusicBrainz album id":         "rel-deluxe",
	})
	fh, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := s.Stage("b3", "song.mp3", fh, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Identify(context.Background(), "b3"); err != nil {
		t.Fatal(err)
	}
	view, _ := s.Batch("b3")
	m := view.Files[0].Match
	if m == nil || m.Album == nil || m.Album.ReleaseID != "rel-deluxe" {
		t.Fatalf("expected auto-pin to rel-deluxe, got %+v", m)
	}
	if m.ProviderTrackID != "4001" {
		t.Fatalf("provider track = %q, want 4001 (edition tracklist)", m.ProviderTrackID)
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
