package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/services/upload"
)

// Uploads

func newBatchID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s", strconv.FormatInt(time.Now().UnixNano(), 36), hex.EncodeToString(b[:]))
}

// handleUploadFiles accepts a multipart batch, stages the files, and runs
// identification synchronously before returning the review payload.
func (s *Server) handleUploadFiles(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	// Bound the body here as well as at the route — the global 5MiB cap is
	// bypassed for this prefix, so this reader is the real ceiling.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil { // #nosec G120 -- r.Body is bounded by MaxBytesReader on the line above
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	form := r.MultipartForm
	if form == nil || len(form.File["files"]) == 0 {
		writeError(w, http.StatusBadRequest, "no files uploaded")
		return
	}

	batchID := newBatchID()
	staged := 0
	for _, fh := range form.File["files"] {
		src, err := fh.Open()
		if err != nil {
			continue
		}
		_, err = s.uploads.Stage(batchID, fh.Filename, src, fh.Size)
		src.Close()
		if err != nil {
			slog.Warn("upload stage failed", "filename", fh.Filename, "error", err)
			continue
		}
		staged++
	}
	if staged == 0 {
		writeError(w, http.StatusBadRequest, "no usable files uploaded")
		return
	}

	if err := s.uploads.Identify(r.Context(), batchID); err != nil {
		writeError(w, http.StatusInternalServerError, "identify failed: "+err.Error())
		return
	}
	view, err := s.uploads.Batch(batchID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load batch")
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleListUploadBatches(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	batches, err := s.uploads.Batches()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list upload batches")
		return
	}
	if batches == nil {
		batches = []models.UploadBatchSummary{}
	}
	writeJSON(w, http.StatusOK, batches)
}

func (s *Server) handleGetUploadBatch(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	view, err := s.uploads.Batch(chi.URLParam(r, "batch"))
	if err != nil {
		writeError(w, http.StatusNotFound, "batch not found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleIdentifyUpload(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	batch := chi.URLParam(r, "batch")
	if err := s.uploads.Identify(r.Context(), batch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	view, err := s.uploads.Batch(batch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load batch")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePatchUploadFile(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	fileID, err := strconv.ParseInt(chi.URLParam(r, "file"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file id")
		return
	}
	var req struct {
		TrackID *int64 `json:"track_id"`
		Skip    *bool  `json:"skip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Skip != nil {
		if err := s.uploads.SetSkip(fileID, *req.Skip); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update file")
			return
		}
	}
	if req.TrackID != nil {
		if err := s.uploads.SetTrackMatch(fileID, *req.TrackID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleCommitUpload(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	var req struct {
		OnDuplicate string `json:"on_duplicate"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	res, err := s.uploads.Commit(r.Context(), chi.URLParam(r, "batch"), strings.ToLower(req.OnDuplicate))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleGetUploadEditions lists the editions of a provider album that isn't in
// the library yet — the edition picker on the upload review page. Provider
// albums already in the library use GET /albums/{id}/editions instead.
func (s *Server) handleGetUploadEditions(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	providerName := r.URL.Query().Get("provider")
	id := r.URL.Query().Get("id")
	if providerName == "" || id == "" {
		writeError(w, http.StatusBadRequest, "provider and id are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	editions, err := s.uploads.Editions(ctx, providerName, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider fetch failed")
		return
	}
	if editions == nil {
		editions = []upload.AlbumEdition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"editions": editions})
}

// handleSetUploadRelease pins a release (edition) for one provider album
// inside a batch — re-matching each of its files against that edition's
// tracklist. release_id null clears the pin back to the group default.
func (s *Server) handleSetUploadRelease(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	var req struct {
		Provider   string  `json:"provider"`
		ProviderID string  `json:"provider_id"`
		ReleaseID  *string `json:"release_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Provider == "" || req.ProviderID == "" {
		writeError(w, http.StatusBadRequest, "provider and provider_id are required")
		return
	}
	batch := chi.URLParam(r, "batch")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.uploads.SetGroupRelease(ctx, batch, req.Provider, req.ProviderID, req.ReleaseID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	view, err := s.uploads.Batch(batch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load batch")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleDiscardUpload(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not configured")
		return
	}
	if err := s.uploads.Discard(chi.URLParam(r, "batch")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
