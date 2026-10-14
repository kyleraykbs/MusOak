// Package youtube is the plain YouTube provider: search and download through
// yt-dlp alone, with no metadata service in between.
package youtube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
	"github.com/kyleraykbs/MusOak/internal/provider"
)

// Name is the provider id used everywhere (config, database, API).
const Name = "youtube"

const (
	defaultSearchLimit = 20
	// playerClient is YouTube's web embedded client. It is the fallback, not the
	// first thing tried: it is routinely slower - measured at twice the wall
	// time for the same track on this machine - and only earns its place when
	// yt-dlp's own default clients are refused.
	playerClient = "youtube:player_client=web_embedded"
)

// Provider implements provider.Provider on top of yt-dlp.
type Provider struct {
	logger *slog.Logger

	// ytdlp is the executable name, overridable in tests.
	ytdlp string

	// How yt-dlp proves it is not a bot: cookies from a signed-in browser, or
	// from a cookies.txt exported out of one. YouTube asks for this more and
	// more, and a download that cannot answer fails with "Sign in to confirm
	// you're not a bot".
	cookiesFromBrowser string
	cookiesFile        string
}

// Options are the provider's tunables, so it does not have to know the shape of
// the server's configuration.
type Options struct {
	CookiesFromBrowser string
	CookiesFile        string
}

// New returns the provider using yt-dlp from PATH.
func New(logger *slog.Logger, opts Options) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		logger:             logger,
		ytdlp:              "yt-dlp",
		cookiesFromBrowser: strings.TrimSpace(opts.CookiesFromBrowser),
		cookiesFile:        strings.TrimSpace(opts.CookiesFile),
	}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return Name }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Caps {
	return provider.Caps{
		Search:   true,
		Download: true,
		Stream:   true,
	}
}

// MissingDeps lists the external binaries this provider needs.
func (p *Provider) MissingDeps() []string {
	var missing []string
	if _, err := exec.LookPath(p.ytdlp); err != nil {
		missing = append(missing, p.ytdlp)
	}
	return append(missing, ffmpeg.Missing()...)
}

// Search runs one yt-dlp search and parses its newline-delimited JSON output.
// The ytsearchN: pseudo-URL bounds the search to the caller's limit.
func (p *Provider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	limit := limitOr(opts.Limit, defaultSearchLimit)

	args := []string{
		"--dump-json",
		"--flat-playlist",
		"--no-warnings",
		"--no-progress",
	}
	args = p.appendCookies(args)
	args = append(args, fmt.Sprintf("ytsearch%d:%s", limit, q))

	cmd := exec.CommandContext(ctx, p.ytdlp, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("youtube search %q: %w: %s", q, err, strings.TrimSpace(stderr.String()))
	}

	tracks := make([]provider.Track, 0, limit)
	scanner := bufio.NewScanner(&stdout)
	// One JSON line carries the whole entry, descriptions included.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var hit ytdlpEntry
		if err := json.Unmarshal(line, &hit); err != nil {
			return nil, fmt.Errorf("youtube search %q: parse yt-dlp output: %w", q, err)
		}
		track, ok := hit.track()
		if !ok {
			continue
		}
		tracks = append(tracks, track)
		if len(tracks) >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("youtube search %q: read yt-dlp output: %w", q, err)
	}
	return tracks, nil
}

