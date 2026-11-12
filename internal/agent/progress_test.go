// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
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
	p := newThrottle(50*time.Millisecond, func(m resource.Activity) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, m.Message)
	})
	p.say(resource.Activity{Message: "Pulling nginx:1.27"})
	p.say(resource.Activity{Message: "Pulling nginx:1.27: 12 of 48 MB"})
	p.say(resource.Activity{Message: "Starting web"})
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
	p.say(resource.Activity{Message: "checking health"})
	p.stop()
	time.Sleep(100 * time.Millisecond)
	if got := heard(); len(got) != 2 {
		t.Errorf("sent %q after the call ended", got)
	}
}

// A new phase goes at once, after what waited, so each phase a step reaches shows
// however soon the step ends, with the last of a pull's bytes before it.
func TestAPhaseGoesAtOnce(t *testing.T) {
	var mu sync.Mutex
	var sent []resource.Activity
	p := newThrottle(time.Hour, func(a resource.Activity) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, a)
	})
	p.say(resource.Activity{Message: "Pulling shop:1.5", Phase: resource.PhasePulling})
	p.say(resource.Activity{Message: "Pulling shop:1.5: 12 of 48 MB", Phase: resource.PhasePulling, Done: 12, Total: 48})
	p.say(resource.Activity{Message: "Pulling shop:1.5: 24 of 48 MB", Phase: resource.PhasePulling, Done: 24, Total: 48})
	p.say(resource.Activity{Message: "Starting web", Phase: resource.PhaseStarting})
	p.say(resource.Activity{Message: "Checking that web stays up", Phase: resource.PhaseChecking})
	p.stop()
	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, a := range sent {
		got = append(got, a.Message)
	}
	want := []string{"Pulling shop:1.5", "Pulling shop:1.5: 24 of 48 MB", "Starting web", "Checking that web stays up"}
	if !slices.Equal(got, want) {
		t.Errorf("sent %q, want %q", got, want)
	}
}
