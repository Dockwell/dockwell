// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"maps"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
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

// another makes a second container answering to web's service, as a rollout that
// was cut short, or a hand, leaves one: numbered number, and left as state has
// it. It returns its ID.
func (s *stand) another(t *testing.T, number, state string) string {
	t.Helper()
	web := s.e.Container("web")
	if web == nil {
		t.Fatal("there's no web to make another of")
	}
	cfg := web.Config
	cfg.Labels = maps.Clone(cfg.Labels)
	cfg.Labels[docker.LabelNumber] = number
	if s.revision != "" {
		// As a deploy of another revision made it, or, with "none", a hand did.
		cfg.Labels[docker.LabelRevision] = s.revision
		if s.revision == "none" {
			delete(cfg.Labels, docker.LabelRevision)
		}
	}
	if s.adopted {
		cfg.Labels[docker.LabelAdopted] = "web"
	}
	if s.otherImage {
		// As a pull that found a newer image made it: the same tag, another image.
		s.e.Registry["nginx:1.28"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx", "-g", "daemon off;"}}}
		if err := s.e.Client().PullImage(t.Context(), "nginx:1.28", nil); err != nil {
			t.Fatal(err)
		}
		cfg.Image = "nginx:1.28"
	}
	name := "web-another-" + number + "-" + state
	id, err := s.e.Client().CreateContainer(t.Context(), name, &docker.CreateRequest{Config: cfg, HostConfig: web.HostConfig})
	if err != nil {
		t.Fatal(err)
	}
	switch state {
	case "created":
	case "failed":
		// Made, and Docker couldn't start it: a start-first rollout's new
		// container whose start was refused, left for a rollback that didn't come.
		s.e.FailedToStart(name, "driver failed programming external connectivity: port is already allocated")
	case "running", "exited", "dead", "removing":
		if err := s.e.Client().StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		switch state {
		case "exited":
			s.e.Stop(name)
		case "dead":
			s.e.Stop(name)
			s.e.Dead(name)
		case "removing":
			s.e.Stop(name)
			s.e.Removing(name)
		}
	default:
		t.Fatalf("no such state to leave one in: %s", state)
	}
	return id
}

// A start-first rollout that fails, and can't remove the container it made,
// leaves a higher-numbered one dead beside the one that runs. It mustn't stand
// for the service: a stop of the live one while the control plane is there is
// noted all the same, so once both are pruned the agent doesn't make and start
// it (the review of #411, its high).
func TestALeavingLeftoverDoesntHideAStop(t *testing.T) {
	for _, state := range []string{"dead", "removing"} {
		t.Run(state, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			live := s.e.Container("web").ID
			left := s.another(t, "2", state)
			s.presence.set(true, time.Now())
			s.noted(t)
			if rec, _, _ := s.store.Read(webRef); rec.Stopped {
				t.Fatal("a running service was noted as stopped for its leftover")
			}
			if err := s.e.Client().StopContainer(t.Context(), live, time.Second); err != nil {
				t.Fatal(err)
			}
			s.noted(t)
			if rec, _, _ := s.store.Read(webRef); !rec.Stopped {
				t.Fatalf("someone stopped the live container, and the %s leftover hid it", state)
			}
			// A prune takes both, and the control plane goes away.
			s.e.Remove(left)
			s.e.Remove(live)
			s.presence.set(false, time.Now().Add(-time.Hour))
			s.pass(t)
			if c := s.e.Container("web"); c != nil {
				t.Fatalf("made again what someone stopped: %+v", c.State)
			}
			if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "It was stopped") {
				t.Errorf("the journal says %+v", got)
			}
		})
	}
}

// A rollout cut short after it made the new container leaves that one made and
// never started beside the one that runs. The service runs: it isn't noted as
// stopped, by the monitor's list or by the reconciler's own look, and nothing is
// started (the review of #411, its medium).
func TestALeftoverThatNeverRanDoesntMarkARunningServiceStopped(t *testing.T) {
	for _, state := range []string{"created", "exited"} {
		t.Run(state, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			live := s.e.Container("web").ID
			left := s.another(t, "2", state)
			s.presence.set(true, time.Now())
			s.noted(t)
			s.presence.set(false, time.Now().Add(-time.Hour))
			s.pass(t)
			if rec, _, _ := s.store.Read(webRef); rec.Stopped {
				t.Fatalf("a running service was noted as stopped for a higher-numbered container that's %s", state)
			}
			st, err := s.local.Reviver(webRef).RunState(t.Context(), webRef)
			if err != nil || st.ID != live || !st.Running {
				t.Fatalf("the driver's own look reads %+v, %v; want the live container, %s", st, err, live)
			}
			if c, err := s.e.Client().InspectContainer(t.Context(), left); err != nil || c.State.Running {
				t.Fatalf("the leftover is %+v, %v; want it left as it was", c, err)
			}
		})
	}
}

