package api

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiter is a per-client token bucket with a small sweep so idle clients do
// not accumulate forever.
type limiter struct {
	rate  rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*clientBucket
}

type clientBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

// idleTTL is how long a client's bucket is kept after its last request.
const idleTTL = 10 * time.Minute

func newLimiter(perMinute, burst int) *limiter {
	if perMinute <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &limiter{
		rate:    rate.Limit(float64(perMinute) / 60.0),
		burst:   burst,
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
		if limit != nil && !limit.allow(clientKey(r)) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey identifies the caller for rate limiting: the peer address.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
