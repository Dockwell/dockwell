// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"sync"
	"time"
)

// checksPerMinute bounds how often one caller may have the control server resolve
// names. The domain form reads one every few seconds while a person waits for a
// record, well within it.
const checksPerMinute = 60

// mostCallers bounds what's remembered of callers, as a flood of new ones mustn't
// grow memory without bound.
const mostCallers = 1000

// callers counts each caller's checks in the current minute.
type callers struct {
	mu   sync.Mutex
	most int
	now  func() time.Time
	m    map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newCallers(most int) *callers {
	return &callers{most: most, now: time.Now, m: map[string]*window{}}
}

// allow reports whether caller may check another name now, and counts it if so.
func (c *callers) allow(caller string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	w, ok := c.m[caller]
	if !ok {
		if len(c.m) >= mostCallers {
			c.forget(now)
		}
		w = &window{start: now}
		c.m[caller] = w
	}
	if now.Sub(w.start) >= time.Minute {
		w.start, w.n = now, 0
	}
	if w.n >= c.most {
		return false
	}
	w.n++
	return true
}

// forget drops callers whose minute is over, then any others it must.
func (c *callers) forget(now time.Time) {
	for k, w := range c.m {
		if now.Sub(w.start) >= time.Minute {
			delete(c.m, k)
		}
	}
	for k := range c.m {
		if len(c.m) < mostCallers {
			return
		}
		delete(c.m, k)
	}
}
