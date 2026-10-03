// Package library browses and synchronises the collections that sit above
// tracks: albums and artists, each of which exists once canonically while its
// provider releases hang off it as variants. Provider playlists are browsed
// here too, but they stay provider-scoped: two services offering a playlist of
// the same name are different lists, so they are never merged.
//
// Synchronising means going the other way from a provider: fetching a release's
// tracklist, matching every track into the canonical library, and attaching the
// result to the album. That is what makes an album from one source playable
// through the renditions of another.
package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/match"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// maxProviderAlbums bounds how many albums one artist sync will pull, so a
// prolific artist cannot turn a single request into hundreds.
const maxProviderAlbums = 50

// Errors.
var (
	// ErrNoAlbum means the album does not exist.
	ErrNoAlbum = errors.New("album not found")
	// ErrNoArtist means the artist does not exist.
	ErrNoArtist = errors.New("artist not found")
	// ErrNoSources means the collection has no provider release to sync from.
	ErrNoSources = errors.New("nothing to sync from: search for it on a provider first")
	// ErrNoPlaylist means the provider playlist does not exist.
	ErrNoPlaylist = errors.New("playlist not found")
)

// ProviderError is one provider's failure during a browse or a sync; the rest
// of the work still stands.
type ProviderError struct {
	Provider string
	Error    string
}

// Store is the repository slice the library needs.
type Store interface {
	store.TrackRepo
	store.VariantRepo
	store.AlbumRepo
	store.ArtistRepo
	store.ExternalPlaylistRepo
	store.PlaylistRepo
}

// Service browses and syncs albums and artists.
type Service struct {
	db        Store
	providers *provider.Registry
	matcher   *match.Matcher
	logger    *slog.Logger
}

// New returns the service.
func New(db Store, providers *provider.Registry, matcher *match.Matcher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, providers: providers, matcher: matcher, logger: logger}
}

// Album is an album with everything a caller needs to show it.
type Album struct {
	Album    store.Album
	Artists  []store.Artist
	Variants []store.AlbumVariant
	Tracks   []store.Track
	// TrackArtists is the credit list per track, in the same order as Tracks.
	TrackArtists [][]store.Artist
}

// Artist is an artist with their provider pages and albums.
type Artist struct {
	Artist   store.Artist
	Variants []store.ArtistVariant
	Albums   []store.Album
}

// SyncResult reports what a sync did.
type SyncResult struct {
	AlbumID   uuid.UUID
	Providers []string
	Added     int
	Tracks    []store.Track
	Errors    []ProviderError
}

// PlaylistSyncResult reports what syncing a provider playlist did.
type PlaylistSyncResult struct {
	Playlist store.ExternalPlaylist
	Added    int
	Tracks   []store.Track
}

// ArtistSyncResult reports what an artist sync did.
type ArtistSyncResult struct {
	ArtistID  uuid.UUID
	Providers []string
	Albums    []store.Album
	Added     int
	Errors    []ProviderError
}

// SearchAlbums fans out to the album-capable providers and matches every hit
// into the canonical library, so the same record from two sources is one album.
// An empty providers list searches every provider.
func (s *Service) SearchAlbums(
	ctx context.Context, query string, limit int, providers []string,
) ([]store.Album, []ProviderError, error) {
	results := s.providers.SearchSomeAlbums(ctx, providers, query, provider.SearchOpts{Limit: limit})

	var (
		albums   []store.Album
		seen     = map[uuid.UUID]bool{}
		problems []ProviderError
	)
	for _, result := range results {
		if result.Err != nil {
			if result.IsSearchable() {
				problems = append(problems, ProviderError{Provider: result.Provider, Error: result.Err.Error()})
			}
			continue
		}
		for _, hit := range result.Albums {
			album, _, _, err := s.matcher.MatchAlbum(ctx, result.Provider, hit)
			if err != nil {
				return nil, problems, fmt.Errorf("match %s/%s: %w", result.Provider, hit.ProviderAlbumID, err)
			}
			if seen[album.ID] {
				continue
			}
			seen[album.ID] = true
			albums = append(albums, *album)
		}
	}
	return albums, problems, nil
}

