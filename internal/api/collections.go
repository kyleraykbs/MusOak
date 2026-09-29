package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/library"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

type albumVariantResponse struct {
	ID              string   `json:"id"`
	Provider        string   `json:"provider"`
	ProviderAlbumID string   `json:"providerAlbumId"`
	Title           string   `json:"title"`
	Artists         []string `json:"artists"`
	Year            string   `json:"year,omitempty"`
	TrackCount      int      `json:"trackCount,omitempty"`
}

type albumResponse struct {
	ID         string                 `json:"id"`
	Title      string                 `json:"title"`
	Artists    []string               `json:"artists"`
	Year       string                 `json:"year,omitempty"`
	TrackCount int                    `json:"trackCount"`
	Providers  []string               `json:"providers"`
	ArtworkURL string                 `json:"artworkUrl,omitempty"`
	Variants   []albumVariantResponse `json:"variants,omitempty"`
	Tracks     []trackResponse        `json:"tracks,omitempty"`
}

type artistVariantResponse struct {
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	ProviderArtistID string `json:"providerArtistId"`
	Name             string `json:"name"`
}

type artistResponse struct {
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	Providers  []string                `json:"providers"`
	ArtworkURL string                  `json:"artworkUrl,omitempty"`
	Variants   []artistVariantResponse `json:"variants,omitempty"`
	Albums     []albumResponse         `json:"albums,omitempty"`
}

type syncRequest struct {
	// Providers selects what to sync from; empty means every release known.
	Providers []string `json:"providers"`
	// Resolve also makes sure each track has something playable.
	Resolve *bool `json:"resolve"`
	// SyncAlbums pulls the tracklists of an artist's albums too.
	SyncAlbums bool `json:"syncAlbums"`
}

type syncResponse struct {
	AlbumID   string            `json:"albumId,omitempty"`
	ArtistID  string            `json:"artistId,omitempty"`
	Providers []string          `json:"providers"`
	Added     int               `json:"added"`
	Tracks    []trackResponse   `json:"tracks,omitempty"`
	Albums    []albumResponse   `json:"albums,omitempty"`
	Errors    []providerProblem `json:"providerErrors,omitempty"`
}

func (s *Server) buildAlbum(ctx context.Context, album *library.Album, withTracks bool) (albumResponse, error) {
	out := albumResponse{
		ID:         album.Album.ID.String(),
		Title:      album.Album.Title,
		Artists:    make([]string, 0, len(album.Artists)),
		TrackCount: len(album.Tracks),
		Providers:  make([]string, 0, len(album.Variants)),
		Variants:   make([]albumVariantResponse, 0, len(album.Variants)),
	}
	for _, artist := range album.Artists {
		out.Artists = append(out.Artists, artist.Name)
	}
	if album.Album.ArtworkURL != "" {
		out.ArtworkURL = artworkPath(store.ArtworkAlbum, album.Album.ID)
	}
	seen := map[string]bool{}
	for _, variant := range album.Variants {
		if !seen[variant.Provider] {
			seen[variant.Provider] = true
			out.Providers = append(out.Providers, variant.Provider)
		}
		if out.Year == "" && variant.Year != "" {
			out.Year = variant.Year
		}
		out.Variants = append(out.Variants, albumVariantResponse{
			ID:              variant.ID.String(),
			Provider:        variant.Provider,
			ProviderAlbumID: variant.ProviderAlbumID,
			Title:           variant.Title,
			Artists:         nonNilStrings(variant.Artists),
			Year:            variant.Year,
			TrackCount:      variant.TrackCount,
		})
	}

	if withTracks {
		out.Tracks = make([]trackResponse, 0, len(album.Tracks))
		for _, track := range album.Tracks {
			response, err := s.buildTrack(ctx, track)
			if err != nil {
				return albumResponse{}, err
			}
			out.Tracks = append(out.Tracks, response)
		}
	}
	return out, nil
}

func (s *Server) buildArtist(ctx context.Context, artist *library.Artist, withAlbums bool) (artistResponse, error) {
	out := artistResponse{
		ID:        artist.Artist.ID.String(),
		Name:      artist.Artist.Name,
		Providers: make([]string, 0, len(artist.Variants)),
		Variants:  make([]artistVariantResponse, 0, len(artist.Variants)),
	}
	if artist.Artist.ArtworkURL != "" {
		out.ArtworkURL = artworkPath(store.ArtworkArtist, artist.Artist.ID)
	}
	for _, variant := range artist.Variants {
		out.Providers = append(out.Providers, variant.Provider)
		out.Variants = append(out.Variants, artistVariantResponse{
			ID:               variant.ID.String(),
			Provider:         variant.Provider,
			ProviderArtistID: variant.ProviderArtistID,
			Name:             variant.Name,
		})
	}
	if withAlbums {
		for _, album := range artist.Albums {
			detailed, err := s.library.GetAlbum(ctx, album.ID)
			if err != nil {
				return artistResponse{}, err
			}
			response, err := s.buildAlbum(ctx, detailed, false)
			if err != nil {
				return artistResponse{}, err
			}
			out.Albums = append(out.Albums, response)
		}
	}
	return out, nil
}

// handleAlbumSearch fans out to the album-capable providers and returns the
// canonical albums the hits matched.
func (s *Server) handleAlbumSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit := clampLimit(intParam(r, "limit", defaultSearchLimit))

	albums, problems, err := s.library.SearchAlbums(r.Context(), query, limit)
	if err != nil {
		writeStoreError(w, err, "album search failed")
		return
	}

	out := make([]albumResponse, 0, len(albums))
	for i := range albums {
		detailed, err := s.library.GetAlbum(r.Context(), albums[i].ID)
		if err != nil {
			writeStoreError(w, err, "album search failed")
			return
		}
		response, err := s.buildAlbum(r.Context(), detailed, false)
		if err != nil {
			writeStoreError(w, err, "album search failed")
			return
		}
		out = append(out, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":          query,
		"albums":         out,
		"providerErrors": problemsJSON(problems),
	})
}

