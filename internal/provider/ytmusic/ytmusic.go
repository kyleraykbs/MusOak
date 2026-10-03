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

	"codeberg.org/kyleraykbs/musoak/internal/ffmpeg"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/support"
)

// Name is the provider id used everywhere (config, database, API).
const Name = "ytmusic"

const (
	defaultSearchLimit = 20
	// defaultRadioLimit is how many tracks a station contributes when the
	// caller does not say.
	defaultRadioLimit = 25
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

// New returns the provider using the binaries from PATH.
func New(logger *slog.Logger, opts Options) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		logger:             logger,
		python:             "python3",
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
		Search:        true,
		Download:      true,
		SearchAlbums:  true,
		SearchArtists: true,
		Radio:         true,
		Playlists:     true,
	}
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
	hits, err := p.songSearch(ctx, "songs", q, limitOr(opts.Limit, defaultSearchLimit))
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// SearchAlbums implements provider.AlbumSearcher.
func (p *Provider) SearchAlbums(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Album, error) {
	out, err := p.run(ctx, "search", "albums", q, strconv.Itoa(limitOr(opts.Limit, defaultSearchLimit)))
	if err != nil {
		return nil, err
	}
	var hits []albumHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("ytmusic album search %q: parse helper output: %w", q, err)
	}
	albums := make([]provider.Album, 0, len(hits))
	for _, hit := range hits {
		if hit.ID == "" {
			continue
		}
		albums = append(albums, provider.Album{
			ProviderAlbumID: hit.ID,
			Title:           hit.Title,
			Artists:         hit.Artists,
			Year:            hit.Year,
			TrackCount:      hit.TrackCount,
			ArtworkURL:      hit.ArtworkURL,
		})
	}
	return albums, nil
}

// SearchArtists implements provider.ArtistSearcher.
func (p *Provider) SearchArtists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Artist, error) {
	out, err := p.run(ctx, "search", "artists", q, strconv.Itoa(limitOr(opts.Limit, defaultSearchLimit)))
	if err != nil {
		return nil, err
	}
	var hits []artistHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("ytmusic artist search %q: parse helper output: %w", q, err)
	}
	artists := make([]provider.Artist, 0, len(hits))
	for _, hit := range hits {
		if hit.ID == "" || hit.Name == "" {
			continue
		}
		artists = append(artists, provider.Artist{
			ProviderArtistID: hit.ID,
			Name:             hit.Name,
			ArtworkURL:       hit.ArtworkURL,
		})
	}
	return artists, nil
}

// SearchPlaylists implements provider.PlaylistSearcher.
func (p *Provider) SearchPlaylists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Playlist, error) {
	out, err := p.run(ctx, "playlists", q, strconv.Itoa(limitOr(opts.Limit, defaultSearchLimit)))
	if err != nil {
		return nil, err
	}
	var hits []playlistHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("ytmusic playlist search %q: parse helper output: %w", q, err)
	}
	playlists := make([]provider.Playlist, 0, len(hits))
	for _, hit := range hits {
		if hit.ID == "" || hit.Title == "" {
			continue
		}
		playlists = append(playlists, provider.Playlist{
			ProviderPlaylistID: hit.ID,
			Title:              hit.Title,
			Owner:              hit.Owner,
			Description:        hit.Description,
			TrackCount:         hit.TrackCount,
			ArtworkURL:         hit.ArtworkURL,
		})
	}
	return playlists, nil
}

// Playlist implements provider.PlaylistSearcher: one playlist's tracks, in the
// order the playlist has them.
func (p *Provider) Playlist(ctx context.Context, providerPlaylistID string) (*provider.PlaylistDetail, error) {
	out, err := p.run(ctx, "playlist", providerPlaylistID)
	if err != nil {
		return nil, err
	}
	var hit playlistDetailHit
	if err := json.Unmarshal(out, &hit); err != nil {
		return nil, fmt.Errorf("ytmusic playlist %s: parse helper output: %w", providerPlaylistID, err)
	}

	detail := &provider.PlaylistDetail{
		Playlist: provider.Playlist{
			ProviderPlaylistID: providerPlaylistID,
			Title:              hit.Title,
			Owner:              hit.Owner,
			Description:        hit.Description,
			TrackCount:         hit.TrackCount,
			ArtworkURL:         hit.ArtworkURL,
		},
		Tracks: make([]provider.Track, 0, len(hit.Tracks)),
	}
	for _, track := range hit.Tracks {
		if track.ID == "" {
			continue
		}
		detail.Tracks = append(detail.Tracks, provider.Track{
			ProviderTrackID: track.ID,
			Title:           track.Title,
			Artists:         track.Artists,
			Album:           track.Album,
			DurationMs:      track.DurationMs,
			ArtworkURL:      track.ArtworkURL,
		})
	}
	return detail, nil
}

// Album implements provider.AlbumSearcher: it fetches one album's tracklist.
func (p *Provider) Album(ctx context.Context, providerAlbumID string) (*provider.AlbumDetail, error) {
	out, err := p.run(ctx, "album", providerAlbumID)
	if err != nil {
		return nil, err
	}
	var hit albumDetailHit
	if err := json.Unmarshal(out, &hit); err != nil {
		return nil, fmt.Errorf("ytmusic album %s: parse helper output: %w", providerAlbumID, err)
	}

	detail := &provider.AlbumDetail{
		Album: provider.Album{
			ProviderAlbumID: providerAlbumID,
			Title:           hit.Title,
			Artists:         hit.Artists,
			Year:            hit.Year,
			TrackCount:      hit.TrackCount,
			ArtworkURL:      hit.ArtworkURL,
		},
		Tracks: make([]provider.Track, 0, len(hit.Tracks)),
	}
	for _, track := range hit.Tracks {
		if track.ID == "" {
			continue
		}
		detail.Tracks = append(detail.Tracks, provider.Track{
			ProviderTrackID: track.ID,
			Title:           track.Title,
			Artists:         track.Artists,
			Album:           firstNonEmpty(track.Album, hit.Title),
			DurationMs:      track.DurationMs,
			ArtworkURL:      firstNonEmpty(track.ArtworkURL, hit.ArtworkURL),
		})
	}
	return detail, nil
}

