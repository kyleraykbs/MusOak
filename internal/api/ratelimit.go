package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiter is a per-client token bucket with a small sweep so idle clients do
// not accumulate forever.
type limiter struct {
	rate  rate.Limit
	burst int

	// trusted are the proxy networks whose X-Forwarded-For is believed when
	// resolving the client address. Empty means the socket peer is the client.
	trusted []*net.IPNet

	mu      sync.Mutex
	buckets map[string]*clientBucket
}

type clientBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

// idleTTL is how long a client's bucket is kept after its last request.
const idleTTL = 10 * time.Minute

// newLimiter builds a limiter for perMinute requests with the given burst.
// trusted carries the reverse proxies whose forwarded client address is
// believed; a caller with none can omit it. A non-positive perMinute disables
// the limit and returns nil.
func newLimiter(perMinute, burst int, trusted ...*net.IPNet) *limiter {
	if perMinute <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &limiter{
		rate:    rate.Limit(float64(perMinute) / 60.0),
		burst:   burst,
		trusted: trusted,
		buckets: make(map[string]*clientBucket),
	}
}

// allow reports whether key may make a request now.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	bucket, ok := l.buckets[key]
	if !ok {
		bucket = &clientBucket{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[key] = bucket
	}
	bucket.seen = now

	if len(l.buckets) > 64 {
		l.sweepLocked(now)
	}
	return bucket.limiter.Allow()
}

func (l *limiter) sweepLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.seen) > idleTTL {
			delete(l.buckets, key)
		}
	}
}

// withRateLimits applies the configured budgets to the expensive endpoints.
// Search costs provider quota; register and login cost argon2id work.
func (s *Server) withRateLimits(next http.Handler) http.Handler {
	if s.searchLimiter == nil && s.loginLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var limit *limiter
		switch {
		case s.searchLimiter != nil && r.URL.Path == "/api/v1/search":
			limit = s.searchLimiter
		case s.loginLimiter != nil && (r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/auth/register"):
			limit = s.loginLimiter
		}
		if limit != nil && !limit.allow(clientKey(r, limit.trusted)) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey identifies the caller for rate limiting. Normally that is the
// socket peer. When the peer is one of the trusted proxies, the client is
// taken from X-Forwarded-For instead: walking from the right, entries that are
// themselves trusted proxies are skipped and the first one that is not is the
// client. An absent, empty or malformed header falls back to the peer.
func clientKey(r *http.Request, trusted []*net.IPNet) string {
	peer := peerHost(r.RemoteAddr)
	if len(trusted) == 0 {
		return peer
	}
	ip := net.ParseIP(peer)
	if ip == nil || !trustedContains(trusted, ip) {
		return peer
	}
	if client, ok := forwardedClient(r.Header.Get("X-Forwarded-For"), trusted); ok {
		return client
	}
	return peer
}

// peerHost returns the host part of a socket address, or the whole address
// when it does not have a port to split off.
func peerHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// forwardedClient returns the client address named by an X-Forwarded-For
// value. It walks the list right to left, skipping the trusted proxies, and
// stops at the first entry that is not one. ok is false when no such entry
// exists (an empty, malformed or wholly-trusted chain), so the caller falls
// back to the peer.
func forwardedClient(header string, trusted []*net.IPNet) (string, bool) {
	entries := strings.Split(header, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		entry := strings.TrimSpace(entries[i])
		ip := net.ParseIP(entry)
		if ip == nil {
			return "", false
		}
		if trustedContains(trusted, ip) {
			continue
		}
		return ip.String(), true
	}
	return "", false
}

// trustedContains reports whether ip falls in one of the trusted networks.
func trustedContains(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
