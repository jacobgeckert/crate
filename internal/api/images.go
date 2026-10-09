package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Uploaded artist images / album covers live in <dataDir>/images as
// <kind>-<id>-<ts>.<ext> and are served back at /api/images/<name>. The unique
// filename makes cache-busting free — re-uploading just mints a new name and
// the old file is swept.
const maxImageUpload = 12 << 20 // 12MiB — comfortably covers phone photos

// handleServeImage streams an uploaded image. The name is generated
// server-side; the basename check rejects any traversal attempt.
func (s *Server) handleServeImage(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	path := filepath.Join(s.imagesDir, name)
	if _, err := os.Stat(path); err != nil { // #nosec G703 -- name is basename-verified, no traversal
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	// Filenames are unique per upload — cache forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path) // #nosec G703 -- name is basename-verified above
}

func (s *Server) handleUploadArtistImage(w http.ResponseWriter, r *http.Request) {
	s.handleUploadImage(w, r, "artist")
}

func (s *Server) handleUploadAlbumCover(w http.ResponseWriter, r *http.Request) {
	s.handleUploadImage(w, r, "album")
}

func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request, kind string) {
	id, err := parseID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxImageUpload)
	if err := r.ParseMultipartForm(4 << 20); err != nil { // #nosec G120 -- r.Body is bounded by MaxBytesReader on the line above
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["file"]) == 0 {
		writeError(w, http.StatusBadRequest, "no file uploaded")
		return
	}
	fh := r.MultipartForm.File["file"][0]
	src, err := fh.Open()
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable upload")
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, maxImageUpload+1))
	if err != nil || int64(len(data)) > maxImageUpload {
		writeError(w, http.StatusRequestEntityTooLarge, "image too large (max 12 MB)")
		return
	}

	// Trust the bytes, not the declared content-type.
	ext, ok := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/webp": ".webp",
		"image/gif":  ".gif",
	}[http.DetectContentType(data)]
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "file must be a jpeg, png, webp, or gif image")
		return
	}

	var setter func(int64, string) error
	var urlKey string
	if kind == "artist" {
		if _, err := s.queries.GetArtist(id); errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "artist not found")
			return
		}
		setter, urlKey = s.queries.SetArtistImageURL, "image_url"
	} else {
		if _, err := s.queries.GetAlbum(id); errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "album not found")
			return
		}
		setter, urlKey = s.queries.SetAlbumCoverURL, "cover_url"
	}

	if err := os.MkdirAll(s.imagesDir, 0750); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create image dir")
		return
	}
	name := fmt.Sprintf("%s-%d-%d%s", kind, id, time.Now().UnixNano(), ext)
	if err := os.WriteFile(filepath.Join(s.imagesDir, name), data, 0644); err != nil { // #nosec G306 G703 -- name is server-generated, served publicly
		writeError(w, http.StatusInternalServerError, "failed to store image")
		return
	}
	url := "/api/images/" + name
	if err := setter(id, url); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save image")
		return
	}

	// Sweep previous uploads for this entity — the new name is already stored.
	if old, err := filepath.Glob(filepath.Join(s.imagesDir, fmt.Sprintf("%s-%d-*", kind, id))); err == nil {
		for _, p := range old {
			if filepath.Base(p) != name {
				if rerr := os.Remove(p); rerr != nil { // #nosec G703 -- glob output stays inside imagesDir
					slog.Warn("images: sweep old upload", "path", p, "error", rerr)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{urlKey: url})
}
