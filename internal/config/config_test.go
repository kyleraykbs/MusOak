package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestLoadExample(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("config.example.json must load: %v", err)
	}
	if cfg.Listen != ":4420" {
		t.Errorf("listen = %q, want :4420", cfg.Listen)
	}
	if cfg.StorageDir == "" {
		t.Error("storageDir must be filled")
	}
	if got := cfg.DefaultProviderOrder; len(got) != 2 || got[0] != "ytmusic" || got[1] != "spotify" {
		t.Errorf("defaultProviderOrder = %v", got)
	}
	if cfg.ListenTogether.ReadyTimeoutSeconds != 30 {
		t.Errorf("readyTimeoutSeconds = %d, want 30", cfg.ListenTogether.ReadyTimeoutSeconds)
	}
	if cfg.Match.Threshold != DefaultMatchThreshold {
		t.Errorf("match.threshold = %v, want %v", cfg.Match.Threshold, DefaultMatchThreshold)
	}
	if cfg.RateLimit.SearchPerMinute != 30 || cfg.RateLimit.LoginPerMinute != 10 {
		t.Errorf("rateLimit = %+v", cfg.RateLimit)
	}
	if !cfg.Providers.YTMusic.Enabled || !cfg.Providers.YouTube.Enabled || cfg.Providers.Spotify.Enabled {
		t.Errorf("unexpected provider enablement: %+v", cfg.Providers)
	}
}

func TestLoadEnablesYouTube(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"providers": {"youtube": {"enabled": true, "cookiesFromBrowser": "firefox"}},
		"defaultProviderOrder": ["ytmusic", "youtube", "spotify"]
	}`))
	if err != nil {
		t.Fatalf("youtube is opt-in but must be a first-class provider: %v", err)
	}
	if !cfg.Providers.YouTube.Enabled {
		t.Error("youtube should be enabled")
	}
	if cfg.Providers.YouTube.CookiesFromBrowser != "firefox" {
		t.Errorf("cookiesFromBrowser = %q", cfg.Providers.YouTube.CookiesFromBrowser)
	}
}

func TestLoadMinimalUsesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("empty object must be valid: %v", err)
	}
	def := Default()
	if cfg.Listen != def.Listen || cfg.PrefetchCount != def.PrefetchCount {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadMissingExplicitPathFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("explicitly requested missing config must be an error")
	}
}

func TestLoadMissingDefaultPathUsesDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("missing default config must not error: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q", cfg.Listen)
	}
}

func TestLoadRejectsBadTrustedProxy(t *testing.T) {
	_, err := Load(writeConfig(t, `{"trustedProxies": ["127.0.0.1/32", "nonsense"]}`))
	if err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("want an error naming the bad CIDR, got %v", err)
	}
}

func TestLoadParsesTrustedProxies(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"trustedProxies": ["127.0.0.1/32", "10.0.0.0/8"]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	nets, err := cfg.TrustedProxyNets()
	if err != nil {
		t.Fatalf("TrustedProxyNets: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("got %d networks, want 2", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("127.0.0.1")) || !nets[1].Contains(net.ParseIP("10.1.2.3")) {
		t.Fatalf("unexpected networks: %v", nets)
	}
}

func TestLoadRejectsBadJSON(t *testing.T) {
	_, err := Load(writeConfig(t, `{"listen": `))
	if err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"top level":   `{"lsten": ":8080"}`,
		"nested":      `{"providers": {"ytmusic": {"nabled": true}}}`,
		"deeper":      `{"listenTogether": {"skipThreshold": 2, "nope": 1}}`,
		"provider id": `{"providers": {"deezer": {"enabled": true}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, content))
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("want unknown field error, got %v", err)
			}
		})
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	_, err := Load(writeConfig(t, `{} {}`))
	if err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("want trailing data error, got %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"bad listen", func(c *Config) { c.Listen = "nope" }, "listen"},
		{"bad port", func(c *Config) { c.Listen = ":99999" }, "port"},
		{"empty storage", func(c *Config) { c.StorageDir = "" }, "storageDir"},
		{"negative prefetch", func(c *Config) { c.PrefetchCount = -1 }, "prefetchCount"},
		{"skip threshold high", func(c *Config) { c.ListenTogether.SkipThreshold = 5.1 }, "skipThreshold"},
		{"skip threshold zero", func(c *Config) { c.ListenTogether.SkipThreshold = 0 }, "skipThreshold"},
		{"no voters", func(c *Config) { c.ListenTogether.MinVotersForSkip = 0 }, "minVotersForSkip"},
		{"fraction high", func(c *Config) { c.ListenTogether.VoterFractionForSkip = 1.5 }, "voterFractionForSkip"},
		{"negative timeout", func(c *Config) { c.ListenTogether.ReadyTimeoutSeconds = -1 }, "readyTimeoutSeconds"},
		{"bad match threshold", func(c *Config) { c.Match.Threshold = 1.5 }, "match.threshold"},
		{"negative search limit", func(c *Config) { c.RateLimit.SearchPerMinute = -1 }, "rateLimit.searchPerMinute"},
		{"negative login limit", func(c *Config) { c.RateLimit.LoginPerMinute = -1 }, "rateLimit.loginPerMinute"},
		{"negative quota", func(c *Config) { c.Media.QuotaMB = -5 }, "media.quotaMB"},
		{"bad trusted proxy", func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/33"} }, "10.0.0.0/33"},
		{"empty order", func(c *Config) { c.DefaultProviderOrder = nil }, "must not be empty"},
		{"unknown order", func(c *Config) { c.DefaultProviderOrder = []string{"deezer"} }, "unknown provider"},
		{"duplicate order", func(c *Config) { c.DefaultProviderOrder = []string{"ytmusic", "ytmusic"} }, "duplicate provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("want error containing %q, got %v", tt.wantSub, err)
			}
		})
	}
}