// Two with one number, as a stop-first swap cut short leaves them: whichever the
// list has first, the monitor's list and the driver's own look take the same one,
// the one that runs, and with neither running the same one again.
func TestATieIsDecidedTheSameWayByBothLooks(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	live := s.e.Container("web").ID
	s.another(t, "1", "exited")
	s.presence.set(true, time.Now())
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil || len(list) != 2 {
		t.Fatalf("listed %d containers, %v", len(list), err)
	}
	for _, order := range [][]docker.ContainerSummary{{list[0], list[1]}, {list[1], list[0]}} {
		s.r.noted(t.Context(), time.Now(), order)
		s.r.noted(t.Context(), time.Now(), order)
		if rec, _, _ := s.store.Read(webRef); rec.Stopped {
			t.Fatalf("with %s first, a running service was noted as stopped", order[0].ID[:8])
		}
	}
	if st, err := s.local.Reviver(webRef).RunState(t.Context(), webRef); err != nil || st.ID != live {
		t.Fatalf("the driver's own look reads %+v, %v; want the one that runs", st, err)
	}

	// Neither running: both looks still take one and the same.
	if err := s.e.Client().StopContainer(t.Context(), live, time.Second); err != nil {
		t.Fatal(err)
	}
	list, err = s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil || len(list) != 2 {
		t.Fatalf("listed %d containers, %v", len(list), err)
	}
	want := list[0].Stand()
	if list[1].Stand().Over(want) {
		want = list[1].Stand()
	}
	st, err := s.local.Reviver(webRef).RunState(t.Context(), webRef)
	if err != nil || st.ID != want.ID {
		t.Fatalf("the driver's own look reads %s, %v; the list's choice is %s", st.ID, err, want.ID)
	}
}

// Dead alone, with nothing else answering to the service: no news to either look.
// It's not noted as stopped, nothing is started, and nothing is written down.
func TestOneThatsDeadAloneIsNoNews(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.noted(t)
	s.e.Stop("web")
	s.e.Dead("web")
	s.noted(t)
	s.pass(t)
	if rec, _, _ := s.store.Read(webRef); rec.Stopped {
		t.Fatal("a dead container was noted as stopped")
	}
	if entries, _, _ := s.journal.Pending(); len(entries) != 0 {
		t.Errorf("the journal says %+v of a dead container", entries)
	}
}

// The driver's own look says a container on its way out is leaving, and reads a
// live one beside it instead.
func TestTheDriversLookSaysLeaving(t *testing.T) {
	for _, state := range []string{"dead", "removing"} {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		live := s.e.Container("web").ID
		left := s.another(t, "2", state)
		rv := s.local.Reviver(webRef)
		if st, err := rv.RunState(t.Context(), webRef); err != nil || st.Leaving || st.ID != live {
			t.Fatalf("with a %s one beside a live one, it reads %+v, %v; want the live one", state, st, err)
		}
		s.e.Remove(live)
		if st, err := rv.RunState(t.Context(), webRef); err != nil || !st.Exists || !st.Leaving || st.ID != left {
			t.Fatalf("with only a %s one, it reads %+v, %v; want it, leaving", state, st, err)
		}
	}
}

// Deployed twice through the driver, starting the new container first: the second
// is numbered 2, by the driver's own numbering, and it's still noted. Stopped by
// someone while the control plane is there, and pruned, it waits once it's away.
// This is the fault as a server has it; at #357 it was made again and started.
func TestAServiceDeployedTwiceIsStillNoted(t *testing.T) {
	s := newStand(t)
	api := resource.Ref{Kind: service.Kind, Scope: standEnv, Name: "api"}
	deploy := func(n string) *resourcev1.Service {
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "nginx:1.27", Env: map[string]string{"DEPLOY": n},
			Rollout: &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST},
		}}}
	}
	s.run(t, api, deploy("1"), nil)
	s.run(t, api, deploy("2"), nil)
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil || len(list) != 1 {
		t.Fatalf("after two deploys there are %d containers, %v; want the second alone", len(list), err)
	}
	if n := list[0].Number(); n < 2 {
		t.Fatalf("the second deploy's container is numbered %d; this test needs the driver to number it on", n)
	}
	s.presence.set(true, time.Now())
	s.noted(t)
	if err := s.e.Client().StopContainer(t.Context(), list[0].ID, time.Second); err != nil {
		t.Fatal(err)
	}
	s.noted(t)
	if rec, _, _ := s.store.Read(api); !rec.Stopped {
		t.Fatal("stopped by someone, a twice-deployed service wasn't noted as stopped")
	}
	s.e.Remove(list[0].ID)
	s.presence.set(false, time.Now().Add(-time.Hour))
	s.pass(t)
	if again, _ := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}}); len(again) != 0 {
		t.Fatalf("made again what someone stopped: %d containers", len(again))
	}
	if got := s.said(api); got.What != desired.Waiting || !strings.Contains(got.Reason, "It was stopped") {
		t.Errorf("the journal says %+v", got)
	}
}

