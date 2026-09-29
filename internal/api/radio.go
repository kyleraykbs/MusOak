package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/radio"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
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
		if !s.providerKnown(name) {
			writeError(w, http.StatusBadRequest, "unknown provider "+name)
			return
		}
	}

	result, err := s.radio.Generate(r.Context(), seedID, req.Providers, req.Length)
	switch {
	case errors.Is(err, radio.ErrNoSeed):
		writeError(w, http.StatusNotFound, "seed track not found")
		return
	case errors.Is(err, radio.ErrNoSeedVariant):
		writeError(w, http.StatusConflict,
			"none of the selected providers has a rendition of the seed track; resolve it first")
		return
	case err != nil:
		writeStoreError(w, err, "could not build the radio")
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
	summary := playlistSummary(playlist)
	return &summary, nil
}

// providerKnown reports whether name is an enabled provider.
func (s *Server) providerKnown(name string) bool {
	_, ok := s.providers.Get(name)
	return ok
}

var _ = provider.ErrRadioUnsupported