// SearchArtists fans out to the artist-capable providers. An empty providers
// list searches every provider.
func (s *Service) SearchArtists(
	ctx context.Context, query string, limit int, providers []string,
) ([]store.Artist, []ProviderError, error) {
	results := s.providers.SearchSomeArtists(ctx, providers, query, provider.SearchOpts{Limit: limit})

	var (
		artists  []store.Artist
		seen     = map[uuid.UUID]bool{}
		problems []ProviderError
	)
	for _, result := range results {
		if result.Err != nil {
			if result.IsSearchable() {
				problems = append(problems, ProviderError{Provider: result.Provider, Error: result.Err.Error()})
			}
			continue
		}
		for _, hit := range result.Artists {
			artist, _, _, err := s.matcher.MatchArtist(ctx, result.Provider, hit)
			if err != nil {
				return nil, problems, fmt.Errorf("match %s/%s: %w", result.Provider, hit.ProviderArtistID, err)
			}
			if seen[artist.ID] {
				continue
			}
			seen[artist.ID] = true
			artists = append(artists, *artist)
		}
	}
	return artists, problems, nil
}

// SearchPlaylists fans out to the playlist-capable providers and records every
// hit, so the library can serve its cover and remember what it synced. An
// empty providers list searches every provider.
func (s *Service) SearchPlaylists(
	ctx context.Context, query string, limit int, providers []string,
) ([]store.ExternalPlaylist, []ProviderError, error) {
	results := s.providers.SearchSomePlaylists(ctx, providers, query, provider.SearchOpts{Limit: limit})

	var (
		playlists []store.ExternalPlaylist
		seen      = map[uuid.UUID]bool{}
		problems  []ProviderError
	)
	for _, result := range results {
		if result.Err != nil {
			if result.IsSearchable() {
				problems = append(problems, ProviderError{Provider: result.Provider, Error: result.Err.Error()})
			}
			continue
		}
		for _, hit := range result.Playlists {
			stored, _, err := s.db.UpsertExternalPlaylist(ctx, &store.ExternalPlaylist{
				Provider:           result.Provider,
				ProviderPlaylistID: hit.ProviderPlaylistID,
				Title:              hit.Title,
				Owner:              hit.Owner,
				Description:        hit.Description,
				TrackCount:         hit.TrackCount,
				ArtworkURL:         hit.ArtworkURL,
			})
			if err != nil {
				return nil, problems, fmt.Errorf("store playlist %s/%s: %w",
					result.Provider, hit.ProviderPlaylistID, err)
			}
			if seen[stored.ID] {
				continue
			}
			seen[stored.ID] = true
			playlists = append(playlists, *stored)
		}
	}
	return playlists, problems, nil
}

// GetPlaylist loads a provider playlist with the tracks synced from it.
func (s *Service) GetPlaylist(ctx context.Context, playlistID uuid.UUID) (*store.ExternalPlaylist, []store.Track, error) {
	playlist, err := s.db.ExternalPlaylist(ctx, playlistID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNoPlaylist, playlistID)
		}
		return nil, nil, err
	}
	tracks, err := s.db.ExternalPlaylistTracks(ctx, playlistID)
	if err != nil {
		return nil, nil, err
	}
	return playlist, tracks, nil
}

// PlaylistImportResult says what an import brought over.
type PlaylistImportResult struct {
	// Playlist is the account's own playlist, the one that was created.
	Playlist *store.Playlist
	// Source is the provider playlist it was copied from.
	Source provider.Playlist
	// Added is how many tracks the playlist now holds.
	Added int
	// Playable is how many of them ended up with a rendition that can be
	// played; the rest are metadata until something can be found for them.
	Playable int
	Errors   []ProviderError
}

