package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/library"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
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
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Year       string   `json:"year,omitempty"`
	TrackCount int      `json:"trackCount"`
	// Kind is "album", "single" or "ep" for a release, and "singles" for the
	// container an artist's single releases are collected into.
	Kind       string                 `json:"kind,omitempty"`
	Providers  []string               `json:"providers"`
	ArtworkURL string                 `json:"artworkUrl,omitempty"`
	Variants   []albumVariantResponse `json:"variants,omitempty"`
	Tracks     []trackResponse        `json:"tracks,omitempty"`
	// Catalogue is set on the singles container: the artist's catalogue read is
	// what fills it, so the page can say it is still arriving.
	Catalogue *library.CatalogueState `json:"catalogue,omitempty"`
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
	// Catalogue is the artist's catalogue read: opening an artist starts one,
	// and a page showing songs as they land polls it.
	Catalogue *library.CatalogueState `json:"catalogue,omitempty"`
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
	kind := albumKind(album)
	out := albumResponse{
		ID:         album.Album.ID.String(),
		Title:      album.Album.Title,
		Artists:    make([]string, 0, len(album.Artists)),
		TrackCount: len(album.Tracks),
		Kind:       kind,
		Providers:  make([]string, 0, len(album.Variants)),
		Variants:   make([]albumVariantResponse, 0, len(album.Variants)),
	}
	for _, artist := range album.Artists {
		out.Artists = append(out.Artists, artist.Name)
	}
	// The singles container is not a release and has no cover of its own: the
	// one it can hold is a single's, inherited when its tracks linked their
	// albums. Serving it under the container's name is one single's sleeve
	// presented as the artist's singles, so no client is told there is one -
	// each draws its own idea of the container instead.
	if album.Album.ArtworkURL != "" && kind != store.AlbumKindSingles {
		out.ArtworkURL = artworkPath(store.ArtworkAlbum, album.Album.ID)
	}
	seen := map[string]bool{}
	earliest := 0
	for _, variant := range album.Variants {
		if !seen[variant.Provider] {
			seen[variant.Provider] = true
			out.Providers = append(out.Providers, variant.Provider)
		}
		// The record's own year is its earliest release - a remaster does not move
		// it to the year it was remastered - and it is the year the artist's albums
		// are ordered by, so the page and the order cannot disagree.
		if year, ok := provider.YearValue(variant.Year); ok && (earliest == 0 || year < earliest) {
			earliest, out.Year = year, variant.Year
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
			// A single is not a record: this page lists the records and the one
			// container the singles are collected into. The single's own album is
			// still there to open from a search.
			if isSingleReleaseKind(response.Kind) {
				continue
			}
			out.Albums = append(out.Albums, response)
		}
	}
	return out, nil
}

// albumKind is what an album is, in the response's words: the singles container
// when it is one, and otherwise what its releases say - a record when any release
// of it is a record, an EP when the most any release says is EP, and a single
// when every release is one. An album nobody has classified is a record, which is
// what everything was before the providers were asked.
func albumKind(album *library.Album) string {
	if album.Album.Kind == store.AlbumKindSingles {
		return store.AlbumKindSingles
	}
	rank := map[string]int{
		"":                      0,
		store.ReleaseKindSingle: 1,
		store.ReleaseKindEP:     2,
		store.ReleaseKindAlbum:  3,
	}
	best := ""
	for _, variant := range album.Variants {
		if rank[variant.Kind] > rank[best] {
			best = variant.Kind
		}
	}
	if best == "" {
		return store.ReleaseKindAlbum
	}
	return best
}

func isSingleReleaseKind(kind string) bool {
	return kind == store.ReleaseKindSingle || kind == store.ReleaseKindEP
}

