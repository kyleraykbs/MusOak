package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/internal/config"
	"github.com/kyleraykbs/MusOak/pkg/client"
)

// localItem is the delivery for a rendition already on disk.
func localItem(path string) playbackItem {
	return playbackItem{source: path, localPath: path}
}

// newSlotApp is a minimal App enough for slot resolution and delivery: a
// client pointed at a server and an empty cache.
func newSlotApp(t *testing.T, baseURL string) *App {
	t.Helper()
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	return &App{
		cfg:    config.Default(),
		logger: slog.New(slog.DiscardHandler),
		client: client.New(baseURL),
		cache:  cache,
	}
}

// TestSlotItemStreamsUncachedTrackAsURL checks that a rendition which is not on
// disk yet is delivered as its stream URL, and that delivery does not wait for
// the download. The slot's local channel is left empty on purpose: if delivery
// blocked on it, this test would hang.
func TestSlotItemStreamsUncachedTrackAsURL(t *testing.T) {
	app := newSlotApp(t, "http://127.0.0.1:1")
	s := &slot{item: queueItem{TrackID: "t1"}, variantID: "v1", local: make(chan client.CacheEntry, 1)}

	done := make(chan playbackItem, 1)
	go func() { done <- app.slotItem(s, 0) }()

	select {
	case item := <-done:
		if want := app.client.MediaURL("v1", 0); item.source != want {
			t.Errorf("source = %q, want the stream URL %q", item.source, want)
		}
		if item.ready != s.local {
			t.Error("the local copy was not registered for a later handover")
		}
		if item.localPath != "" {
			t.Errorf("localPath = %q, want it empty until the copy lands", item.localPath)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery waited for the download")
	}
}

// TestSlotItemUsesLocalPathWhenCached checks that a rendition already on disk
// is delivered as its local path.
func TestSlotItemUsesLocalPathWhenCached(t *testing.T) {
	app := newSlotApp(t, "http://127.0.0.1:1")
	path := app.cache.Path("v1")
	if err := os.WriteFile(path, []byte("tone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.cache.Put(client.CacheEntry{VariantID: "v1", TrackID: "t1", Path: path}); err != nil {
		t.Fatal(err)
	}

	s := &slot{item: queueItem{TrackID: "t1"}, variantID: "v1", local: make(chan client.CacheEntry, 1)}
	item := app.slotItem(s, 0)
	if item.source != path {
		t.Errorf("source = %q, want the local path %q", item.source, path)
	}
	if item.localPath != path {
		t.Errorf("localPath = %q, want %q", item.localPath, path)
	}
	if item.ready != nil {
		t.Error("a cached track needs no handover channel")
	}
}

// TestSlotItemStreamsFromTheRequestedPosition pins the whole point: a song that
// is not on disk, started at 45 s, is delivered as a stream URL that begins
// there, and the player is told the position so it neither seeks (a stream has
// nothing to seek within) nor draws the timeline from zero.
func TestSlotItemStreamsFromTheRequestedPosition(t *testing.T) {
	app := newSlotApp(t, "http://127.0.0.1:1")
	s := &slot{item: queueItem{TrackID: "t1"}, variantID: "v1", local: make(chan client.CacheEntry, 1)}

	item := app.slotItem(s, 45_000)
	if want := app.client.MediaURL("v1", 45_000); item.source != want {
		t.Errorf("source = %q, want %q", item.source, want)
	}
	if item.source != "http://127.0.0.1:1/api/v1/media/v1?start=45000" {
		t.Errorf("source = %q, want the stream to begin at 45 s", item.source)
	}
	if item.positionMs != 45_000 {
		t.Errorf("positionMs = %d, want 45000", item.positionMs)
	}
	if item.seekMs() != 0 {
		t.Errorf("seekMs = %d, want 0: the stream already starts there", item.seekMs())
	}
}

// TestSlotItemSeeksALocalCopyToThePosition is the other half: a copy already on
// disk is played directly, and the position is a seek.
func TestSlotItemSeeksALocalCopyToThePosition(t *testing.T) {
	app := newSlotApp(t, "http://127.0.0.1:1")
	path := app.cache.Path("v1")
	if err := os.WriteFile(path, []byte("tone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.cache.Put(client.CacheEntry{VariantID: "v1", TrackID: "t1", Path: path}); err != nil {
		t.Fatal(err)
	}

	s := &slot{item: queueItem{TrackID: "t1"}, variantID: "v1", local: make(chan client.CacheEntry, 1)}
	item := app.slotItem(s, 45_000)
	if item.source != path {
		t.Errorf("source = %q, want the local path %q", item.source, path)
	}
	if item.seekMs() != 45_000 {
		t.Errorf("seekMs = %d, want 45000", item.seekMs())
	}
}

// TestPlayerReadsStreamDurationFromTheHeader checks that a streamed entry's
// length comes from the stream's own header - the only place it exists before
// the file does - and is recorded for the status line.
func TestPlayerReadsStreamDurationFromTheHeader(t *testing.T) {
	ctx := context.Background()
	p := newPlayer(io.Discard, nil, []queueItem{{TrackID: "t1", Title: "Song"}})
	asked := make(chan struct{}, 1)
	p.streamInfo = func(ctx context.Context, variantID string, startMs int64) (client.StreamInfo, error) {
		if variantID != "v1" || startMs != 45_000 {
			t.Errorf("probe(%q, %d), want v1 at 45000", variantID, startMs)
		}
		asked <- struct{}{}
		return client.StreamInfo{Streaming: true, DurationMs: 213_000}, nil
	}

	item := playbackItem{variantID: "v1", source: "http://s/media/v1?start=45000", positionMs: 45_000, ready: make(chan client.CacheEntry)}
	index := p.addItem(item)
	p.watchDuration(ctx, index, item)

	select {
	case <-asked:
	case <-time.After(time.Second):
		t.Fatal("the stream was never asked for its length")
	}
	waitForCondition(t, ctx, "the header's length to be recorded", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.items[index].durationMs == 213_000
	})
}

// TestPlayerDrawsAStreamedSongFromItsOffset checks that the status line counts
// a stream's playhead from where the stream began: mpv reports time relative to
// the bytes it was handed, and the offset is what makes the number the position
// in the song.
func TestPlayerDrawsAStreamedSongFromItsOffset(t *testing.T) {
	dir := t.TempDir()
	stream := tone(t, dir, "stream.opus", 3.0)

	p, cancel := newTestPlayer(t, stream)
	defer cancel()
	ctx := context.Background()

	// Delivered as a stream (ready is non-nil) that began 45 s in, so mpv is
	// handed it at zero and the offset is added back for the status line.
	ready := make(chan client.CacheEntry, 1)
	if err := p.startTrack(ctx, playbackItem{
		variantID: "v1", source: stream, ready: ready, positionMs: 45_000,
	}); err != nil {
		t.Fatalf("startTrack: %v", err)
	}

	waitForCondition(t, ctx, "the playhead to be counted from the offset", func() bool {
		p.refresh(ctx)
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.positionMs >= 45_000 && p.positionMs < 46_000
	})
}

// TestPlayerSwapsToLocalWhenPlayingCopyLands checks the handover: when the
// local copy of the track that is playing lands, mpv switches to it at the
// position it had reached, and stays paused if it was paused.
func TestPlayerSwapsToLocalWhenPlayingCopyLands(t *testing.T) {
	dir := t.TempDir()
	stream := tone(t, dir, "stream.opus", 3.0)
	local := tone(t, dir, "local.opus", 3.0)

	p, cancel := newTestPlayer(t, stream)
	defer cancel()
	ctx := context.Background()

	ready := make(chan client.CacheEntry, 1)
	if err := p.startTrack(ctx, playbackItem{variantID: "v1", source: stream, ready: ready}); err != nil {
		t.Fatalf("startTrack: %v", err)
	}

	// Get playback going, jump into the file, and pause: the handover must
	// preserve both the position and the pause state.
	waitForCondition(t, ctx, "playback to start", func() bool {
		position, err := p.sink.process.positionMs(ctx)
		return err == nil && position > 0
	})
	if err := p.sink.process.seek(ctx, 700); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if err := p.sink.process.setPause(ctx, true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// The download lands.
	ready <- client.CacheEntry{VariantID: "v1", TrackID: "t1", Path: local}

	waitForCondition(t, ctx, "the swap to the local copy", func() bool {
		return currentPath(ctx, t, p) == local
	})
	if paused, err := p.sink.process.pauseState(ctx); err != nil || !paused {
		t.Fatalf("paused = %v (err %v), want the swap to keep it paused", paused, err)
	}
	waitForCondition(t, ctx, "the position to survive the swap", func() bool {
		where, err := p.sink.process.positionMs(ctx)
		return err == nil && where >= 600 && where <= 1600
	})

	p.mu.Lock()
	got := p.items[0]
	p.mu.Unlock()
	if got.localPath != local || got.source != local {
		t.Errorf("item after swap = %+v, want it playing %q", got, local)
	}
}

// TestPlayerHandsOverQueuedCopyWhenItStartsPlaying checks the other half of the
// handover: a copy that lands for an entry still queued behind another must not
// disturb what is playing, and must be used once that entry starts playing.
func TestPlayerHandsOverQueuedCopyWhenItStartsPlaying(t *testing.T) {
	dir := t.TempDir()
	first := tone(t, dir, "first.opus", 3.0)
	second := tone(t, dir, "second.opus", 3.0)
	local := tone(t, dir, "second-local.opus", 3.0)

	p, cancel := newTestPlayer(t, first, second)
	defer cancel()
	ctx := context.Background()

	if err := p.startTrack(ctx, localItem(first)); err != nil {
		t.Fatalf("startTrack: %v", err)
	}
	ready := make(chan client.CacheEntry, 1)
	if err := p.appendTrack(ctx, playbackItem{variantID: "v2", source: second, ready: ready}); err != nil {
		t.Fatalf("appendTrack: %v", err)
	}

	// The copy lands while the second entry is still queued.
	ready <- client.CacheEntry{VariantID: "v2", TrackID: "t2", Path: local}
	time.Sleep(100 * time.Millisecond)
	if got := currentPath(ctx, t, p); got != first {
		t.Fatalf("a queued entry's copy swapped the playing track: path = %q", got)
	}

	// Once it is the one playing, it is handed over.
	if err := p.sink.process.nextEntry(ctx); err != nil {
		t.Fatalf("nextEntry: %v", err)
	}
	waitForCondition(t, ctx, "the queued track to be handed over", func() bool {
		p.refresh(ctx)
		return currentPath(ctx, t, p) == local
	})
}

// currentPath reads the file mpv currently has loaded.
func currentPath(ctx context.Context, t *testing.T, p *player) string {
	t.Helper()
	raw, err := p.sink.process.property(ctx, "path")
	if err != nil {
		return ""
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return ""
	}
	return got
}

// TestSlotDownloadSurvivesContextCancel checks that cancelling the command's
// context — what a skip or a track change amounts to — does not abandon an
// in-flight download. The slot detaches the transfer, so the file still lands.
func TestSlotDownloadSurvivesContextCancel(t *testing.T) {
	const variantID = "v1"
	release := make(chan struct{})
	// started fires when the server has the request in hand and is writing the
	// body, which is what "the transfer is in flight" means here.
	started := make(chan struct{}, 1)
	body := []byte("first-half-second-half")
	hashed := sha256.Sum256(body)
	etag := hex.EncodeToString(hashed[:])

	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v1/ranking", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"effective": []string{"p"}})
	})
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"variants": []map[string]any{
			{"id": variantID, "provider": "p", "downloadable": true},
		}})
	})
	mux.HandleFunc("/api/v1/media/"+variantID+"/download", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, client.MediaStatus{VariantID: variantID, State: client.MediaReady})
	})
	mux.HandleFunc("/api/v1/media/"+variantID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+etag+`"`)
		_, _ = w.Write(body[:len("first-half")])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write(body[len("first-half"):])
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	app := newSlotApp(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The play session's provider order, as cmdPlay would resolve it once.
	order := func(context.Context) []string { return []string{"p"} }
	s := app.startSlot(ctx, queueItem{TrackID: "t1", Title: "Song"}, order)
	<-s.resolved
	if s.err != nil {
		t.Fatalf("resolve: %v", s.err)
	}

	// Wait until the transfer is actually in flight, then cancel the context
	// the way a skip would and let the server finish the response.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the download did not start")
	}
	cancel()
	close(release)

	select {
	case entry, ok := <-s.local:
		if !ok {
			t.Fatal("the download was abandoned when the context was cancelled")
		}
		if entry.VariantID != variantID {
			t.Fatalf("entry = %+v, want variant %s", entry, variantID)
		}
		if _, err := os.Stat(filepath.Join(app.cache.Dir(), variantID+".opus")); err != nil {
			t.Errorf("the cached file is missing: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the download did not finish after the context was cancelled")
	}
}
