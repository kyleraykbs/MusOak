// Package ytmusic is the YouTube Music provider: search through ytmusicapi,
// audio through yt-dlp.
package ytmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/internal/ffmpeg"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/support"
)

// Name is the provider id used everywhere (config, database, API).
const Name = "ytmusic"

const (
	defaultSearchLimit = 20
	// playerClient is YouTube's web embedded client. The default clients are
	// routinely blocked with HTTP 403, so this is attempted first and plain
	// defaults are the fallback.
	playerClient = "youtube:player_client=web_embedded"
)

// Provider implements provider.Provider on top of ytmusicapi and yt-dlp.
type Provider struct {
	logger *slog.Logger

	// Executable names, overridable in tests.
	python string
	ytdlp  string
}

// New returns the provider using the binaries from PATH.
func New(logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{logger: logger, python: "python3", ytdlp: "yt-dlp"}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return Name }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true}
}

// MissingDeps lists the external binaries this provider needs.
func (p *Provider) MissingDeps() []string {
	var missing []string
	for _, bin := range []string{p.python, p.ytdlp} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	return append(missing, ffmpeg.Missing()...)
}

// Search runs the embedded ytmusicapi helper and parses its JSON output.
func (p *Provider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	script, err := support.YTMusicSearchScript()
	if err != nil {
		return nil, fmt.Errorf("ytmusic: %w", err)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	cmd := exec.CommandContext(ctx, p.python, "-", q, strconv.Itoa(limit))
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ytmusic search %q: %w: %s", q, err, strings.TrimSpace(stderr.String()))
	}

	var hits []searchHit
	if err := json.Unmarshal(stdout.Bytes(), &hits); err != nil {
		return nil, fmt.Errorf("ytmusic search %q: parse helper output: %w", q, err)
	}

	tracks := make([]provider.Track, 0, len(hits))
	for _, h := range hits {
		if h.ID == "" {
			continue
		}
		tracks = append(tracks, provider.Track{
			ProviderTrackID: h.ID,
			Title:           h.Title,
			Artists:         h.Artists,
			Album:           h.Album,
			DurationMs:      h.DurationMs,
		})
	}
	return tracks, nil
}

type searchHit struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Album      string   `json:"album"`
	DurationMs int64    `json:"durationMs"`
}

// Download fetches the best available audio and leaves an Ogg/Opus file at
// destPath. The parent directory must exist.
func (p *Provider) Download(ctx context.Context, providerTrackID, destPath string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(destPath), ".ytmusic-*")
	if err != nil {
		return fmt.Errorf("ytmusic download: temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	url := "https://music.youtube.com/watch?v=" + providerTrackID

	src, err := p.fetch(ctx, url, tmpDir, "--extractor-args", playerClient)
	if err != nil {
		p.logger.Warn("ytmusic download failed with the web_embedded client, retrying with yt-dlp defaults",
			"provider_track_id", providerTrackID, "error", err)
		if src, err = p.fetch(ctx, url, tmpDir); err != nil {
			return err
		}
	}

	if err := ffmpeg.ToOpus(ctx, src, destPath); err != nil {
		return fmt.Errorf("ytmusic download %s: transcode: %w", providerTrackID, err)
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

// firstAudioFile returns the first real media file yt-dlp wrote, skipping the
// thumbnails and partial downloads it can leave behind.
func firstAudioFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("ytmusic download: read output dir: %w", err)
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
	return "", fmt.Errorf("ytmusic download: yt-dlp wrote no audio file into %s", dir)
}
