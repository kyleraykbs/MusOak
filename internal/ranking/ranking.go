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

	"codeberg.org/kyleraykbs/musoak/internal/config"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// ErrInvalidRanking means the submitted order names an unknown or repeated
// provider.
var ErrInvalidRanking = fmt.Errorf("invalid provider ranking")

// SlotSelf and SlotUploaded are the two reserved names an effective order
// carries besides real providers. They are not providers: whichever side
// chooses a rendition resolves them against a track's user-sourced variants,
// SlotSelf to the caller's own upload and SlotUploaded to the best-liked
// upload by somebody else.
const (
	SlotSelf     = "self"
	SlotUploaded = "uploaded"
)

// slotNames is the reserved pair in preference order: your own copy first,
// the household's favourite second.
func slotNames() []string { return []string{SlotSelf, SlotUploaded} }

// IsSlot reports whether name is a reserved, non-provider order slot.
func IsSlot(name string) bool {
	return name == SlotSelf || name == SlotUploaded
}

// withSlots puts the reserved slots at the head of order, leaving the place of
// any slot the user ranked themselves. An order stored before the slots
// existed gains them here, so nobody loses the new choices.
func withSlots(order []string) []string {
	present := make(map[string]bool, 2)
	for _, name := range order {
		if IsSlot(name) {
			present[name] = true
		}
	}
	if len(present) == len(slotNames()) {
		return order
	}
	out := make([]string, 0, len(order)+len(slotNames())-len(present))
	for _, name := range slotNames() {
		if !present[name] {
			out = append(out, name)
		}
	}
	return append(out, order...)
}

// providersOnly strips the reserved slots, leaving the real providers of a
// stored order. Slots are positions in a user's own list, never providers that
// the aggregate should average.
func providersOnly(order []string) []string {
	out := make([]string, 0, len(order))
	for _, name := range order {
		if !IsSlot(name) {
			out = append(out, name)
		}
	}
	return out
}

// Service stores per-user rankings and caches the aggregate order.
type Service struct {
	db       store.RankingRepo
	defaults []string
	logger   *slog.Logger

	mu        sync.RWMutex
	enabled   []string
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

// SetEnabledProviders records the enabled provider names in registration
// order. Every order this service hands out is completed with them, so a
// configured default that predates a provider still covers it.
func (s *Service) SetEnabledProviders(names []string) {
	s.mu.Lock()
	s.enabled = append([]string(nil), names...)
	s.dirty = true
	s.mu.Unlock()
}

// complete appends the enabled providers that order does not already name, in
// registration order. It never disturbs what is already there.
func complete(enabled, order []string) []string {
	if len(enabled) == 0 {
		return order
	}
	seen := make(map[string]bool, len(order)+len(enabled))
	for _, name := range order {
		seen[name] = true
	}
	missing := 0
	for _, name := range enabled {
		if !seen[name] {
			missing++
		}
	}
	if missing == 0 {
		return order
	}
	out := make([]string, 0, len(order)+missing)
	out = append(out, order...)
	for _, name := range enabled {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Validate checks a submitted order: known providers, no repeats.
func Validate(providers []string) error {
	if len(providers) == 0 {
		return fmt.Errorf("%w: at least one provider is required", ErrInvalidRanking)
	}
	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if !config.KnownProvider(provider) && !IsSlot(provider) {
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
// Either way it ends with any enabled provider the order did not name.
func (s *Service) User(ctx context.Context, userID uuid.UUID) ([]string, error) {
	own, err := s.db.Ranking(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(own) > 0 {
		s.mu.RLock()
		enabled := append([]string(nil), s.enabled...)
		s.mu.RUnlock()
		return withSlots(complete(enabled, own)), nil
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
	enabled := append([]string(nil), s.enabled...)
	s.mu.RUnlock()

	all, err := s.db.AllRankings(ctx)
	if err != nil {
		return nil, err
	}

	// A stored order may name the reserved slots; they lead every effective
	// order but are not providers, so the mean is over the providers alone.
	rankings := make([][]string, 0, len(all))
	for _, providers := range all {
		if ranked := providersOnly(providers); len(ranked) > 0 {
			rankings = append(rankings, ranked)
		}
	}
	if len(rankings) == 0 {
		return withSlots(complete(enabled, append([]string(nil), s.defaults...))), nil
	}

	universe := make(map[string]bool)
	for _, provider := range s.defaults {
		universe[provider] = true
	}
	contributing := len(rankings)
	for _, providers := range rankings {
		for _, provider := range providers {
			universe[provider] = true
		}
	}

	sum := make(map[string]float64, len(universe))
	for _, providers := range rankings {
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

	order = withSlots(complete(enabled, order))

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
