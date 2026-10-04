// Package ffmpeg wraps the ffmpeg and ffprobe binaries: probing and the
// opus transcode every stored rendition goes through.
package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Executable names; overridable for tests or bundled copies.
var (
	ffmpegName  = "ffmpeg"
	ffprobeName = "ffprobe"
)

// SetBinaries overrides the ffmpeg and ffprobe executables.
func SetBinaries(ffmpeg, ffprobe string) {
	ffmpegName = ffmpeg
	ffprobeName = ffprobe
}

// Missing lists required binaries that are not on PATH.
func Missing() []string {
	var missing []string
	for _, name := range []string{ffmpegName, ffprobeName} {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// Duration probes the container duration of path.
func Duration(ctx context.Context, path string) (time.Duration, error) {
	out, err := run(ctx, ffprobeName,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path)
	if err != nil {
		return 0, err
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe: duration %q of %s: %w", strings.TrimSpace(out), path, err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// AudioCodec returns the codec name of the first audio stream.
func AudioCodec(ctx context.Context, path string) (string, error) {
	out, err := run(ctx, ffprobeName,
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path)
	if err != nil {
		return "", err
	}
	codec := strings.TrimSpace(out)
	if codec == "" {
		return "", fmt.Errorf("ffprobe: no audio stream in %s", path)
	}
	return codec, nil
}

// ToOpus writes dst as an Ogg/Opus file. Opus input is remuxed bit-exactly,
// anything else is encoded. A file that ends in silence is cut where the sound
// stops, so the stored length is the length of the music.
func ToOpus(ctx context.Context, src, dst string) error {
	return TrimTo(ctx, src, dst, TrailingSilence(ctx, src))
}

// TrailingSilence reports where a file's sound stops, or zero when there is
// nothing worth cutting.
//
// A download often ends in a second or two of silence. Kept, it becomes a
// second or two of the room sitting in silence before the next song: the room
// plays to the end of the file, and the file ends where the silence ends.
func TrailingSilence(ctx context.Context, src string) time.Duration {
	duration, err := Duration(ctx, src)
	if err != nil {
		return 0
	}
	end, err := AudibleEnd(ctx, src, duration)
	if err != nil || end <= 0 || duration-end < trailingSilenceFloor {
		return 0
	}
	return end
}

// TrimTo writes src as an Ogg/Opus file, stopping at end when it is positive.
// Opus input is copied bit-exactly; anything else is encoded, and either way
// every sample that is kept is the sample that was there.
func TrimTo(ctx context.Context, src, dst string, end time.Duration) error {
	codec, err := AudioCodec(ctx, src)
	if err != nil {
		return err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src, "-vn"}
	if strings.EqualFold(codec, "opus") {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "libopus", "-b:a", "192k")
	}
	if end > 0 {
		args = append(args, "-t", strconv.FormatFloat(end.Seconds(), 'f', 3, 64))
	}
	args = append(args, "-f", "opus", dst)
	if _, err := run(ctx, ffmpegName, args...); err != nil {
		return err
	}
	return nil
}

// trailingSilenceFloor is how much silence at the end of a file is worth
// cutting. Anything shorter is the tail of a fade, not a gap.
const trailingSilenceFloor = 300 * time.Millisecond

var (
	silenceStartPattern = regexp.MustCompile(`silence_start: ([0-9.]+)`)
	silenceEndPattern   = regexp.MustCompile(`silence_end: ([0-9.]+)`)
)

// AudibleEnd reports where a file's sound stops, or zero when it does not end
// in silence. Downloads routinely carry a second or two of it, and a room that
// plays to the container's end sits in that silence before the next song.
func AudibleEnd(ctx context.Context, path string, duration time.Duration) (time.Duration, error) {
	out, err := runCombined(ctx, ffmpegName,
		"-hide_banner", "-nostats",
		"-i", path,
		"-af", "silencedetect=noise=-50dB:d=0.3",
		"-f", "null", "-")
	if err != nil {
		return 0, err
	}
	starts := silenceStartPattern.FindAllStringSubmatch(out, -1)
	ends := silenceEndPattern.FindAllStringSubmatch(out, -1)
	if len(starts) == 0 || len(ends) == 0 {
		return 0, nil
	}
	// Only a silence that runs to the end of the file is trailing silence; one
	// in the middle is part of the music.
	lastStart, err := strconv.ParseFloat(starts[len(starts)-1][1], 64)
	if err != nil {
		return 0, nil
	}
	lastEnd, err := strconv.ParseFloat(ends[len(ends)-1][1], 64)
	if err != nil {
		return 0, nil
	}
	if duration.Seconds()-lastEnd > 0.15 {
		return 0, nil
	}
	return time.Duration(lastStart * float64(time.Second)), nil
}

// runCombined runs a command and returns everything it said, whichever stream
// said it: silencedetect reports on stderr.
func runCombined(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w", bin, err)
	}
	return out.String(), nil
}

func run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("%s: %w", bin, err)
		}
		return "", fmt.Errorf("%s: %w: %s", bin, err, msg)
	}
	return stdout.String(), nil
}
