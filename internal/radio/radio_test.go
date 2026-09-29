package radio

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// fakeRadio is a provider that returns a canned station and records the seed it
// was asked about.
type fakeRadio struct {
	name string
	caps provider.Caps

	tracks []provider.Track
	err    error

	calls   int
	seeds   []string
	album   *provider.AlbumDetail
	albums  []provider.Album
	artists []provider.Artist
}

func (f *fakeRadio) Name() string                { return f.name }
func (f *fakeRadio) Capabilities() provider.Caps { return f.caps }

func (f *fakeRadio) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (f *fakeRadio) Download(ctx context.Context, id, dest string) error { return nil }

func (f *fakeRadio) Radio(ctx context.Context, seed provider.Track, limit int) ([]provider.Track, error) {
	f.calls++
	f.seeds = append(f.seeds, seed.ProviderTrackID)
	if f.err != nil {
		return nil, f.err
	}
	tracks := f.tracks
	if limit > 0 && len(tracks) > limit {
		tracks = tracks[:limit]
	}
	return tracks, nil
}

func (f *fakeRadio) SearchAlbums(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Album, error) {
	return f.albums, nil
}
func (f *fakeRadio) Album(ctx context.Context, id string) (*provider.AlbumDetail, error) {
	return f.album, nil
}
func (f *fakeRadio) SearchArtists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Artist, error) {
	return f.artists, nil
}
func (f *fakeRadio) ArtistAlbums(ctx context.Context, id string) ([]provider.Album, error) {
	return f.albums, nil
}

func hit(id, title string) provider.Track {
	return provider.Track{ProviderTrackID: id, Title: title, Artists: []string{"Some Artist"}, DurationMs: 200_000}
}

type fixture struct {
	service *Service
	db      *store.DB
}

