package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ytdlpWaitDelay is how long a yt-dlp that has been told to stop - or has
// exited - is given to let go of its output before it is taken away.
const ytdlpWaitDelay = 5 * time.Second

// YtdlpFetch runs yt-dlp with args, which write one audio file into dir, and
// returns that file. Every progress line it prints goes to progress. label
// names the provider in the one error that is about the directory rather than
// the tool ("ytmusic", "youtube").
//
// This is the download both YouTube providers share, and the reason it lives in
// one place is how it stops. A download is cancelled all day long - every play
// takes the pipe from the fetches behind it - and killing yt-dlp alone is not
// stopping it: for some formats it hands the transfer to an ffmpeg of its own,
// which goes on downloading with yt-dlp's stdout still open. A reader waiting
// for that stdout to close then waits for a process nobody asked for, and the
// variant's one download slot is held for as long as it runs. So the tool gets a
// process group of its own and the group is what is killed, and Wait is given a
// bound on how long output may stay open after that.
func YtdlpFetch(ctx context.Context, bin, label, url, dir string, args []string, progress ProgressFunc) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	killGroupOnCancel(cmd)
	cmd.WaitDelay = ytdlpWaitDelay

	// stdout is a writer rather than a pipe this function reads: os/exec then
	// owns the copying, and WaitDelay is what bounds it. stderr is where a
	// failure is reported, so it is kept whole for the error below.
	lines := &lineWriter{line: NewYtdlpProgressReporter(progress).Line}
	var stderr bytes.Buffer
	cmd.Stdout = lines
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	lines.flush()
	return FirstAudioFile(label, dir)
}

// FirstAudioFile returns the first real media file yt-dlp wrote into dir,
// skipping the thumbnails and partial downloads it can leave behind.
func FirstAudioFile(label, dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("%s download: read output dir: %w", label, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case "", ".part", ".ytdl", ".webp", ".jpg", ".jpeg", ".png":
			continue
		}
		return filepath.Join(dir, e.Name()), nil
	}
	return "", fmt.Errorf("%s download: yt-dlp wrote no audio file into %s", label, dir)
}

// lineWriter hands what is written to it on, one line at a time. yt-dlp's
// progress arrives a line per update; a write is whatever the pipe had.
type lineWriter struct {
	line func(string)
	rest []byte
}

// maxProgressLine is the longest line kept while waiting for its end. A
// progress line is under a hundred bytes; anything this long is not one, and is
// dropped rather than held.
const maxProgressLine = 64 * 1024

func (w *lineWriter) Write(p []byte) (int, error) {
	w.rest = append(w.rest, p...)
	for {
		end := bytes.IndexByte(w.rest, '\n')
		if end < 0 {
			break
		}
		w.line(strings.TrimRight(string(w.rest[:end]), "\r"))
		w.rest = w.rest[end+1:]
	}
	if len(w.rest) > maxProgressLine {
		w.rest = w.rest[:0]
	}
	return len(p), nil
}

// flush hands on a last line that had no newline after it.
func (w *lineWriter) flush() {
	if len(w.rest) > 0 {
		w.line(strings.TrimRight(string(w.rest), "\r"))
		w.rest = nil
	}
}
