package prefetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakePlaylists struct {
	tracks []uuid.UUID
	err    error
}

func (f fakePlaylists) PlaylistTrackIDs(context.Context) ([]uuid.UUID, error) {
	return f.tracks, f.err
}

type fakeResolver struct {
	variants map[uuid.UUID]uuid.UUID
	missing  map[uuid.UUID]bool
	resolved []uuid.UUID
}

func (f *fakeResolver) Resolve(_ context.Context, trackID uuid.UUID) (uuid.UUID, error) {
	f.resolved = append(f.resolved, trackID)
	if f.missing[trackID] {
		return uuid.Nil, errors.New("nothing playable")
	}
	return f.variants[trackID], nil
}

type fakeMedia struct {
	ready   map[uuid.UUID]bool
	fail    map[uuid.UUID]bool
	ensured []uuid.UUID
}

func (f *fakeMedia) Ready(_ context.Context, variantID uuid.UUID) bool { return f.ready[variantID] }

func (f *fakeMedia) Ensure(_ context.Context, variantID uuid.UUID) (string, error) {
	if f.fail[variantID] {
		return "", errors.New("provider said no")
	}
	if f.ready == nil {
		f.ready = map[uuid.UUID]bool{}
	}
	f.ready[variantID] = true
	f.ensured = append(f.ensured, variantID)
	return "/media/" + variantID.String(), nil
}

func newTestPrefetcher(t *testing.T, playlists Playlists, resolver Resolver, media Media) *Prefetcher {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(playlists, resolver, media, logger, Options{Gap: time.Millisecond})
}

func TestPassFetchesWhatThePlaylistsHold(t *testing.T) {
	one, two := uuid.New(), uuid.New()
	playlists := &fakePlaylists{tracks: []uuid.UUID{one, two}}
	resolver := &fakeResolver{variants: map[uuid.UUID]uuid.UUID{one: uuid.New(), two: uuid.New()}}
	media := &fakeMedia{}

	newTestPrefetcher(t, playlists, resolver, media).pass(context.Background())

	if len(media.ensured) != 2 {
		t.Fatalf("fetched %d songs, want 2", len(media.ensured))
	}
}

func TestPassLeavesWhatIsAlreadyHereAlone(t *testing.T) {
	one, two := uuid.New(), uuid.New()
	here := uuid.New()
	playlists := &fakePlaylists{tracks: []uuid.UUID{one, two}}
	resolver := &fakeResolver{variants: map[uuid.UUID]uuid.UUID{one: here, two: uuid.New()}}
	media := &fakeMedia{ready: map[uuid.UUID]bool{here: true}}

	newTestPrefetcher(t, playlists, resolver, media).pass(context.Background())

	if len(media.ensured) != 1 {
		t.Fatalf("fetched %d songs, want only the one that was missing", len(media.ensured))
	}
	if media.ensured[0] == here {
		t.Fatal("fetched a rendition that was already on disk")
	}
}

// A song nobody can play, or a provider that refuses, is not a reason to give
// up on the rest of the playlists.
func TestPassCarriesOnPastFailures(t *testing.T) {
	missing, refused, good := uuid.New(), uuid.New(), uuid.New()
	refusedVariant := uuid.New()
	playlists := &fakePlaylists{tracks: []uuid.UUID{missing, refused, good}}
	resolver := &fakeResolver{
		variants: map[uuid.UUID]uuid.UUID{refused: refusedVariant, good: uuid.New()},
		missing:  map[uuid.UUID]bool{missing: true},
	}
	media := &fakeMedia{fail: map[uuid.UUID]bool{refusedVariant: true}}

	newTestPrefetcher(t, playlists, resolver, media).pass(context.Background())

	if len(resolver.resolved) != 3 {
		t.Fatalf("resolved %d tracks, want all 3 tried", len(resolver.resolved))
	}
	if len(media.ensured) != 1 {
		t.Fatalf("fetched %d songs, want the one that could be fetched", len(media.ensured))
	}
}

// The playlists can change between passes, so a song added later is picked up
// by the next one.
func TestPassPicksUpWhatWasAddedSince(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	playlists := &fakePlaylists{tracks: []uuid.UUID{first}}
	resolver := &fakeResolver{variants: map[uuid.UUID]uuid.UUID{first: uuid.New(), second: uuid.New()}}
	media := &fakeMedia{}
	p := newTestPrefetcher(t, playlists, resolver, media)

	p.pass(context.Background())
	playlists.tracks = append(playlists.tracks, second)
	p.pass(context.Background())

	if len(media.ensured) != 2 {
		t.Fatalf("fetched %d songs over two passes, want 2", len(media.ensured))
	}
}
