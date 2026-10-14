package library

import (
	"context"
	"fmt"
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

// BenchmarkSearchPlaylists measures a playlist search that finds many hits. It
// documents the one per-hit query left in place: each hit is upserted on its
// own, so the search issues one read per hit, and the store has no batch upsert
// to replace it with. The first pass inserts; every pass after finds the rows
// unchanged and writes nothing, so what is timed is the read per hit.
func BenchmarkSearchPlaylists(b *testing.B) {
	db, err := store.Open("file:library-bench-pl-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	logger := slog.New(slog.DiscardHandler)
	registry := provider.NewRegistry(logger, 5*time.Second)

	const hits = 25
	fake := &fakeProvider{name: "ytmusic"}
	for i := range hits {
		fake.playlists = append(fake.playlists, provider.Playlist{
			ProviderPlaylistID: fmt.Sprintf("PL%d", i),
			Title:              fmt.Sprintf("Playlist %d", i),
			TrackCount:         10,
		})
	}
	registry.Register(fake)
	service := New(db, registry, match.New(db, registry, 0.8, logger), logger)

	ctx := context.Background()
	if _, _, err := service.SearchPlaylists(ctx, "mix", hits, nil); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, _, err := service.SearchPlaylists(ctx, "mix", hits, nil); err != nil {
			b.Fatal(err)
		}
	}
}