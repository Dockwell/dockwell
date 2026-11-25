// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// RateLimitPrefix is the path prefix Caddy calls on signin.sock for per-address rate limiting.
	RateLimitPrefix = "/ratelimit/"
	// maxRateVisitors bounds tracked visitor buckets per route so a flood of unique IPs
	// cannot grow agent memory unboundedly.
	maxRateVisitors = 4096
)

type routeBucket struct {
	mu       sync.Mutex
	rpm      uint32
	burst    uint32
	visitors map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// serveRateLimit enforces a per-visitor-address token bucket for /ratelimit/<routeID>?rpm=<N>&burst=<B>.
// Only Caddy on the server's root/proxy-only unix socket reaches this endpoint.
func (s *Server) serveRateLimit(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	rpm, err := strconv.ParseUint(q.Get("rpm"), 10, 32)
	if err != nil || rpm == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	burst, _ := strconv.ParseUint(q.Get("burst"), 10, 32)
	client := clientOf(r)
	if client == "" {
		client = "unknown"
	}
	ok, retryAfter := s.allowRate(id, client, uint32(rpm), uint32(burst))
	if ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(max(1, retryAfter)))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("Too Many Requests\n"))
}

func (s *Server) allowRate(routeID, client string, rpm, burst uint32) (allowed bool, retryAfterSec int) {
	s.mu.Lock()
	if s.rateBuckets == nil {
		s.rateBuckets = map[string]*routeBucket{}
	}
	rb, ok := s.rateBuckets[routeID]
	if !ok {
		rb = &routeBucket{rpm: rpm, burst: burst, visitors: map[string]*tokenBucket{}}
		s.rateBuckets[routeID] = rb
	}
	s.mu.Unlock()

	rb.mu.Lock()
	defer rb.mu.Unlock()

	now := s.now()
	ratePerSec := float64(rpm) / 60.0
	capacity := 1.0 + float64(burst)

	if rb.rpm != rpm || rb.burst != burst {
		rb.rpm, rb.burst = rpm, burst
		clear(rb.visitors)
	}

	b, exists := rb.visitors[client]
	if !exists {
		if len(rb.visitors) >= maxRateVisitors {
			// Prune idle/refilled buckets first, then evict oldest if still at cap.
			idleAfter := time.Duration(math.Ceil(capacity/ratePerSec)) * time.Second
			if idleAfter < time.Minute {
				idleAfter = time.Minute
			}
			var oldestKey string
			var oldestTime time.Time
			for k, v := range rb.visitors {
				if now.Sub(v.last) >= idleAfter {
					delete(rb.visitors, k)
					continue
				}
				if oldestKey == "" || v.last.Before(oldestTime) {
					oldestKey, oldestTime = k, v.last
				}
			}
			if len(rb.visitors) >= maxRateVisitors && oldestKey != "" {
				delete(rb.visitors, oldestKey)
			}
		}
		b = &tokenBucket{tokens: capacity, last: now}
		rb.visitors[client] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = min(capacity, b.tokens+elapsed*ratePerSec)
			b.last = now
		}
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true, 0
	}
	needed := (1.0 - b.tokens) / ratePerSec
	return false, max(1, int(math.Ceil(needed)))
}
