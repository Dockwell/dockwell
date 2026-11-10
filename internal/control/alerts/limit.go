// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"sync"
	"time"
)

// TestsPerMinute is how many tests one person or token sends in a minute at most:
// enough to fix a channel, too few to use Dockwell to knock on a network.
const TestsPerMinute = 6

// mostTesters is how many people and tokens' minutes are kept before the oldest
// are forgotten.
const mostTesters = 1000

// testers counts each person's and token's tests in the last minute.
type testers struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// allow counts a test by actor at now, and says how long until it may send one,
// or 0 when it may now.
func (t *testers) allow(actor string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	recent := t.seen[actor][:0]
	for _, at := range t.seen[actor] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= TestsPerMinute {
		t.seen[actor] = recent
		return recent[0].Add(time.Minute).Sub(now)
	}
	if _, ok := t.seen[actor]; !ok && len(t.seen) >= mostTesters {
		for a, times := range t.seen {
			if len(times) == 0 || now.Sub(times[len(times)-1]) >= time.Minute {
				delete(t.seen, a)
			}
		}
	}
	t.seen[actor] = append(recent, now)
	return 0
}
