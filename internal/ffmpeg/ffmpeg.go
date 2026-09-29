// Package ffmpeg wraps the ffmpeg and ffprobe binaries: probing and the
// opus transcode every stored rendition goes through.
package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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
// anything else is encoded.
func ToOpus(ctx context.Context, src, dst string) error {
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
	args = append(args, "-f", "opus", dst)
	if _, err := run(ctx, ffmpegName, args...); err != nil {
		return err
	}
	return nil
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