func newFixture(t *testing.T, providers ...*fakeRadio) *fixture {
	t.Helper()
	db, err := store.Open("file:radio-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.DiscardHandler)
	registry := provider.NewRegistry(logger, 5*time.Second)
	for _, p := range providers {
		registry.Register(p)
	}
	matcher := match.New(db, registry, 0.8, logger)
	return &fixture{service: New(db, registry, matcher, logger), db: db}
}

// seedTrack creates a canonical track with one rendition per provider.
func (f *fixture) seedTrack(t *testing.T, title string, providerNames ...string) *store.Track {
	t.Helper()
	ctx := context.Background()
	track := &store.Track{Title: title, DurationMs: 200_000}
	if err := f.db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTrackArtists(ctx, track.ID, []string{"Some Artist"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range providerNames {
		variant := &store.Variant{
			TrackID:         track.ID,
			Provider:        name,
			ProviderTrackID: name + "-seed",
			Title:           title,
			Artists:         []string{"Some Artist"},
			DurationMs:      200_000,
			Downloadable:    true,
		}
		if err := f.db.CreateVariant(ctx, variant); err != nil {
			t.Fatal(err)
		}
	}
	return track
}

func TestGenerateInterleavesTheSelectedProviders(t *testing.T) {
	ytmusic := &fakeRadio{
		name:   "ytmusic",
		caps:   provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{hit("yt-1", "YT One"), hit("yt-2", "YT Two"), hit("yt-3", "YT Three")},
	}
	spotify := &fakeRadio{
		name:   "spotify",
		caps:   provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{hit("sp-1", "Spotify One"), hit("sp-2", "Spotify Two")},
	}
	f := newFixture(t, ytmusic, spotify)
	seed := f.seedTrack(t, "Seed", "ytmusic", "spotify")

	result, err := f.service.Generate(context.Background(), seed.ID, []string{"ytmusic", "spotify"}, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Tracks) != 5 {
		t.Fatalf("tracks = %d, want 5 (%v)", len(result.Tracks), titles(result.Tracks))
	}

	// The mix alternates, so the first four are one from each in turn.
	want := []string{"YT One", "Spotify One", "YT Two", "Spotify Two"}
	for i, title := range want {
		if result.Tracks[i].Title != title {
			t.Fatalf("track %d = %q, want %q (all: %v)", i, result.Tracks[i].Title, title, titles(result.Tracks))
		}
	}

	// Each provider was asked about its own rendition of the seed.
	if len(ytmusic.seeds) != 1 || ytmusic.seeds[0] != "ytmusic-seed" {
		t.Errorf("ytmusic seeds = %v", ytmusic.seeds)
	}
	if len(spotify.seeds) != 1 || spotify.seeds[0] != "spotify-seed" {
		t.Errorf("spotify seeds = %v", spotify.seeds)
	}
}

func TestGenerateHonoursASingleProvider(t *testing.T) {
	ytmusic := &fakeRadio{
		name:   "ytmusic",
		caps:   provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{hit("yt-1", "YT One")},
	}
	spotify := &fakeRadio{
		name:   "spotify",
		caps:   provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{hit("sp-1", "Spotify One")},
	}
	f := newFixture(t, ytmusic, spotify)
	seed := f.seedTrack(t, "Seed", "ytmusic", "spotify")

	result, err := f.service.Generate(context.Background(), seed.ID, []string{"spotify"}, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if ytmusic.calls != 0 {
		t.Errorf("ytmusic was asked %d times, want 0: the caller chose spotify only", ytmusic.calls)
	}
	if spotify.calls != 1 {
		t.Errorf("spotify calls = %d, want 1", spotify.calls)
	}
	if len(result.Tracks) != 1 || result.Tracks[0].Title != "Spotify One" {
		t.Fatalf("tracks = %v", titles(result.Tracks))
	}
	if len(result.Providers) != 1 || result.Providers[0] != "spotify" {
		t.Errorf("providers = %v", result.Providers)
	}
}

func TestGenerateReportsProviderFailure(t *testing.T) {
	ytmusic := &fakeRadio{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Radio: true},
		err:  errors.New("station unavailable"),
	}
	spotify := &fakeRadio{
		name:   "spotify",
		caps:   provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{hit("sp-1", "Spotify One")},
	}
	f := newFixture(t, ytmusic, spotify)
	seed := f.seedTrack(t, "Seed", "ytmusic", "spotify")

	result, err := f.service.Generate(context.Background(), seed.ID, nil, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Tracks) != 1 {
		t.Fatalf("tracks = %v, want the one provider that worked", titles(result.Tracks))
	}
	if len(result.Errors) != 1 || result.Errors[0].Provider != "ytmusic" {
		t.Fatalf("errors = %+v", result.Errors)
	}
}

func TestGenerateRejectsUnusableSeeds(t *testing.T) {
	ytmusic := &fakeRadio{name: "ytmusic", caps: provider.Caps{Search: true, Radio: true}}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	if _, err := f.service.Generate(ctx, uuid.New(), nil, 0); !errors.Is(err, ErrNoSeed) {
		t.Errorf("unknown seed: err = %v, want ErrNoSeed", err)
	}

	// A seed with no rendition in the selected provider cannot grow a station.
	seed := f.seedTrack(t, "Seed") // no variants
	_, err := f.service.Generate(ctx, seed.ID, []string{"ytmusic"}, 0)
	if !errors.Is(err, ErrNoSeedVariant) {
		t.Fatalf("seed without renditions: err = %v, want ErrNoSeedVariant", err)
	}
	if ytmusic.calls != 0 {
		t.Error("the provider was asked about a seed it does not have")
	}
}

func TestGenerateSkipsProvidersWithoutRadio(t *testing.T) {
	metadataOnly := &fakeRadio{
		name:   "metadata",
		caps:   provider.Caps{Search: true}, // no Radio capability
		tracks: []provider.Track{hit("m-1", "Should Not Appear")},
	}
	f := newFixture(t, metadataOnly)
	seed := f.seedTrack(t, "Seed", "metadata")

	result, err := f.service.Generate(context.Background(), seed.ID, nil, 0)
	if !errors.Is(err, ErrNoSeedVariant) {
		t.Fatalf("err = %v, want ErrNoSeedVariant", err)
	}
	if metadataOnly.calls != 0 {
		t.Error("a provider without the radio capability was asked for a station")
	}
	if len(result.Tracks) != 0 {
		t.Errorf("tracks = %v, want none", titles(result.Tracks))
	}
}

func TestGenerateDropsTheSeedAndDuplicates(t *testing.T) {
	ytmusic := &fakeRadio{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{
			hit("yt-seed", "Seed"),   // the seed itself, echoed by the provider
			hit("yt-1", "Same Song"), // matches the spotify hit below
			hit("yt-2", "Only Once"),
		},
	}
	spotify := &fakeRadio{
		name:   "spotify",
		caps:   provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{hit("sp-1", "Same Song")},
	}
	f := newFixture(t, ytmusic, spotify)
	seed := f.seedTrack(t, "Seed", "ytmusic", "spotify")

	result, err := f.service.Generate(context.Background(), seed.ID, nil, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, track := range result.Tracks {
		if track.ID == seed.ID {
			t.Error("the seed track is in its own radio")
		}
	}
	if len(result.Tracks) != 2 {
		t.Fatalf("tracks = %v, want the duplicate merged away", titles(result.Tracks))
	}
}

func TestGenerateRespectsLength(t *testing.T) {
	many := make([]provider.Track, 0, 10)
	for i := 0; i < 10; i++ {
		many = append(many, hit(uuid.NewString(), "Track "+string(rune('A'+i))))
	}
	ytmusic := &fakeRadio{name: "ytmusic", caps: provider.Caps{Search: true, Radio: true}, tracks: many}
	f := newFixture(t, ytmusic)
	seed := f.seedTrack(t, "Seed", "ytmusic")

	result, err := f.service.Generate(context.Background(), seed.ID, nil, 3)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(result.Tracks))
	}

	if _, err := f.service.Generate(context.Background(), seed.ID, nil, MaxLength+500); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func titles(tracks []store.Track) []string {
	out := make([]string, 0, len(tracks))
	for _, track := range tracks {
		out = append(out, track.Title)
	}
	return out
}
