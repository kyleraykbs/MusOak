// Package config loads and validates MusOak's JSON configuration.
//
// The file is strict: unknown keys are an error, so a typo can never be
// silently ignored. Absent keys keep their defaults, which makes an empty
// object a valid (if useless) configuration.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const appName = "musoak"

// DefaultListen is the address the HTTP server binds to.
const DefaultListen = ":4420"

// ProviderSpec configures a provider that needs nothing but a switch.
type ProviderSpec struct {
	Enabled bool `json:"enabled"`
}

// SpotifySpec configures the metadata-only Spotify provider.
type SpotifySpec struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// YTMusicSpec configures the YT Music provider. Audio comes from yt-dlp, and
// YouTube increasingly answers it with "Sign in to confirm you're not a bot":
// cookies from a browser that is signed in are what gets past that.
type YTMusicSpec struct {
	Enabled bool `json:"enabled"`
	// CookiesFromBrowser is yt-dlp's own syntax: a browser name ("firefox",
	// "chrome"), or one with a profile path for a fork that keeps its own
	// ("firefox:/home/you/.librewolf/abc.default-release").
	CookiesFromBrowser string `json:"cookiesFromBrowser"`
	// CookiesFile is a Netscape-format cookies.txt exported from a browser,
	// which is the form the yt-dlp wiki recommends when the browser is awkward
	// to reach.
	CookiesFile string `json:"cookiesFile"`
}

// YouTubeSpec configures the plain YouTube provider. It is on by default so
// the platform exists to search; its hits are videos rather than recordings,
// so clients leave it out of their own filters until somebody asks for it.
// Like YT Music it downloads through yt-dlp, and the cookie fields are its
// escape hatch when YouTube demands a signed-in session.
type YouTubeSpec struct {
	Enabled bool `json:"enabled"`
	// CookiesFromBrowser is yt-dlp's own syntax: a browser name ("firefox",
	// "chrome"), or one with a profile path for a fork that keeps its own
	// ("firefox:/home/you/.librewolf/abc.default-release").
	CookiesFromBrowser string `json:"cookiesFromBrowser"`
	// CookiesFile is a Netscape-format cookies.txt exported from a browser,
	// which is the form the yt-dlp wiki recommends when the browser is awkward
	// to reach.
	CookiesFile string `json:"cookiesFile"`
}

// Providers holds the per-provider configuration.
type Providers struct {
	YTMusic YTMusicSpec `json:"ytmusic"`
	YouTube YouTubeSpec `json:"youtube"`
	Spotify SpotifySpec `json:"spotify"`
}

// ListenTogether holds the room tunables.
type ListenTogether struct {
	SkipThreshold        float64 `json:"skipThreshold"`
	MinVotersForSkip     int     `json:"minVotersForSkip"`
	VoterFractionForSkip float64 `json:"voterFractionForSkip"`
	// ReadyFraction is how much of the room has to have the song before it
	// starts: waiting for the last member is waiting for the slowest one.
	ReadyFraction float64 `json:"readyFraction"`
	// ReadyTimeoutSeconds is how long the room waits for that fraction before
	// starting anyway. Members who missed it are catching up.
	ReadyTimeoutSeconds int `json:"readyTimeoutSeconds"`
}

// Match tunes cross-provider track merging.
type Match struct {
	Threshold float64 `json:"threshold"`
}

// RateLimit bounds the endpoints that cost real work or invite abuse. Zero
// disables a limit.
type RateLimit struct {
	// SearchPerMinute is the per-client search budget.
	SearchPerMinute int `json:"searchPerMinute"`
	// LoginPerMinute is the per-client budget for register and login.
	LoginPerMinute int `json:"loginPerMinute"`
}

// Media tunes the on-disk media cache.
type Media struct {
	// QuotaMB caps the media directory. Zero means no limit. When the cache
	// grows past it, the least recently used renditions are evicted.
	QuotaMB int `json:"quotaMB"`
	// PrefetchPlaylists keeps the songs in people's playlists downloaded before
	// anybody plays them, one at a time in the background. It is what makes a
	// first play cost nothing, at the price of fetching songs nobody may reach.
	PrefetchPlaylists bool `json:"prefetchPlaylists"`
}

