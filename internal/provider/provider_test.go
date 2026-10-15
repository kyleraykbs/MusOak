package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeProvider struct {
	name       string
	caps       Caps
	tracks     []Track
	streamBody io.ReadCloser
	streamMIME string
	err        error
	delay      time.Duration
	deps       []string
	calls      int
}

func (f *fakeProvider) Name() string          { return f.name }
func (f *fakeProvider) Capabilities() Caps    { return f.caps }
func (f *fakeProvider) MissingDeps() []string { return f.deps }
func (f *fakeProvider) Download(ctx context.Context, id, dest string, _ ProgressFunc) error {
	f.calls++
	return f.err
}

func (f *fakeProvider) Stream(ctx context.Context, id string) (io.ReadCloser, string, error) {
	f.calls++
	return f.streamBody, f.streamMIME, f.err
}

func (f *fakeProvider) Search(ctx context.Context, q string, opts SearchOpts) ([]Track, error) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.tracks, f.err
}

func newTestRegistry(t *testing.T, timeout time.Duration) *Registry {
	t.Helper()
	return NewRegistry(slog.New(slog.DiscardHandler), timeout)
}

func TestSearchFansOutAndIsolatesFailures(t *testing.T) {
	reg := newTestRegistry(t, time.Second)
	ok := &fakeProvider{
		name:   "ok",
		caps:   Caps{Search: true, Download: true},
		tracks: []Track{{ProviderTrackID: "1", Title: "Hit"}},
	}
	bad := &fakeProvider{name: "bad", caps: Caps{Search: true}, err: errors.New("upstream down")}
	noSearch := &fakeProvider{name: "local", caps: Caps{Download: true}}
	reg.Register(ok)
	reg.Register(bad)
	reg.Register(noSearch)

	results := reg.Search(context.Background(), "query", SearchOpts{})
	if len(results) != 3 {
		t.Fatalf("results = %d, want one per provider", len(results))
	}
	if results[0].Provider != "ok" || len(results[0].Tracks) != 1 || results[0].Err != nil {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].Provider != "bad" || results[1].Err == nil {
		t.Errorf("results[1] = %+v, want the provider's error", results[1])
	}
	if results[2].IsSearchable() {
		t.Errorf("results[2] = %+v, want a non-searchable marker", results[2])
	}
	if ok.calls != 1 || bad.calls != 1 {
		t.Errorf("calls: ok=%d bad=%d", ok.calls, bad.calls)
	}
}

func TestSearchAppliesPerProviderTimeout(t *testing.T) {
	reg := newTestRegistry(t, 30*time.Millisecond)
	slow := &fakeProvider{name: "slow", caps: Caps{Search: true}, delay: 2 * time.Second}
	fast := &fakeProvider{name: "fast", caps: Caps{Search: true}, tracks: []Track{{ProviderTrackID: "x"}}}
	reg.Register(slow)
	reg.Register(fast)

	start := time.Now()
	results := reg.Search(context.Background(), "query", SearchOpts{})
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("search took %v; the slow provider was not cut off", elapsed)
	}
	if !errors.Is(results[0].Err, context.DeadlineExceeded) {
		t.Errorf("slow provider err = %v, want deadline exceeded", results[0].Err)
	}
	if results[1].Err != nil || len(results[1].Tracks) != 1 {
		t.Errorf("fast provider = %+v, want its results", results[1])
	}
}

func TestSearchIsParallel(t *testing.T) {
	reg := newTestRegistry(t, 2*time.Second)
	const n = 4
	for _, name := range []string{"a", "b", "c", "d"} {
		reg.Register(&fakeProvider{name: name, caps: Caps{Search: true}, delay: 200 * time.Millisecond})
	}
	start := time.Now()
	reg.Search(context.Background(), "query", SearchOpts{})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("four 200ms providers took %v; fan-out is not concurrent", elapsed)
	}
}

