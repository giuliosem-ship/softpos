package api

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultAuthRateLimitPerMinute is the per-IP budget for /api/v1/auth/* requests.
const DefaultAuthRateLimitPerMinute = 30

// RateLimiter is a fixed-window, per-key in-memory limiter. It is a first line
// of defence for a single instance; multi-replica deployments should also
// limit at the gateway.
type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*rateBucket
	now     func() time.Time
	sweepAt time.Time
}

type rateBucket struct {
	start time.Time
	count int
}

// NewRateLimiter allows limit requests per key per window (limit <= 0 disables it).
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, buckets: map[string]*rateBucket{}, now: time.Now}
}

// Allow records one request for key and reports whether it is within budget;
// when not, it also returns the Retry-After value in seconds.
func (l *RateLimiter) Allow(key string) (bool, string) {
	if l == nil || l.limit <= 0 {
		return true, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.After(l.sweepAt) { // drop expired buckets so memory stays bounded
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		l.sweepAt = now.Add(l.window)
	}
	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		b = &rateBucket{start: now}
		l.buckets[key] = b
	}
	b.count++
	if b.count > l.limit {
		wait := b.start.Add(l.window).Sub(now)
		return false, strconv.FormatInt(int64(wait/time.Second)+1, 10)
	}
	return true, ""
}

// clientIP is the TCP peer address. X-Forwarded-For is deliberately ignored
// (spoofable); behind a proxy, rate limiting belongs at the gateway.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