// Client configures client-side behaviour (the CLI, or any other API client)
// rather than the server.
type Client struct {
	// ServerURL is the server to talk to. Empty means the CLI runs its own
	// server in-process, over loopback.
	ServerURL string `json:"serverURL"`
	// CacheDir holds downloaded renditions and the local queue.
	CacheDir string `json:"cacheDir"`
}

// Config is the complete configuration, shared by the server and the CLI.
type Config struct {
	Listen               string         `json:"listen"`
	StorageDir           string         `json:"storageDir"`
	RequireLogin         bool           `json:"requireLogin"`
	RegistrationOpen     bool           `json:"registrationOpen"`
	Providers            Providers      `json:"providers"`
	DefaultProviderOrder []string       `json:"defaultProviderOrder"`
	PrefetchCount        int            `json:"prefetchCount"`
	ListenTogether       ListenTogether `json:"listenTogether"`
	Match                Match          `json:"match"`
	RateLimit            RateLimit      `json:"rateLimit"`
	Media                Media          `json:"media"`
	Client               Client         `json:"client"`

	// TrustedProxies are the reverse proxies whose X-Forwarded-For is believed
	// when resolving the client address for rate limiting. It is empty by
	// default: a directly-reachable server trusts no forwarded header.
	TrustedProxies []string `json:"trustedProxies"`
}

// DefaultMatchThreshold is the score above which two renditions are the same
// recording.
const DefaultMatchThreshold = 0.8

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Listen:           DefaultListen,
		StorageDir:       DefaultStorageDir(),
		RegistrationOpen: true,
		Providers: Providers{
			YTMusic: YTMusicSpec{Enabled: true},
			// Plain YouTube is on so the platform exists to search; clients
			// leave it out of their own filters until somebody asks for it,
			// which is what keeps video results out of the way by default.
			YouTube: YouTubeSpec{Enabled: true},
		},
		DefaultProviderOrder: []string{"ytmusic", "youtube", "spotify"},
		PrefetchCount:        3,
		ListenTogether: ListenTogether{
			SkipThreshold:        2.0,
			MinVotersForSkip:     2,
			VoterFractionForSkip: 0.5,
			ReadyFraction:        0.75,
			// Thirty, not six: the module, the example and the README all say
			// thirty, and a default that only disagrees with every one of them
			// is a default nobody can reason about. A deployment that wants a
			// shorter backstop sets it.
			ReadyTimeoutSeconds: 30,
		},
		Match:  Match{Threshold: DefaultMatchThreshold},
		Client: Client{CacheDir: DefaultCacheDir()},
		RateLimit: RateLimit{
			SearchPerMinute: 30,
			LoginPerMinute:  10,
		},
		Media: Media{QuotaMB: 0, PrefetchPlaylists: true},
	}
}

// DefaultPath is $XDG_CONFIG_HOME/musoak/config.json. It is empty when no
// config directory can be determined.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, appName, "config.json")
}

// DefaultStorageDir is $XDG_DATA_HOME/musoak. It is empty when no data
// directory can be determined.
func DefaultStorageDir() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, appName)
}

// DefaultCacheDir is $XDG_CACHE_HOME/musoak, where a client keeps the
// renditions it has downloaded. It is empty when no cache directory can be
// determined.
func DefaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, appName)
}

// ConfigEnvVar points at a configuration file, for deployments that install
// one system-wide (the NixOS module does). An explicit --config still wins.
const ConfigEnvVar = "MUSOAK_CONFIG"

// ResolvePath decides which configuration file to load: the explicit flag, then
// the environment, then the per-user XDG location.
func ResolvePath(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	return strings.TrimSpace(os.Getenv(ConfigEnvVar))
}

