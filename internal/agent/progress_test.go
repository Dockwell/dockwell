// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// Progress goes out at most once an interval, but a message that comes
// sooner isn't lost: the latest goes when the interval ends, so a change
// of phase always shows. Once the call ends, what waits is dropped, as
// the call's result says the last word.
func TestProgressKeepsTheLatest(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	heard := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sent)
	}
	p := newThrottle(50*time.Millisecond, func(m string) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, m)
	})
	p.say("Pulling nginx:1.27")
	p.say("Pulling nginx:1.27: 12 of 48 MB")
	p.say("Starting web")
	if got := heard(); !slices.Equal(got, []string{"Pulling nginx:1.27"}) {
		t.Errorf("sent %q at once, want the first message alone", got)
	}
	deadline := time.Now().Add(time.Second)
	for len(heard()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := heard(); !slices.Equal(got, []string{"Pulling nginx:1.27", "Starting web"}) {
		t.Errorf("sent %q, want the latest once the interval ended", got)
	}
	p.say("checking health")
	p.stop()
	time.Sleep(100 * time.Millisecond)
	if got := heard(); len(got) != 2 {
		t.Errorf("sent %q after the call ended", got)
	}
}
