package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/radio"
	"github.com/kyleraykbs/MusOak/internal/store"
)

type radioRequest struct {
	SeedTrackID string `json:"seedTrackId"`
	// Providers selects what the station is made of. Empty means every
	// enabled provider that can build one.
	Providers []string `json:"providers"`
	Length    int      `json:"length"`
	// Save stores the station as a playlist of the caller's. It needs a login,
	// because playlists belong to a user.
	Save *bool  `json:"save"`
	Name string `json:"name"`
}

type radioResponse struct {
	Seed           trackResponse     `json:"seed"`
	Providers      []string          `json:"providers"`
	ProviderErrors []providerProblem `json:"providerErrors,omitempty"`
	Tracks         []trackResponse   `json:"tracks"`
	Playlist       *playlistResponse `json:"playlist,omitempty"`
}

// handleRadio starts a station from a seed track, optionally saving it as a
// playlist. Guests may listen to a generated station; saving needs an account.
func (s *Server) handleRadio(w http.ResponseWriter, r *http.Request) {
	var req radioRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SeedTrackID == "" {
		writeError(w, http.StatusBadRequest, "seedTrackId is required")
		return
	}
	seedID, err := uuid.Parse(req.SeedTrackID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid seedTrackId")
		return
	}
	for _, name := range req.Providers {
		p, ok := s.providers.Get(name)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown provider "+name)
			return
		}
		if !p.Capabilities().Radio {
			// A provider that cannot build a station is not a provider that
			// was asked and had nothing: naming it here is what tells the
			// caller which of its choices cannot contribute at all, instead of
			// silently building the station without it (or reporting every
			// provider as having no rendition of the seed, which is not why).
			writeError(w, http.StatusBadRequest, "provider "+name+" cannot build a radio")
			return
		}
	}

	result, err := s.radio.Generate(r.Context(), seedID, req.Providers, req.Length)
	switch {
	case errors.Is(err, radio.ErrNoSeed):
		writeError(w, http.StatusNotFound, "seed track not found")
		return
	case errors.Is(err, radio.ErrNoSeedVariant):
		writeError(w, http.StatusConflict, noSeedVariantMessage(result))
		return
	case err != nil:
		writeStoreError(w, err, "could not build the radio")
		return
	}
	if len(result.Tracks) == 0 {
		// An empty station is not a station. Answering with an empty list says
		// nothing about why - a provider that could not be reached, one that
		// had nothing to suggest and one that was never asked all look the
		// same - so the reason is what comes back.
		writeError(w, emptyStationStatus(result), emptyStationMessage(result))
		return
	}

	response := radioResponse{
		Providers: result.Providers,
		Tracks:    make([]trackResponse, 0, len(result.Tracks)),
	}
	seed, err := s.buildTrack(r.Context(), result.Seed)
	if err != nil {
		writeStoreError(w, err, "could not build the radio")
		return
	}
	response.Seed = seed
	for _, track := range result.Tracks {
		built, err := s.buildTrack(r.Context(), track)
		if err != nil {
			writeStoreError(w, err, "could not build the radio")
			return
		}
		response.Tracks = append(response.Tracks, built)
	}
	refs := append([]*trackResponse{&response.Seed}, trackRefs(response.Tracks)...)
	s.withPlays(r.Context(), s.currentUser(r), refs)
	for _, problem := range result.Errors {
		response.ProviderErrors = append(response.ProviderErrors, providerProblem{
			Provider: problem.Provider,
			Error:    problem.Error,
		})
	}

	// Saving is a convenience: radio + playlist is the useful pair.
	save := req.Save == nil || *req.Save
	if save {
		user, ok := s.requireUser(w, r)
		if !ok {
			return
		}
		playlist, err := s.saveRadio(r, user.ID, req.Name, result)
		if err != nil {
			writeStoreError(w, err, "could not save the radio")
			return
		}
		response.Playlist = playlist
	}

	writeJSON(w, http.StatusOK, response)
}

// saveRadio stores a generated station as a playlist.
func (s *Server) saveRadio(r *http.Request, userID uuid.UUID, name string, result *radio.Result) (*playlistResponse, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = result.Seed.Title + " radio"
		if len(result.Providers) > 0 {
			name += " (" + strings.Join(result.Providers, "+") + ")"
		}
	}

	playlist := &store.Playlist{UserID: userID, Name: name}
	if err := s.store.CreatePlaylist(r.Context(), playlist); err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(result.Tracks))
	for _, track := range result.Tracks {
		ids = append(ids, track.ID)
	}
	if err := s.store.AppendPlaylistItems(r.Context(), playlist.ID, ids); err != nil {
		return nil, err
	}

	playlist.TrackCount = len(ids)
	// A new playlist is public until its owner says otherwise, which is the
	// column's own default; the row is what says so, and it was just written.
	playlist.Public = true
	summary := playlistSummary(playlist, userID)
	return &summary, nil
}

// providerKnown reports whether name is an enabled provider.
func (s *Server) providerKnown(name string) bool {
	_, ok := s.providers.Get(name)
	return ok
}

// noSeedVariantMessage says which providers were asked for: the station could
// not be grown from the seed because none of them has a rendition of it, and
// which ones those are is what tells the caller what to resolve.
func noSeedVariantMessage(result *radio.Result) string {
	if result == nil || len(result.Providers) == 0 {
		return "none of the selected providers has a rendition of the seed track; resolve it first"
	}
	return "none of " + strings.Join(result.Providers, ", ") +
		" has a rendition of the seed track; resolve it first"
}

// emptyStationStatus is what an empty station is reported as. Every provider
// that was asked failing is an upstream failure and worth another try; nothing
// at all coming back from providers that answered is not, and repeating the
// request will not produce a station either.
func emptyStationStatus(result *radio.Result) int {
	if len(result.Errors) > 0 {
		return http.StatusBadGateway
	}
	return http.StatusConflict
}

// emptyStationMessage says why a station came back empty: the providers that
// failed, by name and in their own words, or - when none of them failed - that
// they had no suggestions for this seed.
func emptyStationMessage(result *radio.Result) string {
	if len(result.Errors) == 0 {
		return "no station could be built: the providers had no suggestions for this seed"
	}
	reasons := make([]string, 0, len(result.Errors))
	for _, problem := range result.Errors {
		reasons = append(reasons, problem.Provider+": "+problem.Error)
	}
	return "no station could be built: " + strings.Join(reasons, "; ")
}
