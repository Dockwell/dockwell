// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"net"
	"sync"
	"time"
)

// Code sign-in limits. Each email address and each source address may get a
// few codes wrong; after that every further try waits twice as long as the
// one before, up to maxBackoff. After lockAfter wrong authenticator codes in
// a row, code sign-in locks for the email until the owner unlocks it. Codes
// refused without counting towards the lock are audited at most once every
// refusedReportEvery per account.
const (
	freeAccountFailures = 3
	freeSourceFailures  = 5
	baseBackoff         = time.Second
	maxBackoff          = 15 * time.Minute
	lockAfter           = 10
	refusedReportEvery  = time.Hour
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

// bounded holds state for each key, such as a source address or a token, in
// memory, for at most max keys: a flood of new keys mustn't grow memory
// without bound. When a new key would pass max, it first drops the keys
// whose state forgettable says can go, then, if that isn't enough,
// arbitrary ones. Its owner locks it.
type bounded[V any] struct {
	max         int
	m           map[string]*V
	forgettable func(v *V, now time.Time) bool
}

func newBounded[V any](maxKeys int, forgettable func(*V, time.Time) bool) bounded[V] {
	return bounded[V]{max: maxKeys, m: map[string]*V{}, forgettable: forgettable}
}

// get returns key's state, and whether it was just made, as the zero V.
func (b *bounded[V]) get(key string, now time.Time) (*V, bool) {
	if v, ok := b.m[key]; ok {
		return v, false
	}
	if len(b.m) >= b.max {
		b.prune(now)
	}
	v := new(V)
	b.m[key] = v
	return v, true
}

func (b *bounded[V]) prune(now time.Time) {
	for k, v := range b.m {
		if b.forgettable(v, now) {
			delete(b.m, k)
		}
	}
	for k := range b.m {
		if len(b.m) < b.max {
			break
		}
		delete(b.m, k)
	}
}

func (b *bounded[V]) forget(key string) { delete(b.m, key) }

// failures counts wrong attempts in a row for each source address and says
// how long the next attempt must wait. It forgets keys that have been quiet
// for longer than the longest backoff first, and holds at most maxKeys.
type failures struct {
	mu   sync.Mutex
	free int
	keys bounded[failureCount]
}

type failureCount struct {
	n    int
	last time.Time
}

func newFailures(free, maxKeys int) *failures {
	return &failures{free: free, keys: newBounded(maxKeys, func(c *failureCount, now time.Time) bool {
		return now.Sub(c.last) > 2*maxBackoff
	})}
}

// try counts an attempt for key and returns zero, or, when key must wait
// first, how long, without counting. Checking and counting happen together,
// so parallel attempts can't all pass under one count. Call reset after a
// good attempt.
func (f *failures) try(key string, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, _ := f.keys.get(key, now)
	if w := c.last.Add(backoff(c.n, f.free)).Sub(now); c.n > 0 && w > 0 {
		return w
	}
	c.n++
	c.last = now
	return 0
}

// reset forgets key after a good attempt.
func (f *failures) reset(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys.forget(key)
}

// buckets limits how often each source address may start a ceremony or try
// a link: a burst, then a steady rate. It keeps unauthenticated callers from
// filling the ceremony store. It forgets full buckets first, and holds at
// most maxKeys.
type buckets struct {
	mu     sync.Mutex
	burst  float64
	perSec float64
	keys   bounded[bucket]
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newBuckets(burst int, every time.Duration, maxKeys int) *buckets {
	b := &buckets{burst: float64(burst), perSec: 1 / every.Seconds()}
	b.keys = newBounded(maxKeys, func(k *bucket, now time.Time) bool {
		return k.tokens+now.Sub(k.last).Seconds()*b.perSec >= b.burst
	})
	return b
}

// take spends one token for key, and reports false when it has none.
func (b *buckets) take(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	k, made := b.keys.get(key, now)
	if made {
		*k = bucket{tokens: b.burst, last: now}
	}
	k.tokens = min(b.burst, k.tokens+now.Sub(k.last).Seconds()*b.perSec)
	k.last = now
	if k.tokens < 1 {
		return false
	}
	k.tokens--
	return true
}

// sourceKey is the key for limits by source address.
func sourceKey(addr string) string { return "src:" + networkKey(addr) }

// networkKey groups addresses that one client can easily hold many of, as
// it rotates through them: an IPv6 /64 counts as one, as "2001:db8:1:2::/64".
// An IPv4 address is itself, whether written as IPv4 or as IPv6
// ("::ffff:203.0.113.9"). addr may have a port; anything that isn't an
// address is itself.
func networkKey(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
