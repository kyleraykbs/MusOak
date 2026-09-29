package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/api"
	"codeberg.org/kyleraykbs/prismusic/internal/config"
)

// lockedBuffer collects CLI output from the playback goroutine too.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func requireMpv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv is not installed")
	}
	// Headless playback: no window, no audio device, no terminal noise.
	t.Setenv("PRISM_MPV_ARGS", "--ao=null --no-video --really-quiet")
}

func testApp(t *testing.T, mutate func(*config.Config)) (*App, *lockedBuffer) {
	t.Helper()
	requireMpv(t)

	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Client.CacheDir = t.TempDir()
	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false
	if mutate != nil {
		mutate(cfg)
	}

	out := &lockedBuffer{}
	app, err := New(cfg, slog.New(slog.DiscardHandler), out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return app, out
}

func tone(t *testing.T, dir, name string, seconds float64) string {
	t.Helper()
	dst := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+strconv.FormatFloat(seconds, 'f', 2, 64),
		"-c:a", "libopus", "-f", "opus", dst)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg unavailable: %v: %s", err, stderr.String())
	}
	return dst
}

func TestStandaloneModeRunsItsOwnServer(t *testing.T) {
	app, out := testApp(t, nil)
	ctx := context.Background()

	if app.embedded == nil {
		t.Fatal("with no client.serverURL the CLI must run its own server")
	}
	if got := app.client.BaseURL(); len(got) < len("http://127.0.0.1:") {
		t.Fatalf("client base URL = %q, want loopback", got)
	}

	// The embedded server is a full server: importing works through the API.
	dir := t.TempDir()
	tone(t, dir, "Standalone.opus", 0.3)
	if err := app.Run(ctx, []string{"library", "import", dir}); err != nil {
		t.Fatalf("library import: %v", err)
	}
	if !bytes.Contains([]byte(out.String()), []byte("Standalone")) {
		t.Errorf("import output = %q", out.String())
	}
	if entries := app.cache.Entries(); len(entries) != 1 {
		t.Errorf("cache entries = %d, want the imported rendition", len(entries))
	}
}

func TestRemoteServerMode(t *testing.T) {
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false

	server, err := api.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		_ = server.Close()
	})

	requireMpv(t)
	remoteCfg := config.Default()
	remoteCfg.StorageDir = t.TempDir()
	remoteCfg.Client.CacheDir = t.TempDir()
	remoteCfg.Client.ServerURL = httpServer.URL
	remoteCfg.Providers.YTMusic.Enabled = false
	remoteCfg.Providers.Spotify.Enabled = false

	out := &lockedBuffer{}
	app, err := New(remoteCfg, slog.New(slog.DiscardHandler), out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	if app.embedded != nil {
		t.Fatal("with client.serverURL set the CLI must not start a server")
	}
	if app.client.BaseURL() != httpServer.URL {
		t.Fatalf("base URL = %q, want %q", app.client.BaseURL(), httpServer.URL)
	}
	if err := app.Run(context.Background(), []string{"providers"}); err != nil {
		t.Fatalf("providers: %v", err)
	}
}

func TestImportQueueAndPlay(t *testing.T) {
	app, out := testApp(t, func(cfg *config.Config) {
		cfg.PrefetchCount = 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	tone(t, dir, "First.opus", 0.5)
	tone(t, dir, "Second.opus", 0.5)
	if err := app.Run(ctx, []string{"library", "import", dir}); err != nil {
		t.Fatalf("library import: %v", err)
	}

	entries := app.cache.Entries()
	if len(entries) != 2 {
		t.Fatalf("cache entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.TrackID == "" {
			t.Fatal("cached rendition was not indexed against its track")
		}
		if err := app.Run(ctx, []string{"queue", "add", entry.TrackID}); err != nil {
			t.Fatalf("queue add: %v", err)
		}
	}
	if len(app.state.Queue) != 2 {
		t.Fatalf("queue = %d entries, want 2", len(app.state.Queue))
	}

	if err := app.Run(ctx, []string{"queue", "list"}); err != nil {
		t.Fatalf("queue list: %v", err)
	}
	listing := out.String()
	if !bytes.Contains([]byte(listing), []byte("First")) || !bytes.Contains([]byte(listing), []byte("Second")) {
		t.Errorf("queue listing = %q", listing)
	}

	// Playing the queue downloads everything it needs and returns once mpv has
	// played it all out.
	start := time.Now()
	if err := app.Run(ctx, []string{"play"}); err != nil {
		t.Fatalf("play: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("play returned after %v; the two 0.5s tracks should take about a second", elapsed)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("play took %v", elapsed)
	}

	// Both renditions are on disk, so a second play needs no download at all.
	for _, entry := range entries {
		if _, ok := app.cache.Lookup(entry.VariantID); !ok {
			t.Errorf("variant %s is not in the cache after playing", entry.VariantID)
		}
	}
}

// TestQueuePlaysGaplessly checks the property the plan asks for: the next file
// is queued in mpv while the current one is still playing, so there is no gap
// between tracks.
func TestQueuePlaysGaplessly(t *testing.T) {
	requireMpv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	first := tone(t, dir, "First.opus", 0.6)
	second := tone(t, dir, "Second.opus", 0.4)

	socket := filepath.Join(t.TempDir(), "mpv.sock")
	sink, err := NewMpvSink(ctx, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMpvSink: %v", err)
	}
	defer sink.Close()

	if err := sink.StartFile(ctx, first, 0); err != nil {
		t.Fatalf("StartFile: %v", err)
	}

	// The second file must be queued while the first is still playing.
	time.Sleep(150 * time.Millisecond)
	if idle, err := sink.process.idle(ctx); err != nil || idle {
		t.Fatalf("mpv is idle %v after starting playback (err %v)", idle, err)
	}
	if err := sink.AppendFile(ctx, second); err != nil {
		t.Fatalf("AppendFile: %v", err)
	}
	count, err := sink.PlaylistCount(ctx)
	if err != nil {
		t.Fatalf("PlaylistCount: %v", err)
	}
	if count < 2 {
		t.Fatalf("playlist has %d entries right after the append, want 2", count)
	}
	if idle, _ := sink.process.idle(ctx); idle {
		t.Fatal("mpv went idle between the two tracks")
	}

	start := time.Now()
	if err := sink.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Errorf("both tracks finished in %v, faster than their 1.0s total", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("playback took %v; there was likely a gap", elapsed)
	}
}

func TestSearchWithoutProvidersReportsNothing(t *testing.T) {
	app, out := testApp(t, nil)
	if err := app.Run(context.Background(), []string{"search", "anything"}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if !bytes.Contains([]byte(out.String()), []byte("no results")) {
		t.Errorf("search output = %q", out.String())
	}
}
