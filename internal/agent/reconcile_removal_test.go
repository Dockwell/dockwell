// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"maps"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
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

// A service deployed more than once has a container numbered past 1: each
// rollout that starts the new one first numbers it one on. The monitor's list
// still notes how it stands, so what someone stops while the control plane is
// there isn't made again once it's away. Before, only number 1 was noted, which
// left every redeployed service out (found by the chaos tests' second Docker
// kill, where a service on its ninth revision was never looked at).
func TestARedeployedServicesContainerIsNoted(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.presence.set(true, time.Now())
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil || len(list) != 1 {
		t.Fatalf("listed %d containers, %v", len(list), err)
	}
	// As the list has it after five rollouts: the same container, numbered 6.
	list[0].Labels = maps.Clone(list[0].Labels)
	list[0].Labels[docker.LabelNumber] = "6"
	s.r.noted(t.Context(), time.Now(), list)
	s.r.noted(t.Context(), time.Now(), list)
	if rec, _, _ := s.store.Read(webRef); !rec.Stopped {
		t.Fatal("a redeployed service's container, stopped by someone, wasn't noted as stopped")
	}

	// With an older one of the same service still listed, whichever comes first,
	// it's the highest number that says how the service stands.
	for _, olderLast := range []bool{true, false} {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		s.presence.set(true, time.Now())
		list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
		if err != nil || len(list) != 1 {
			t.Fatalf("listed %d containers, %v", len(list), err)
		}
		current := list[0]
		current.Labels = maps.Clone(current.Labels)
		current.Labels[docker.LabelNumber] = "6"
		older := list[0]
		older.ID, older.State = "an-older-one-left-behind", "exited"
		older.Labels = maps.Clone(older.Labels)
		older.Labels[docker.LabelNumber] = "5"
		both := []docker.ContainerSummary{older, current}
		if olderLast {
			both = []docker.ContainerSummary{current, older}
		}
		s.r.noted(t.Context(), time.Now(), both)
		s.r.noted(t.Context(), time.Now(), both)
		if rec, _, _ := s.store.Read(webRef); rec.Stopped {
			t.Fatalf("with the older one last=%t, a running service was noted as stopped", olderLast)
		}
	}
}