func TestSearchSomeRunsOnlyNamedProviders(t *testing.T) {
	reg := newTestRegistry(t, time.Second)
	a := &fakeProvider{name: "a", caps: Caps{Search: true}, tracks: []Track{{ProviderTrackID: "a1"}}}
	b := &fakeProvider{name: "b", caps: Caps{Search: true}, tracks: []Track{{ProviderTrackID: "b1"}}}
	bad := &fakeProvider{name: "bad", caps: Caps{Search: true}, err: errors.New("upstream down")}
	reg.Register(a)
	reg.Register(b)
	reg.Register(bad)

	results := reg.SearchSome(context.Background(), []string{"b"}, "query", SearchOpts{})
	if len(results) != 1 || results[0].Provider != "b" || len(results[0].Tracks) != 1 {
		t.Fatalf("results = %+v, want only b's hit", results)
	}
	if a.calls != 0 || bad.calls != 0 || b.calls != 1 {
		t.Errorf("calls: a=%d b=%d bad=%d, want only b searched", a.calls, b.calls, bad.calls)
	}

	// An empty selection means every provider, exactly like Search, and one
	// provider failing still leaves the others' results usable.
	all := reg.SearchSome(context.Background(), nil, "query", SearchOpts{})
	if len(all) != 3 {
		t.Fatalf("results = %d, want one per provider", len(all))
	}
	if all[0].Err != nil || len(all[0].Tracks) != 1 {
		t.Errorf("all[0] = %+v, want a's hit", all[0])
	}
	if all[2].Provider != "bad" || all[2].Err == nil {
		t.Errorf("all[2] = %+v, want the failing provider's error", all[2])
	}

	// A name that is not registered is not searched; callers reject it before
	// getting here.
	none := reg.SearchSome(context.Background(), []string{"missing"}, "query", SearchOpts{})
	if len(none) != 0 {
		t.Errorf("results = %+v, want none for an unregistered name", none)
	}
}

