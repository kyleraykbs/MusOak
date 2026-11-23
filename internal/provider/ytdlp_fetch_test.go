package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A cancelled download has to come back. yt-dlp hands some transfers to a child
// of its own, which keeps running - and keeps the output open - when only yt-dlp
// is killed; the fetch then waited for a process nobody asked for, holding the
// song's one download slot the whole time.
func TestYtdlpFetchStopsTheProcessesItStarted(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	script := filepath.Join(dir, "fake-yt-dlp")
	// The child inherits stdout, as yt-dlp's own ffmpeg does, and outlives a
	// kill of the script alone.
	body := "#!/bin/sh\nsleep 30 &\ntouch " + started + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := YtdlpFetch(ctx, script, "test", "https://example.test/watch?v=abc", dir, nil, nil)
		done <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in for yt-dlp never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled fetch reported success")
		}
	case <-time.After(ytdlpWaitDelay - time.Second):
		t.Fatal("the fetch was still waiting on its child's output after it was cancelled")
	}
}

func TestLineWriterHandsOnWholeLines(t *testing.T) {
	var got []string
	w := &lineWriter{line: func(line string) { got = append(got, line) }}
	for _, piece := range []string{"[download]  10% of 1MiB\n[down", "load]  20% of 1MiB\r\n", "tail"} {
		if _, err := w.Write([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	want := []string{"[download]  10% of 1MiB", "[download]  20% of 1MiB", "tail"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}
