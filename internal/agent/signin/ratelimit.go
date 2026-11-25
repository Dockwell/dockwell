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
	// maxRateVisitors bounds tracked visitor buckets per route.
	maxRateVisitors = 4096
	// maxTotalRateBuckets caps total visitor buckets across all routes on the agent
	// so a multi-route IP flood stays well under the agent's 40 MB RSS budget (ADR 0109).
	maxTotalRateBuckets = 8192
)

// RateLimiter enforces per-visitor-address token buckets across routes with O(1)
// oldest-idle LRU eviction, both per-route and globally across the agent.
type RateLimiter struct {
	MaxPerRoute int
	MaxTotal    int

	mu      sync.Mutex
	routes  map[string]*routeBucket
	lruHead *rateEntry // most recently used
	lruTail *rateEntry // oldest idle (evicted first)
	total   int
}

type routeBucket struct {
	rpm      uint32
	burst    uint32
	visitors map[string]*rateEntry
	rHead    *rateEntry // most recently used on this route
	rTail    *rateEntry // oldest idle on this route
}

type rateEntry struct {
	routeID string
	client  string
	tokens  float64
	last    time.Time

	prev, next   *rateEntry // global LRU list
	rPrev, rNext *rateEntry // per-route LRU list
}

func (l *RateLimiter) maxPerRoute() int {
	if l.MaxPerRoute > 0 {
		return l.MaxPerRoute
	}
	return maxRateVisitors
}

func (l *RateLimiter) maxTotal() int {
	if l.MaxTotal > 0 {
		return l.MaxTotal
	}
	return maxTotalRateBuckets
}

// Allow checks and consumes one token for (routeID, client) at now.
func (l *RateLimiter) Allow(routeID, client string, rpm, burst uint32, now time.Time) (allowed bool, retryAfterSec int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.routes == nil {
		l.routes = map[string]*routeBucket{}
	}
	rb, ok := l.routes[routeID]
	if !ok {
		rb = &routeBucket{rpm: rpm, burst: burst, visitors: map[string]*rateEntry{}}
		l.routes[routeID] = rb
	}

	ratePerSec := float64(rpm) / 60.0
	capacity := 1.0 + float64(burst)

	if rb.rpm != rpm || rb.burst != burst {
		l.clearRouteLocked(rb)
		rb.rpm, rb.burst = rpm, burst
	}

	e, exists := rb.visitors[client]
	if !exists {
		if len(rb.visitors) >= l.maxPerRoute() && rb.rTail != nil {
			l.evictLocked(rb, rb.rTail)
		}
		for l.total >= l.maxTotal() && l.lruTail != nil {
			victim := l.lruTail
			vrb := l.routes[victim.routeID]
			l.evictLocked(vrb, victim)
			if vrb != nil && len(vrb.visitors) == 0 && victim.routeID != routeID {
				delete(l.routes, victim.routeID)
			}
		}
		e = &rateEntry{routeID: routeID, client: client, tokens: capacity, last: now}
		rb.visitors[client] = e
		l.pushFrontGlobal(e)
		l.pushFrontRoute(rb, e)
		l.total++
	} else {
		elapsed := now.Sub(e.last).Seconds()
		if elapsed > 0 {
			e.tokens = min(capacity, e.tokens+elapsed*ratePerSec)
			e.last = now
		}
		l.moveToFrontGlobal(e)
		l.moveToFrontRoute(rb, e)
	}

	if e.tokens >= 1.0 {
		e.tokens -= 1.0
		return true, 0
	}
	needed := (1.0 - e.tokens) / ratePerSec
	return false, max(1, int(math.Ceil(needed)))
}

// Drop frees all buckets belonging to routeID.
func (l *RateLimiter) Drop(routeID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rb, ok := l.routes[routeID]
	if !ok {
		return
	}
	l.clearRouteLocked(rb)
	delete(l.routes, routeID)
}

// Count returns the total number of tracked visitor buckets across all routes.
func (l *RateLimiter) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Routes returns the number of routes currently holding buckets.
func (l *RateLimiter) Routes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.routes)
}

func (l *RateLimiter) clearRouteLocked(rb *routeBucket) {
	for e := rb.rHead; e != nil; {
		next := e.rNext
		l.unlinkGlobal(e)
		e.rPrev, e.rNext = nil, nil
		l.total--
		e = next
	}
	clear(rb.visitors)
	rb.rHead, rb.rTail = nil, nil
}

func (l *RateLimiter) evictLocked(rb *routeBucket, e *rateEntry) {
	l.unlinkGlobal(e)
	if rb != nil {
		l.unlinkRoute(rb, e)
		delete(rb.visitors, e.client)
	}
	l.total--
}

func (l *RateLimiter) pushFrontGlobal(e *rateEntry) {
	e.prev = nil
	e.next = l.lruHead
	if l.lruHead != nil {
		l.lruHead.prev = e
	} else {
		l.lruTail = e
	}
	l.lruHead = e
}

func (l *RateLimiter) unlinkGlobal(e *rateEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if l.lruHead == e {
		l.lruHead = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if l.lruTail == e {
		l.lruTail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (l *RateLimiter) moveToFrontGlobal(e *rateEntry) {
	if l.lruHead == e {
		return
	}
	l.unlinkGlobal(e)
	l.pushFrontGlobal(e)
}

func (l *RateLimiter) pushFrontRoute(rb *routeBucket, e *rateEntry) {
	e.rPrev = nil
	e.rNext = rb.rHead
	if rb.rHead != nil {
		rb.rHead.rPrev = e
	} else {
		rb.rTail = e
	}
	rb.rHead = e
}

func (l *RateLimiter) unlinkRoute(rb *routeBucket, e *rateEntry) {
	if e.rPrev != nil {
		e.rPrev.rNext = e.rNext
	} else if rb.rHead == e {
		rb.rHead = e.rNext
	}
	if e.rNext != nil {
		e.rNext.rPrev = e.rPrev
	} else if rb.rTail == e {
		rb.rTail = e.rPrev
	}
	e.rPrev, e.rNext = nil, nil
}

func (l *RateLimiter) moveToFrontRoute(rb *routeBucket, e *rateEntry) {
	if rb.rHead == e {
		return
	}
	l.unlinkRoute(rb, e)
	l.pushFrontRoute(rb, e)
}

func (s *Server) rateLimiter() *RateLimiter {
	if s.RateLimits != nil {
		return s.RateLimits
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownRateLimits == nil {
		s.ownRateLimits = &RateLimiter{}
	}
	return s.ownRateLimits
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
	ok, retryAfter := s.rateLimiter().Allow(id, client, uint32(rpm), uint32(burst), s.now())
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
