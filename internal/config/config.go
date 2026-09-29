// Package config loads and validates Prismusic's JSON configuration.
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
)

const appName = "prismusic"

// DefaultListen is the address the HTTP server binds to.
const DefaultListen = ":8080"

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

// Providers holds the per-provider configuration.
type Providers struct {
	YTMusic ProviderSpec `json:"ytmusic"`
	Spotify SpotifySpec  `json:"spotify"`
}

// ListenTogether holds the room tunables.
type ListenTogether struct {
	SkipThreshold        float64 `json:"skipThreshold"`
	MinVotersForSkip     int     `json:"minVotersForSkip"`
	VoterFractionForSkip float64 `json:"voterFractionForSkip"`
	ReadyTimeoutSeconds  int     `json:"readyTimeoutSeconds"`
}

// Match tunes cross-provider track merging.
type Match struct {
	Threshold float64 `json:"threshold"`
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
}

// DefaultMatchThreshold is the score above which two renditions are the same
// recording.
const DefaultMatchThreshold = 0.8

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Listen:               DefaultListen,
		StorageDir:           DefaultStorageDir(),
		RegistrationOpen:     true,
		Providers:            Providers{YTMusic: ProviderSpec{Enabled: true}},
		DefaultProviderOrder: []string{"ytmusic", "spotify"},
		PrefetchCount:        3,
		ListenTogether: ListenTogether{
			SkipThreshold:        2.0,
			MinVotersForSkip:     2,
			VoterFractionForSkip: 0.5,
			ReadyTimeoutSeconds:  30,
		},
		Match: Match{Threshold: DefaultMatchThreshold},
	}
}

// DefaultPath is $XDG_CONFIG_HOME/prismusic/config.json. It is empty when no
// config directory can be determined.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, appName, "config.json")
}

// DefaultStorageDir is $XDG_DATA_HOME/prismusic. It is empty when no data
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
	if lt.ReadyTimeoutSeconds < 0 {
		return fmt.Errorf("listenTogether.readyTimeoutSeconds must be >= 0, got %d", lt.ReadyTimeoutSeconds)
	}

	if c.Match.Threshold <= 0 || c.Match.Threshold > 1 {
		return fmt.Errorf("match.threshold must be in (0, 1], got %g", c.Match.Threshold)
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

// KnownProvider reports whether name is a provider the ranking system accepts.
// "local" is the on-disk library.
func KnownProvider(name string) bool {
	switch name {
	case "local", "ytmusic", "spotify":
		return true
	}
	return false
}
