package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// providerPlaylistResponse is a provider playlist. Unlike an album it belongs to one
// provider, so its id is the library's own and the provider is named.
type providerPlaylistResponse struct {
	ID string `json:"id"`
	// ProviderPlaylistID is the platform's own id. ID is this library's row for
	// the playlist; importing one by id needs the platform's.
	ProviderPlaylistID string `json:"providerPlaylistId"`
	Provider           string `json:"provider"`
	Title              string `json:"title"`
	Owner              string `json:"owner,omitempty"`
	Description        string `json:"description,omitempty"`
	TrackCount         int    `json:"trackCount"`
	ArtworkURL         string `json:"artworkUrl,omitempty"`
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
	query, limit, providers, ok := s.searchRequest(w, r)
	if !ok {
		return
	}
	value, err := s.searches.do(r.Context(), searchKey("playlists", query, providers, limit), func() (any, error) {
		// Detached: one caller hanging up must not take a shared answer with it.
		return s.playlistSearchAnswer(context.WithoutCancel(r.Context()), query, limit, providers)
	})
	if err != nil {
		writeStoreError(w, err, "playlist search failed")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// playlistSearchAnswer is the playlist search itself, with the shape, the
// caching and the sharing the other three searches have.
func (s *Server) playlistSearchAnswer(ctx context.Context, query string, limit int, providers []string) (any, error) {
	playlists, problems, err := s.library.SearchPlaylists(ctx, query, limit, providers)
	if err != nil {
		return nil, err
	}
	out := make([]providerPlaylistResponse, 0, len(playlists))
	for i := range playlists {
		response, err := s.buildPlaylist(ctx, &playlists[i])
		if err != nil {
			return nil, err
		}
		out = append(out, response)
	}
	return map[string]any{
		"query":          query,
		"playlists":      out,
		"providerErrors": problemsJSON(problems),
	}, nil
}

// providerTrackResponse is one track of a platform playlist as the provider
// lists it, before any of it is in the library: browsing is not importing, so
// these rows carry no library id to play or queue.
type providerTrackResponse struct {
	ProviderTrackID string   `json:"providerTrackId"`
	Title           string   `json:"title"`
	Artists         []string `json:"artists,omitempty"`
	DurationMs      int64    `json:"durationMs,omitempty"`
	ArtworkURL      string   `json:"artworkUrl,omitempty"`
}

// providerTracksJSON shapes provider tracks for a browse: what a page shows,
// with no library id on any of them, because none exists until the playlist is
// imported.
func providerTracksJSON(tracks []provider.Track) []providerTrackResponse {
	out := make([]providerTrackResponse, 0, len(tracks))
	for _, track := range tracks {
		out = append(out, providerTrackResponse{
			ProviderTrackID: track.ProviderTrackID,
			Title:           track.Title,
			Artists:         track.Artists,
			DurationMs:      track.DurationMs,
			ArtworkURL:      track.ArtworkURL,
		})
	}
	return out
}

// handleProviderPlaylist shows a platform playlist as the provider has it, and
// writes nothing: it is the page that opens when somebody clicks a playlist in
// search, which must show the songs rather than start an import.
//
// The playlist can be named two ways, because a click out of search carries one
// and a pasted link carries the other: the id of the library's row for it, or -
// with `provider` - the platform's own id, which the library may never have
// seen. Either way the tracks are read live.
func (s *Server) handleProviderPlaylist(w http.ResponseWriter, r *http.Request) {
	// A row id is a row id whatever else the request says. `provider` marks the
	// id as the platform's own, and a caller that sends both by mistake - a
	// click carrying the row it came from - would otherwise have a UUID read as
	// a playlist id on the platform, which is a provider error rather than an
	// answer. The library's own ids are uuids; the platforms' never are.
	id := strings.TrimSpace(r.PathValue("playlistId"))
	providerName := strings.TrimSpace(r.URL.Query().Get("provider"))
	if providerName != "" {
		if _, err := uuid.Parse(id); err == nil {
			providerName = ""
		}
	}
	if providerName != "" {
		tracks, err := s.library.ProviderPlaylistByPlatform(r.Context(), providerName, id)
		if err != nil {
			writeLibraryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"playlist": map[string]any{"provider": providerName, "providerPlaylistId": id},
			"tracks":   providerTracksJSON(tracks),
		})
		return
	}

	playlistID, ok := s.playlistID(w, r)
	if !ok {
		return
	}
	playlist, tracks, err := s.library.ProviderPlaylist(r.Context(), playlistID)
	if err != nil {
		writeLibraryError(w, err)
		return
	}
	shaped, err := s.buildPlaylist(r.Context(), playlist)
	if err != nil {
		writeStoreError(w, err, "playlist read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playlist": shaped,
		"tracks":   providerTracksJSON(tracks),
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
		ID:                 playlist.ID.String(),
		ProviderPlaylistID: playlist.ProviderPlaylistID,
		Provider:           playlist.Provider,
		Title:              playlist.Title,
		Owner:              playlist.Owner,
		Description:        playlist.Description,
		TrackCount:         playlist.TrackCount,
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
