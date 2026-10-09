package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TheOutdoorProgrammer/crate/internal/models"
)

// Smallest valid PNG (1x1).
var pngBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
	0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

func uploadImage(t *testing.T, env *testEnv, path string, body []byte, fname string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", fname)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	env.server.ServeHTTP(w, req)
	env.server.WaitBackground()
	return w
}

func TestUploadArtistImage(t *testing.T) {
	env := newTestEnv(t)
	artist := models.Artist{Name: "Art", Provider: "test", ProviderID: "a1", Status: models.ArtistStatusWatched}
	if err := env.queries.CreateArtist(&artist); err != nil {
		t.Fatal(err)
	}
	path := "/api/images/upload/artist/1"

	w := uploadImage(t, env, path, pngBytes, "pic.png")
	if w.Code != http.StatusOK {
		t.Fatalf("upload: got %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.ImageURL, "/api/images/artist-1-") {
		t.Fatalf("unexpected image_url %q", resp.ImageURL)
	}

	// Served back with the right content-type.
	req := httptest.NewRequest(http.MethodGet, resp.ImageURL, nil)
	w = httptest.NewRecorder()
	env.server.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("serve: got %d", w.Code)
	}
	serveBody, _ := io.ReadAll(w.Result().Body)
	if !bytes.Equal(serveBody, pngBytes) {
		t.Fatal("served bytes differ from upload")
	}

	// A second upload supersedes the first and sweeps the old file.
	w = uploadImage(t, env, path, pngBytes, "pic2.png")
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload: got %d", w.Code)
	}
	var resp2 struct {
		ImageURL string `json:"image_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp2)
	if resp2.ImageURL == resp.ImageURL {
		t.Fatal("re-upload returned the same URL — expected a fresh name")
	}
	// Old upload was swept.
	req = httptest.NewRequest(http.MethodGet, resp.ImageURL, nil)
	w = httptest.NewRecorder()
	env.server.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("old upload still served: got %d", w.Code)
	}
}

func TestUploadImageTraversalAndValidation(t *testing.T) {
	env := newTestEnv(t)
	artist := models.Artist{Name: "Art", Provider: "test", ProviderID: "a1", Status: models.ArtistStatusWatched}
	if err := env.queries.CreateArtist(&artist); err != nil {
		t.Fatal(err)
	}

	// Traversal is rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/images/..%2f..%2fcrate.db", nil)
	w := httptest.NewRecorder()
	env.server.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("traversal request served a file")
	}

	// Non-image bytes are rejected.
	w = uploadImage(t, env, "/api/images/upload/artist/1", []byte("not an image"), "x.png")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-image upload: got %d", w.Code)
	}

	// Unknown artist 404s.
	w = uploadImage(t, env, "/api/images/upload/artist/999", pngBytes, "x.png")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing artist: got %d", w.Code)
	}
}
