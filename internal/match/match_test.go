package match

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

const threshold = 0.8

// fakeProvider serves canned search hits.
type fakeProvider struct {
	name string
	caps provider.Caps
	hits []provider.Track
	err  error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Capabilities() provider.Caps {
	return f.caps
}

func (f *fakeProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func (f *fakeProvider) Download(ctx context.Context, id, dest string) error { return nil }

func TestRuntimeNormalization(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Never Gonna Give You Up", "never gonna give you up"},
		{"Never Gonna Give You Up (Official Video)", "never gonna give you up"},
		{"Never Gonna Give You Up (Official Music Video)", "never gonna give you up"},
		{"Song - Remastered 2011", "song"},
		{"Song (Remastered)", "song"},
		{"Song [Lyric Video]", "song"},
		{"Song (feat. Someone)", "song"},
		{"Song feat. Someone", "song"},
		{"Song ft. Someone", "song"},
		{"Song (Live at Wembley)", "song live at wembley"},
		{"Song (Tiësto Remix)", "song tiësto remix"},
		{"Song (Acoustic)", "song acoustic"},
		{"Sgt. Pepper's Lonely Hearts Club Band", "sgt pepper s lonely hearts club band"},
		{"  Weird   Spacing  ", "weird spacing"},
	}
	for _, tt := range tests {
		if got := NormalizeTitle(tt.in); got != tt.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	artists := map[string]string{
		"Rick Astley":         "rick astley",
		"Rick Astley - Topic": "rick astley",
		"Simon & Garfunkel":   "simon garfunkel",
		"Simon and Garfunkel": "simon and garfunkel",
	}
	for in, want := range artists {
		if got := NormalizeArtist(in); got != want {
			t.Errorf("NormalizeArtist(%q) = %q, want %q", in, got, want)
		}
	}
}

// fixture builds the metadata a provider would report for a recording.
func fixture(title, artist, album string, durationMs int64) Candidate {
	return Candidate{Title: title, Artists: []string{artist}, Album: album, DurationMs: durationMs}
}

func TestScoreFixtures(t *testing.T) {
	studio := "Never Gonna Give You Up"
	album := "Whenever You Need Somebody"
	const duration = 213_000

	tests := []struct {
		name  string
		a     Candidate
		b     Candidate
		match bool
	}{
		{
			name:  "identical metadata",
			a:     fixture(studio, "Rick Astley", album, duration),
			b:     fixture(studio, "Rick Astley", album, duration),
			match: true,
		},
		{
			name:  "official video suffix",
			a:     fixture(studio, "Rick Astley", album, duration),
			b:     fixture(studio+" (Official Video)", "Rick Astley", album, duration),
			match: true,
		},
		{
			name:  "remastered suffix",
			a:     fixture(studio, "Rick Astley", album, duration),
			b:     fixture(studio+" - Remastered 2011", "Rick Astley", album, duration),
			match: true,
		},
		{
			name:  "featuring credits in the title",
			a:     fixture("Song", "Artist", "Album", duration),
			b:     fixture("Song (feat. Guest)", "Artist", "Album", duration),
			match: true,
		},
		{
			name:  "durations round differently",
			a:     fixture(studio, "Rick Astley", album, duration),
			b:     fixture(studio, "Rick Astley", album, duration+4000),
			match: true,
		},
		{
			name:  "same isrc despite different titles",
			a:     Candidate{Title: studio, Artists: []string{"Rick Astley"}, DurationMs: duration, ISRC: "GBAYE8700001"},
			b:     Candidate{Title: "Never Gonna Give You Up (Radio Edit)", Artists: []string{"R. Astley"}, DurationMs: duration - 30_000, ISRC: "gbaye8700001"},
			match: true,
		},
		{
			name:  "missing duration still matches on title and artist",
			a:     Candidate{Title: studio, Artists: []string{"Rick Astley"}, Album: album},
			b:     fixture(studio, "Rick Astley", album, duration),
			match: true,
		},
		{
			name:  "live version is a different recording",
			a:     fixture(studio, "Rick Astley", album, duration),
			b:     fixture(studio+" (Live at Wembley)", "Rick Astley", "Live at Wembley", duration+20_000),
			match: false,
		},
		{
			name:  "remix is a different recording",
			a:     fixture("Song", "Artist", "Album", duration),
			b:     fixture("Song (Tiësto Remix)", "Artist", "Remixes", duration+15_000),
			match: false,
		},
		{
			name:  "acoustic version is a different recording",
			a:     fixture("Song", "Artist", "Album", duration),
			b:     fixture("Song (Acoustic)", "Artist", "Acoustic Sessions", duration-10_000),
			match: false,
		},
		{
			name:  "same title, different artist",
			a:     fixture("Yesterday", "The Beatles", "Help!", duration),
			b:     fixture("Yesterday", "Someone Else", "Covers", duration),
			match: false,
		},
		{
			name:  "duration beyond tolerance is never the same recording",
			a:     fixture(studio, "Rick Astley", album, duration),
			b:     fixture(studio, "Rick Astley", album, duration+6000),
			match: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := Score(tt.a, tt.b)
			if score < 0 || score > 1 {
				t.Fatalf("score %v out of range", score)
			}
			if got := score >= threshold; got != tt.match {
				t.Errorf("score = %.3f; match = %v, want %v", score, got, tt.match)
			}
		})
	}
}

func TestScoreExactValues(t *testing.T) {
	a := fixture("Song", "Artist", "Album", 180_000)

	if got := Score(a, a); got != 1 {
		t.Errorf("identical candidates score %v, want 1", got)
	}
	if got := Score(a, fixture("Song", "Artist", "Album", 190_000)); got != 0 {
		t.Errorf("far durations score %v, want 0", got)
	}
	withISRC := Candidate{Title: "Totally Different", DurationMs: 10, ISRC: "USABC1234567"}
	if got := Score(withISRC, Candidate{Title: "Whatever", ISRC: "USABC1234567"}); got != 1 {
		t.Errorf("identical ISRCs score %v, want 1", got)
	}
}

func newTestMatcher(t *testing.T, providers ...provider.Provider) (*Matcher, *store.DB) {
	t.Helper()
	db, err := store.Open("file:match-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	registry := provider.NewRegistry(slog.New(slog.DiscardHandler), 5*time.Second)
	for _, p := range providers {
		registry.Register(p)
	}
	return New(db, registry, threshold, slog.New(slog.DiscardHandler)), db
}

func ytHit(title, artist, album string, durationMs int64) provider.Track {
	return provider.Track{ProviderTrackID: uuid.NewString(), Title: title, Artists: []string{artist}, Album: album, DurationMs: durationMs}
}

func TestGroupMergesProviderHitsIntoOneTrack(t *testing.T) {
	yt := &fakeProvider{name: "ytmusic", caps: provider.Caps{Search: true, Download: true}}
	sp := &fakeProvider{name: "spotify", caps: provider.Caps{Search: true}}
	m, db := newTestMatcher(t, yt, sp)
	ctx := context.Background()

	results := []provider.Result{
		{Provider: "ytmusic", Tracks: []provider.Track{
			ytHit("Never Gonna Give You Up (Official Video)", "Rick Astley", "Whenever You Need Somebody", 213_000),
			ytHit("Never Gonna Give You Up (Live at Wembley)", "Rick Astley", "Live at Wembley", 233_000),
		}},
		{Provider: "spotify", Tracks: []provider.Track{
			ytHit("Never Gonna Give You Up", "Rick Astley", "Whenever You Need Somebody", 213_400),
		}},
	}

	groups, err := m.Group(ctx, results)
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (studio merged, live separate)", len(groups))
	}
	studio := groups[0]
	if len(studio.Variants) != 2 {
		t.Fatalf("studio variants = %d, want 2", len(studio.Variants))
	}
	downloadable := 0
	for _, v := range studio.Variants {
		if v.Downloadable {
			downloadable++
		}
	}
	if downloadable != 1 {
		t.Errorf("downloadable variants = %d, want exactly the ytmusic one", downloadable)
	}
	if len(groups[1].Variants) != 1 {
		t.Errorf("live variants = %d, want 1", len(groups[1].Variants))
	}

	// A second identical search must not create new canonical tracks.
	again, err := m.Group(ctx, results)
	if err != nil {
		t.Fatalf("Group again: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("groups after repeat = %d, want 2", len(again))
	}
	tracks, err := db.CandidateTracks(ctx, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Errorf("canonical tracks = %d, want 2", len(tracks))
	}
}

func TestAttachIsIdempotentPerProviderTrack(t *testing.T) {
	yt := &fakeProvider{name: "ytmusic", caps: provider.Caps{Search: true, Download: true}}
	m, db := newTestMatcher(t, yt)
	ctx := context.Background()

	hit := ytHit("Song", "Artist", "Album", 180_000)
	first, v1, err := m.Attach(ctx, "ytmusic", hit)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	second, v2, err := m.Attach(ctx, "ytmusic", hit)
	if err != nil {
		t.Fatalf("Attach again: %v", err)
	}
	if first.ID != second.ID || v1.ID != v2.ID {
		t.Errorf("attach is not idempotent: %s/%s vs %s/%s", first.ID, v1.ID, second.ID, v2.ID)
	}
	variants, err := db.VariantsForTrack(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Errorf("variants = %d, want 1", len(variants))
	}
}

func TestAttachDoesNotMergeDifferentSongsWithSameTitle(t *testing.T) {
	yt := &fakeProvider{name: "ytmusic", caps: provider.Caps{Search: true, Download: true}}
	m, db := newTestMatcher(t, yt)
	ctx := context.Background()

	a, _, err := m.Attach(ctx, "ytmusic", ytHit("Yesterday", "The Beatles", "Help!", 125_000))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := m.Attach(ctx, "ytmusic", ytHit("Yesterday", "Some Cover Band", "Covers Vol 3", 125_000))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("covers must not merge into one canonical track")
	}
	if tracks, _ := db.CandidateTracks(ctx, 0, 0, 10); len(tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(tracks))
	}
}

