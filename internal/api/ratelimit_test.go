package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
)

func TestSearchRateLimit(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) {
		cfg.RateLimit.SearchPerMinute = 60
		cfg.RateLimit.LoginPerMinute = 60
	})

	// The shared test helper counts every search; give one client a tiny burst.
	c.searchLimiter = newLimiter(1, 1)

	first := c.do(http.MethodGet, "/api/v1/search?q=one", "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first search = %d, want 200", first.Code)
	}
	second := c.do(http.MethodGet, "/api/v1/search?q=two", "", nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second search = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("a rate-limited response should say when to retry")
	}

	// Another client has its own budget.
	other := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=three", nil)
	other.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	c.Server.Handler().ServeHTTP(rec, other)
	if rec.Code != http.StatusOK {
		t.Errorf("a different client = %d, want 200", rec.Code)
	}
}

func TestLoginRateLimitIsSeparateFromSearch(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) {
		cfg.RateLimit.SearchPerMinute = 60
		cfg.RateLimit.LoginPerMinute = 60
	})
	c.loginLimiter = newLimiter(1, 1)
	c.searchLimiter = newLimiter(1, 1)

	if rec := c.do(http.MethodGet, "/api/v1/search?q=x", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("search = %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "a", "password": "hunter2hunter2"}); rec.Code == http.StatusTooManyRequests {
		t.Fatalf("login hit the search limit: %d", rec.Code)
	}
	second := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "a", "password": "hunter2hunter2"})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second login = %d, want 429", second.Code)
	}

	// Unrelated endpoints are never limited.
	for i := 0; i < 5; i++ {
		if rec := c.do(http.MethodGet, "/api/v1/providers", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("providers = %d, want 200", rec.Code)
		}
	}
}

func TestLimitsCanBeDisabled(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) {
		cfg.RateLimit.SearchPerMinute = 0
		cfg.RateLimit.LoginPerMinute = 0
	})
	if c.searchLimiter != nil || c.loginLimiter != nil {
		t.Fatal("a limit of zero must disable the limiter")
	}
	for i := 0; i < 20; i++ {
		if rec := c.do(http.MethodGet, "/api/v1/search?q=x", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("search %d = %d, want 200", i, rec.Code)
		}
	}
}
