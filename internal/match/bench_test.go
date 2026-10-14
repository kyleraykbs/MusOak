package match

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// seedCandidates fills the library with n tracks that all share one duration,
// so every attach scores the whole set: this is the shape of a search against a
// library that has grown, and the point of the benchmark is how much the
// scoring step costs per candidate.
func seedCandidates(b *testing.B, db *store.DB, n int) []store.Track {
	b.Helper()
	ctx := context.Background()
	tracks := make([]store.Track, 0, n)
	for range n {
		track := store.Track{Title: "Candidate " + uuid.NewString(), DurationMs: 200_000}
		if err := db.CreateTrack(ctx, &track); err != nil {
			b.Fatal(err)
		}
		if err := db.SetTrackArtists(ctx, track.ID, []string{"An Artist"}); err != nil {
			b.Fatal(err)
		}
		if err := db.SetTrackAlbums(ctx, track.ID, []string{"An Album"}); err != nil {
			b.Fatal(err)
		}
		tracks = append(tracks, track)
	}
	return tracks
}

// BenchmarkAttachScoresCandidates measures one attach against a candidate set:
// the attached hit matches an existing track, so the canonical set does not
// grow while the benchmark runs and every iteration scans the same n tracks.
func BenchmarkAttachScoresCandidates(b *testing.B) {
	ctx := context.Background()
	db, err := store.Open("file:match-bench-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	const candidates = 200
	tracks := seedCandidates(b, db, candidates)

	registry := provider.NewRegistry(slog.New(slog.DiscardHandler), 5*time.Second)
	m := New(db, registry, 0.8, slog.New(slog.DiscardHandler))
	// The background match would spawn a goroutine and search the providers for
	// every iteration; the cost under test is the scoring, not that.
	m.background = nil

	hit := provider.Track{
		Title:      tracks[0].Title,
		Artists:    []string{"An Artist"},
		Album:      "An Album",
		DurationMs: 200_000,
	}

	b.ResetTimer()
	for b.Loop() {
		hit.ProviderTrackID = uuid.NewString()
		if _, _, err := m.Attach(ctx, "ytmusic", hit); err != nil {
			b.Fatal(err)
		}
	}
}
