// Package ranking decides which provider to prefer when a track has several
// renditions: a user's own order, or the aggregate across all users.
package ranking

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// ErrInvalidRanking means the submitted order names an unknown or repeated
// provider.
var ErrInvalidRanking = fmt.Errorf("invalid provider ranking")

// Service stores per-user rankings and caches the aggregate order.
type Service struct {
	db       store.RankingRepo
	defaults []string
	logger   *slog.Logger

	mu        sync.RWMutex
	aggregate []string
	dirty     bool
}

// New returns a ranking service falling back to defaults when nobody has
// ranked anything.
func New(db store.RankingRepo, defaults []string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:       db,
		defaults: append([]string(nil), defaults...),
		logger:   logger,
		dirty:    true,
	}
}

// Validate checks a submitted order: known providers, no repeats.
func Validate(providers []string) error {
	if len(providers) == 0 {
		return fmt.Errorf("%w: at least one provider is required", ErrInvalidRanking)
	}
	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if !config.KnownProvider(provider) {
			return fmt.Errorf("%w: unknown provider %q", ErrInvalidRanking, provider)
		}
		if seen[provider] {
			return fmt.Errorf("%w: duplicate provider %q", ErrInvalidRanking, provider)
		}
		seen[provider] = true
	}
	return nil
}

// Set replaces a user's order and invalidates the aggregate cache.
func (s *Service) Set(ctx context.Context, userID uuid.UUID, providers []string) error {
	if err := Validate(providers); err != nil {
		return err
	}
	if err := s.db.SetRanking(ctx, userID, providers); err != nil {
		return err
	}
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	return nil
}

// User returns the effective order for a user: their own, or the aggregate.
func (s *Service) User(ctx context.Context, userID uuid.UUID) ([]string, error) {
	own, err := s.db.Ranking(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(own) > 0 {
		return own, nil
	}
	return s.Aggregate(ctx)
}

// Aggregate returns the mean-position order across all ranked users. Providers
// a user did not rank count as last for that user. With no rankings at all it
// returns the configured default order.
func (s *Service) Aggregate(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	if !s.dirty && s.aggregate != nil {
		out := append([]string(nil), s.aggregate...)
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	all, err := s.db.AllRankings(ctx)
	if err != nil {
		return nil, err
	}

	universe := make(map[string]bool)
	for _, provider := range s.defaults {
		universe[provider] = true
	}
	contributing := 0
	for _, providers := range all {
		if len(providers) == 0 {
			continue
		}
		contributing++
		for _, provider := range providers {
			universe[provider] = true
		}
	}
	if contributing == 0 {
		return append([]string(nil), s.defaults...), nil
	}

	sum := make(map[string]float64, len(universe))
	for _, providers := range all {
		if len(providers) == 0 {
			continue
		}
		rank := make(map[string]float64, len(providers))
		for i, provider := range providers {
			rank[provider] = float64(i) / float64(len(providers))
		}
		for provider := range universe {
			if r, ranked := rank[provider]; ranked {
				sum[provider] += r
			} else {
				// Unranked providers count as last for this user.
				sum[provider] += 1
			}
		}
	}

	order := make([]string, 0, len(universe))
	for provider := range universe {
		order = append(order, provider)
	}
	mean := func(provider string) float64 { return sum[provider] / float64(contributing) }
	sort.Slice(order, func(i, j int) bool {
		mi, mj := mean(order[i]), mean(order[j])
		if mi != mj {
			return mi < mj
		}
		di, dj := s.defaultIndex(order[i]), s.defaultIndex(order[j])
		if di != dj {
			return di < dj
		}
		return order[i] < order[j]
	})

	s.mu.Lock()
	s.aggregate = append([]string(nil), order...)
	s.dirty = false
	s.mu.Unlock()

	s.logger.Debug("ranking: aggregate recomputed", "order", order, "users", contributing)
	return order, nil
}

// Pick selects the variant to play given an effective provider order: the
// earliest-ranked downloadable variant. Providers outside the order sort last
// and tie-break by name, so the choice is always deterministic. Metadata-only
// variants (Spotify) are never picked for playback.
func Pick(order []string, variants []store.Variant) (store.Variant, bool) {
	position := make(map[string]int, len(order))
	for i, provider := range order {
		if _, seen := position[provider]; !seen {
			position[provider] = i
		}
	}

	best := -1
	bestPos := 0
	for i, variant := range variants {
		if !variant.Downloadable {
			continue
		}
		pos, known := position[variant.Provider]
		if !known {
			pos = len(order)
		}
		switch {
		case best == -1:
		case pos < bestPos:
		case pos == bestPos && variant.Provider < variants[best].Provider:
		default:
			continue
		}
		best, bestPos = i, pos
	}
	if best == -1 {
		return store.Variant{}, false
	}
	return variants[best], true
}

// DefaultIndex returns the position of a provider in the configured default
// order, or len(defaults) when it is absent; it breaks aggregate ties.
func (s *Service) defaultIndex(provider string) int {
	for i, name := range s.defaults {
		if name == provider {
			return i
		}
	}
	return len(s.defaults)
}
