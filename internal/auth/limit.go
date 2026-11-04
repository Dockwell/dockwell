// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net"
	"sync"
	"time"
)

// Code sign-in limits. Each account and each source address may get a few
// codes wrong; after that every further try waits twice as long as the one
// before, up to maxBackoff. After lockAfter wrong codes in a row, code
// sign-in locks for the account until the owner unlocks it.
const (
	freeAccountFailures = 3
	freeSourceFailures  = 5
	baseBackoff         = time.Second
	maxBackoff          = 15 * time.Minute
	lockAfter           = 10
)

// backoff is how long to wait after failures wrong attempts in a row.
func backoff(failures, free int) time.Duration {
	if failures < free {
		return 0
	}
	n := failures - free
	if n > 20 {
		return maxBackoff
	}
	return min(baseBackoff<<n, maxBackoff)
}

// failures counts wrong attempts in a row for each key (a source address,
// or an email that matches no account) and says how long the next attempt
// must wait. It forgets keys that have been quiet for a while, and holds at
// most maxKeys.
type failures struct {
	mu      sync.Mutex
	free    int
	maxKeys int
	keys    map[string]*failureCount
}

type failureCount struct {
	n    int
	last time.Time
}

func newFailures(free, maxKeys int) *failures {
	return &failures{free: free, maxKeys: maxKeys, keys: map[string]*failureCount{}}
}

// try counts an attempt for key and returns zero, or, when key must wait
// first, how long, without counting. It also returns the attempts counted
// in a row. Checking and counting happen together, so parallel attempts
// can't all pass under one count. Call reset after a good attempt.
func (f *failures) try(key string, now time.Time) (time.Duration, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.keys[key]
	if !ok {
		if len(f.keys) >= f.maxKeys {
			f.pruneLocked(now)
		}
		c = &failureCount{}
		f.keys[key] = c
	}
	if w := c.last.Add(backoff(c.n, f.free)).Sub(now); c.n > 0 && w > 0 {
		return w, c.n
	}
	c.n++
	c.last = now
	return 0, c.n
}

// reset forgets key after a good attempt.
func (f *failures) reset(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, key)
}

// pruneLocked drops keys quiet for longer than the longest backoff, and if
// that isn't enough, arbitrary ones: a flood of new keys mustn't grow memory
// without bound.
func (f *failures) pruneLocked(now time.Time) {
	for k, c := range f.keys {
		if now.Sub(c.last) > 2*maxBackoff {
			delete(f.keys, k)
		}
	}
	for k := range f.keys {
		if len(f.keys) < f.maxKeys {
			break
		}
		delete(f.keys, k)
	}
}

// buckets limits how often each source address may start a ceremony or try
// a link: a burst, then a steady rate. It keeps unauthenticated callers from
// filling the ceremony store.
type buckets struct {
	mu      sync.Mutex
	burst   float64
	perSec  float64
	maxKeys int
	keys    map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newBuckets(burst int, every time.Duration, maxKeys int) *buckets {
	return &buckets{burst: float64(burst), perSec: 1 / every.Seconds(), maxKeys: maxKeys, keys: map[string]*bucket{}}
}

// take spends one token for key, and reports false when it has none.
func (b *buckets) take(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	k, ok := b.keys[key]
	if !ok {
		if len(b.keys) >= b.maxKeys {
			for old, v := range b.keys {
				if now.Sub(v.last).Seconds()*b.perSec >= b.burst || len(b.keys) >= b.maxKeys {
					delete(b.keys, old)
				}
			}
		}
		k = &bucket{tokens: b.burst, last: now}
		b.keys[key] = k
	}
	k.tokens = min(b.burst, k.tokens+now.Sub(k.last).Seconds()*b.perSec)
	k.last = now
	if k.tokens < 1 {
		return false
	}
	k.tokens--
	return true
}

// sourceKey groups addresses an attacker could easily hold many of: an IPv6
// /64 counts as one source.
func sourceKey(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "src:" + host
	}
	if v4 := ip.To4(); v4 != nil {
		return "src:" + v4.String()
	}
	return "src:" + ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
