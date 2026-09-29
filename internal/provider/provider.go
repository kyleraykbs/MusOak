// Package provider defines the plugin boundary for music sources.
//
// Nothing outside this package and its subpackages knows what YT Music or
// Spotify are: providers are registered as a name plus a capability set, and
// the rest of the server treats them uniformly.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Caps describes what a provider can do. Spotify, for instance, can search but
// never download: its audio is DRM-protected, so its tracks are only playable
// through a matched variant from a downloadable provider.
type Caps struct {
	Search   bool
	Download bool
	// SearchAlbums and SearchArtists report collection browsing.
	SearchAlbums  bool
	SearchArtists bool
	// Radio reports that the provider can build a station from a seed track.
	Radio bool
}

// Track is a provider's search hit, before cross-provider matching.
type Track struct {
	ProviderTrackID string
	Title           string
	Artists         []string
	Album           string
	DurationMs      int64
	ISRC            string
}

// SearchOpts tunes a search.
type SearchOpts struct {
	Limit int
}

// Album is a provider's album hit.
type Album struct {
	ProviderAlbumID string
	Title           string
	Artists         []string
	Year            string
	TrackCount      int
}

// Artist is a provider's artist hit.
type Artist struct {
	ProviderArtistID string
	Name             string
}

// AlbumDetail is an album with its tracklist, in album order.
type AlbumDetail struct {
	Album
	Tracks []Track
}

// AlbumSearcher is implemented by providers that can browse albums.
type AlbumSearcher interface {
	SearchAlbums(ctx context.Context, q string, opts SearchOpts) ([]Album, error)
	// Album returns the tracklist of one provider album.
	Album(ctx context.Context, providerAlbumID string) (*AlbumDetail, error)
}

// ArtistSearcher is implemented by providers that can browse artists.
type ArtistSearcher interface {
	SearchArtists(ctx context.Context, q string, opts SearchOpts) ([]Artist, error)
	// ArtistAlbums lists an artist's albums.
	ArtistAlbums(ctx context.Context, providerArtistID string) ([]Album, error)
}

// RadioProvider is implemented by providers that can build a station from a
// seed track, which is how a radio mixes the sources the caller picked.
type RadioProvider interface {
	Radio(ctx context.Context, seed Track, limit int) ([]Track, error)
}

// Provider is one music source.
type Provider interface {
	Name() string
	Capabilities() Caps
	Search(ctx context.Context, q string, opts SearchOpts) ([]Track, error)
	// Download writes an Ogg/Opus file at destPath, whose parent directory
	// must exist. It is only called when Capabilities().Download is true.
	Download(ctx context.Context, providerTrackID, destPath string) error
}

// DependencyChecker is implemented by providers that shell out to binaries, so
// the daemon can report missing tooling at startup.
type DependencyChecker interface {
	MissingDeps() []string
}

var (
	// ErrDownloadUnsupported means the provider cannot be downloaded from.
	ErrDownloadUnsupported = errors.New("provider does not support downloads")
	// ErrRadioUnsupported means the provider cannot build a radio.
	ErrRadioUnsupported = errors.New("provider does not support radio")
	// ErrNotEnabled means no such provider is registered.
	ErrNotEnabled = errors.New("provider not enabled")
)

// Result is one provider's contribution to a fan-out search. A provider that
// failed reports its error here; the other results stay usable.
type Result struct {
	Provider string
	Tracks   []Track
	Err      error
}

// AlbumResult is one provider's contribution to an album search.
type AlbumResult struct {
	Provider string
	Albums   []Album
	Err      error
}

// ArtistResult is one provider's contribution to an artist search.
type ArtistResult struct {
	Provider string
	Artists  []Artist
	Err      error
}

// Registry holds the enabled providers.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string
	timeout   time.Duration
	logger    *slog.Logger
}

// NewRegistry returns an empty registry. perProviderTimeout bounds each
// provider's share of a search.
func NewRegistry(logger *slog.Logger, perProviderTimeout time.Duration) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		providers: make(map[string]Provider),
		timeout:   perProviderTimeout,
		logger:    logger,
	}
}

// Register adds a provider. A later registration under the same name wins.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := p.Name()
	if _, ok := r.providers[name]; !ok {
		r.order = append(r.order, name)
	}
	r.providers[name] = p
}

// Get returns the provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names lists registered providers in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// All lists registered providers in registration order.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.providers[name])
	}
	return out
}

// Search fans out to every searchable provider concurrently and returns one
// result per provider, in registration order. One provider failing (or timing
// out) never fails the whole search; providers that cannot search are skipped.
func (r *Registry) Search(ctx context.Context, q string, opts SearchOpts) []Result {
	providers := r.All()
	results := make([]Result, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		if !p.Capabilities().Search {
			results[i] = Result{Provider: p.Name(), Err: errNotSearchable}
			continue
		}
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			tracks, err := p.Search(cctx, q, opts)
			if err != nil {
				r.logger.Warn("provider search failed", "provider", p.Name(), "query", q, "error", err)
			}
			results[i] = Result{Provider: p.Name(), Tracks: tracks, Err: err}
		}(i, p)
	}
	wg.Wait()
	return results
}

// errNotSearchable marks providers that are registered but cannot search.
var errNotSearchable = errors.New("provider does not support search")

// IsSearchable reports whether a result came from a provider that ran a search.
func (r Result) IsSearchable() bool { return !errors.Is(r.Err, errNotSearchable) }