// The final check of #357: Docker's error on a container is news of its service,
// which the agent may act on by starting it, only for one that stands alone and
// is of a revision the record covers (docker.CouldntStart). Each of these had the
// agent start, or could have had it start, what a person stopped.

// started says whether anything was started, or written down as started.
func (s *stand) nothingStarted(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if c, err := s.e.Client().InspectContainer(t.Context(), id); err == nil && c.State.Running {
			t.Errorf("%s runs: the agent started it", id[:12])
		}
	}
	entries, _, _ := s.journal.Pending()
	for i := range entries {
		if e := &entries[i]; e.What == desired.Started || e.What == desired.Made {
			t.Errorf("the journal says %+v", *e)
		}
	}
}

// A start-first deploy that failed leaves its new container, numbered one on,
// with Docker's error, beside the one that runs; the rollback that would remove
// it hasn't come. A person stops the one that runs. Neither runs now, and the
// higher number stands, with an error that reads as "Docker couldn't start it":
// the agent must not start it, as that starts a service a person stopped, at a
// revision that never passed. The stop is what's noted.
func TestAFailedRolloutsLeftoverIsntStartedForAServiceSomeoneStopped(t *testing.T) {
	for _, connected := range []bool{true, false} {
		for _, revision := range []string{"2", "1", "none"} {
			name := "away"
			if connected {
				name = "connected"
			}
			t.Run(name+", the leftover of revision "+revision, func(t *testing.T) {
				s := newStand(t)
				s.run(t, webRef, nginx, nil)
				live := s.e.Container("web").ID
				s.revision = revision
				left := s.another(t, "2", "failed")
				s.presence.set(connected, time.Now().Add(-time.Hour))
				s.noted(t)
				if err := s.e.Client().StopContainer(t.Context(), live, time.Second); err != nil {
					t.Fatal(err)
				}
				s.noted(t)
				s.presence.set(false, time.Now().Add(-time.Hour))
				s.pass(t)
				s.r.tries = map[resource.Ref]*tries{}
				s.pass(t)
				s.nothingStarted(t, live, left)
				if rec, _, _ := s.store.Read(webRef); !rec.Stopped {
					t.Error("the stop of the one that ran wasn't noted")
				}
			})
		}
	}
}

// The mirror image: the lower number is the one Docker couldn't start, and a
// newer one was stopped by someone. And both with an error. With more than one
// container answering to a service the agent starts none of them: which is the
// service is the control plane's to say.
func TestWithMoreThanOneContainerNothingIsStarted(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{"the older couldn't start, the newer was stopped", "failed", "exited"},
		{"neither could start", "failed", "failed"},
		{"the older was stopped, the newer couldn't start", "exited", "failed"},
		{"the older couldn't start, the newer was only made", "failed", "created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			first := s.e.Container("web").ID
			switch tc.first {
			case "failed":
				s.e.FailedToStart("web", "port is already allocated")
			case "exited":
				s.e.Stop("web")
			}
			second := s.another(t, "2", tc.second)
			s.presence.set(true, time.Now())
			s.noted(t)
			s.presence.set(false, time.Now().Add(-time.Hour))
			s.pass(t)
			s.r.tries = map[resource.Ref]*tries{}
			s.pass(t)
			s.nothingStarted(t, first, second)
		})
	}
}

