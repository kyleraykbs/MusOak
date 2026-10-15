package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
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
	query, limit, providers, ok := s.searchRequest(w, r)
	if !ok {
		return
	}
	response, err := s.searchTracksAnswer(r, query, limit, providers)
	if err != nil {
		writeStoreError(w, err, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// searchTracksAnswer is the tracks search, held under the same caching and
// sharing every other search is: the same question, asked again inside the
// window, is answered from here rather than from the providers.
//
// Its answer is also the only one that carries something live - each
// rendition's download state - so it is held through doLive and that state is
// asked for again on every read (freshSearchMedia).
func (s *Server) searchTracksAnswer(r *http.Request, query string, limit int, providers []string) (searchResponse, error) {
	value, err := s.searches.doLive(r.Context(), searchKey("tracks", query, providers, limit), func() (any, error) {
		// Detached: one caller hanging up must not take a shared answer with it.
		ctx := context.WithoutCancel(r.Context())
		results := s.providers.SearchSome(ctx, providers, query, provider.SearchOpts{Limit: limit})
		groups, err := s.matcher.Group(ctx, results)
		if err != nil {
			return nil, err
		}
		response := searchResponse{Query: query, Groups: make([]searchGroup, 0, len(groups))}
		// Matching user uploads surface above provider results: what the
		// household uploaded itself comes first.
		uploads, err := s.userUploadGroups(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		response.Groups = append(response.Groups, uploads...)
		for _, group := range groups {
			track, err := s.buildTrack(ctx, group.Track)
			if err != nil {
				return nil, err
			}
			response.Groups = append(response.Groups, searchGroup{
				Track:    track,
				Variants: s.buildVariants(ctx, group.Variants),
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
		return response, nil
	}, func(held any) any {
		return s.freshSearchMedia(r.Context(), held)
	})
	if err != nil {
		return searchResponse{}, err
	}
	response := value.(searchResponse)
	// The cached answer is shared by everyone who asked the same thing, so the
	// groups are copied before this account's own counts are written into them.
	response.Groups = append([]searchGroup(nil), response.Groups...)
	refs := make([]*trackResponse, 0, len(response.Groups))
	for i := range response.Groups {
		refs = append(refs, &response.Groups[i].Track)
	}
	s.withPlays(r.Context(), s.currentUser(r), refs)
	return response, nil
}

// freshSearchMedia re-reads the download state of every rendition in a held
// search answer, and answers with a copy of it carrying those states.
//
// A search answer is held for the whole window (searchanswers.go), and what it
// says about a rendition's file is live: a song downloaded a moment ago must
// not keep being reported as downloading because the question was answered
// before the download finished. The expensive parts - the providers' round
// trip and the matching - are what the window is for and stay held; the file
// state is asked for again on every read, which is one query for the listing
// plus a memory lookup per rendition (media.Manager.Status).
//
// The held answer is shared by every reader, so the groups and each group's
// renditions are copied before their states are written.
func (s *Server) freshSearchMedia(ctx context.Context, held any) any {
	response, ok := held.(searchResponse)
	if !ok {
		return held
	}
	groups := make([]searchGroup, len(response.Groups))
	copy(groups, response.Groups)

	ids := make([]uuid.UUID, 0, len(groups))
	for i := range groups {
		for _, variant := range groups[i].Variants {
			if id, err := uuid.Parse(variant.ID); err == nil {
				ids = append(ids, id)
			}
		}
	}
	ready, err := s.store.MediaFilesFor(ctx, ids)
	if err != nil {
		// A listing is still worth answering; the manager below fills every row.
		ready = nil
	}
	for i := range groups {
		variants := groups[i].Variants
		stamped := make([]variantResponse, len(variants))
		for j := range variants {
			stamped[j] = variants[j]
			id, err := uuid.Parse(variants[j].ID)
			if err != nil {
				continue
			}
			stamped[j].Media = s.variantMediaStatus(ctx, id, ready)
		}
		groups[i].Variants = stamped
	}
	response.Groups = groups
	return response
}

// searchRequest is the shape every search shares: the words, how many, and
// which providers to ask - read, checked and clamped in one place, so the four
// search endpoints cannot drift apart in what they accept.
func (s *Server) searchRequest(w http.ResponseWriter, r *http.Request) (string, int, []string, bool) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return "", 0, nil, false
	}
	limit := clampLimit(intParam(r, "limit", defaultSearchLimit))
	providers, err := s.providerNames(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", 0, nil, false
	}
	return query, limit, providers, true
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