// handleArtistSearch does the same for artists.
func (s *Server) handleArtistSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit := clampLimit(intParam(r, "limit", defaultSearchLimit))

	artists, problems, err := s.library.SearchArtists(r.Context(), query, limit)
	if err != nil {
		writeStoreError(w, err, "artist search failed")
		return
	}

	out := make([]artistResponse, 0, len(artists))
	for i := range artists {
		detailed, err := s.library.GetArtist(r.Context(), artists[i].ID)
		if err != nil {
			writeStoreError(w, err, "artist search failed")
			return
		}
		response, err := s.buildArtist(r.Context(), detailed, false)
		if err != nil {
			writeStoreError(w, err, "artist search failed")
			return
		}
		out = append(out, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":          query,
		"artists":        out,
		"providerErrors": problemsJSON(problems),
	})
}

// problemsJSON renders collection provider failures with the same keys the
// track search uses, so a client parses one shape everywhere.
func problemsJSON(problems []library.ProviderError) []providerProblem {
	out := make([]providerProblem, 0, len(problems))
	for _, problem := range problems {
		out = append(out, providerProblem{Provider: problem.Provider, Error: problem.Error})
	}
	return out
}

// handleAlbum returns one album with its tracks.
func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	albumID, ok := s.collectionID(w, r, "albumId")
	if !ok {
		return
	}
	album, err := s.library.GetAlbum(r.Context(), albumID)
	if err != nil {
		writeLibraryError(w, err)
		return
	}
	response, err := s.buildAlbum(r.Context(), album, true)
	if err != nil {
		writeStoreError(w, err, "album unavailable")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleArtist returns one artist with their albums.
func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	artistID, ok := s.collectionID(w, r, "artistId")
	if !ok {
		return
	}
	artist, err := s.library.GetArtist(r.Context(), artistID)
	if err != nil {
		writeLibraryError(w, err)
		return
	}
	response, err := s.buildArtist(r.Context(), artist, true)
	if err != nil {
		writeStoreError(w, err, "artist unavailable")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleAlbumSync pulls the album's provider tracklists into the library.
func (s *Server) handleAlbumSync(w http.ResponseWriter, r *http.Request) {
	albumID, ok := s.collectionID(w, r, "albumId")
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

	resolve := req.Resolve != nil && *req.Resolve
	result, err := s.library.SyncAlbum(r.Context(), albumID, req.Providers, resolve)
	if err != nil {
		writeLibraryError(w, err)
		return
	}

	response := syncResponse{
		AlbumID:   result.AlbumID.String(),
		Providers: result.Providers,
		Added:     result.Added,
		Tracks:    make([]trackResponse, 0, len(result.Tracks)),
	}
	for _, track := range result.Tracks {
		built, err := s.buildTrack(r.Context(), track)
		if err != nil {
			writeStoreError(w, err, "album sync failed")
			return
		}
		response.Tracks = append(response.Tracks, built)
	}
	for _, problem := range result.Errors {
		response.Errors = append(response.Errors, providerProblem{Provider: problem.Provider, Error: problem.Error})
	}
	writeJSON(w, http.StatusOK, response)
}

// handleArtistSync records the artist's provider pages and pulls their albums.
func (s *Server) handleArtistSync(w http.ResponseWriter, r *http.Request) {
	artistID, ok := s.collectionID(w, r, "artistId")
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

	resolve := req.Resolve != nil && *req.Resolve
	result, err := s.library.SyncArtist(r.Context(), artistID, req.Providers, req.SyncAlbums, resolve)
	if err != nil {
		writeLibraryError(w, err)
		return
	}

	response := syncResponse{
		ArtistID:  result.ArtistID.String(),
		Providers: result.Providers,
		Added:     result.Added,
		Albums:    make([]albumResponse, 0, len(result.Albums)),
	}
	for _, album := range result.Albums {
		detailed, err := s.library.GetAlbum(r.Context(), album.ID)
		if err != nil {
			writeStoreError(w, err, "artist sync failed")
			return
		}
		built, err := s.buildAlbum(r.Context(), detailed, false)
		if err != nil {
			writeStoreError(w, err, "artist sync failed")
			return
		}
		response.Albums = append(response.Albums, built)
	}
	for _, problem := range result.Errors {
		response.Errors = append(response.Errors, providerProblem{Provider: problem.Provider, Error: problem.Error})
	}
	writeJSON(w, http.StatusOK, response)
}

// collectionID parses an album or artist id path value.
func (s *Server) collectionID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	if len(raw) == 0 || len(raw) > maxMediaVariantIDLen {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

// providersKnown validates a provider selection before doing any work.
func (s *Server) providersKnown(w http.ResponseWriter, providers []string) bool {
	for _, name := range providers {
		if !s.providerKnown(name) {
			writeError(w, http.StatusBadRequest, "unknown provider "+name)
			return false
		}
	}
	return true
}

func writeLibraryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, library.ErrNoAlbum), errors.Is(err, library.ErrNoArtist),
		errors.Is(err, library.ErrNoPlaylist), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, library.ErrNoSources):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeStoreError(w, err, "request failed")
	}
}

func clampLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > maxSearchLimit {
		return maxSearchLimit
	}
	return limit
}