func TestSearchSomeAppliesPerProviderTimeout(t *testing.T) {
	reg := newTestRegistry(t, 30*time.Millisecond)
	slow := &fakeProvider{name: "slow", caps: Caps{Search: true}, delay: 2 * time.Second}
	fast := &fakeProvider{name: "fast", caps: Caps{Search: true}, tracks: []Track{{ProviderTrackID: "x"}}}
	reg.Register(slow)
	reg.Register(fast)

	start := time.Now()
	results := reg.SearchSome(context.Background(), []string{"slow", "fast"}, "query", SearchOpts{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("search took %v; the slow provider was not cut off", elapsed)
	}
	if !errors.Is(results[0].Err, context.DeadlineExceeded) {
		t.Errorf("slow provider err = %v, want deadline exceeded", results[0].Err)
	}
	if results[1].Err != nil || len(results[1].Tracks) != 1 {
		t.Errorf("fast provider = %+v, want its results", results[1])
	}
}

func TestDownloadRouting(t *testing.T) {
	reg := newTestRegistry(t, time.Second)
	dl := &fakeProvider{name: "ytmusic", caps: Caps{Search: true, Download: true}}
	meta := &fakeProvider{name: "spotify", caps: Caps{Search: true}}
	reg.Register(dl)
	reg.Register(meta)

	if err := reg.Download(context.Background(), "ytmusic", "id", "/tmp/x.opus", nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if dl.calls != 1 {
		t.Errorf("download calls = %d, want 1", dl.calls)
	}

	err := reg.Download(context.Background(), "spotify", "id", "/tmp/x.opus", nil)
	if !errors.Is(err, ErrDownloadUnsupported) {
		t.Errorf("spotify download: err = %v, want ErrDownloadUnsupported", err)
	}
	if meta.calls != 0 {
		t.Error("a metadata-only provider must never be asked to download")
	}

	if err := reg.Download(context.Background(), "deezer", "id", "/tmp/x.opus", nil); !errors.Is(err, ErrNotEnabled) {
		t.Errorf("unknown provider: err = %v, want ErrNotEnabled", err)
	}
}

// bareProvider implements Provider but not Streamer, to prove the registry
// looks at the interface as well as the capability flag.
type bareProvider struct{ name string }

func (b *bareProvider) Name() string       { return b.name }
func (b *bareProvider) Capabilities() Caps { return Caps{Search: true} }
func (b *bareProvider) Search(context.Context, string, SearchOpts) ([]Track, error) {
	return nil, nil
}
func (b *bareProvider) Download(context.Context, string, string, ProgressFunc) error {
	return ErrDownloadUnsupported
}

func TestStreamRouting(t *testing.T) {
	reg := newTestRegistry(t, time.Second)
	stream := &fakeProvider{
		name:       "ytmusic",
		caps:       Caps{Search: true, Stream: true},
		streamBody: io.NopCloser(strings.NewReader("audio bytes")),
		streamMIME: "audio/webm",
	}
	// A metadata-only provider carries the interface but not the capability.
	meta := &fakeProvider{name: "spotify", caps: Caps{Search: true}}
	noIface := &bareProvider{name: "plain"}
	reg.Register(stream)
	reg.Register(meta)
	reg.Register(noIface)

	body, mimeType, err := reg.Stream(context.Background(), "ytmusic", "id")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if mimeType != "audio/webm" {
		t.Errorf("mimeType = %q", mimeType)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	body.Close()
	if string(got) != "audio bytes" {
		t.Errorf("audio = %q", got)
	}
	if stream.calls != 1 {
		t.Errorf("stream calls = %d, want 1", stream.calls)
	}

	// A provider without the capability is refused even though it carries the
	// interface, and is never asked.
	if _, _, err := reg.Stream(context.Background(), "spotify", "id"); !errors.Is(err, ErrStreamUnsupported) {
		t.Errorf("spotify stream: err = %v, want ErrStreamUnsupported", err)
	}
	if meta.calls != 0 {
		t.Error("a metadata-only provider must never be asked to stream")
	}

	// A provider that does not implement Streamer at all is refused the same way.
	if _, _, err := reg.Stream(context.Background(), "plain", "id"); !errors.Is(err, ErrStreamUnsupported) {
		t.Errorf("plain stream: err = %v, want ErrStreamUnsupported", err)
	}

	if _, _, err := reg.Stream(context.Background(), "deezer", "id"); !errors.Is(err, ErrNotEnabled) {
		t.Errorf("unknown provider: err = %v, want ErrNotEnabled", err)
	}
}

func TestMIMEForExt(t *testing.T) {
	for _, tc := range []struct{ ext, want string }{
		{"m4a", "audio/mp4"},
		{"webm", "audio/webm"},
		{"opus", "audio/ogg"},
		{"ogg", "audio/ogg"},
		{"M4A", "audio/mp4"},
		{".webm", "audio/webm"},
	} {
		got, err := MIMEForExt(tc.ext)
		if err != nil {
			t.Errorf("MIMEForExt(%q): %v", tc.ext, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MIMEForExt(%q) = %q, want %q", tc.ext, got, tc.want)
		}
	}

	// An unknown container is refused rather than guessed.
	if got, err := MIMEForExt("flv"); err == nil {
		t.Errorf("MIMEForExt(flv) = %q, want an error", got)
	}
}

var _ Streamer = (*fakeProvider)(nil)

func TestMissingDepsAggregation(t *testing.T) {
	reg := newTestRegistry(t, time.Second)
	reg.Register(&fakeProvider{name: "ytmusic", caps: Caps{Search: true}, deps: []string{"yt-dlp", "ffmpeg"}})
	reg.Register(&fakeProvider{name: "clean", caps: Caps{Search: true}})

	deps := reg.MissingDeps()
	if len(deps) != 1 || len(deps["ytmusic"]) != 2 {
		t.Errorf("deps = %v", deps)
	}
	if _, ok := deps["clean"]; ok {
		t.Errorf("providers without missing deps must be absent: %v", deps)
	}
}

func TestNamesAndGetPreserveRegistrationOrder(t *testing.T) {
	reg := newTestRegistry(t, time.Second)
	for _, name := range []string{"b", "a", "c"} {
		reg.Register(&fakeProvider{name: name, caps: Caps{Search: true}})
	}
	names := reg.Names()
	want := []string{"b", "a", "c"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
	if _, ok := reg.Get("a"); !ok {
		t.Error("Get(a) missing")
	}
	if _, ok := reg.Get("z"); ok {
		t.Error("Get(z) must not resolve")
	}
}

// A provider that is not enabled can still answer for a playlist: that is the
// whole point of RegisterBrowsing, and it is what makes a Spotify playlist
// importable on a server that never enabled Spotify (its public embed page
// needs no account).
func TestBrowsingProviderAnswersAPlaylist(t *testing.T) {
	registry := NewRegistry(slog.New(slog.DiscardHandler), time.Second)

	built := 0
	registry.RegisterBrowsing("spotify", func() (Provider, error) {
		built++
		return &fakePlaylistProvider{}, nil
	})

	if _, err := registry.Playlist(context.Background(), "spotify", "public-playlist"); err != nil {
		t.Fatalf("a browsing provider answered with an error: %v", err)
	}
	if _, err := registry.Playlist(context.Background(), "spotify", "another"); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if built != 1 {
		t.Errorf("built %d times, want once: a browsing provider is built once and kept", built)
	}
	if _, err := registry.Playlist(context.Background(), "ytmusic", "x"); !errors.Is(err, ErrNotEnabled) {
		t.Errorf("a name with no provider and no browsing fallback = %v, want ErrNotEnabled", err)
	}
}

// An enabled provider wins over a browsing fallback for the same name, and the
// fallback is only consulted for the playlist read: enabling nothing must not
// turn into searching everything.
func TestBrowsingProviderIsOnlyForPlaylists(t *testing.T) {
	registry := NewRegistry(slog.New(slog.DiscardHandler), time.Second)
	registry.RegisterBrowsing("spotify", func() (Provider, error) { return &fakePlaylistProvider{}, nil })

	if names := registry.Names(); len(names) != 0 {
		t.Errorf("registered names = %v, want none: browsing is not enabling", names)
	}
	results := registry.Search(context.Background(), "anything", SearchOpts{Limit: 5})
	if len(results) != 0 {
		t.Errorf("search results = %d, want none from a browsing provider", len(results))
	}

	enabled := &fakePlaylistProvider{}
	registry.Register(enabled)
	if _, err := registry.Playlist(context.Background(), "spotify", "x"); err != nil {
		t.Fatalf("playlist: %v", err)
	}
}

// fakePlaylistProvider answers a playlist, and nothing else.
type fakePlaylistProvider struct{}

func (f *fakePlaylistProvider) Name() string       { return "spotify" }
func (f *fakePlaylistProvider) Capabilities() Caps { return Caps{Playlists: true} }
func (f *fakePlaylistProvider) Search(context.Context, string, SearchOpts) ([]Track, error) {
	return nil, nil
}
func (f *fakePlaylistProvider) Download(context.Context, string, string, ProgressFunc) error {
	return nil
}
func (f *fakePlaylistProvider) SearchPlaylists(context.Context, string, SearchOpts) ([]Playlist, error) {
	return nil, nil
}

func (f *fakePlaylistProvider) Playlist(context.Context, string) (*PlaylistDetail, error) {
	return &PlaylistDetail{
		Playlist: Playlist{Title: "A playlist"},
		Tracks:   []Track{{ProviderTrackID: "t1", Title: "A song"}},
	}, nil
}