// ArtistAlbums implements provider.ArtistSearcher. YouTube Music has no plain
// "artist albums" call, so the artist's song hit is used to reach the albums
// the search endpoint exposes for that artist name.
func (p *Provider) ArtistAlbums(ctx context.Context, providerArtistID string) ([]provider.Album, error) {
	// The helper searches by name; the id is what the caller holds, so ask the
	// provider for the artist's own page and fall back to a name search.
	out, err := p.run(ctx, "artist", providerArtistID)
	if err != nil {
		return nil, err
	}
	var hit struct {
		Name   string     `json:"name"`
		Albums []albumHit `json:"albums"`
	}
	if err := json.Unmarshal(out, &hit); err != nil {
		return nil, fmt.Errorf("ytmusic artist %s: parse helper output: %w", providerArtistID, err)
	}
	albums := make([]provider.Album, 0, len(hit.Albums))
	for _, album := range hit.Albums {
		if album.ID == "" {
			continue
		}
		artists := album.Artists
		if len(artists) == 0 && hit.Name != "" {
			artists = []string{hit.Name}
		}
		albums = append(albums, provider.Album{
			ProviderAlbumID: album.ID,
			Title:           album.Title,
			Artists:         artists,
			Year:            album.Year,
			TrackCount:      album.TrackCount,
			ArtworkURL:      album.ArtworkURL,
		})
	}
	return albums, nil
}

// Radio implements provider.RadioProvider with YouTube Music's own station.
func (p *Provider) Radio(ctx context.Context, seed provider.Track, limit int) ([]provider.Track, error) {
	if seed.ProviderTrackID == "" {
		return nil, fmt.Errorf("ytmusic radio: the seed has no ytmusic rendition")
	}
	hits, err := p.songSearch(ctx, "radio", seed.ProviderTrackID, limitOr(limit, defaultRadioLimit))
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// songSearch runs a helper command that returns song-shaped JSON.
func (p *Provider) songSearch(ctx context.Context, kind, argument string, limit int) ([]provider.Track, error) {
	args := []string{"search", kind, argument}
	if kind == "radio" {
		args = []string{"radio", argument, strconv.Itoa(limit)}
	} else {
		args = append(args, strconv.Itoa(limit))
	}

	out, err := p.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var hits []searchHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("ytmusic %s %q: parse helper output: %w", kind, argument, err)
	}

	// YT Music mixes songs and music videos in the same lists - its radio
	// especially - and a video's audio is the video: intro, skits and all. Songs
	// win wherever there is one, and videos are kept only when they are all the
	// provider offered.
	songs := make([]provider.Track, 0, len(hits))
	videos := make([]provider.Track, 0)
	for _, hit := range hits {
		if hit.ID == "" {
			continue
		}
		track := provider.Track{
			ProviderTrackID: hit.ID,
			Title:           hit.Title,
			Artists:         hit.Artists,
			Album:           hit.Album,
			DurationMs:      hit.DurationMs,
			ArtworkURL:      hit.ArtworkURL,
			Video:           hit.Video,
		}
		if hit.Video {
			videos = append(videos, track)
			continue
		}
		songs = append(songs, track)
	}
	if len(songs) == 0 {
		return videos, nil
	}
	return songs, nil
}

// run feeds the embedded helper to python3 on stdin and returns its stdout.
func (p *Provider) run(ctx context.Context, args ...string) ([]byte, error) {
	script, err := support.YTMusicScript()
	if err != nil {
		return nil, fmt.Errorf("ytmusic: %w", err)
	}
	cmd := exec.CommandContext(ctx, p.python, append([]string{"-"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ytmusic %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
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

type albumHit struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Year       string   `json:"year"`
	TrackCount int      `json:"trackCount"`
	ArtworkURL string   `json:"artworkUrl"`
}

type albumDetailHit struct {
	albumHit
	Tracks []searchHit `json:"tracks"`
}

type playlistHit struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Owner       string `json:"owner"`
	Description string `json:"description"`
	TrackCount  int    `json:"trackCount"`
	ArtworkURL  string `json:"artworkUrl"`
}

type playlistDetailHit struct {
	playlistHit
	Tracks []searchHit `json:"tracks"`
}

type artistHit struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ArtworkURL string `json:"artworkUrl"`
}

type searchHit struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Album      string   `json:"album"`
	DurationMs int64    `json:"durationMs"`
	ArtworkURL string   `json:"artworkUrl"`
	// Video is set by the helper for a music video rather than the recording.
	Video bool `json:"video"`
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
	// Without these YouTube answers "Sign in to confirm you're not a bot" and
	// there is nothing to download.
	if p.cookiesFromBrowser != "" {
		args = append(args, "--cookies-from-browser", p.cookiesFromBrowser)
	}
	if p.cookiesFile != "" {
		args = append(args, "--cookies", p.cookiesFile)
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
