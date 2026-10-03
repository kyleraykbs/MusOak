package api

import (
	"context"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/media"
	"codeberg.org/kyleraykbs/musoak/internal/prefetch"
)

// Keeping the playlists downloaded.
//
// What people collect is what they are likely to play, and the first play of a
// song that is not on disk is a provider fetch. The server walks the playlists
// in the background so that first play costs nothing; the work is paced and
// best-effort, and a play that wants the same song shares the download rather
// than racing it.

// playlistPrefetch adapts the server's own pieces to what the prefetcher needs:
// the playlists, the matcher that finds a playable rendition, and the media
// store that fetches it.
type playlistPrefetch struct {
	server *Server
}

func (a playlistPrefetch) PlaylistTrackIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.server.store.PlaylistTrackIDs(ctx)
}

func (a playlistPrefetch) Resolve(ctx context.Context, trackID uuid.UUID) (uuid.UUID, error) {
	// A track that only arrived as metadata needs a rendition found for it
	// first; that is the same step a play takes.
	if _, err := a.server.matcher.Resolve(ctx, trackID); err != nil {
		return uuid.Nil, err
	}
	variant, err := a.server.playableVariant(ctx, trackID)
	if err != nil {
		return uuid.Nil, err
	}
	return variant.ID, nil
}

func (a playlistPrefetch) Ready(ctx context.Context, variantID uuid.UUID) bool {
	return a.server.media.Status(ctx, variantID).State == media.StateReady
}

func (a playlistPrefetch) Ensure(ctx context.Context, variantID uuid.UUID) (string, error) {
	return a.server.media.Ensure(ctx, variantID)
}

// startPrefetch builds the prefetcher when the configuration wants one.
func (s *Server) startPrefetch() {
	if !s.cfg.Media.PrefetchPlaylists {
		return
	}
	// The one adapter answers all three questions: what the playlists hold, how
	// to get a playable rendition of a track, and how to fetch it.
	adapter := playlistPrefetch{server: s}
	s.prefetcher = prefetch.New(adapter, adapter, adapter, s.logger, prefetch.Options{})
}

// nudgePrefetch says the playlists changed, so the next pass is soon rather
// than whenever the clock comes round.
func (s *Server) nudgePrefetch() {
	if s.prefetcher != nil {
		s.prefetcher.Nudge()
	}
}