// handleAlbumSearch fans out to the album-capable providers and returns the
// canonical albums the hits matched.
func (s *Server) handleAlbumSearch(w http.ResponseWriter, r *http.Request) {
	query, limit, providers, ok := s.searchRequest(w, r)
	if !ok {
		return
	}
	value, err := s.searches.do(r.Context(), searchKey("albums", query, providers, limit), func() (any, error) {
		// Detached: one caller hanging up must not take a shared answer with it.
		return s.albumSearchAnswer(context.WithoutCancel(r.Context()), query, limit, providers)
	})
	if err != nil {
		writeStoreError(w, err, "album search failed")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// albumSearchAnswer is the album search itself: the same shape, the same
// caching and the same sharing the track search has.
func (s *Server) albumSearchAnswer(ctx context.Context, query string, limit int, providers []string) (any, error) {
	albums, problems, err := s.library.SearchAlbums(ctx, query, limit, providers)
	if err != nil {
		return nil, err
	}
	out := make([]albumResponse, 0, len(albums))
	for i := range albums {
		detailed, err := s.library.GetAlbum(ctx, albums[i].ID)
		if err != nil {
			return nil, err
		}
		response, err := s.buildAlbum(ctx, detailed, false)
		if err != nil {
			return nil, err
		}
		out = append(out, response)
	}
	return map[string]any{
		"query":          query,
		"albums":         out,
		"providerErrors": problemsJSON(problems),
	}, nil
}

// handleArtistSearch does the same for artists.
func (s *Server) handleArtistSearch(w http.ResponseWriter, r *http.Request) {
	query, limit, providers, ok := s.searchRequest(w, r)
	if !ok {
		return
	}
	value, err := s.searches.do(r.Context(), searchKey("artists", query, providers, limit), func() (any, error) {
		return s.artistSearchAnswer(context.WithoutCancel(r.Context()), query, limit, providers)
	})
	if err != nil {
		writeStoreError(w, err, "artist search failed")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// artistSearchAnswer is the artist search itself.
func (s *Server) artistSearchAnswer(ctx context.Context, query string, limit int, providers []string) (any, error) {
	artists, problems, err := s.library.SearchArtists(ctx, query, limit, providers)
	if err != nil {
		return nil, err
	}
	out := make([]artistResponse, 0, len(artists))
	for i := range artists {
		detailed, err := s.library.GetArtist(ctx, artists[i].ID)
		if err != nil {
			return nil, err
		}
		response, err := s.buildArtist(ctx, detailed, false)
		if err != nil {
			return nil, err
		}
		out = append(out, response)
	}
	return map[string]any{
		"query":          query,
		"artists":        out,
		"providerErrors": problemsJSON(problems),
	}, nil
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
//
// An album's page shows every song it has, so the read that gets it there happens
// before the answer: a release nobody has read, or an album a single song left
// behind, is filled in now rather than by a button. The page waits for it, which
// is one provider call.
func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	albumID, ok := s.collectionID(w, r, "albumId")
	if !ok {
		return
	}
	s.library.EnsureAlbumTracks(r.Context(), albumID)

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
	// The singles container holds what the artist's catalogue read brings in, so
	// opening it starts (or joins) that read and says how far it has got.
	if album.Album.Kind == store.AlbumKindSingles && len(album.Artists) > 0 {
		state := s.library.StartCatalogue(album.Artists[0].ID)
		response.Catalogue = &state
	}
	s.withPlays(r.Context(), s.currentUser(r), trackRefs(response.Tracks))
	writeJSON(w, http.StatusOK, response)
}

// handleArtist returns one artist with their albums, and starts reading their
// catalogue: the records are there at once and their songs arrive as they are
// found, which is what the page polls for.
func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	artistID, ok := s.collectionID(w, r, "artistId")
	if !ok {
		return
	}
	state := s.library.StartCatalogue(artistID)

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
	response.Catalogue = &state
	writeJSON(w, http.StatusOK, response)
}

// handleArtistCatalogue reports what an artist's catalogue read is doing. A page
// that is showing songs as they arrive asks this until it is done.
func (s *Server) handleArtistCatalogue(w http.ResponseWriter, r *http.Request) {
	artistID, ok := s.collectionID(w, r, "artistId")
	if !ok {
		return
	}
	state, started := s.library.CatalogueStatus(artistID)
	if !started {
		// Nothing has been read for this artist yet, and somebody is asking: that
		// is as good a reason to start as opening the page.
		state = s.library.StartCatalogue(artistID)
	}
	writeJSON(w, http.StatusOK, state)
}

// handleArtistTracks lists an artist's songs, by title, with the query matched
// anywhere in the title: the songs panel's search box and its scrolling.
func (s *Server) handleArtistTracks(w http.ResponseWriter, r *http.Request) {
	artistID, ok := s.collectionID(w, r, "artistId")
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := clampArtistTracksLimit(intParam(r, "limit", artistTracksLimit))
	offset := intParam(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}

	ctx := r.Context()
	tracks, err := s.store.ArtistTracks(ctx, artistID, query, limit, offset)
	if err != nil {
		writeStoreError(w, err, "artist songs unavailable")
		return
	}
	total, err := s.store.CountArtistTracks(ctx, artistID, query)
	if err != nil {
		writeStoreError(w, err, "artist songs unavailable")
		return
	}
	out := make([]trackResponse, 0, len(tracks))
	for _, track := range tracks {
		built, err := s.buildTrack(ctx, track)
		if err != nil {
			writeStoreError(w, err, "artist songs unavailable")
			return
		}
		out = append(out, built)
	}
	s.withPlays(ctx, s.currentUser(r), trackRefs(out))
	writeJSON(w, http.StatusOK, struct {
		Tracks []trackResponse `json:"tracks"`
		Total  int             `json:"total"`
	}{Tracks: out, Total: total})
}

// artistTracksLimit is the default page size for an artist's songs. The songs
// panel is a scrollable catalogue, not a search suggestion list, so its cap is
// larger than the general search cap.
const artistTracksLimit = 200

func clampArtistTracksLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > artistTracksLimit {
		return artistTracksLimit
	}
	return limit
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
