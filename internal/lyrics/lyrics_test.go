package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// fakeStore is the LyricsRepo plus the metadata a query is built from, held in
// memory so the service can be exercised without a database.
type fakeStore struct {
	lyrics  map[string]*store.Lyrics
	saves   []*store.Lyrics
	track   *store.Track
	artists []store.Artist
	albums  []store.Album
	variant *store.Variant
}

func key(trackID, variantID uuid.UUID) string {
	return trackID.String() + "/" + variantID.String()
}

func (f *fakeStore) Lyrics(_ context.Context, trackID, variantID uuid.UUID) (*store.Lyrics, error) {
	if l, ok := f.lyrics[key(trackID, variantID)]; ok {
		return l, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) SaveLyrics(_ context.Context, l *store.Lyrics) error {
	if f.lyrics == nil {
		f.lyrics = map[string]*store.Lyrics{}
	}
	f.saves = append(f.saves, l)
	f.lyrics[key(l.TrackID, l.VariantID)] = l
	return nil
}

func (f *fakeStore) Track(_ context.Context, id uuid.UUID) (*store.Track, error) {
	if f.track == nil || f.track.ID != id {
		return nil, store.ErrNotFound
	}
	return f.track, nil
}

func (f *fakeStore) TrackArtists(context.Context, uuid.UUID) ([]store.Artist, error) {
	return f.artists, nil
}

func (f *fakeStore) TrackAlbums(context.Context, uuid.UUID) ([]store.Album, error) {
	return f.albums, nil
}

func (f *fakeStore) Variant(_ context.Context, id uuid.UUID) (*store.Variant, error) {
	if f.variant == nil || f.variant.ID != id {
		return nil, store.ErrNotFound
	}
	return f.variant, nil
}

// fakeFetcher answers with one canned Result, or one canned error, and counts
// how often it was asked.
type fakeFetcher struct {
	result Result
	err    error
	calls  []Query
}

func (f *fakeFetcher) Fetch(_ context.Context, q Query) (Result, error) {
	f.calls = append(f.calls, q)
	return f.result, f.err
}

func testTrack(id uuid.UUID) *store.Track {
	return &store.Track{ID: id, Title: "A Song", DurationMs: 180_000}
}

func TestParseLRCTimingsAndMultipleTags(t *testing.T) {
	text := "[ar:Somebody]\n" +
		"[ti:A Song]\n" +
		"[00:12.50] first line\n" +
		"[00:01.00][01:30.25] chorus\n" +
		"[00:05] no fraction\n"

	got := ParseLRC(text)
	want := []store.LyricLine{
		{AtMs: 1_000, Text: "chorus"},
		{AtMs: 5_000, Text: "no fraction"},
		{AtMs: 12_500, Text: "first line"},
		{AtMs: 90_250, Text: "chorus"},
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseLRCIgnoresWhatIsNotWords(t *testing.T) {
	text := "[offset:500]\n" +
		"[al:An Album]\n" +
		"\n" +
		"   \n" +
		"[00:03.000] [by:Somebody] still words\n" +
		"not a timed line\n"

	got := ParseLRC(text)
	if len(got) != 1 {
		t.Fatalf("lines = %+v, want only the timed one", got)
	}
	// A metadata tag after a time tag is not consumed as words: it is part of
	// the text, because a real time tag came first.
	if got[0].AtMs != 3_000 || got[0].Text != "[by:Somebody] still words" {
		t.Errorf("line = %+v", got[0])
	}
}

func TestParseLRCEmptyText(t *testing.T) {
	if got := ParseLRC(""); got != nil {
		t.Errorf("empty text = %+v, want nil", got)
	}
	if got := ParseLRC("[ar:only metadata]"); got != nil {
		t.Errorf("metadata only = %+v, want nil", got)
	}
}

func TestResolveReturnsTheCachedAnswerWithoutFetching(t *testing.T) {
	trackID := uuid.New()
	variantID := uuid.New()
	cached := &store.Lyrics{TrackID: trackID, VariantID: variantID, Plain: "cached", Source: "lrclib"}
	fetcher := &fakeFetcher{}
	svc := New(&fakeStore{lyrics: map[string]*store.Lyrics{key(trackID, variantID): cached}}, fetcher, nil)

	got, err := svc.Resolve(context.Background(), trackID, variantID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != cached {
		t.Errorf("got %+v, want the cached answer", got)
	}
	if len(fetcher.calls) != 0 {
		t.Errorf("a cache hit fetched: %+v", fetcher.calls)
	}
}

func TestResolveFetchesAndCachesOnAMiss(t *testing.T) {
	trackID := uuid.New()
	variantID := uuid.New()
	fetcher := &fakeFetcher{result: Result{
		Synced: []store.LyricLine{{AtMs: 1_000, Text: "words"}},
		Plain:  "words\n",
	}}
	fs := &fakeStore{track: testTrack(trackID)}
	svc := New(fs, fetcher, nil)

	got, err := svc.Resolve(context.Background(), trackID, variantID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil || got.Source != "lrclib" || got.Plain != "words" || len(got.Synced) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got.VariantID != variantID {
		t.Errorf("cached under %s, want %s", got.VariantID, variantID)
	}
	if len(fs.saves) != 1 || fs.saves[0] != got {
		t.Errorf("saves = %+v, want the answer once", fs.saves)
	}

	// The second ask is served from the cache.
	if _, err := svc.Resolve(context.Background(), trackID, variantID); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if len(fetcher.calls) != 1 {
		t.Errorf("fetched %d times, want 1", len(fetcher.calls))
	}
}

func TestResolveFetchFailureIsNotAnAnswer(t *testing.T) {
	trackID := uuid.New()
	fetcher := &fakeFetcher{err: errors.New("network down")}
	fs := &fakeStore{track: testTrack(trackID)}

	got, err := New(fs, fetcher, nil).Resolve(context.Background(), trackID, uuid.Nil)
	if err != nil {
		t.Fatalf("a failing source must not be an error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
	if len(fs.saves) != 0 {
		t.Errorf("a failure was cached: %+v", fs.saves)
	}

	// Nothing found is likewise no answer and nothing to cache.
	fetcher = &fakeFetcher{result: Result{}}
	got, err = New(fs, fetcher, nil).Resolve(context.Background(), trackID, uuid.Nil)
	if err != nil || got != nil {
		t.Errorf("no result: got (%+v, %v), want (nil, nil)", got, err)
	}
	if len(fs.saves) != 0 {
		t.Errorf("an empty answer was cached: %+v", fs.saves)
	}
}

func TestResolveCachesAnInstrumentalAsARealAnswer(t *testing.T) {
	trackID := uuid.New()
	variantID := uuid.New()
	fetcher := &fakeFetcher{result: Result{Instrumental: true}}
	fs := &fakeStore{track: testTrack(trackID), variant: &store.Variant{ID: variantID, DurationMs: 240_000}}

	got, err := New(fs, fetcher, nil).Resolve(context.Background(), trackID, variantID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil || got.Source != "lrclib-instrumental" {
		t.Fatalf("got %+v, want a cached instrumental", got)
	}
	if len(got.Synced) != 0 || got.Plain != "" {
		t.Errorf("an instrumental has no words: %+v", got)
	}
	if len(fs.saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(fs.saves))
	}

	// It is remembered, so the second ask does not fetch again.
	if _, err := New(fs, fetcher, nil).Resolve(context.Background(), trackID, variantID); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if len(fetcher.calls) != 1 {
		t.Errorf("fetched %d times, want 1", len(fetcher.calls))
	}
}

func TestResolveQueryCarriesTheRenditionDurationAndCredits(t *testing.T) {
	trackID := uuid.New()
	variantID := uuid.New()
	track := testTrack(trackID)
	variant := &store.Variant{ID: variantID, TrackID: trackID, DurationMs: 245_678}
	fs := &fakeStore{
		track:   track,
		artists: []store.Artist{{Name: "First"}, {Name: "Second"}},
		albums:  []store.Album{{Title: "Debut"}, {Title: "Later"}},
		variant: variant,
	}
	fetcher := &fakeFetcher{result: Result{Plain: "words"}}

	if _, err := New(fs, fetcher, nil).Resolve(context.Background(), trackID, variantID); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("fetcher calls = %d, want 1", len(fetcher.calls))
	}
	q := fetcher.calls[0]
	if q.DurationMs != variant.DurationMs {
		t.Errorf("duration = %d, want the variant's %d", q.DurationMs, variant.DurationMs)
	}
	if q.Track != "A Song" || q.Artist != "First" || q.Album != "Debut" {
		t.Errorf("query = %+v, want the track's title and first credits", q)
	}

	// Naming no rendition falls back to the track's own duration.
	fs.variant = nil
	fetcher.calls = nil
	if _, err := New(fs, fetcher, nil).Resolve(context.Background(), trackID, uuid.Nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if q := fetcher.calls[0]; q.DurationMs != track.DurationMs {
		t.Errorf("duration = %d, want the track's %d", q.DurationMs, track.DurationMs)
	}
}

func TestResolveUnknownTrackIsAnError(t *testing.T) {
	fs := &fakeStore{}
	_, err := New(fs, &fakeFetcher{}, nil).Resolve(context.Background(), uuid.New(), uuid.Nil)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

func TestLRCLIBFetchSendsTheQueryAndReadsTheAnswer(t *testing.T) {
	var gotQuery map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/get" {
			t.Errorf("path = %q, want /api/get", r.URL.Path)
		}
		gotQuery = map[string]string{}
		for key := range r.URL.Query() {
			gotQuery[key] = r.URL.Query().Get(key)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"syncedLyrics": "[00:01.00] words",
			"plainLyrics":  "words",
			"instrumental": false,
			"trackName":    "A Song",
			"artistName":   "Somebody",
		})
	}))
	defer server.Close()

	fetcher := &LRCLIB{base: server.URL, client: server.Client()}
	result, err := fetcher.Fetch(context.Background(), Query{
		Artist: "Somebody", Track: "A Song", Album: "Debut", DurationMs: 245_678,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotQuery["artist_name"] != "Somebody" || gotQuery["track_name"] != "A Song" || gotQuery["album_name"] != "Debut" {
		t.Errorf("query = %+v", gotQuery)
	}
	if gotQuery["duration"] != "246" {
		t.Errorf("duration = %q, want 246 rounded seconds", gotQuery["duration"])
	}
	if len(result.Synced) != 1 || result.Synced[0].Text != "words" || result.Plain != "words" {
		t.Errorf("result = %+v", result)
	}
}

func TestLRCLIBNothingFoundAndInstrumental(t *testing.T) {
	status := http.StatusNotFound
	body := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	fetcher := &LRCLIB{base: server.URL, client: server.Client()}

	result, err := fetcher.Fetch(context.Background(), Query{Artist: "A", Track: "B"})
	if err != nil || result.Instrumental || len(result.Synced) != 0 || result.Plain != "" {
		t.Errorf("not found: got (%+v, %v), want an empty result", result, err)
	}

	status = http.StatusOK
	body = `{"instrumental": true, "syncedLyrics": "", "plainLyrics": ""}`
	result, err = fetcher.Fetch(context.Background(), Query{Artist: "A", Track: "B"})
	if err != nil || !result.Instrumental {
		t.Errorf("instrumental: got (%+v, %v)", result, err)
	}
}

func TestLRCLIBErrorStatusIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := (&LRCLIB{base: server.URL, client: server.Client()}).Fetch(
		context.Background(), Query{Artist: "A", Track: "B"}); err == nil {
		t.Error("a 500 must be an error, not an empty answer")
	}
}