// ImportPlaylist copies a provider's playlist into one of the account's own.
//
// Each track is matched into the canonical library, and then resolved: that
// second step is the point of the whole thing. A Spotify playlist has no audio
// here, so its songs are looked for on a provider that does have them - the
// household ends up with its own playlist of renditions it can play.
func (s *Service) ImportPlaylist(
	ctx context.Context, userID uuid.UUID, providerName, providerPlaylistID, name string,
	progress func(done, total int),
) (*PlaylistImportResult, error) {
	detail, err := s.providers.Playlist(ctx, providerName, providerPlaylistID)
	if err != nil {
		return nil, err
	}
	if len(detail.Tracks) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrNoPlaylist, providerPlaylistID)
	}

	result := &PlaylistImportResult{Source: detail.Playlist}
	total := len(detail.Tracks)
	if progress != nil {
		progress(0, total)
	}
	ids := make([]uuid.UUID, 0, total)
	for index, hit := range detail.Tracks {
		track, _, err := s.matcher.Attach(ctx, providerName, hit)
		if err != nil {
			result.Errors = append(result.Errors, ProviderError{Provider: providerName, Error: err.Error()})
		} else {
			ids = append(ids, track.ID)
			if _, err := s.matcher.Resolve(ctx, track.ID); err == nil {
				result.Playable++
			}
		}
		// One track at a time is the only honest measure of this job: each one
		// is a provider search.
		if progress != nil {
			progress(index+1, total)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: nothing in %s could be matched", ErrNoPlaylist, providerPlaylistID)
	}

	title := strings.TrimSpace(name)
	if title == "" {
		title = strings.TrimSpace(detail.Title)
	}
	if title == "" {
		title = "Imported playlist"
	}
	playlist := &store.Playlist{UserID: userID, Name: title}
	if err := s.db.CreatePlaylist(ctx, playlist); err != nil {
		return nil, err
	}
	if err := s.db.AppendPlaylistItems(ctx, playlist.ID, ids); err != nil {
		return nil, err
	}
	result.Playlist = playlist
	result.Added = len(ids)
	s.logger.Info("playlist imported", "playlist", playlist.ID, "provider", providerName,
		"source", providerPlaylistID, "tracks", result.Added, "playable", result.Playable)
	return result, nil
}

// SyncPlaylist pulls a provider playlist's tracks into the canonical library,
// in the playlist's own order, and returns them. Syncing twice is harmless: the
// tracklist is replaced, not appended to.
func (s *Service) SyncPlaylist(
	ctx context.Context, playlistID uuid.UUID, providers []string,
) (*PlaylistSyncResult, error) {
	playlist, err := s.db.ExternalPlaylist(ctx, playlistID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNoPlaylist, playlistID)
		}
		return nil, err
	}
	if len(providers) > 0 && !containsString(providers, playlist.Provider) {
		return nil, fmt.Errorf("%w: %s", ErrNoSources, playlist.Title)
	}

	detail, err := s.providers.Playlist(ctx, playlist.Provider, playlist.ProviderPlaylistID)
	if err != nil {
		return nil, fmt.Errorf("sync playlist %s: %w", playlist.Title, err)
	}

	// The playlist's own metadata is fresher than the search hit's.
	if _, _, err := s.db.UpsertExternalPlaylist(ctx, &store.ExternalPlaylist{
		Provider:           playlist.Provider,
		ProviderPlaylistID: playlist.ProviderPlaylistID,
		Title:              detail.Title,
		Owner:              detail.Owner,
		Description:        detail.Description,
		TrackCount:         detail.TrackCount,
		ArtworkURL:         detail.ArtworkURL,
	}); err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(detail.Tracks))
	for _, hit := range detail.Tracks {
		track, _, err := s.matcher.Attach(ctx, playlist.Provider, hit)
		if err != nil {
			return nil, fmt.Errorf("match %s/%s: %w", playlist.Provider, hit.ProviderTrackID, err)
		}
		ids = append(ids, track.ID)
	}

	added, err := s.db.SetExternalPlaylistTracks(ctx, playlistID, ids)
	if err != nil {
		return nil, err
	}
	tracks, err := s.db.ExternalPlaylistTracks(ctx, playlistID)
	if err != nil {
		return nil, err
	}
	stored, err := s.db.ExternalPlaylist(ctx, playlistID)
	if err != nil {
		return nil, err
	}

	s.logger.Info("playlist synced", "playlist", playlistID, "provider", playlist.Provider,
		"tracks", len(tracks), "added", added)
	return &PlaylistSyncResult{Playlist: *stored, Added: added, Tracks: tracks}, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// GetAlbum loads an album with its credits, releases and tracks.
