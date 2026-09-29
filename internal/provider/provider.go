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
