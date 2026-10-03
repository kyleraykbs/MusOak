package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/musoak/internal/config"
)

// TestServeAppliesMiddleware guards a real bug: the daemon used to serve the
// bare router, so a request that went through a real socket skipped
// authentication and the rate limits entirely, while the tests (which use
// Handler) never noticed.
func TestServeAppliesMiddleware(t *testing.T) {
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false
	// The plain YouTube provider would reach the network; a test wants none of it.
	cfg.Providers.YouTube.Enabled = false
	cfg.RequireLogin = true

	server, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer server.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	base := "http://" + listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	// Anonymous access is refused when requireLogin is set.
	resp, err := client.Get(base + "/api/v1/search?q=x")
	if err != nil {
		t.Fatalf("anonymous request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous search = %d, want 401 from the served handler", resp.StatusCode)
	}

	// Registering over the same socket yields a token that authenticates.
	body := strings.NewReader(`{"username":"socket-user","password":"hunter2hunter2"}`)
	resp, err = client.Post(base+"/api/v1/auth/register", "application/json", body)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	payload, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d: %s", resp.StatusCode, payload)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(payload, &auth); err != nil {
		t.Fatalf("decode register: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	payload, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	var me struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if !me.Authenticated || me.User.Username != "socket-user" {
		t.Fatalf("me = %s; the token issued over the socket did not authenticate", payload)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve did not shut down")
	}
}
