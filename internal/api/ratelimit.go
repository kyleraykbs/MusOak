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

	// The sweep's sizes. They are fields so a test can shrink them; production
	// takes the defaults below.
	sweepThreshold int
	sweepBatch     int
	maxBuckets     int

	mu      sync.Mutex
	buckets map[string]*clientBucket
}

type clientBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

const (
	// idleTTL is how long a client's bucket is kept after its last request.
	idleTTL = 10 * time.Minute
	// sweepThreshold is the map size past which a request prunes idle buckets.
	sweepThreshold = 64
	// sweepBatch is how many buckets one request may prune. Walking the whole
	// map on every request is quadratic once enough distinct clients are live;
	// a bounded walk ages the map out over the next handful of requests at O(1)
	// each.
	sweepBatch = 16
	// maxBuckets caps the map. The sweep keeps it far below this while clients
	// come and go; the cap is the backstop for a flood of distinct clients
	// arriving faster than they can age out.
	maxBuckets = 1 << 16
)

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
		rate:           rate.Limit(float64(perMinute) / 60.0),
		burst:          burst,
		trusted:        trusted,
		sweepThreshold: sweepThreshold,
		sweepBatch:     sweepBatch,
		maxBuckets:     maxBuckets,
		buckets:        make(map[string]*clientBucket),
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

	// Amortised sweep: once the map is past the threshold a request prunes a
	// bounded handful of idle buckets. The walk starts somewhere different each
	// time, so it lands on every part of the map over the next requests.
	if len(l.buckets) > l.sweepThreshold {
		l.sweepLocked(now, l.sweepBatch)
	}
	// The backstop: if a flood of distinct clients outruns the sweep, the map
	// is trimmed to the cap rather than growing without end.
	if len(l.buckets) > l.maxBuckets {
		l.evictLocked(len(l.buckets) - l.maxBuckets)
	}
	return bucket.limiter.Allow()
}

// sweepLocked drops up to budget idle buckets, leaving the rest to later
// requests so no single one walks the whole map.
func (l *limiter) sweepLocked(now time.Time, budget int) {
	for key, bucket := range l.buckets {
		if budget <= 0 {
			return
		}
		budget--
		if now.Sub(bucket.seen) > idleTTL {
			delete(l.buckets, key)
		}
	}
}

// evictLocked drops n buckets to keep the map bounded. A bucket dropped here
// may not be idle - that client loses the budget it had built up - but the map
// stays in hand, which is the point of the cap.
func (l *limiter) evictLocked(n int) {
	for key := range l.buckets {
		if n <= 0 {
			return
		}
		delete(l.buckets, key)
		n--
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
