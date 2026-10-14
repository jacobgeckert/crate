package api

import (
	"errors"
	"net/http"

	"github.com/TheOutdoorProgrammer/crate/internal/services/discover"
)

func (s *Server) handleGetDiscover(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "true" || r.URL.Query().Get("refresh") == "1"
	suggestions, err := s.discover.Suggestions(r.Context(), refresh)
	if errors.Is(err, discover.ErrNotConfigured) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "suggestions": []discover.Suggestion{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch suggestions: "+err.Error())
		return
	}
	if suggestions == nil {
		suggestions = []discover.Suggestion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "suggestions": suggestions})
}
