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

// A download that ends in silence must not keep it: a room plays to the end of
// the file, so a file that ends in two seconds of silence is two seconds of the
// room sitting quiet before the next song.
func TestToOpusTrimsTrailingSilence(t *testing.T) {
	dir := t.TempDir()
	if len(Missing()) > 0 {
		t.Skipf("ffmpeg/ffprobe unavailable: %v", Missing())
	}
	ctx := context.Background()

	src := filepath.Join(dir, "padded.opus")
	if _, err := run(ctx, ffmpegName,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1.5",
		"-af", "apad", "-t", "3.5",
		"-c:a", "libopus", "-f", "opus", src); err != nil {
		t.Fatalf("render padded tone: %v", err)
	}
	if srcDur, err := Duration(ctx, src); err != nil || srcDur < 3400*time.Millisecond {
		t.Fatalf("fixture duration = %v (err %v), want ~3.5s", srcDur, err)
	}

	dst := filepath.Join(dir, "trimmed.opus")
	if err := ToOpus(ctx, src, dst); err != nil {
		t.Fatalf("ToOpus: %v", err)
	}
	dstDur, err := Duration(ctx, dst)
	if err != nil {
		t.Fatalf("Duration(dst): %v", err)
	}
	if dstDur > 1900*time.Millisecond {
		t.Errorf("duration = %v, want the trailing silence cut back to ~1.5s", dstDur)
	}
	if dstDur < 1300*time.Millisecond {
		t.Errorf("duration = %v, want the tone kept", dstDur)
	}
}

// The key/value lines below are what `ffmpeg -progress pipe:1` writes, taken
// from ffmpeg 9.0.1 transcoding a 2 second file: a block per stats period, the
// same position under both time names, and progress=end closing the run.
func TestParseProgressLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		line  string
		value time.Duration
		end   bool
		ok    bool
	}{
		{
			name:  "microseconds",
			line:  "out_time_us=1653500",
			value: 1653500 * time.Microsecond,
			ok:    true,
		},
		{
			name:  "the ms name carries microseconds too",
			line:  "out_time_ms=1653500",
			value: 1653500 * time.Microsecond,
			ok:    true,
		},
		{
			name: "a whole second",
			line: "out_time_us=2000000",
			// ffmpeg prints the same number for out_time_ms, which is why both
			// names read as microseconds here.
			value: 2 * time.Second,
			ok:    true,
		},
		{name: "the end of the run", line: "progress=end", end: true, ok: true},
		{name: "still going", line: "progress=continue", ok: true},
		{name: "bytes written is not a position", line: "total_size=53551", ok: false},
		{name: "speed is not a position", line: "speed= 282x", ok: false},
		{name: "a line with no value", line: "not a key value line", ok: false},
		{name: "a negative position is refused", line: "out_time_us=-1", ok: false},
		{name: "an empty line", line: "", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, end, ok := parseProgressLine(tc.line)
			if ok != tc.ok || end != tc.end {
				t.Fatalf("parseProgressLine(%q) = %v, %v, %v; want end %v, ok %v",
					tc.line, value, end, ok, tc.end, tc.ok)
			}
			if ok && value != tc.value {
				t.Errorf("value = %v, want %v", value, tc.value)
			}
		})
	}
}

// A conversion reports its own position while it runs: the fraction only ever
// climbs, and it ends at 1 rather than stopping wherever the trim stopped
// ffmpeg.
func TestToOpusWithProgressClimbsToComplete(t *testing.T) {
	dir := t.TempDir()
	if len(Missing()) > 0 {
		t.Skipf("ffmpeg/ffprobe unavailable: %v", Missing())
	}
	ctx := context.Background()

	// A tone with trailing silence to cut, so the transcode stops short of the
	// source's end: that is exactly when the closing reading has to be the
	// whole length rather than the last position ffmpeg wrote.
	src := filepath.Join(dir, "padded.opus")
	if _, err := run(ctx, ffmpegName,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1.5",
		"-af", "apad", "-t", "3.5",
		"-c:a", "libopus", "-f", "opus", src); err != nil {
		t.Fatalf("render padded tone: %v", err)
	}

	var fractions []float64
	dst := filepath.Join(dir, "progress.opus")
	if err := ToOpusWithProgress(ctx, src, dst, func(done, total time.Duration) {
		if total <= 0 {
			t.Errorf("total = %v, want the source's length", total)
			return
		}
		fractions = append(fractions, float64(done.Milliseconds())/float64(total.Milliseconds()))
	}); err != nil {
		t.Fatalf("ToOpusWithProgress: %v", err)
	}

	if len(fractions) == 0 {
		t.Fatal("the conversion reported nothing")
	}
	for i := 1; i < len(fractions); i++ {
		if fractions[i] < fractions[i-1] {
			t.Fatalf("progress went backwards: %v", fractions)
		}
	}
	if last := fractions[len(fractions)-1]; last != 1 {
		t.Errorf("progress ended at %v, want 1", last)
	}
	if srcDur, err := Duration(ctx, src); err != nil || srcDur < 3400*time.Millisecond {
		t.Fatalf("fixture duration = %v (err %v), want ~3.5s", srcDur, err)
	} else if dstDur, err := Duration(ctx, dst); err != nil {
		t.Fatalf("Duration(dst): %v", err)
	} else if srcDur-dstDur < time.Second {
		t.Errorf("stored %v of a %v source, want the trailing silence cut", dstDur, srcDur)
	}
}
