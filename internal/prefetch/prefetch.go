// Package prefetch keeps what people have collected on disk before they ask
// for it.
//
// A playlist is a promise about what will be played, and the first play of a
// song that is not on disk is a provider fetch: seconds of nothing. Walking the
// playlists quietly, one song at a time, is what makes that first play cost
// nothing - for the same reason a player warms the tracks it is about to reach.
//
// The work is best-effort. A song with nothing playable, or a provider that is
// down, is skipped and tried again on the next pass, and nothing here is ever
// what a person's play is waiting behind: the media store shares one download
// between everybody who wants the same file.
package prefetch

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultInterval is how long between passes when nothing asks for one.
	DefaultInterval = 10 * time.Minute
	// DefaultGap is the breath between songs that actually needed fetching.
	// It paces a pass over a large library and keeps this behind live work.
	DefaultGap = 2 * time.Second
	// DefaultTimeout bounds one pass, so a provider that hangs does not hold
	// the loop for ever.
	DefaultTimeout = 30 * time.Minute
)

// Playlists reads what has been collected.
type Playlists interface {
	// PlaylistTrackIDs lists every track any playlist holds, once each.
	PlaylistTrackIDs(ctx context.Context) ([]uuid.UUID, error)
}

// Resolver turns a track into a rendition that can be played, finding one from
// a provider when the library only knows the track by name.
type Resolver interface {
	Resolve(ctx context.Context, trackID uuid.UUID) (uuid.UUID, error)
}

// Media is the download store: what is already on disk, and how to fetch it.
type Media interface {
	// Ready reports whether a rendition is already on disk.
	Ready(ctx context.Context, variantID uuid.UUID) bool
	// Ensure fetches a rendition, sharing one download with anybody else who
	// wants the same file.
	Ensure(ctx context.Context, variantID uuid.UUID) (string, error)
}

// Options tune a Prefetcher. A zero value is the default behaviour.
type Options struct {
	// Interval is how often the playlists are walked on their own, so one that
	// changed without saying so is still picked up.
	Interval time.Duration
	// Gap is the pause after a song that had to be fetched.
	Gap time.Duration
	// Timeout bounds a single pass.
	Timeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Gap <= 0 {
		o.Gap = DefaultGap
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	return o
}

// Prefetcher walks the playlists and gets their songs onto disk.
type Prefetcher struct {
	playlists Playlists
	resolver  Resolver
	media     Media
	logger    *slog.Logger
	options   Options
	nudge     chan struct{}
}

func New(playlists Playlists, resolver Resolver, media Media, logger *slog.Logger, options Options) *Prefetcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Prefetcher{
		playlists: playlists,
		resolver:  resolver,
		media:     media,
		logger:    logger,
		options:   options.withDefaults(),
		nudge:     make(chan struct{}, 1),
	}
}

// Run walks the playlists until the context ends: once at the start, whenever
// somebody asks, and every interval in case nothing did.
func (p *Prefetcher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.options.Interval)
	defer ticker.Stop()
	for {
		p.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.nudge:
		case <-ticker.C:
		}
	}
}

// Nudge asks for a pass soon, for when the playlists changed.
func (p *Prefetcher) Nudge() {
	select {
	case p.nudge <- struct{}{}:
	default:
		// A pass is already pending, and one is all it takes.
	}
}

// pass walks every playlist song once, fetching the ones that are missing.
func (p *Prefetcher) pass(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, p.options.Timeout)
	defer cancel()

	tracks, err := p.playlists.PlaylistTrackIDs(ctx)
	if err != nil {
		p.logger.Warn("prefetch: could not read the playlists", "error", err)
		return
	}

	ready, fetched := 0, 0
	for _, trackID := range tracks {
		if ctx.Err() != nil {
			break
		}
		variantID, err := p.resolver.Resolve(ctx, trackID)
		if err != nil {
			p.logger.Debug("prefetch: no playable rendition yet", "track", trackID, "error", err)
			continue
		}
		if p.media.Ready(ctx, variantID) {
			ready++
			continue
		}
		if _, err := p.media.Ensure(ctx, variantID); err != nil {
			p.logger.Debug("prefetch: could not fetch a playlist song", "variant", variantID, "error", err)
			continue
		}
		fetched++
		// Only a real fetch is worth pacing: a library that is already here
		// should be walked through quickly.
		if !sleep(ctx, p.options.Gap) {
			break
		}
	}
	p.logger.Info("prefetch pass done", "songs", len(tracks), "already here", ready, "fetched", fetched)
}

// sleep waits, and reports whether the wait finished rather than being called
// off.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