// Download fetches the best available audio and leaves an Ogg/Opus file at
// destPath. The parent directory must exist.
func (p *Provider) Download(ctx context.Context, providerTrackID, destPath string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(destPath), ".youtube-*")
	if err != nil {
		return fmt.Errorf("youtube download: temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	url := "https://www.youtube.com/watch?v=" + providerTrackID

	src, err := p.fetch(ctx, url, tmpDir)
	if err != nil {
		p.logger.Warn("youtube download failed with yt-dlp defaults, retrying with the web_embedded client",
			"provider_track_id", providerTrackID, "error", err)
		if src, err = p.fetch(ctx, url, tmpDir, "--extractor-args", playerClient); err != nil {
			return err
		}
	}

	if err := ffmpeg.ToOpus(ctx, src, destPath); err != nil {
		return fmt.Errorf("youtube download %s: transcode: %w", providerTrackID, err)
	}
	return nil
}

// streamExtWait bounds how long Stream waits for yt-dlp to report the container
// it chose. The report is written before any audio, so it either arrives during
// startup or the fetch never got that far.
const streamExtWait = 30 * time.Second

// streamExtPoll is how often Stream re-checks for that report.
const streamExtPoll = 25 * time.Millisecond

// Stream implements provider.Streamer: it runs yt-dlp exactly as Download does,
// but sends the audio to a pipe and hands that to the caller instead of writing
// a file. The bytes arrive as they are fetched, never the whole track buffered.
//
// ctx is the listener's; cancelling it kills the fetch, which belongs to that
// one listener and must not disturb the download running alongside it.
func (p *Provider) Stream(ctx context.Context, providerTrackID string) (io.ReadCloser, string, error) {
	url := "https://www.youtube.com/watch?v=" + providerTrackID

	body, mimeType, err := p.stream(ctx, url)
	if err != nil {
		p.logger.Warn("youtube stream failed with yt-dlp defaults, retrying with the web_embedded client",
			"provider_track_id", providerTrackID, "error", err)
		if body, mimeType, err = p.stream(ctx, url, "--extractor-args", playerClient); err != nil {
			return nil, "", err
		}
	}
	return body, mimeType, nil
}

// stream starts one yt-dlp fetch with the flags a download uses, but -o - sends
// the audio to stdout and --print-to-file records the container yt-dlp chose.
// That report arrives before any audio, so waiting for it both learns the MIME
// type and proves the fetch really started; until then a failure is still an
// ordinary error rather than a truncated stream.
func (p *Provider) stream(ctx context.Context, url string, extra ...string) (io.ReadCloser, string, error) {
	extFile, err := os.CreateTemp("", "musoak-youtube-ext-*")
	if err != nil {
		return nil, "", fmt.Errorf("youtube stream %s: ext file: %w", url, err)
	}
	extPath := extFile.Name()
	extFile.Close()
	defer os.Remove(extPath)

	args := p.ytdlpArgs(extra...)
	args = append(args, "-o", "-", "--print-to-file", "%(ext)s", extPath, url)

	cmd := exec.CommandContext(ctx, p.ytdlp, args...)
	// An explicit pipe, rather than StdoutPipe, keeps the audio out of cmd's
	// bookkeeping: Wait can run as soon as the fetch starts without racing the
	// reader for the tail of the stream.
	audioReader, audioWriter, err := os.Pipe()
	if err != nil {
		return nil, "", fmt.Errorf("youtube stream %s: pipe: %w", url, err)
	}
	cmd.Stdout = audioWriter
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		audioReader.Close()
		audioWriter.Close()
		return nil, "", fmt.Errorf("yt-dlp %s: %w", url, err)
	}
	// The child holds its own copy of the write end; closing ours is what turns
	// its exit into EOF for the reader.
	audioWriter.Close()

	proc := &streamProcess{audio: audioReader, cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(proc.done)
	}()

	mimeType, err := waitForContainer(proc, extPath)
	if err != nil {
		proc.Close()
		return nil, "", fmt.Errorf("youtube stream %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return proc, mimeType, nil
}

// waitForContainer reads the container yt-dlp reported, polling until the file
// is complete or the fetch ends. yt-dlp writes it before any audio, so it
// appears almost immediately; if it never does, the fetch is reported rather
// than handed on as an empty stream.
func waitForContainer(proc *streamProcess, path string) (string, error) {
	deadline := time.Now().Add(streamExtWait)
	for {
		if mimeType, done, err := readContainer(path); done {
			return mimeType, err
		}
		select {
		case <-proc.done:
			// A quick fetch can land its report and exit between polls, so look
			// once more before giving up.
			if mimeType, done, err := readContainer(path); done {
				return mimeType, err
			}
			return "", fmt.Errorf("yt-dlp exited before reporting the audio container")
		case <-time.After(streamExtPoll):
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for yt-dlp to report the audio container", streamExtWait)
		}
	}
}

// readContainer reads yt-dlp's container report once it is complete: the value
// is written with a trailing newline, which is what tells a finished report from
// a partially written file. A complete but unsupported container is reported as
// an error, never mapped to a guess.
func readContainer(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return "", false, nil
	}
	mimeType, err := provider.MIMEForExt(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", true, err
	}
	return mimeType, true, nil
}

// streamProcess owns one yt-dlp fetch whose audio arrives on audio. Reading
// hands the audio on as it comes and reaches io.EOF when it ends; Close ends
// the fetch, killing the process if it is still running and waiting for it to
// be reaped.
type streamProcess struct {
	audio *os.File
	cmd   *exec.Cmd
	done  chan struct{}
}

// Read hands on the audio as it arrives.
func (s *streamProcess) Read(p []byte) (int, error) { return s.audio.Read(p) }

// Close ends the fetch. It is safe to call more than once, and after the audio
// has already reached io.EOF.
func (s *streamProcess) Close() error {
	s.audio.Close()
	if s.cmd.Process != nil {
		// Closing the pipe alone only ends yt-dlp at its next write; killing it
		// ends a stalled fetch now.
		_ = s.cmd.Process.Kill()
	}
	// Wait until the process is reaped; it already is when the audio reached EOF.
	<-s.done
	return nil
}

// ytdlpArgs is the argument prefix every yt-dlp invocation shares: one video at
// a time, quiet, the same audio format selection, and the cookie flags that
// answer YouTube's bot check. Download and Stream both build on it, so the
// audio a stream carries is the format a download would have produced.
func (p *Provider) ytdlpArgs(extra ...string) []string {
	args := []string{
		"--no-playlist",
		"--no-progress",
		"--quiet",
		"--no-warnings",
		"-f", "bestaudio",
	}
	args = p.appendCookies(args)
	return append(args, extra...)
}

// fetch runs yt-dlp into dir and returns the produced audio file.
func (p *Provider) fetch(ctx context.Context, url, dir string, extra ...string) (string, error) {
	args := p.ytdlpArgs(extra...)
	args = append(args, "-o", filepath.Join(dir, "%(id)s.%(ext)s"), url)

	cmd := exec.CommandContext(ctx, p.ytdlp, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return firstAudioFile(dir)
}

// appendCookies adds the configured cookie flags, if any, so a signed-in
// session can answer YouTube's bot check.
func (p *Provider) appendCookies(args []string) []string {
	if p.cookiesFromBrowser != "" {
		args = append(args, "--cookies-from-browser", p.cookiesFromBrowser)
	}
	if p.cookiesFile != "" {
		args = append(args, "--cookies", p.cookiesFile)
	}
	return args
}

func limitOr(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	return limit
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ytdlpEntry is one line of `yt-dlp --dump-json --flat-playlist` output.
type ytdlpEntry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Uploader string `json:"uploader"`
	Channel  string `json:"channel"`
	// Duration is in seconds and is absent or null for a live stream, so a
	// pointer keeps "unknown" distinct from a real zero.
	Duration *float64 `json:"duration"`
	// Thumbnail is the flat scalar form; Thumbnails is the list yt-dlp usually
	// gives, ordered smallest to largest.
	Thumbnail  string `json:"thumbnail"`
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
}

// track converts a yt-dlp entry into a provider track. Entries without an id
// or a title are not usable hits and are skipped.
func (e ytdlpEntry) track() (provider.Track, bool) {
	if e.ID == "" || e.Title == "" {
		return provider.Track{}, false
	}
	track := provider.Track{
		ProviderTrackID: e.ID,
		Title:           e.Title,
		ArtworkURL:      e.artworkURL(),
	}
	if artist := firstNonEmpty(e.Uploader, e.Channel); artist != "" {
		track.Artists = []string{artist}
	}
	if e.Duration != nil {
		track.DurationMs = int64(*e.Duration * 1000)
	}
	return track, true
}

// artworkURL picks the best thumbnail on offer: the flat scalar if present,
// otherwise the largest of the list.
func (e ytdlpEntry) artworkURL() string {
	if e.Thumbnail != "" {
		return e.Thumbnail
	}
	for i := len(e.Thumbnails) - 1; i >= 0; i-- {
		if e.Thumbnails[i].URL != "" {
			return e.Thumbnails[i].URL
		}
	}
	return ""
}

// firstAudioFile returns the first real media file yt-dlp wrote, skipping the
// thumbnails and partial downloads it can leave behind.
func firstAudioFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("youtube download: read output dir: %w", err)
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
	return "", fmt.Errorf("youtube download: yt-dlp wrote no audio file into %s", dir)
}
