package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

func decodeAll(t *testing.T, input string) []key {
	t.Helper()
	reader := newKeyReader(bytes.NewReader([]byte(input)))
	var keys []key
	for {
		k := reader.next(200 * time.Millisecond)
		if k == keyNone {
			// A trailing keyNone means the input is exhausted.
			if _, ok := reader.await(time.Millisecond); !ok {
				return keys
			}
			continue
		}
		keys = append(keys, k)
		if len(keys) > len(input)+4 {
			t.Fatalf("decoder does not terminate on %q", input)
		}
	}
}

func TestKeyReaderLetters(t *testing.T) {
	got := decodeAll(t, "jklh")
	want := []key{keyVolumeDown, keyVolumeUp, keyNext, keyPrevious}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func TestKeyReaderArrows(t *testing.T) {
	got := decodeAll(t, "\x1b[A\x1b[B\x1b[C\x1b[D")
	want := []key{keyVolumeUp, keyVolumeDown, keyNext, keyPrevious}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func TestKeyReaderPauseAndQuit(t *testing.T) {
	got := decodeAll(t, " q\x03")
	want := []key{keyPause, keyQuit, keyQuit}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func TestKeyReaderIgnoresUnknownKeys(t *testing.T) {
	got := decodeAll(t, "z!0")
	if len(got) != 0 {
		t.Fatalf("keys = %v, want none", got)
	}
}

// TestKeyReaderLoneEscapeDoesNotBlock checks that a bare ESC (or an unknown
// escape sequence) is a no-op rather than a hang or a bogus key.
func TestKeyReaderLoneEscapeDoesNotBlock(t *testing.T) {
	pipe, writer := io.Pipe()
	defer writer.Close()
	reader := newKeyReader(pipe)

	if _, err := writer.Write([]byte{27}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if k := reader.next(200 * time.Millisecond); k != keyNone {
		t.Fatalf("lone ESC decoded as %v", k)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("lone ESC waited %v; the parser blocks on partial sequences", elapsed)
	}
}

func TestVolumeBar(t *testing.T) {
	tests := map[int]string{
		0:   "----------",
		50:  "#####-----",
		100: "##########",
		7:   "#---------",
		-5:  "----------",
		120: "##########",
	}
	for volume, want := range tests {
		if got := volumeBar(volume); got != want {
			t.Errorf("volumeBar(%d) = %q, want %q", volume, got, want)
		}
	}
}

func TestStatusLine(t *testing.T) {
	line := formatStatus(status{
		position:   1,
		count:      5,
		title:      "Never Gonna Give You Up",
		positionMs: 83_000,
		durationMs: 214_000,
		volume:     40,
	})
	for _, want := range []string{"> [2/5]", "Never Gonna Give You Up", "1:23/3:34", "vol [####------]  40%"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line %q is missing %q", line, want)
		}
	}

	paused := formatStatus(status{position: 0, count: 2, title: "Song", volume: 100, paused: true})
	if !strings.HasPrefix(paused, "|| [1/2]") {
		t.Errorf("paused line = %q, want a pause marker", paused)
	}

	idle := formatStatus(status{position: -1, count: 0, volume: 0})
	if !strings.Contains(idle, "[0/0]") {
		t.Errorf("idle line = %q, want an empty position", idle)
	}

	withMessage := formatStatus(status{position: 0, count: 3, title: "Song", volume: 10, message: "next track is still downloading"})
	if !strings.Contains(withMessage, "next track is still downloading") {
		t.Errorf("line %q is missing its message", withMessage)
	}
}

// --- controls against a real mpv -------------------------------------------

func newTestPlayer(t *testing.T, tracks ...string) (*player, context.CancelFunc) {
	t.Helper()
	requireMpv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	sink, err := NewMpvSink(ctx, filepath.Join(t.TempDir(), "mpv.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		cancel()
		t.Fatalf("NewMpvSink: %v", err)
	}
	t.Cleanup(func() {
		sink.Close()
		cancel()
	})

	queue := make([]queueItem, 0, len(tracks))
	for i, path := range tracks {
		queue = append(queue, queueItem{TrackID: path, Title: filepath.Base(path)})
		_ = i
	}
	return newPlayer(io.Discard, sink, queue), cancel
}

func TestPlayerVolumeKeys(t *testing.T) {
	p, cancel := newTestPlayer(t, tone(t, t.TempDir(), "First.opus", 1.5))
	defer cancel()
	ctx := context.Background()

	if err := p.sink.process.setVolume(ctx, 50); err != nil {
		t.Fatalf("setVolume: %v", err)
	}

	p.handle(ctx, keyVolumeUp)
	if got := readVolume(t, p); got != 55 {
		t.Errorf("volume after k = %d, want 55", got)
	}
	p.handle(ctx, keyVolumeDown)
	p.handle(ctx, keyVolumeDown)
	if got := readVolume(t, p); got != 45 {
		t.Errorf("volume after j j = %d, want 45", got)
	}

	// The level is clamped rather than pushed past the ends.
	p.handle(ctx, keyVolumeUp)
	p.changeVolume(ctx, 1000)
	if got := readVolume(t, p); got != 100 {
		t.Errorf("volume after a large increase = %d, want 100", got)
	}
	p.changeVolume(ctx, -1000)
	if got := readVolume(t, p); got != 0 {
		t.Errorf("volume after a large decrease = %d, want 0", got)
	}
}

func TestPlayerPauseKey(t *testing.T) {
	dir := t.TempDir()
	p, cancel := newTestPlayer(t, tone(t, dir, "First.opus", 1.5))
	defer cancel()
	ctx := context.Background()

	if err := p.startTrack(ctx, client.CacheEntry{Path: p.queue[0].TrackID}); err != nil {
		t.Fatalf("startTrack: %v", err)
	}

	p.handle(ctx, keyPause)
	if paused, err := p.sink.process.pauseState(ctx); err != nil || !paused {
		t.Fatalf("paused = %v (err %v), want paused after space", paused, err)
	}
	p.handle(ctx, keyPause)
	if paused, err := p.sink.process.pauseState(ctx); err != nil || paused {
		t.Fatalf("paused = %v (err %v), want playing after the second space", paused, err)
	}
}

func TestPlayerNextAndPreviousKeys(t *testing.T) {
	dir := t.TempDir()
	first := tone(t, dir, "First.opus", 1.5)
	second := tone(t, dir, "Second.opus", 1.5)

	p, cancel := newTestPlayer(t, first, second)
	defer cancel()
	ctx := context.Background()

	if err := p.startTrack(ctx, client.CacheEntry{Path: first}); err != nil {
		t.Fatalf("startTrack: %v", err)
	}

	// Nothing is queued behind yet: "next" waits for it instead of stopping.
	p.handle(ctx, keyNext)
	p.mu.Lock()
	pending, message := p.pendingSkip, p.message
	p.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pendingSkip = %d, want 1", pending)
	}
	if message == "" {
		t.Error("the player should say the next track is still downloading")
	}

	// When that track lands, the earlier key press takes effect.
	if err := p.appendTrack(ctx, client.CacheEntry{Path: second}); err != nil {
		t.Fatalf("appendTrack: %v", err)
	}
	waitForCondition(t, ctx, "the queued skip to fire", func() bool {
		position, err := p.sink.process.playlistPosition(ctx)
		return err == nil && position == 1
	})
	if pending := pendingSkips(p); pending != 0 {
		t.Errorf("pendingSkip = %d, want 0 once it fired", pending)
	}

	// Left goes back to the previous entry.
	p.handle(ctx, keyPrevious)
	waitForCondition(t, ctx, "the previous entry", func() bool {
		position, err := p.sink.process.playlistPosition(ctx)
		return err == nil && position == 0
	})

	// And at the start, "previous" does nothing rather than wrapping.
	p.handle(ctx, keyPrevious)
	if position, err := p.sink.process.playlistPosition(ctx); err != nil || position != 0 {
		t.Errorf("position = %d (err %v), want to stay at the first entry", position, err)
	}
}

func TestPlayerQuitKeyStopsPlayback(t *testing.T) {
	dir := t.TempDir()
	p, cancel := newTestPlayer(t, tone(t, dir, "First.opus", 2.0))
	defer cancel()
	ctx := context.Background()

	if err := p.startTrack(ctx, client.CacheEntry{Path: p.queue[0].TrackID}); err != nil {
		t.Fatalf("startTrack: %v", err)
	}

	p.handle(ctx, keyQuit)
	select {
	case <-p.done():
	case <-time.After(2 * time.Second):
		t.Fatal("q did not stop the player")
	}
	if err := p.wait(ctx); err != nil {
		t.Errorf("wait after quit = %v, want nil", err)
	}
}

func TestPlayerStatusLineReflectsMpv(t *testing.T) {
	dir := t.TempDir()
	first := tone(t, dir, "First.opus", 2.0)
	second := tone(t, dir, "Second.opus", 2.0)

	p, cancel := newTestPlayer(t, first, second)
	defer cancel()
	ctx := context.Background()

	if err := p.startTrack(ctx, client.CacheEntry{Path: first}); err != nil {
		t.Fatal(err)
	}
	if err := p.appendTrack(ctx, client.CacheEntry{Path: second}); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, ctx, "the status line to catch up", func() bool {
		p.refresh(ctx)
		return strings.Contains(p.statusLine(), "[1/2]") && strings.Contains(p.statusLine(), "First.opus")
	})
	if line := p.statusLine(); !strings.Contains(line, "vol [") {
		t.Errorf("status line %q has no volume", line)
	}
}

func readVolume(t *testing.T, p *player) int {
	t.Helper()
	volume, err := p.sink.process.volume(context.Background())
	if err != nil {
		t.Fatalf("volume: %v", err)
	}
	return volume
}

func pendingSkips(p *player) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pendingSkip
}

func waitForCondition(t *testing.T, ctx context.Context, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", what, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
