package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// cmdPlay plays the local queue through mpv: the next tracks are prepared
// while the current one plays, and a track that is not on disk yet is streamed
// from the server over HTTP. As soon as its download lands, the playing track
// is swapped to the local file without losing its position.
func (a *App) cmdPlay(ctx context.Context, args []string) error {
	queue := a.state.Queue
	if len(queue) == 0 {
		return errors.New("musoak: the queue is empty; add tracks with \"musoak queue add\"")
	}

	sink, err := NewMpvSink(ctx, a.socketPath(), a.logger)
	if err != nil {
		return err
	}
	defer sink.Close()

	ui := newPlayer(a.out, sink, queue)
	ui.mediaStatus = a.client.MediaStatus
	ui.start(ctx)
	defer ui.stop()

	prefetch := a.cfg.PrefetchCount
	if prefetch < 0 {
		prefetch = 0
	}
	if !ui.interactive {
		a.printf("playing %d track(s), %d prefetched ahead\n", len(queue), prefetch)
	}

	// The provider order decides which rendition plays and does not change
	// between tracks, so it is resolved at most once for the whole play: once
	// per slot meant two round trips per track, and none at all for a track
	// already on disk.
	var (
		orderOnce sync.Once
		order     []string
	)
	providerOrder := func(ctx context.Context) []string {
		orderOnce.Do(func() { order = a.providerOrder(ctx) })
		return order
	}

	// One slot per queue entry, started as the window slides forward. A slot
	// resolves its rendition and downloads it in the background; the main loop
	// only waits for the resolution, never for the bytes.
	slots := make([]*slot, len(queue))
	queued := 0
	slots[0] = a.startSlot(ctx, queue[0], providerOrder)
	for queued < len(queue) {
		for i := queued; i < len(queue) && i <= queued+prefetch; i++ {
			if slots[i] == nil {
				slots[i] = a.startSlot(ctx, queue[i], providerOrder)
			}
		}

		current := slots[queued]
		<-current.resolved
		if current.err != nil {
			return fmt.Errorf("musoak: %s: %w", current.item.Title, current.err)
		}
		if !ui.interactive {
			a.printf("%3d. %s\n", queued, current.item.Title)
		}

		item := a.slotItem(current)
		if queued == 0 {
			if err := ui.startTrack(ctx, item); err != nil {
				return err
			}
		} else if err := ui.appendTrack(ctx, item); err != nil {
			return err
		}
		queued++
	}

	return ui.wait(ctx)
}

// slot prepares one queue entry. Resolution, delivery and completion are
// deliberately separate: the resolved variant id — which the stream URL
// depends on — is published through resolved as soon as it is known, while the
// finished file arrives on local later. The player can therefore start a track
// from its URL long before the download finishes.
type slot struct {
	item queueItem

	// variantID and err are set before resolved is closed.
	variantID string
	err       error
	resolved  chan struct{}

	// local receives the finished rendition, then closes. It stays empty when
	// resolution failed or the local copy could not be written.
	local chan client.CacheEntry
}

// playbackItem is what the player hands mpv for one queue entry: where to play
// it from now, and where the local copy will land.
type playbackItem struct {
	variantID string
	// source is what mpv loads now: a local path, or a stream URL for a
	// rendition the server is still preparing.
	source string
	// localPath is the on-disk copy; it is set once the file is there.
	localPath string
	// ready yields the local copy when the download lands. It is nil when the
	// copy was already on disk (source is then the local path).
	ready <-chan client.CacheEntry
}

// startSlot resolves one queue entry and makes sure a local copy exists. The
// provider order is the play session's, resolved lazily by the caller so every
// slot shares it.
func (a *App) startSlot(ctx context.Context, item queueItem, order func(context.Context) []string) *slot {
	s := &slot{
		item:     item,
		resolved: make(chan struct{}),
		local:    make(chan client.CacheEntry, 1),
	}
	logger := a.logger
	if logger == nil {
		logger = slog.Default()
	}

	go func() {
		defer close(s.local)

		// A copy this client already has needs no resolution and no download.
		if entry, ok := a.cache.LookupTrack(item.TrackID); ok {
			s.variantID = entry.VariantID
			s.local <- entry
			close(s.resolved)
			return
		}

		variant, err := a.resolveVariant(ctx, item, order(ctx))
		if err != nil {
			s.err = err
			close(s.resolved)
			return
		}
		s.variantID = variant.ID
		close(s.resolved)

		// The transfer must outlive a skip, a track change or quitting play:
		// none of those may strand a half-written file. Detaching the command
		// context keeps the download running to completion, exactly as the
		// server detaches its own transfer from the request that started it.
		entry, err := a.cache.Fetch(context.WithoutCancel(ctx), a.client, variant.ID)
		if err != nil {
			logger.Warn("musoak: could not cache the rendition", "track", item.TrackID, "variant", variant.ID, "error", err)
			return
		}
		entry.TrackID = item.TrackID
		if err := a.cache.Put(entry); err != nil {
			logger.Warn("musoak: index the cached rendition", "track", item.TrackID, "error", err)
		}
		s.local <- entry
	}()
	return s
}

// slotItem turns a resolved slot into what the player should play. It never
// waits: a rendition that is not on disk yet is delivered as its stream URL
// and the local copy is handed over later through ready.
func (a *App) slotItem(s *slot) playbackItem {
	if entry, ok := a.cache.Lookup(s.variantID); ok {
		return playbackItem{
			variantID: s.variantID,
			source:    entry.Path,
			localPath: entry.Path,
		}
	}
	return playbackItem{
		variantID: s.variantID,
		source:    a.client.MediaURL(s.variantID),
		ready:     s.local,
	}
}

// resolveVariant picks the rendition to play for a track: the earliest-ranked
// downloadable variant, resolving one first when the track only has
// metadata-only renditions. order is the play session's effective provider
// order, resolved once by the caller.
func (a *App) resolveVariant(ctx context.Context, item queueItem, order []string) (client.Variant, error) {
	variants, err := a.client.TrackVariants(ctx, item.TrackID)
	if err != nil {
		return client.Variant{}, err
	}
	if variant, ok := pickVariant(order, variants); ok {
		return variant, nil
	}

	resolved, playable, err := a.client.ResolveTrack(ctx, item.TrackID)
	if err != nil {
		return client.Variant{}, err
	}
	if !playable {
		return client.Variant{}, errors.New("no downloadable rendition found")
	}
	variant, ok := pickVariant(order, resolved)
	if !ok {
		return client.Variant{}, errors.New("no downloadable rendition found")
	}
	return variant, nil
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

	// A rendition's place is its slot when it has one - the account's own
	// upload and the household's favourite are positions in the same order -
	// and its provider's name otherwise.
	place := func(variant client.Variant) string {
		if variant.Slot != "" {
			return variant.Slot
		}
		return variant.Provider
	}

	best := -1
	bestPos := 0
	for i, variant := range variants {
		if !variant.Downloadable {
			continue
		}
		pos, known := position[place(variant)]
		if !known {
			pos = len(order)
		}
		switch {
		case best == -1:
		case pos < bestPos:
		case pos == bestPos && place(variant) < place(variants[best]):
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
