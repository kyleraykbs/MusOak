package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// providerPlaylistResponse is a provider playlist. Unlike an album it belongs to one
// provider, so its id is the library's own and the provider is named.
type providerPlaylistResponse struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Title       string `json:"title"`
	Owner       string `json:"owner,omitempty"`
	Description string `json:"description,omitempty"`
	TrackCount  int    `json:"trackCount"`
	ArtworkURL  string `json:"artworkUrl,omitempty"`
	// Synced reports whether the tracklist has been pulled into the library,
	// which is what playing or importing it needs.
	Synced bool `json:"synced"`
}

// providerPlaylistSyncResponse reports what a sync pulled in.
type providerPlaylistSyncResponse struct {
	Playlist providerPlaylistResponse `json:"playlist"`
	Added    int                      `json:"added"`
	Tracks   []trackResponse          `json:"tracks"`
	Errors   []providerProblem        `json:"providerErrors,omitempty"`
}

// handlePlaylistSearch searches the providers that can browse playlists.
func (s *Server) handlePlaylistSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit := clampLimit(intParam(r, "limit", defaultSearchLimit))

	playlists, problems, err := s.library.SearchPlaylists(r.Context(), query, limit)
	if err != nil {
		writeStoreError(w, err, "playlist search failed")
		return
	}

	out := make([]providerPlaylistResponse, 0, len(playlists))
	for i := range playlists {
		response, err := s.buildPlaylist(r.Context(), &playlists[i])
		if err != nil {
			writeStoreError(w, err, "playlist search failed")
			return
		}
		out = append(out, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":          query,
		"playlists":      out,
		"providerErrors": problemsJSON(problems),
	})
}

// handlePlaylistSync pulls a provider playlist's tracks into the library, so
// they can be played or copied into a playlist of the caller's own.
func (s *Server) handlePlaylistSync(w http.ResponseWriter, r *http.Request) {
	playlistID, ok := s.playlistID(w, r)
	if !ok {
		return
	}
	var req syncRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	if !s.providersKnown(w, req.Providers) {
		return
	}

	result, err := s.library.SyncPlaylist(r.Context(), playlistID, req.Providers)
	if err != nil {
		writeLibraryError(w, err)
		return
	}

	playlist, err := s.buildPlaylist(r.Context(), &result.Playlist)
	if err != nil {
		writeStoreError(w, err, "playlist sync failed")
		return
	}
	response := providerPlaylistSyncResponse{
		Playlist: playlist,
		Added:    result.Added,
		Tracks:   make([]trackResponse, 0, len(result.Tracks)),
	}
	for _, track := range result.Tracks {
		built, err := s.buildTrack(r.Context(), track)
		if err != nil {
			writeStoreError(w, err, "playlist sync failed")
			return
		}
		response.Tracks = append(response.Tracks, built)
	}
	writeJSON(w, http.StatusOK, response)
}

// buildPlaylist shapes one stored playlist for the API.
func (s *Server) buildPlaylist(ctx context.Context, playlist *store.ExternalPlaylist) (providerPlaylistResponse, error) {
	out := providerPlaylistResponse{
		ID:          playlist.ID.String(),
		Provider:    playlist.Provider,
		Title:       playlist.Title,
		Owner:       playlist.Owner,
		Description: playlist.Description,
		TrackCount:  playlist.TrackCount,
	}
	if playlist.ArtworkURL != "" {
		out.ArtworkURL = artworkPath(store.ArtworkPlaylist, playlist.ID)
	}
	_, tracks, err := s.library.GetPlaylist(ctx, playlist.ID)
	if err == nil && len(tracks) > 0 {
		out.Synced = true
		if out.TrackCount == 0 {
			out.TrackCount = len(tracks)
		}
	}
	return out, nil
}

// playlistID reads the playlist id from the path.
func (s *Server) playlistID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("playlistId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid playlistId")
		return uuid.Nil, false
	}
	return id, true
}