func TestResolveFindsPlayableVariantForMetadataOnlyTrack(t *testing.T) {
	sp := &fakeProvider{name: "spotify", caps: provider.Caps{Search: true}}
	yt := &fakeProvider{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Download: true},
		hits: []provider.Track{ytHit("Never Gonna Give You Up (Official Video)", "Rick Astley", "Whenever You Need Somebody", 213_000)},
	}
	m, db := newTestMatcher(t, yt, sp)
	ctx := context.Background()

	// The track starts out as Spotify-only.
	track, _, err := m.Attach(ctx, "spotify", provider.Track{
		ProviderTrackID: "sp-1",
		Title:           "Never Gonna Give You Up",
		Artists:         []string{"Rick Astley"},
		Album:           "Whenever You Need Somebody",
		DurationMs:      213_400,
		ISRC:            "GBAYE8700001",
	})
	if err != nil {
		t.Fatal(err)
	}

	variants, err := m.Resolve(ctx, track.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !hasDownloadable(variants) {
		t.Fatalf("no downloadable variant after resolve: %+v", variants)
	}
	if len(variants) != 2 {
		t.Errorf("variants = %d, want the spotify one plus the matched ytmusic one", len(variants))
	}

	// Resolution is idempotent.
	again, err := m.Resolve(ctx, track.ID)
	if err != nil {
		t.Fatalf("Resolve again: %v", err)
	}
	if len(again) != 2 {
		t.Errorf("variants after second resolve = %d, want 2", len(again))
	}
	if files, _ := db.VariantsForTrack(ctx, track.ID); len(files) != 2 {
		t.Errorf("variants in store = %d, want 2", len(files))
	}
}