func (s *Service) GetAlbum(ctx context.Context, albumID uuid.UUID) (*Album, error) {
	stored, err := s.db.Album(ctx, albumID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNoAlbum, albumID)
		}
		return nil, err
	}

	out := &Album{Album: *stored}
	if out.Artists, err = s.db.AlbumArtists(ctx, albumID); err != nil {
		return nil, err
	}
	if out.Variants, err = s.db.AlbumVariants(ctx, albumID); err != nil {
		return nil, err
	}
	if out.Tracks, err = s.db.AlbumTracks(ctx, albumID); err != nil {
		return nil, err
	}
	for _, track := range out.Tracks {
		artists, err := s.db.TrackArtists(ctx, track.ID)
		if err != nil {
			return nil, err
		}
		out.TrackArtists = append(out.TrackArtists, artists)
	}
	return out, nil
}

// GetArtist loads an artist with their provider pages and albums.
func (s *Service) GetArtist(ctx context.Context, artistID uuid.UUID) (*Artist, error) {
	stored, err := s.db.Artist(ctx, artistID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNoArtist, artistID)
		}
		return nil, err
	}

	out := &Artist{Artist: *stored}
	if out.Variants, err = s.db.ArtistVariants(ctx, artistID); err != nil {
		return nil, err
	}
	if out.Albums, err = s.db.AlbumsForArtist(ctx, artistID); err != nil {
		return nil, err
	}
	return out, nil
}

// SyncAlbum pulls the tracklists of an album's provider releases into the
// canonical library. With no providers named it syncs every release it knows;
// with resolve it also makes sure each track has something playable.
func (s *Service) SyncAlbum(ctx context.Context, albumID uuid.UUID, providers []string, resolve bool) (*SyncResult, error) {
	album, err := s.db.Album(ctx, albumID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNoAlbum, albumID)
		}
		return nil, err
	}

	variants, err := s.db.AlbumVariants(ctx, albumID)
	if err != nil {
		return nil, err
	}
	selected := selectVariants(variants, providers)
	if len(selected) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSources, album.Title)
	}

	result := &SyncResult{AlbumID: albumID, Providers: providersFor(selected)}
	// Attaching a track links it to its album, so the album's own tracklist is
	// already growing while the loop runs: what the sync added is the album's
	// growth, not what AppendAlbumTracks had left to insert.
	before, err := s.db.AlbumTracks(ctx, albumID)
	if err != nil {
		return nil, err
	}
	for _, variant := range selected {
		detail, err := s.providers.Album(ctx, variant.Provider, variant.ProviderAlbumID)
		if err != nil {
			result.Errors = append(result.Errors, ProviderError{Provider: variant.Provider, Error: err.Error()})
			continue
		}
		// The release's size is what tells a client whether this album is
		// complete, and the tracklist is the only place to learn it.
		if err := s.db.SetAlbumVariantTrackCount(ctx, variant.ID, len(detail.Tracks)); err != nil {
			return nil, err
		}

		ids := make([]uuid.UUID, 0, len(detail.Tracks))
		for _, hit := range detail.Tracks {
			track, _, err := s.matcher.Attach(ctx, variant.Provider, hit)
			if err != nil {
				return nil, fmt.Errorf("match %s/%s: %w", variant.Provider, hit.ProviderTrackID, err)
			}
			ids = append(ids, track.ID)
		}
		if _, err := s.db.AppendAlbumTracks(ctx, albumID, ids); err != nil {
			return nil, err
		}
		s.logger.Info("album synced", "album", albumID, "provider", variant.Provider,
			"tracks", len(ids))
	}

	tracks, err := s.db.AlbumTracks(ctx, albumID)
	if err != nil {
		return nil, err
	}
	result.Tracks = tracks
	result.Added = len(tracks) - len(before)

	// An album found without credits learns them from its own tracklist, so a
	// bare provider result becomes a properly attributed album.
	if credits, err := s.db.AlbumArtists(ctx, albumID); err == nil && len(credits) == 0 {
		for _, track := range tracks {
			artists, err := s.db.TrackArtists(ctx, track.ID)
			if err != nil {
				return nil, err
			}
			if len(artists) == 0 {
				continue
			}
			names := make([]string, 0, len(artists))
			for _, artist := range artists {
				names = append(names, artist.Name)
			}
			if err := s.db.SetAlbumArtists(ctx, albumID, names); err != nil {
				return nil, err
			}
			break
		}
	}

	if resolve {
		for _, track := range tracks {
			if _, err := s.matcher.Resolve(ctx, track.ID); err != nil && !errors.Is(err, match.ErrNoPlayableVariant) {
				return nil, err
			}
		}
	}
	return result, nil
}