// Alone, a container Docker couldn't start is started when its record covers it:
// made for a revision no later than the record's, or taken on, or, saying no
// revision, as one a release from before that label made, of the record's own
// image. One the record doesn't cover is left down: not started, not noted as
// stopped, as nobody stopped it, and Activity is told once why. Of another image
// than the record's it's never covered, whatever it says: a pull that found a
// newer image labels its container with the revision the record already has.
func TestOneThatCouldntStartIsStartedOnlyWhenTheRecordCoversIt(t *testing.T) {
	for _, tc := range []struct {
		name                string
		revision            string
		adopted, otherImage bool
		record              string // what the record says of its image: "id", "digest" or "nothing"
		started             bool
		why                 string
	}{
		{"the record's own revision", "1", false, false, "id", true, ""},
		{"a later revision, which never passed", "2", false, false, "id", false, "later revision"},
		{"taken on, with no revision", "none", true, false, "id", true, ""},
		{"taken on, and a record with only a digest", "none", true, false, "digest", true, ""},
		{"taken on, and a record that names no image", "none", true, false, "nothing", true, ""},
		{"no revision, of the record's image, as an older release made it", "none", false, false, "id", true, ""},
		{"no revision, of the record's image by its digest, as after a fill", "none", false, false, "digest", true, ""},
		{"no revision, and a record that names no image", "none", false, false, "nothing", false, "nothing on it says"},
		{"a revision that isn't one, of the record's image", "not a number", false, false, "id", true, ""},
		{"no revision, of another image", "none", false, true, "id", false, "another image"},
		{"the record's revision, of another image, as a pull that never passed", "1", false, true, "id", false, "another image"},
		{"the record's revision, of another image by its digest", "1", false, true, "digest", false, "another image"},
		{"taken on, of another image", "none", true, true, "id", false, "another image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			own := s.e.Container("web").ID
			rec, _, _ := s.store.Read(webRef)
			switch tc.record {
			case "digest":
				rec.Image = resource.Image{Digest: rec.Image.Digest}
			case "nothing":
				rec.Image = resource.Image{}
			}
			if err := s.store.Keep(rec); err != nil {
				t.Fatal(err)
			}
			s.revision, s.adopted, s.otherImage = tc.revision, tc.adopted, tc.otherImage
			only := s.another(t, "2", "failed")
			s.e.Remove(own) // what ran is gone, and this is all there is of the service
			s.presence.set(false, time.Now().Add(-time.Hour))
			s.noted(t)
			if rec, _, _ := s.store.Read(webRef); rec.Stopped {
				t.Fatal("right after the monitor's list, one Docker couldn't start is noted as stopped")
			}
			s.pass(t)
			s.pass(t)
			c, err := s.e.Client().InspectContainer(t.Context(), only)
			if err != nil {
				t.Fatal(err)
			}
			if c.State.Running != tc.started {
				t.Fatalf("it runs=%t; want %t (the journal says %+v)", c.State.Running, tc.started, s.said(webRef))
			}
			if tc.started {
				return
			}
			if rec, _, _ := s.store.Read(webRef); rec.Stopped {
				t.Error("left down, it was noted as stopped, which nobody did")
			}
			entries, _, _ := s.journal.Pending()
			if len(entries) != 1 || entries[0].What != desired.Waiting || !strings.Contains(entries[0].Reason, "the agent leaves it down") ||
				!strings.Contains(entries[0].Reason, tc.why) || !strings.Contains(entries[0].Reason, "Start it from the control plane") {
				t.Errorf("after two looks the journal says %+v; want one entry that it was left down, with %q, and what to do", entries, tc.why)
			}
			if tc.record == "nothing" {
				return // with no image in its record, it couldn't be made again either
			}
			// Gone later, it's made again from its record, as nobody stopped it.
			s.e.Remove(only)
			s.pass(t)
			if c := s.e.Container("web"); c == nil || !c.State.Running {
				t.Errorf("gone later, it wasn't made again: %+v", s.said(webRef))
			}
		})
	}

	// A record moved on by a fill, with the container left as it was: still its own.
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	rec, _, _ := s.store.Read(webRef)
	rec.Revision = 5
	if err := s.store.Keep(rec); err != nil {
		t.Fatal(err)
	}
	s.e.FailedToStart("web", "port is already allocated")
	s.pass(t)
	if c := s.e.Container("web"); c == nil || !c.State.Running {
		t.Fatalf("a container of an earlier revision than its record's, which a fill leaves, wasn't started: %+v", s.said(webRef))
	}
}

