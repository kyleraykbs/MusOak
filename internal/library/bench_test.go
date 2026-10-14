package library

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/match"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// BenchmarkGetAlbum reads an album whose tracklist is long enough that the
// per-track credit read dominates: one query per song against the batched
// read's one for the whole list.
func BenchmarkGetAlbum(b *testing.B) {
	db, err := store.Open("file:library-bench-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	logger := slog.New(slog.DiscardHandler)
	registry := provider.NewRegistry(logger, 5*time.Second)
	service := New(db, registry, match.New(db, registry, 0.8, logger), logger)

	ctx := context.Background()
	albumID, err := db.EnsureAlbum(ctx, "Whenever You Need Somebody", []string{"Rick Astley"})
	if err != nil {
		b.Fatal(err)
	}
	const tracks = 30
	ids := make([]uuid.UUID, 0, tracks)
	for range tracks {
		track := store.Track{Title: "Track " + uuid.NewString(), DurationMs: 200_000}
		if err := db.CreateTrack(ctx, &track); err != nil {
			b.Fatal(err)
		}
		if err := db.SetTrackArtists(ctx, track.ID, []string{"Rick Astley"}); err != nil {
			b.Fatal(err)
		}
		ids = append(ids, track.ID)
	}
	if _, err := db.AppendAlbumTracks(ctx, albumID, ids); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := service.GetAlbum(ctx, albumID); err != nil {
			b.Fatal(err)
		}
	}
}