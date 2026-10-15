package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/services/discover"
)

const (
	discoverImageProvider = "deezer" // same source enrichArtistImage uses
	discoverImageTTL      = 14 * 24 * time.Hour
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
	if dismissed, derr := s.queries.ListDiscoverDismissals(); derr == nil && len(dismissed) > 0 {
		kept := suggestions[:0]
		for _, sg := range suggestions {
			if !dismissed[discover.DismissalKey(sg)] {
				kept = append(kept, sg)
			}
		}
		suggestions = kept
	}
	s.enrichSuggestionImages(r.Context(), suggestions, refresh)
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "suggestions": suggestions})
}

// handleDismissDiscover records a suggestion the user doesn't want to see —
// it stays dismissed across refreshes (no un-dismiss UI for now).
func (s *Server) handleDismissDiscover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeError(w, http.StatusBadRequest, "key required")
		return
	}
	if err := s.queries.DismissDiscoverSuggestion(req.Key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// enrichSuggestionImages fills ImageURL on each suggestion by looking the name
// up on the image provider (deezer). Results — including misses — cache two
// weeks so repeat page loads don't re-search. Runs a few lookups in parallel;
// the provider's own rate limiter keeps it inside the deezer budget.
func (s *Server) enrichSuggestionImages(ctx context.Context, suggestions []discover.Suggestion, refresh bool) {
	if !s.providers.IsHealthy(discoverImageProvider) {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range suggestions {
		wg.Add(1)
		go func(sug *discover.Suggestion) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sug.ImageURL = s.suggestionImage(ctx, sug.Name, refresh)
		}(&suggestions[i])
	}
	wg.Wait()
}

func (s *Server) suggestionImage(ctx context.Context, name string, refresh bool) string {
	cacheKey := "discover:image:" + strings.ToLower(strings.TrimSpace(name))
	if !refresh {
		if raw, hit := s.cache.Get(cacheKey); hit {
			var cached string
			if json.Unmarshal(raw, &cached) == nil {
				return cached
			}
		}
	}
	res, err := s.providers.SearchWithProvider(ctx, discoverImageProvider, name, 5, 0)
	if err != nil {
		slog.Debug("discover: image lookup failed", "artist", name, "error", err)
		return ""
	}
	url := ""
	var fallback string
	for _, a := range res.Artists {
		if a.ImageUrl == "" {
			continue
		}
		if fallback == "" {
			fallback = a.ImageUrl
		}
		if strings.EqualFold(a.Name, name) {
			url = a.ImageUrl
			break
		}
	}
	if url == "" {
		url = fallback
	}
	if raw, err := json.Marshal(url); err == nil {
		s.cache.Set(cacheKey, raw, discoverImageTTL)
	}
	return url
}