// What the monitor's list notes, read right after it and before any look of the
// reconciler's, which would correct it. A stop someone made while a failed
// rollout's leftover sits beside the one that ran is noted as a stop at once: if
// it weren't, and both were pruned before the control plane went away, the
// service would be made again. One Docker couldn't start, alone, isn't a stop.
func TestWhatTheMonitorsListNotesOfAnError(t *testing.T) {
	stopped := func(t *testing.T, s *stand) bool {
		t.Helper()
		rec, ok, err := s.store.Read(webRef)
		if err != nil || !ok {
			t.Fatalf("the record reads %t, %v", ok, err)
		}
		return rec.Stopped
	}
	for _, revision := range []string{"2", "1", "none"} {
		t.Run("a leftover of revision "+revision+" beside one a person stops", func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			live := s.e.Container("web").ID
			s.revision = revision
			left := s.another(t, "2", "failed")
			s.presence.set(true, time.Now())
			s.noted(t)
			if stopped(t, s) {
				t.Fatal("with the service running, its leftover had it noted as stopped")
			}
			if err := s.e.Client().StopContainer(t.Context(), live, time.Second); err != nil {
				t.Fatal(err)
			}
			s.noted(t)
			if !stopped(t, s) {
				t.Fatal("right after the list, a stop someone made isn't noted: the leftover's error was taken for news")
			}
			// Both pruned, and only then the control plane goes away: it waits.
			s.e.Remove(left)
			s.e.Remove(live)
			s.presence.set(false, time.Now().Add(-time.Hour))
			s.pass(t)
			if c := s.e.Container("web"); c != nil {
				t.Fatal("made again what someone stopped")
			}
		})
	}
	t.Run("one Docker couldn't start, alone", func(t *testing.T) {
		for _, revision := range []string{"", "2", "none"} {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			own := s.e.Container("web").ID
			s.revision = revision
			if revision == "" {
				s.e.FailedToStart("web", "port is already allocated")
			} else {
				s.another(t, "2", "failed")
				s.e.Remove(own)
			}
			s.presence.set(true, time.Now())
			s.noted(t)
			if stopped(t, s) {
				t.Fatalf("of revision %q, alone, one Docker couldn't start is noted as stopped", revision)
			}
		}
	})
}

// A twin made by hand that Docker couldn't start, beside the real one, stopped
// by someone: not started, and the stop is noted.
func TestAHandMadeTwinThatCouldntStartIsntStarted(t *testing.T) {
	for _, number := range []string{"1", "2", "none"} {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		live := s.e.Container("web").ID
		s.revision = "none"
		twin := s.another(t, number, "failed")
		s.presence.set(true, time.Now())
		if err := s.e.Client().StopContainer(t.Context(), live, time.Second); err != nil {
			t.Fatal(err)
		}
		s.noted(t)
		s.presence.set(false, time.Now().Add(-time.Hour))
		s.pass(t)
		s.nothingStarted(t, live, twin)
		if rec, _, _ := s.store.Read(webRef); !rec.Stopped {
			t.Errorf("with a twin numbered %s, the stop of the real one wasn't noted", number)
		}
	}
}

// A dead container numbered above one Docker couldn't start: the one that
// couldn't start stands alone, as the dead one is leaving, and it's the one the
// agent starts, not the dead one the driver's own order has first.
func TestAStartIsOfTheOneThatStandsNotADeadOneAboveIt(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	own := s.e.Container("web").ID
	dead := s.another(t, "2", "dead")
	s.e.FailedToStart("web", "port is already allocated")
	st, err := s.local.Reviver(webRef).RunState(t.Context(), webRef)
	if err != nil || st.ID != own || !st.Sole || st.Leaving || st.Failed == "" {
		t.Fatalf("the driver's look reads %+v, %v; want the one that couldn't start, alone", st, err)
	}
	s.pass(t)
	if c, _ := s.e.Client().InspectContainer(t.Context(), own); c == nil || !c.State.Running {
		t.Fatalf("the one that couldn't start wasn't started: %+v", s.said(webRef))
	}
	if c, _ := s.e.Client().InspectContainer(t.Context(), dead); c != nil && c.State.Running {
		t.Fatal("the dead one was started")
	}
}

// The agent's own read with a dead number 2 beside a stopped number 1 reads the
// stopped one, not leaving: a leaving container doesn't compete for the service.
func TestTheDriversLookReadsAStoppedOneNotADeadOneAboveIt(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	own := s.e.Container("web").ID
	s.another(t, "2", "dead")
	s.e.Stop("web")
	st, err := s.local.Reviver(webRef).RunState(t.Context(), webRef)
	if err != nil || st.ID != own || st.Leaving || st.Running || !st.Sole {
		t.Fatalf("the driver's look reads %+v, %v; want the stopped one, alone and not leaving", st, err)
	}
}
