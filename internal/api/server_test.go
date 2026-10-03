package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestEnabledProvidersAreRegistered(t *testing.T) {
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if names := s.providers.Names(); len(names) != 2 || names[0] != "ytmusic" || names[1] != "youtube" {
		t.Errorf("providers = %v, want [ytmusic youtube] (what the default configuration enables)", names)
	}

	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false
	// The plain YouTube provider would reach the network; a test wants none of it.
	cfg.Providers.YouTube.Enabled = false
	off, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if names := off.providers.Names(); len(names) != 0 {
		t.Errorf("providers = %v, want none", names)
	}
}