// SyncArtist records an artist's provider pages, then pulls their albums (and,
// when asked, each album's tracklist) into the canonical library.
func (s *Service) SyncArtist(ctx context.Context, artistID uuid.UUID, providers []string, syncAlbums, resolve bool) (*ArtistSyncResult, error) {
	artist, err := s.db.Artist(ctx, artistID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNoArtist, artistID)
		}
		return nil, err
	}

	variants, err := s.db.ArtistVariants(ctx, artistID)
	if err != nil {
		return nil, err
	}
	selected := selectArtistVariants(variants, providers)
	if len(selected) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSources, artist.Name)
	}

	result := &ArtistSyncResult{ArtistID: artistID, Providers: artistProviders(selected)}

	// Each provider's albums are fetched in parallel; the database writes below
	// stay on this goroutine.
	type fetched struct {
		provider string
		albums   []provider.Album
		err      error
	}
	results := make([]fetched, len(selected))
	var wg sync.WaitGroup
	for i, variant := range selected {
		wg.Add(1)
		go func(i int, variant store.ArtistVariant) {
			defer wg.Done()
			albums, err := s.providers.ArtistAlbums(ctx, variant.Provider, variant.ProviderArtistID)
			results[i] = fetched{provider: variant.Provider, albums: albums, err: err}
		}(i, variant)
	}
	wg.Wait()

	for _, fetched := range results {
		if fetched.err != nil {
			result.Errors = append(result.Errors, ProviderError{Provider: fetched.provider, Error: fetched.err.Error()})
			continue
		}
		albums := fetched.albums
		if len(albums) > maxProviderAlbums {
			albums = albums[:maxProviderAlbums]
		}
		for _, hit := range albums {
			album, _, _, err := s.matcher.MatchAlbum(ctx, fetched.provider, hit)
			if err != nil {
				return nil, fmt.Errorf("match %s/%s: %w", fetched.provider, hit.ProviderAlbumID, err)
			}
			if err := s.db.AttachAlbumArtist(ctx, album.ID, artistID); err != nil {
				return nil, err
			}
			result.Albums = append(result.Albums, *album)
		}
	}

	if syncAlbums {
		for _, album := range result.Albums {
			synced, err := s.SyncAlbum(ctx, album.ID, nil, resolve)
			if err != nil {
				result.Errors = append(result.Errors, ProviderError{Provider: album.Title, Error: err.Error()})
				continue
			}
			result.Added += synced.Added
			result.Errors = append(result.Errors, synced.Errors...)
		}
	}
	return result, nil
}

// selectVariants keeps the requested providers, or everything when none is
// named. Order follows the request.
func selectVariants(variants []store.AlbumVariant, providers []string) []store.AlbumVariant {
	if len(providers) == 0 {
		return variants
	}
	out := make([]store.AlbumVariant, 0, len(variants))
	for _, name := range providers {
		for _, variant := range variants {
			if variant.Provider == name {
				out = append(out, variant)
			}
		}
	}
	return out
}

func selectArtistVariants(variants []store.ArtistVariant, providers []string) []store.ArtistVariant {
	if len(providers) == 0 {
		return variants
	}
	out := make([]store.ArtistVariant, 0, len(variants))
	for _, name := range providers {
		for _, variant := range variants {
			if variant.Provider == name {
				out = append(out, variant)
			}
		}
	}
	return out
}

func providersFor(variants []store.AlbumVariant) []string {
	out := make([]string, 0, len(variants))
	seen := map[string]bool{}
	for _, variant := range variants {
		if !seen[variant.Provider] {
			seen[variant.Provider] = true
			out = append(out, variant.Provider)
		}
	}
	return out
}

func artistProviders(variants []store.ArtistVariant) []string {
	out := make([]string, 0, len(variants))
	seen := map[string]bool{}
	for _, variant := range variants {
		if !seen[variant.Provider] {
			seen[variant.Provider] = true
			out = append(out, variant.Provider)
		}
	}
	return out
}
