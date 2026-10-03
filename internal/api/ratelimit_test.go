package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/config"
)

// mustCIDRs parses trusted-proxy networks for the resolution tests.
func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", cidr, err)
		}
		nets = append(nets, n)
	}
	return nets
}

func TestClientKeyIgnoresForwardedHeaderWithoutTrustedProxies(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	r.RemoteAddr = "203.0.113.7:4321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientKey(r, nil); got != "203.0.113.7" {
		t.Errorf("clientKey = %q, want the socket peer 203.0.113.7", got)
	}
}

func TestClientKeyResolvesForwardedClientBehindTrustedProxy(t *testing.T) {
	trusted := mustCIDRs(t, "127.0.0.1/32", "10.0.0.0/8")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 127.0.0.1")

	if got := clientKey(r, trusted); got != "1.2.3.4" {
		t.Errorf("clientKey = %q, want the forwarded client 1.2.3.4", got)
	}
}

func TestClientKeyAllTrustedChainFallsBackToPeer(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	r.RemoteAddr = "10.0.0.5:5555"
	r.Header.Set("X-Forwarded-For", "10.0.0.9, 10.0.0.5")

	if got := clientKey(r, trusted); got != "10.0.0.5" {
		t.Errorf("clientKey = %q, want the socket peer 10.0.0.5", got)
	}
}

func TestClientKeyMalformedForwardedHeaderFallsBackToPeer(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	cases := map[string]string{
		"absent":    "",
		"garbage":   "not-an-ip",
		"empty mid": "1.2.3.4, , 10.0.0.5",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
			r.RemoteAddr = "10.0.0.5:5555"
			if header != "" {
				r.Header.Set("X-Forwarded-For", header)
			}
			if got := clientKey(r, trusted); got != "10.0.0.5" {
				t.Errorf("clientKey = %q, want the socket peer 10.0.0.5", got)
			}
		})
	}
}

func TestRateLimitKeysOnResolvedClient(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) {
		cfg.RateLimit.SearchPerMinute = 60
		cfg.RateLimit.LoginPerMinute = 60
		cfg.TrustedProxies = []string{"10.0.0.0/8"}
	})
	// One request per client, reusing the trust list the server parsed so a
	// wiring mistake shows up as a wrong bucket below.
	c.searchLimiter = newLimiter(1, 1, c.searchLimiter.trusted...)

	search := func(peer, forwarded string) int {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=x", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", forwarded)
		rec := httptest.NewRecorder()
		c.Server.Handler().ServeHTTP(rec, r)
		return rec.Code
	}

	// The same client, forwarded by two different proxies, shares one bucket.
	if code := search("10.0.0.5:1111", "1.2.3.4, 10.0.0.5"); code != http.StatusOK {
		t.Fatalf("first search = %d, want 200", code)
	}
	if code := search("10.0.0.9:2222", "1.2.3.4, 10.0.0.9"); code != http.StatusTooManyRequests {
		t.Fatalf("same client via another proxy = %d, want 429", code)
	}
	// A different client still has its own budget.
	if code := search("10.0.0.5:3333", "5.6.7.8, 10.0.0.5"); code != http.StatusOK {
		t.Fatalf("different client = %d, want 200", code)
	}
}

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