// IsSearchable reports whether an album result came from a provider that ran a
// search, rather than from one that cannot browse albums at all.
func (r AlbumResult) IsSearchable() bool { return !errors.Is(r.Err, errNotSearchable) }

// IsSearchable reports whether an artist result came from a provider that ran a
// search, rather than from one that cannot browse artists at all.
func (r ArtistResult) IsSearchable() bool { return !errors.Is(r.Err, errNotSearchable) }

// Download routes a download to a provider, refusing providers without the
// capability (Spotify) and unknown names.
func (r *Registry) Download(ctx context.Context, name, providerTrackID, destPath string) error {
	p, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotEnabled, name)
	}
	if !p.Capabilities().Download {
		return fmt.Errorf("%w: %s", ErrDownloadUnsupported, name)
	}
	return p.Download(ctx, providerTrackID, destPath)
}

// MissingDeps maps provider name to the binaries it needs but cannot find.
func (r *Registry) MissingDeps() map[string][]string {
	out := make(map[string][]string)
	for _, p := range r.All() {
		checker, ok := p.(DependencyChecker)
		if !ok {
			continue
		}
		if missing := checker.MissingDeps(); len(missing) > 0 {
			out[p.Name()] = missing
		}
	}
	return out
}

// SearchAlbums fans out to the providers that can browse albums.
func (r *Registry) SearchAlbums(ctx context.Context, q string, opts SearchOpts) []AlbumResult {
	providers := r.All()
	results := make([]AlbumResult, len(providers))
	var wg sync.WaitGroup

	for i, p := range providers {
		searcher, ok := p.(AlbumSearcher)
		if !ok || !p.Capabilities().SearchAlbums {
			results[i] = AlbumResult{Provider: p.Name(), Err: errNotSearchable}
			continue
		}
		wg.Add(1)
		go func(i int, p Provider, searcher AlbumSearcher) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			albums, err := searcher.SearchAlbums(cctx, q, opts)
			if err != nil {
				r.logger.Warn("provider album search failed", "provider", p.Name(), "query", q, "error", err)
			}
			results[i] = AlbumResult{Provider: p.Name(), Albums: albums, Err: err}
		}(i, p, searcher)
	}
	wg.Wait()
	return results
}

// SearchArtists fans out to the providers that can browse artists.
func (r *Registry) SearchArtists(ctx context.Context, q string, opts SearchOpts) []ArtistResult {
	providers := r.All()
	results := make([]ArtistResult, len(providers))
	var wg sync.WaitGroup

	for i, p := range providers {
		searcher, ok := p.(ArtistSearcher)
		if !ok || !p.Capabilities().SearchArtists {
			results[i] = ArtistResult{Provider: p.Name(), Err: errNotSearchable}
			continue
		}
		wg.Add(1)
		go func(i int, p Provider, searcher ArtistSearcher) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			artists, err := searcher.SearchArtists(cctx, q, opts)
			if err != nil {
				r.logger.Warn("provider artist search failed", "provider", p.Name(), "query", q, "error", err)
			}
			results[i] = ArtistResult{Provider: p.Name(), Artists: artists, Err: err}
		}(i, p, searcher)
	}
	wg.Wait()
	return results
}

// Album fetches one provider's album.
func (r *Registry) Album(ctx context.Context, name, providerAlbumID string) (*AlbumDetail, error) {
	p, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotEnabled, name)
	}
	searcher, ok := p.(AlbumSearcher)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotSearchableByAlbum, name)
	}
	return searcher.Album(ctx, providerAlbumID)
}

// ArtistAlbums lists one provider artist's albums.
func (r *Registry) ArtistAlbums(ctx context.Context, name, providerArtistID string) ([]Album, error) {
	p, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotEnabled, name)
	}
	searcher, ok := p.(ArtistSearcher)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotSearchableByArtist, name)
	}
	return searcher.ArtistAlbums(ctx, providerArtistID)
}

// Radio builds a station from seed using exactly the named providers, so a
// caller can choose what the radio is made of. Providers that are missing,
// disabled or unable to build a radio are reported instead of being skipped
// silently.
func (r *Registry) Radio(ctx context.Context, names []string, seed Track, limit int) []Result {
	results := make([]Result, len(names))
	var wg sync.WaitGroup

	for i, name := range names {
		p, ok := r.Get(name)
		if !ok {
			results[i] = Result{Provider: name, Err: fmt.Errorf("%w: %s", ErrNotEnabled, name)}
			continue
		}
		radio, ok := p.(RadioProvider)
		if !ok || !p.Capabilities().Radio {
			results[i] = Result{Provider: name, Err: fmt.Errorf("%w: %s", ErrRadioUnsupported, name)}
			continue
		}
		wg.Add(1)
		go func(i int, p Provider, radio RadioProvider) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			tracks, err := radio.Radio(cctx, seed, limit)
			if err != nil {
				r.logger.Warn("provider radio failed", "provider", p.Name(), "seed", seed.ProviderTrackID, "error", err)
			}
			results[i] = Result{Provider: p.Name(), Tracks: tracks, Err: err}
		}(i, p, radio)
	}
	wg.Wait()
	return results
}

var (
	// ErrNotSearchableByAlbum means the provider cannot browse albums.
	ErrNotSearchableByAlbum = errors.New("provider cannot browse albums")
	// ErrNotSearchableByArtist means the provider cannot browse artists.
	ErrNotSearchableByArtist = errors.New("provider cannot browse artists")
)
