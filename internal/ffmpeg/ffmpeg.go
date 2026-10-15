// Package ffmpeg wraps the ffmpeg and ffprobe binaries: probing and the
// opus transcode every stored rendition goes through.
package ffmpeg

import (
	"bufio"
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
	return trimTo(ctx, src, dst, TrailingSilence(ctx, src), 0, nil)
}

// ProgressFunc receives a transcode's own position while it runs: done out of
// total, both millisecond counts. Total <= 0 means the source's length is not
// known, which is reported rather than guessed. Nil is allowed.
//
// It is not a provider.ProgressFunc on purpose: this package is below the
// provider boundary, and the provider package imports ffmpeg for its test
// build, so reaching back up would be an import cycle under that build tag. The
// caller names the stage: a conversion here is provider.StageConvert.
type ProgressFunc func(done, total time.Duration)

// ToOpusWithProgress is ToOpus with the transcode's own progress: ffmpeg is
// asked for its `-progress` stream and every reading in it is handed to
// progress, so a client's bar moves while a file is being written instead of
// sitting still until it is finished.
//
// The total is the source's length, probed once here rather than again for the
// trim. ffmpeg reaches the end of the output before the end of what it was
// given whenever trailing silence is cut, so its closing reading - the one that
// says it is done - reports the whole length: the stage is over, and a bar left
// at 98% would be lying about that.
func ToOpusWithProgress(ctx context.Context, src, dst string, progress ProgressFunc) error {
	length, err := Duration(ctx, src)
	if err != nil {
		// An unreadable length is not a reason to refuse the file: the
		// conversion reports what it can, and TrimTo reports the failure if the
		// source really cannot be read.
		length = 0
	}
	return trimTo(ctx, src, dst, trailingSilence(ctx, src, length), length, progress)
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
	return trailingSilence(ctx, src, duration)
}

// trailingSilence is TrailingSilence with the length already probed.
func trailingSilence(ctx context.Context, src string, duration time.Duration) time.Duration {
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
	return trimTo(ctx, src, dst, end, 0, nil)
}

// trimTo is the one transcode: total is the source's length, for the progress
// reading to count against, and progress is nil when nobody is listening.
func trimTo(ctx context.Context, src, dst string, end, total time.Duration, progress ProgressFunc) error {
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
	if progress != nil {
		args = append(args, "-progress", "pipe:1")
	}
	args = append(args, "-f", "opus", dst)
	if progress == nil {
		if _, err := run(ctx, ffmpegName, args...); err != nil {
			return err
		}
		return nil
	}
	return runWithProgress(ctx, ffmpegName, args, total, progress)
}

// trailingSilenceFloor is how much silence at the end of a file is worth
// cutting. Anything shorter is the tail of a fade, not a gap.
const trailingSilenceFloor = 300 * time.Millisecond

// execTimeout bounds one ffmpeg or ffprobe run. Every call goes through run,
// runCombined or runWithProgress, and any of them can hang: a probe on a file
// nobody closes, a transcode on a pipe that stops feeding. The caller's own
// deadline cannot be the bound, because the media fetches probe and trim on a
// context detached from the request - a client hanging up must not abandon work
// everybody shares - and a detached context has no deadline at all. This is the
// ceiling on every run, and it is longer than any song takes to probe or encode.
const execTimeout = 15 * time.Minute

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

// runWithProgress runs one ffmpeg pass and reads the key/value stream that
// -progress writes to stdout, reporting the transcode's position as it arrives.
//
// ffmpeg writes a block of keys once per stats period - half a second by
// default - so a conversion reports about twice a second. A reading that repeats
// the last one is dropped, which also collapses the two names ffmpeg gives the
// same position in each block.
func runWithProgress(ctx context.Context, bin string, args []string, total time.Duration, progress ProgressFunc) error {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: %w", bin, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", bin, err)
	}

	var last int64 = -1
	report := func(done time.Duration) {
		ms := done.Milliseconds()
		if ms == last {
			return
		}
		last = ms
		progress(done, total)
	}

	var done time.Duration
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		value, end, ok := parseProgressLine(scanner.Text())
		if !ok {
			continue
		}
		if value > 0 {
			done = value
		}
		if end {
			// ffmpeg is finished with the output. A trim can stop it short of
			// the source's end, and a bar left at 98% would be lying about a
			// stage that is over, so the closing reading is the whole length
			// when one is known.
			if total > 0 {
				done = total
			}
		}
		report(done)
	}
	scanErr := scanner.Err()
	if waitErr := cmd.Wait(); waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("%s: %w", bin, waitErr)
		}
		return fmt.Errorf("%s: %w: %s", bin, waitErr, msg)
	}
	if scanErr != nil {
		return fmt.Errorf("%s: read progress: %w", bin, scanErr)
	}
	return nil
}

// parseProgressLine reads one line of ffmpeg's -progress stream, which is a
// stream of key=value lines. value is the transcode's position when the line
// carries one; end is true for the progress=end line that closes a run; ok is
// false for keys this package has no use for.
//
// out_time_us and out_time_ms both hold microseconds: ffmpeg has printed the
// same number under both names for so long that the "ms" one is a misnomer
// everyone reads as microseconds. total_size is written too, but it counts bytes
// of output while the position here is counted in milliseconds, so mixing the
// two would be a fraction of nothing.
func parseProgressLine(line string) (value time.Duration, end bool, ok bool) {
	key, raw, found := strings.Cut(strings.TrimSpace(line), "=")
	if !found {
		return 0, false, false
	}
	switch strings.TrimSpace(key) {
	case "out_time_us", "out_time_ms":
		micros, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || micros < 0 {
			return 0, false, false
		}
		return time.Duration(micros) * time.Microsecond, false, true
	case "progress":
		return 0, strings.TrimSpace(raw) == "end", true
	}
	return 0, false, false
}

// runCombined runs a command and returns everything it said, whichever stream
// said it: silencedetect reports on stderr.
func runCombined(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
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
