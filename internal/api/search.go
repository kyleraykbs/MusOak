package api

import (
	"net/http"
	"strconv"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
)

type searchResponse struct {
	Query          string            `json:"query"`
	Groups         []searchGroup     `json:"groups"`
	ProviderErrors []providerProblem `json:"providerErrors,omitempty"`
}

type searchGroup struct {
	Track    trackResponse     `json:"track"`
	Variants []variantResponse `json:"variants"`
}

// providerProblem reports one provider's failure without failing the search.
type providerProblem struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

// handleSearch fans out to every enabled provider, merges the hits into
// canonical tracks and reports provider failures alongside the results.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit := intParam(r, "limit", defaultSearchLimit)
	if limit < 1 {
		limit = 1
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	results := s.providers.Search(r.Context(), query, provider.SearchOpts{Limit: limit})

	groups, err := s.matcher.Group(r.Context(), results)
	if err != nil {
		writeStoreError(w, err, "search failed")
		return
	}

	response := searchResponse{Query: query, Groups: make([]searchGroup, 0, len(groups))}
	for _, group := range groups {
		track, err := s.buildTrack(r.Context(), group.Track)
		if err != nil {
			writeStoreError(w, err, "search failed")
			return
		}
		response.Groups = append(response.Groups, searchGroup{
			Track:    track,
			Variants: s.buildVariants(r.Context(), group.Variants),
		})
	}
	for _, result := range results {
		if result.Err != nil && result.IsSearchable() {
			response.ProviderErrors = append(response.ProviderErrors, providerProblem{
				Provider: result.Provider,
				Error:    result.Err.Error(),
			})
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// intParam reads a bounded integer query parameter.
func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