// Load reads the configuration from path. An empty path selects DefaultPath();
// a missing file at that location is not an error and yields Default(). A path
// given explicitly must exist.
func Load(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		path = DefaultPath()
		if path == "" {
			return nil, fmt.Errorf("cannot determine config directory; pass --config")
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return Default(), nil
		}
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	cfg := Default()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse config %s: trailing data after JSON value", path)
	}

	// An explicitly blanked string falls back to the default; JSON cannot
	// distinguish "absent" from "empty" when decoding into filled defaults.
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if cfg.StorageDir == "" {
		cfg.StorageDir = DefaultStorageDir()
	}
	if cfg.Client.CacheDir == "" {
		cfg.Client.CacheDir = DefaultCacheDir()
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports structural errors in the configuration.
func (c *Config) Validate() error {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("listen %q: port must be 1-65535", c.Listen)
	}
	if c.StorageDir == "" {
		return fmt.Errorf("storageDir is empty and cannot be defaulted; set \"storageDir\"")
	}
	if c.PrefetchCount < 0 {
		return fmt.Errorf("prefetchCount must be >= 0, got %d", c.PrefetchCount)
	}

	lt := c.ListenTogether
	if lt.SkipThreshold <= 0 || lt.SkipThreshold > 5 {
		return fmt.Errorf("listenTogether.skipThreshold must be in (0, 5], got %g", lt.SkipThreshold)
	}
	if lt.MinVotersForSkip < 1 {
		return fmt.Errorf("listenTogether.minVotersForSkip must be >= 1, got %d", lt.MinVotersForSkip)
	}
	if lt.VoterFractionForSkip <= 0 || lt.VoterFractionForSkip > 1 {
		return fmt.Errorf("listenTogether.voterFractionForSkip must be in (0, 1], got %g", lt.VoterFractionForSkip)
	}
	if lt.ReadyFraction <= 0 || lt.ReadyFraction > 1 {
		return fmt.Errorf("listenTogether.readyFraction must be in (0, 1], got %g", lt.ReadyFraction)
	}
	if lt.ReadyTimeoutSeconds < 0 {
		return fmt.Errorf("listenTogether.readyTimeoutSeconds must be >= 0, got %d", lt.ReadyTimeoutSeconds)
	}

	if c.Match.Threshold <= 0 || c.Match.Threshold > 1 {
		return fmt.Errorf("match.threshold must be in (0, 1], got %g", c.Match.Threshold)
	}
	if c.RateLimit.SearchPerMinute < 0 {
		return fmt.Errorf("rateLimit.searchPerMinute must be >= 0, got %d", c.RateLimit.SearchPerMinute)
	}
	if c.RateLimit.LoginPerMinute < 0 {
		return fmt.Errorf("rateLimit.loginPerMinute must be >= 0, got %d", c.RateLimit.LoginPerMinute)
	}
	if c.Media.QuotaMB < 0 {
		return fmt.Errorf("media.quotaMB must be >= 0, got %d", c.Media.QuotaMB)
	}
	if _, err := c.TrustedProxyNets(); err != nil {
		return err
	}

	if len(c.DefaultProviderOrder) == 0 {
		return fmt.Errorf("defaultProviderOrder must not be empty")
	}
	seen := make(map[string]bool, len(c.DefaultProviderOrder))
	for _, name := range c.DefaultProviderOrder {
		if !KnownProvider(name) {
			return fmt.Errorf("defaultProviderOrder: unknown provider %q", name)
		}
		if seen[name] {
			return fmt.Errorf("defaultProviderOrder: duplicate provider %q", name)
		}
		seen[name] = true
	}
	return nil
}

// TrustedProxyNets parses TrustedProxies into the networks used to decide
// whether the socket peer is a reverse proxy whose X-Forwarded-For is
// believed. An entry that is not a CIDR is an error naming it.
func (c *Config) TrustedProxyNets() ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(c.TrustedProxies))
	for _, cidr := range c.TrustedProxies {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("trustedProxies: %q is not a valid CIDR", cidr)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// KnownProvider reports whether name is a provider the ranking system accepts.
// "local" is the on-disk library.
func KnownProvider(name string) bool {
	switch name {
	case "local", "ytmusic", "youtube", "spotify":
		return true
	}
	return false
}