func TestResolveReportsWhenNothingMatches(t *testing.T) {
	sp := &fakeProvider{name: "spotify", caps: provider.Caps{Search: true}}
	yt := &fakeProvider{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Download: true},
		hits: []provider.Track{ytHit("Completely Different Song", "Another Artist", "Other Album", 100_000)},
	}
	m, _ := newTestMatcher(t, yt, sp)
	ctx := context.Background()

	track, _, err := m.Attach(ctx, "spotify", provider.Track{
		ProviderTrackID: "sp-2",
		Title:           "Never Gonna Give You Up",
		Artists:         []string{"Rick Astley"},
		Album:           "Whenever You Need Somebody",
		DurationMs:      213_400,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Resolve(ctx, track.ID); !errors.Is(err, ErrNoPlayableVariant) {
		t.Fatalf("err = %v, want ErrNoPlayableVariant", err)
	}
}

func TestResolveKeepsWorkingWhenAProviderFails(t *testing.T) {
	sp := &fakeProvider{name: "spotify", caps: provider.Caps{Search: true}}
	broken := &fakeProvider{name: "ytmusic", caps: provider.Caps{Search: true, Download: true}, err: errors.New("upstream down")}
	m, _ := newTestMatcher(t, broken, sp)
	ctx := context.Background()

	track, _, err := m.Attach(ctx, "spotify", provider.Track{
		ProviderTrackID: "sp-3",
		Title:           "Song",
		Artists:         []string{"Artist"},
		DurationMs:      180_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(ctx, track.ID); !errors.Is(err, ErrNoPlayableVariant) {
		t.Fatalf("err = %v, want ErrNoPlayableVariant rather than a provider error", err)
	}
}
