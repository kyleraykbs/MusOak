package cli

import (
	"context"
	"errors"
	"fmt"

	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

// cmdPlay plays the local queue through mpv, gaplessly: the next tracks are
// downloaded while the current one plays, and each finished file is appended
// to mpv's playlist before it is needed.
func (a *App) cmdPlay(ctx context.Context, args []string) error {
	queue := a.state.Queue
	if len(queue) == 0 {
		return errors.New("prism: the queue is empty; add tracks with \"prism queue add\"")
	}

	sink, err := NewMpvSink(ctx, a.socketPath(), a.logger)
	if err != nil {
		return err
	}
	defer sink.Close()

	prefetch := a.cfg.PrefetchCount
	if prefetch < 0 {
		prefetch = 0
	}
	a.printf("playing %d track(s), %d prefetched ahead\n", len(queue), prefetch)

	// One download per queue entry, started as the window slides forward.
	type ready struct {
		item  queueItem
		entry client.CacheEntry
		err   error
	}
	slots := make([]chan ready, len(queue))
	start := func(index int) {
		slots[index] = make(chan ready, 1)
		go func() {
			entry, err := a.ensureTrack(ctx, queue[index])
			slots[index] <- ready{item: queue[index], entry: entry, err: err}
		}()
	}

	queued := 0
	start(0)
	for queued < len(queue) {
		for i := queued; i < len(queue) && i <= queued+prefetch; i++ {
			if slots[i] == nil {
				start(i)
			}
		}

		slot := <-slots[queued]
		if slot.err != nil {
			return fmt.Errorf("prism: %s: %w", slot.item.Title, slot.err)
		}
		a.printf("%3d. %s\n", queued, slot.item.Title)

		if queued == 0 {
			if err := sink.StartFile(ctx, slot.entry.Path, 0); err != nil {
				return err
			}
		} else if err := sink.AppendFile(ctx, slot.entry.Path); err != nil {
			return err
		}
		queued++
	}

	return sink.WaitIdle(ctx)
}

// ensureTrack makes sure a rendition of the track is on disk locally. A copy
// this client already has is used as-is; otherwise the earliest-ranked
// downloadable variant is fetched (resolving one first when the track only has
// metadata-only renditions).
func (a *App) ensureTrack(ctx context.Context, item queueItem) (client.CacheEntry, error) {
	if entry, ok := a.cache.LookupTrack(item.TrackID); ok {
		return entry, nil
	}

	order := a.providerOrder(ctx)
	variants, err := a.client.TrackVariants(ctx, item.TrackID)
	if err != nil {
		return client.CacheEntry{}, err
	}

	variant, ok := pickVariant(order, variants)
	if !ok {
		resolved, playable, err := a.client.ResolveTrack(ctx, item.TrackID)
		if err != nil {
			return client.CacheEntry{}, err
		}
		if !playable {
			return client.CacheEntry{}, errors.New("no downloadable rendition found")
		}
		if variant, ok = pickVariant(order, resolved); !ok {
			return client.CacheEntry{}, errors.New("no downloadable rendition found")
		}
	}

	entry, err := a.cache.Fetch(ctx, a.client, variant.ID)
	if err != nil {
		return client.CacheEntry{}, err
	}
	entry.TrackID = item.TrackID
	if err := a.cache.Put(entry); err != nil {
		return client.CacheEntry{}, err
	}
	return entry, nil
}

// providerOrder is the order that decides which rendition is played: the
// user's effective ranking, else the downloadable providers, else the
// configured default.
func (a *App) providerOrder(ctx context.Context) []string {
	if ranking, err := a.client.Ranking(ctx); err == nil && len(ranking.Effective) > 0 {
		return ranking.Effective
	}
	providers, err := a.client.Providers(ctx)
	if err == nil && len(providers) > 0 {
		order := make([]string, 0, len(providers))
		for _, provider := range providers {
			if provider.Capabilities.Download {
				order = append(order, provider.Name)
			}
		}
		for _, provider := range providers {
			if !provider.Capabilities.Download {
				order = append(order, provider.Name)
			}
		}
		if len(order) > 0 {
			return order
		}
	}
	return a.cfg.DefaultProviderOrder
}

// pickVariant returns the earliest-ranked downloadable variant. Providers
// outside the order come last, tie-breaking by name so playback is stable.
func pickVariant(order []string, variants []client.Variant) (client.Variant, bool) {
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
		return client.Variant{}, false
	}
	return variants[best], true
}
