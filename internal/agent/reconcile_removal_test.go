// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent/desired"
)

// docker rm -f kills a container and then removes it. A look that lands between
// the two sees it not running, with no error, which is how one someone stopped
// reads. It isn't a stop: whichever of the agent's looks lands there, the
// container is made again once it's gone, and nothing says it was stopped (the
// re-check of #357; what failed away/clock in the chaos tests).
func TestALookDuringAForcedRemovalIsntAStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		look func(t *testing.T, s *stand)
	}{
		{"the monitor's list", func(t *testing.T, s *stand) { s.noted(t) }},
		{"the reconciler's look, acting", func(t *testing.T, s *stand) { s.pass(t) }},
		{"the reconciler's look, only noting", func(t *testing.T, s *stand) { s.r.pass(t.Context(), false) }},
		{"no look at all", func(*testing.T, *stand) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			s.noted(t)
			midRemoval(s)
			tc.look(t, s)
			if rec, _, _ := s.store.Read(webRef); rec.Stopped {
				t.Fatal("a look during the removal marked it stopped")
			}
			s.e.Remove("web")
			s.pass(t)
			if c := s.e.Container("web"); c == nil || !c.State.Running {
				t.Fatalf("not made again after a forced removal: the journal says %+v", s.said(webRef))
			}
			if got := s.said(webRef); got.What != desired.Made {
				t.Errorf("the journal says %+v; want that it was made again", got)
			}
		})
	}
}

// midRemoval leaves web as a forced removal has it between its kill and its
// going: listed, not running, in the state "removing", with no error.
func midRemoval(s *stand) {
	s.e.Stop("web")
	s.e.Removing("web")
}

// A real stop is still a stop: seen stopped, then removed, it waits.
func TestAStopFollowedByARemovalStillWaits(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	s.noted(t)
	s.e.Removing("web") // docker rm, a while after
	s.noted(t)
	s.e.Remove("web")
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made again what someone stopped and then removed")
	}
}
