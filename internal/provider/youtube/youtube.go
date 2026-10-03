// Package youtube is the plain YouTube provider: search and download through
// yt-dlp alone, with no metadata service in between.
package youtube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"codeberg.org/kyleraykbs/musoak/internal/ffmpeg"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
)

// Name is the provider id used everywhere (config, database, API).
const Name = "youtube"

const (
	defaultSearchLimit = 20
	// playerClient is YouTube's web embedded client. The default clients are
	// routinely blocked with HTTP 403, so this is attempted first and plain
	// defaults are the fallback.
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

	src, err := p.fetch(ctx, url, tmpDir, "--extractor-args", playerClient)
	if err != nil {
		p.logger.Warn("youtube download failed with the web_embedded client, retrying with yt-dlp defaults",
			"provider_track_id", providerTrackID, "error", err)
		if src, err = p.fetch(ctx, url, tmpDir); err != nil {
			return err
		}
	}

	if err := ffmpeg.ToOpus(ctx, src, destPath); err != nil {
		return fmt.Errorf("youtube download %s: transcode: %w", providerTrackID, err)
	}
	return nil
}

// fetch runs yt-dlp into dir and returns the produced audio file.
func (p *Provider) fetch(ctx context.Context, url, dir string, extra ...string) (string, error) {
	args := []string{
		"--no-playlist",
		"--no-progress",
		"--quiet",
		"--no-warnings",
		"-f", "bestaudio",
		"-o", filepath.Join(dir, "%(id)s.%(ext)s"),
	}
	args = p.appendCookies(args)
	args = append(args, extra...)
	args = append(args, url)

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
