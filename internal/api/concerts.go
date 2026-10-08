package api

import (
	"errors"
	"net/http"

	"github.com/TheOutdoorProgrammer/crate/internal/services/concerts"
)

func (s *Server) handleGetConcerts(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "true" || r.URL.Query().Get("refresh") == "1"
	shows, err := s.concerts.Shows(r.Context(), refresh)
	if errors.Is(err, concerts.ErrNotConfigured) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "shows": []concerts.Show{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch concerts: "+err.Error())
		return
	}
	if shows == nil {
		shows = []concerts.Show{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "shows": shows})
}
