package match

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

const threshold = 0.8

// fakeProvider serves canned search hits.
type fakeProvider struct {
	name string
	caps provider.Caps
	hits []provider.Track
	// byQuery answers per query, for tests about what a search asks for. A
	// query it does not name gets nothing back.
	byQuery map[string][]provider.Track
	err     error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Capabilities() provider.Caps {
	return f.caps
}

func (f *fakeProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.byQuery != nil {
		return f.byQuery[q], nil
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
		{
			// YouTube Music spells a song twice when it is not written in
			// Latin letters. That is the same recording, and Spotify's public
			// pages give no album to help.
			name:  "youtube appends a romanisation to a non-latin title",
			a:     fixture("トーキョーレギー", "Masayoshi Takanaka", "", 260_333),
			b:     fixture("トーキョーレギー - Tokyo Reggie", "Masayoshi Takanaka", "", 261_000),
			match: true,
		},
		{
			// Spotify's public playlist pages join a track's credits into one
			// line, so a name with a comma in it arrives as two people.
			name: "the same credits arrive split differently",
			a: Candidate{
				Title: "EARFQUAKE", Artists: []string{"Tyler, The Creator"},
				Album: "IGOR", DurationMs: 190_072,
			},
			b: Candidate{
				Title: "EARFQUAKE", Artists: []string{"Tyler", "The Creator"},
				Album: "IGOR", DurationMs: 191_000,
			},
			match: true,
		},
		{
			// The same recording with the year written on the other side of
			// the word, which is how Spotify's pages spell it.
			name:  "a remaster named year first",
			a:     fixture("Modern Love - 2018 Remaster", "David Bowie", "", 288_339),
			b:     fixture("Modern Love", "David Bowie", "", 289_000),
			match: true,
		},
		{
			// One side credits the featured artist, the other leaves it in the
			// title only.
			name: "a feature credited on one side",
			a: Candidate{
				Title: "Jamba (feat. Hodgy)", Artists: []string{"Tyler", "The Creator", "Hodgy"},
				DurationMs: 212_500,
			},
			b: Candidate{
				Title: "Jamba (feat. Hodgy)", Artists: []string{"Tyler, The Creator"},
				DurationMs: 213_000,
			},
			match: true,
		},
		{
			// The romanisation rule must not touch a Latin title: this suffix
			// names a different recording, and the durations agree here, so the
			// title is the only thing keeping them apart.
			name:  "a latin title keeps its version suffix",
			a:     fixture("Song", "Artist", "Album", duration),
			b:     fixture("Song - Live at Wembley", "Artist", "Album", duration),
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

// A library can hold one song twice: a provider's metadata arrives spelled
// differently enough not to match, so a second row is made and the rendition
// ends up on only one of them. Resolving the row a playlist points at has to
// join the two, or that entry can never be played.
func TestResolveJoinsADuplicateTrack(t *testing.T) {
	hit := ytHit("Never Gonna Give You Up", "Rick Astley", "Whenever You Need Somebody", 213_000)
	yt := &fakeProvider{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Download: true},
		hits: []provider.Track{hit},
	}
	sp := &fakeProvider{name: "spotify", caps: provider.Caps{Search: true}}
	m, db := newTestMatcher(t, yt, sp)
	ctx := context.Background()

	// The recording, matched once already: the rendition lives on this row.
	withSource, _, err := m.Attach(ctx, "ytmusic", hit)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// A second row for the same song, with nothing to play.
	dupe := &store.Track{ID: uuid.New(), Title: "Never Gonna Give You Up", DurationMs: 213_400}
	if err := db.CreateTrack(ctx, dupe); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}
	if err := db.SetTrackArtists(ctx, dupe.ID, []string{"Rick Astley"}); err != nil {
		t.Fatalf("SetTrackArtists: %v", err)
	}
	if err := db.SetTrackAlbums(ctx, dupe.ID, []string{"Whenever You Need Somebody"}); err != nil {
		t.Fatalf("SetTrackAlbums: %v", err)
	}

	variants, err := m.Resolve(ctx, dupe.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !hasDownloadable(variants) {
		t.Fatalf("no downloadable variant after resolve: %+v", variants)
	}

	// The two rows are one, and it is the one that had no source, so whatever
	// pointed at it still does.
	if _, err := db.Track(ctx, withSource.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the other row survived the merge: %v", err)
	}
	if _, err := db.Track(ctx, dupe.ID); err != nil {
		t.Errorf("the resolved row went missing: %v", err)
	}
	kept, err := db.VariantsForTrack(ctx, dupe.ID)
	if err != nil {
		t.Fatalf("VariantsForTrack: %v", err)
	}
	if len(kept) != 1 || !kept[0].Downloadable {
		t.Errorf("variants = %+v, want the rendition moved onto the resolved row", kept)
	}
}

// A pick made in the source dialog has to stick to the song it was made for,
// even when the rendition already belongs to another row for the same
// recording. That is the duplicate case again, and the two are joined.
func TestAttachToTrackJoinsADuplicateTrack(t *testing.T) {
	hit := ytHit("Never Gonna Give You Up", "Rick Astley", "Whenever You Need Somebody", 213_000)
	yt := &fakeProvider{name: "ytmusic", caps: provider.Caps{Search: true, Download: true}}
	m, db := newTestMatcher(t, yt)
	ctx := context.Background()

	// The rendition, already attached to one row.
	withSource, _, err := m.Attach(ctx, "ytmusic", hit)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The song somebody is looking at, which is a second row for it.
	other := &store.Track{ID: uuid.New(), Title: "Never Gonna Give You Up", DurationMs: 213_400}
	if err := db.CreateTrack(ctx, other); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}

	kept, variant, err := m.AttachToTrack(ctx, other.ID, "ytmusic", hit)
	if err != nil {
		t.Fatalf("AttachToTrack: %v", err)
	}
	if kept.ID != other.ID {
		t.Errorf("kept = %s, want the track the pick was made for (%s)", kept.ID, other.ID)
	}
	if variant.TrackID != other.ID {
		t.Errorf("variant sits on %s, want %s", variant.TrackID, other.ID)
	}
	if _, err := db.Track(ctx, withSource.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the other row survived the merge: %v", err)
	}
	variants, err := db.VariantsForTrack(ctx, other.ID)
	if err != nil {
		t.Fatalf("VariantsForTrack: %v", err)
	}
	if len(variants) != 1 || !variants[0].Downloadable {
		t.Errorf("variants = %+v, want the picked rendition on this row", variants)
	}
}

// Two candidates can score near enough alike that the difference is noise. The
// provider's own order is what decides between them, and it is information the
// scoring otherwise throws away.
func TestResolvePrefersTheProvidersOwnOrder(t *testing.T) {
	want := int64(213_500)
	first := ytHit("Never Gonna Give You Up", "Rick Astley", "Whenever You Need Somebody", 213_000)
	second := ytHit("Never Gonna Give You Up", "Rick Astley", "Whenever You Need Somebody", want)
	yt := &fakeProvider{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Download: true},
		hits: []provider.Track{first, second},
	}
	m, db := newTestMatcher(t, yt)
	ctx := context.Background()

	track, _, err := m.Attach(ctx, "spotify", provider.Track{
		ProviderTrackID: "sp-order",
		Title:           "Never Gonna Give You Up",
		Artists:         []string{"Rick Astley"},
		Album:           "Whenever You Need Somebody",
		DurationMs:      want,
	})
	if err != nil {
		t.Fatal(err)
	}
	variants, err := m.Resolve(ctx, track.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var matched string
	for _, variant := range variants {
		if variant.Provider == "ytmusic" {
			matched = variant.ProviderTrackID
		}
	}
	// `second` fits the duration better by a hair; `first` is what the provider
	// answered first, and that is the better guess.
	if matched != first.ProviderTrackID {
		t.Errorf("matched %s, want the provider's own first hit (%s)", matched, first.ProviderTrackID)
	}
	_ = db
}

// A query carrying the credits can push the right answer out of the hits
// entirely - credits are what providers spell differently. The title alone is
// the other half of the search, and it is asked for when the first finds
// nothing.
func TestResolveAsksAgainWithTheTitleAlone(t *testing.T) {
	hit := ytHit("Never Gonna Give You Up", "Rick Astley", "", 213_000)
	yt := &fakeProvider{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Download: true},
		byQuery: map[string][]provider.Track{
			"Never Gonna Give You Up Rick Astley": nil, // the credits find nothing
			"Never Gonna Give You Up":             {hit},
		},
	}
	m, _ := newTestMatcher(t, yt)
	ctx := context.Background()

	track, _, err := m.Attach(ctx, "spotify", provider.Track{
		ProviderTrackID: "sp-plain",
		Title:           "Never Gonna Give You Up",
		Artists:         []string{"Rick Astley"},
		DurationMs:      213_400,
	})
	if err != nil {
		t.Fatal(err)
	}
	variants, err := m.Resolve(ctx, track.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !hasDownloadable(variants) {
		t.Fatalf("nothing matched, but the title alone finds it: %+v", variants)
	}
}
