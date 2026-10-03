package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
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
	// UserUpload marks a group of user-uploaded songs, which the response
	// lists above provider results.
	UserUpload bool `json:"userUpload,omitempty"`
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

	providers, err := s.providerNames(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	results := s.providers.SearchSome(r.Context(), providers, query, provider.SearchOpts{Limit: limit})

	groups, err := s.matcher.Group(r.Context(), results)
	if err != nil {
		writeStoreError(w, err, "search failed")
		return
	}

	response := searchResponse{Query: query, Groups: make([]searchGroup, 0, len(groups))}
	// Matching user uploads surface above provider results: what the
	// household uploaded itself comes first.
	uploads, err := s.userUploadGroups(r.Context(), query, limit)
	if err != nil {
		writeStoreError(w, err, "search failed")
		return
	}
	response.Groups = append(response.Groups, uploads...)
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
	refs := make([]*trackResponse, 0, len(response.Groups))
	for i := range response.Groups {
		refs = append(refs, &response.Groups[i].Track)
	}
	s.withPlays(r.Context(), s.currentUser(r), refs)
	writeJSON(w, http.StatusOK, response)
}

// userUploadGroups shapes the matching user uploads into search groups, one
// per canonical track, so several uploads of the same song stay together.
func (s *Server) userUploadGroups(ctx context.Context, query string, limit int) ([]searchGroup, error) {
	uploads, err := s.store.SearchUploads(ctx, query, limit)
	if err != nil || len(uploads) == 0 {
		return nil, err
	}

	groups := make([]searchGroup, 0, len(uploads))
	at := make(map[uuid.UUID]int, len(uploads))
	for i := range uploads {
		upload := &uploads[i]
		slot, seen := at[upload.Variant.TrackID]
		if !seen {
			track, err := s.store.Track(ctx, upload.Variant.TrackID)
			if err != nil {
				return nil, err
			}
			built, err := s.buildTrack(ctx, *track)
			if err != nil {
				return nil, err
			}
			groups = append(groups, searchGroup{Track: built, UserUpload: true})
			slot = len(groups) - 1
			at[upload.Variant.TrackID] = slot
		}
		groups[slot].Variants = append(groups[slot].Variants, s.buildVariant(ctx, upload.Variant))
	}
	return groups, nil
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
