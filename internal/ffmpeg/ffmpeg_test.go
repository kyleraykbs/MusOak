package ffmpeg

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// toneFile renders a short sine wave so the probe paths run against a real file.
func toneFile(t *testing.T, dir string, seconds float64) string {
	t.Helper()
	if len(Missing()) > 0 {
		t.Skipf("ffmpeg/ffprobe unavailable: %v", Missing())
	}
	dst := filepath.Join(dir, "tone.opus")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := run(ctx, ffmpegName,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1.5",
		"-c:a", "libopus", "-f", "opus", dst); err != nil {
		t.Fatalf("render tone: %v", err)
	}
	return dst
}

func TestDurationAndCodec(t *testing.T) {
	src := toneFile(t, t.TempDir(), 1.5)
	ctx := context.Background()

	got, err := Duration(ctx, src)
	if err != nil {
		t.Fatalf("Duration: %v", err)
	}
	if got < 1400*time.Millisecond || got > 1600*time.Millisecond {
		t.Errorf("duration = %v, want ~1.5s", got)
	}

	codec, err := AudioCodec(ctx, src)
	if err != nil {
		t.Fatalf("AudioCodec: %v", err)
	}
	if codec != "opus" {
		t.Errorf("codec = %q, want opus", codec)
	}
}

func TestToOpusRemuxesWithoutReencoding(t *testing.T) {
	dir := t.TempDir()
	src := toneFile(t, dir, 1.5)
	ctx := context.Background()

	dst := filepath.Join(dir, "remuxed.opus")
	if err := ToOpus(ctx, src, dst); err != nil {
		t.Fatalf("ToOpus: %v", err)
	}
	if codec, err := AudioCodec(ctx, dst); err != nil || codec != "opus" {
		t.Fatalf("codec = %q, err = %v", codec, err)
	}
	// A remuxed stream keeps the packets, so the durations agree exactly.
	srcDur, _ := Duration(ctx, src)
	dstDur, _ := Duration(ctx, dst)
	if diff := srcDur - dstDur; diff > 20*time.Millisecond || diff < -20*time.Millisecond {
		t.Errorf("duration drift: %v -> %v", srcDur, dstDur)
	}
}

func TestToOpusEncodesForeignCodec(t *testing.T) {
	dir := t.TempDir()
	if len(Missing()) > 0 {
		t.Skip("ffmpeg unavailable")
	}
	src := filepath.Join(dir, "tone.m4a")
	ctx := context.Background()
	if _, err := run(ctx, ffmpegName,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "aac", src); err != nil {
		t.Fatalf("render m4a: %v", err)
	}
	if codec, _ := AudioCodec(ctx, src); codec != "aac" {
		t.Fatalf("fixture codec = %q, want aac", codec)
	}

	dst := filepath.Join(dir, "out.opus")
	if err := ToOpus(ctx, src, dst); err != nil {
		t.Fatalf("ToOpus: %v", err)
	}
	if codec, err := AudioCodec(ctx, dst); err != nil || codec != "opus" {
		t.Fatalf("codec = %q, err = %v", codec, err)
	}
}

func TestMissingReportsAbsentBinaries(t *testing.T) {
	oldFFmpeg, oldFFprobe := ffmpegName, ffprobeName
	t.Cleanup(func() { ffmpegName, ffprobeName = oldFFmpeg, oldFFprobe })
	SetBinaries("definitely-not-ffmpeg", "definitely-not-ffprobe")

	missing := Missing()
	if len(missing) != 2 {
		t.Fatalf("missing = %v, want both binaries", missing)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("system ffmpeg unavailable; nothing more to check")
	}
}
