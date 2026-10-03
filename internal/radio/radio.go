// Package radio builds a station from a seed track.
//
// A radio is a mix, not a list: the caller chooses which providers feed it, and
// the station interleaves their suggestions so the result reflects that choice
// instead of whatever answered first. Every suggestion is matched into the
// canonical library, so the same recording never appears twice.
package radio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/match"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// DefaultLength is how many tracks a radio holds when the caller does not say.
const DefaultLength = 25

// MaxLength bounds a radio, so a hostile request cannot ask for thousands.
const MaxLength = 200

// Errors.
var (
	// ErrNoSeed means the seed track does not exist.
	ErrNoSeed = errors.New("seed track not found")
	// ErrNoSeedVariant means no selected provider has a rendition of the seed,
	// so no station can be grown from it.
	ErrNoSeedVariant = errors.New("no selected provider has the seed track")
)

// Store is the repository slice the radio needs.
type Store interface {
	store.TrackRepo
	store.VariantRepo
}

// Service turns seeds into stations.
type Service struct {
	db        Store
	providers *provider.Registry
	matcher   *match.Matcher
	logger    *slog.Logger
}

// New returns a radio service.
func New(db Store, providers *provider.Registry, matcher *match.Matcher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, providers: providers, matcher: matcher, logger: logger}
}

// Result is a generated station.
type Result struct {
	Seed      store.Track
	Providers []string
	Tracks    []store.Track
	// Errors reports per-provider failures; a provider that failed simply did
	// not contribute.
	Errors []ProviderError
}

// ProviderError is one provider's failure while building a radio.
type ProviderError struct {
	Provider string
	Error    string
}

// Generate builds a station from a seed track using exactly the given
// providers. An empty provider list means every registered provider that can
// build a radio.
func (s *Service) Generate(ctx context.Context, seedTrackID uuid.UUID, providers []string, length int) (*Result, error) {
	if length <= 0 {
		length = DefaultLength
	}
	if length > MaxLength {
		length = MaxLength
	}

	seed, err := s.db.Track(ctx, seedTrackID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNoSeed, seedTrackID)
		}
		return nil, err
	}
	variants, err := s.db.VariantsForTrack(ctx, seedTrackID)
	if err != nil {
		return nil, err
	}

	selected := s.selectProviders(providers)
	result := &Result{Seed: *seed, Providers: selected}

	// Each provider needs its own rendition of the seed to grow a station.
	seeds := make(map[string]provider.Track, len(selected))
	for _, name := range selected {
		variant, ok := seedVariantFor(variants, name)
		if !ok {
			result.Errors = append(result.Errors, ProviderError{
				Provider: name,
				Error:    fmt.Sprintf("no %s rendition of the seed track; resolve it first", name),
			})
			continue
		}
		seeds[name] = provider.Track{
			ProviderTrackID: variant.ProviderTrackID,
			Title:           variant.Title,
			Artists:         variant.Artists,
			Album:           variant.Album,
			DurationMs:      variant.DurationMs,
			ISRC:            variant.ISRC,
		}
	}

	// Ask only the providers that have a seed, so a station is never grown
	// from silence.
	asking := make([]string, 0, len(seeds))
	for _, name := range selected {
		if _, ok := seeds[name]; ok {
			asking = append(asking, name)
		}
	}
	if len(asking) == 0 {
		return result, ErrNoSeedVariant
	}

	results := s.radioPerProvider(ctx, asking, seeds, length)
	for _, providerResult := range results {
		if providerResult.Err != nil {
			result.Errors = append(result.Errors, ProviderError{
				Provider: providerResult.Provider,
				Error:    providerResult.Err.Error(),
			})
		}
	}

	tracks, err := s.assemble(ctx, results, seed.ID, length)
	if err != nil {
		return nil, err
	}
	result.Tracks = tracks
	return result, nil
}

// radioPerProvider asks each provider for its own station from its own seed
// rendition, in parallel. The registry's fan-out takes a single seed, but every
// provider needs its own rendition id, so they are called one at a time each.
func (s *Service) radioPerProvider(ctx context.Context, names []string, seeds map[string]provider.Track, length int) []provider.Result {
	out := make([]provider.Result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results := s.providers.Radio(ctx, []string{name}, seeds[name], length)
			if len(results) > 0 {
				out[i] = results[0]
			}
		}(i, name)
	}
	wg.Wait()
	return out
}

// assemble interleaves the providers' suggestions, matches them into canonical
// tracks and drops the seed itself.
func (s *Service) assemble(ctx context.Context, results []provider.Result, seedTrack uuid.UUID, length int) ([]store.Track, error) {
	// Round-robin so a station made of two providers alternates between them.
	queues := make([][]provider.Track, 0, len(results))
	names := make([]string, 0, len(results))
	for _, providerResult := range results {
		if providerResult.Err != nil || len(providerResult.Tracks) == 0 {
			continue
		}
		queues = append(queues, providerResult.Tracks)
		names = append(names, providerResult.Provider)
	}

	var (
		tracks []store.Track
		seen   = map[uuid.UUID]bool{seedTrack: true}
	)
	for round := 0; len(tracks) < length; round++ {
		progressed := false
		for i, queue := range queues {
			if round >= len(queue) {
				continue
			}
			progressed = true
			hit := queue[round]
			track, _, err := s.matcher.Attach(ctx, names[i], hit)
			if err != nil {
				return nil, fmt.Errorf("radio: match %s/%s: %w", names[i], hit.ProviderTrackID, err)
			}
			if seen[track.ID] {
				continue
			}
			seen[track.ID] = true
			tracks = append(tracks, *track)
			if len(tracks) >= length {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return tracks, nil
}

// selectProviders keeps the requested order and drops names that cannot
// contribute, reporting them through the result.
func (s *Service) selectProviders(requested []string) []string {
	available := make([]string, 0)
	for _, p := range s.providers.All() {
		if p.Capabilities().Radio {
			available = append(available, p.Name())
		}
	}
	if len(requested) == 0 {
		return available
	}

	selected := make([]string, 0, len(requested))
	for _, name := range requested {
		for _, candidate := range available {
			if candidate == name {
				selected = append(selected, name)
				break
			}
		}
	}
	return selected
}

func seedVariantFor(variants []store.Variant, providerName string) (store.Variant, bool) {
	for _, variant := range variants {
		if variant.Provider == providerName {
			return variant, true
		}
	}
	return store.Variant{}, false
}
