//go:build live

package ytmusic

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/musoak/internal/ffmpeg"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
)

// TestLiveSearchAndDownload exercises the real ytmusicapi, yt-dlp and ffmpeg
// pipeline against YouTube Music.
//
//	go test -tags live ./internal/provider/ytmusic/ -run TestLive -v
func TestLiveSearchAndDownload(t *testing.T) {
	if missing := New(nil).MissingDeps(); len(missing) > 0 {
		t.Skipf("tools unavailable: %v", missing)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	p := New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	tracks, err := p.Search(ctx, "Rick Astley Never Gonna Give You Up", provider.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(tracks) == 0 {
		t.Fatal("search returned no tracks")
	}
	hit := tracks[0]
	t.Logf("hit: %q by %v (%d ms) id=%s", hit.Title, hit.Artists, hit.DurationMs, hit.ProviderTrackID)

	dst := filepath.Join(t.TempDir(), "track.opus")
	if err := p.Download(ctx, hit.ProviderTrackID, dst); err != nil {
		t.Fatalf("download: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("downloaded file is empty")
	}

	codec, err := ffmpeg.AudioCodec(ctx, dst)
	if err != nil {
		t.Fatalf("probe codec: %v", err)
	}
	if codec != "opus" {
		t.Errorf("codec = %q, want opus", codec)
	}

	dur, err := ffmpeg.Duration(ctx, dst)
	if err != nil {
		t.Fatalf("probe duration: %v", err)
	}
	if dur <= 0 {
		t.Fatalf("duration = %v", dur)
	}
	if hit.DurationMs > 0 {
		diff := dur - time.Duration(hit.DurationMs)*time.Millisecond
		if diff < -5*time.Second || diff > 5*time.Second {
			t.Errorf("probed duration %v differs from search metadata %d ms", dur, hit.DurationMs)
		}
	}
	t.Logf("downloaded %s: %v, %d bytes", dst, dur, info.Size())
}
