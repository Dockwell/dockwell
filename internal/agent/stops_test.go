// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// What tells a container someone stopped from one Docker stopped as it came back
// from being killed (ADR 0054), on the fake engine, which acts the engine's part
// as a real one was seen to play it (docs/testing/chaos.md).

func (s *stand) stands(t *testing.T) desired.Standing {
	t.Helper()
	st, ok := s.store.Stands(webRef)
	if !ok {
		t.Fatal("the agent keeps no record of web")
	}
	return st
}

// webRuns says web's container is there and running.
func (s *stand) webRuns() bool {
	c := s.e.Container("web")
	return c != nil && c.State.Running
}

// lookOnce is one list from the monitor, where noted is two.
func (s *stand) lookOnce(t *testing.T) {
	t.Helper()
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil {
		t.Fatal(err)
	}
	s.r.noted(t.Context(), time.Now(), list)
}

// connected has a stream up, so the agent does nothing on its own but start
// what Docker stopped as it came back.
func (s *stand) connected() { s.presence.set(true, time.Now()) }

// A person's stop, each way one is made, is known for a stop: it isn't started,
// connected or away, and once gone it isn't made again.
func TestAStopSomeoneMadeIsKnown(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, s *stand)
	}{
		{"docker stop", func(t *testing.T, s *stand) {
			if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
				t.Fatal(err)
			}
		}},
		{"docker kill", func(t *testing.T, s *stand) {
			s.e.Kill("web")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			s.connected()
			tc.stop(t, s)
			s.noted(t)
			if st := s.stands(t); !st.Stopped || st.Lost || st.Unsure != "" {
				t.Fatalf("after %s it stands %+v; want stopped, known", tc.name, st)
			}
			s.r.startLost(t.Context())
			s.presence.set(false, time.Now().Add(-time.Hour))
			s.pass(t)
			if s.webRuns() {
				t.Fatal("started what someone stopped")
			}
			s.e.Remove("web")
			s.pass(t)
			if s.e.Container("web") != nil {
				t.Fatal("made again what someone stopped, which starts it")
			}
		})
	}
}

// Docker killed and started again stops what outlived it, and leaves it down
// where its policy isn't always. Nobody asked for that, so the agent starts it,
// with the control plane there too, and says why.
func TestWhatDockerStoppedAsItCameBackIsStarted(t *testing.T) {
	for _, connected := range []bool{true, false} {
		name := "away"
		if connected {
			name = "connected"
		}
		t.Run(name, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			if connected {
				s.connected()
			} else {
				// Just gone: not yet long enough for the agent to make anything again.
				s.presence.set(false, time.Now())
			}
			s.noted(t)
			was := s.e.Container("web").ID
			s.e.DaemonKilled()
			if s.webRuns() {
				t.Fatal("the fake engine brought an unless-stopped container back, which a real one doesn't")
			}
			s.noted(t)
			if st := s.stands(t); !st.Lost || st.Stopped || st.Unsure != "" {
				t.Fatalf("it stands %+v; want lost: Docker stopped it as it came back", st)
			}
			select {
			case <-s.r.lost:
			default:
				t.Error("nothing woke the loop to start it")
			}
			if connected {
				s.r.startLost(t.Context())
			} else {
				s.r.pass(t.Context(), false)
			}
			if c := s.e.Container("web"); c == nil || !c.State.Running || c.ID != was {
				t.Fatalf("after the agent's turn web is %+v; want the same container, running", c)
			}
			if got := s.said(webRef); got.What != desired.Started || !strings.Contains(got.Reason, "came back from being killed") {
				t.Errorf("the journal says %+v; want that it was started, and why", got)
			}
			s.noted(t)
			if st := s.stands(t); st.Down() {
				t.Errorf("running again, it still stands %+v", st)
			}

			// Someone stops it after that: known, and left.
			if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
				t.Fatal(err)
			}
			s.noted(t)
			if st := s.stands(t); !st.Stopped {
				t.Fatalf("stopped by someone after the agent started it, it stands %+v", st)
			}
			s.r.startLost(t.Context())
			s.r.pass(t.Context(), true)
			if s.webRuns() {
				t.Fatal("started what someone stopped after Docker came back")
			}
		})
	}
}

// With live-restore, what ran keeps running through the engine's restart, and the
// engine then holds no start of it. A stop there is someone's, not the engine's.
func TestAStopUnderLiveRestoreIsSomeones(t *testing.T) {
	s := newStand(t)
	s.e.LiveRestore = true
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.e.DaemonKilled()
	if !s.webRuns() {
		t.Fatal("with live-restore the container stopped")
	}
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	s.noted(t)
	if st := s.stands(t); !st.Stopped || st.Lost {
		t.Fatalf("it stands %+v; want stopped by someone", st)
	}
	s.r.startLost(t.Context())
	if s.webRuns() {
		t.Fatal("started what someone stopped")
	}
}

// churn has the engine say n more things of another container, as a busy server
// does, so what it holds of the past no longer reaches back to its start.
func (s *stand) churn(t *testing.T, n int) {
	t.Helper()
	other := resource.Ref{Kind: webRef.Kind, Scope: webRef.Scope, Name: "busy"}
	s.run(t, other, &resourcev1.Container{Image: "nginx:1.27"}, nil)
	for range n / 4 {
		if err := s.e.Client().StopContainer(t.Context(), "busy", time.Second); err != nil {
			t.Fatal(err)
		}
		if err := s.e.Client().StartContainer(t.Context(), "busy"); err != nil {
			t.Fatal(err)
		}
	}
}

// When Docker no longer holds everything since it started, a stop with no start
// before it can be someone's or the engine's. The agent doesn't start it, says so
// on Activity once, and doesn't make it again when it's gone.
func TestAStopItCantPlaceIsLeftAndReported(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.e.DaemonKilled()
	s.churn(t, docker.EventsHeld-20) // web's stop is still held, but not all since the start
	s.noted(t)
	st := s.stands(t)
	if st.Unsure == "" || st.Lost || st.Stopped {
		t.Fatalf("it stands %+v; want unsure", st)
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "can't tell whether someone stopped it") ||
		!strings.Contains(got.Reason, "Start it from the control plane") {
		t.Errorf("the journal says %+v; want that it's down, why it wasn't started, and what to do", got)
	}
	before, _, _ := s.journal.Pending()
	s.noted(t)
	s.r.startLost(t.Context())
	s.presence.set(false, time.Now().Add(-time.Hour))
	s.pass(t)
	if s.webRuns() {
		t.Fatal("started one it can't tell wasn't stopped by someone")
	}
	if after, _, _ := s.journal.Pending(); len(after) != len(before) {
		t.Errorf("said it again: %d entries, then %d", len(before), len(after))
	}
	s.e.Remove("web")
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made again one it can't tell wasn't stopped by someone")
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "couldn't tell") {
		t.Errorf("the journal says %+v", got)
	}

	// Everything about it rolled out of what Docker holds: still unsure.
	gone := newStand(t)
	gone.run(t, webRef, nginx, nil)
	gone.connected()
	gone.e.DaemonKilled()
	gone.churn(t, 2*docker.EventsHeld)
	gone.noted(t)
	if st := gone.stands(t); st.Unsure == "" || !strings.Contains(st.Unsure, "no longer holds what happened to it") {
		t.Fatalf("it stands %+v; want unsure, as Docker holds nothing of it", st)
	}
}

// The agent was down while it happened. Starting again, it asks Docker what it
// still holds: a stop someone made is known, what Docker stopped as it came back
// is started, and one that was already down when Docker last started can't be
// told, so it's left and reported.
func TestWhatHappenedWhileTheAgentWasDown(t *testing.T) {
	again := func(s *stand) {
		// A new agent on the same data: nothing in memory, the records as kept.
		s.r = newReconciler(s.r.cfg, s.presence, s.held)
	}
	t.Run("someone stopped it", func(t *testing.T) {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		s.connected()
		s.noted(t)
		if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
			t.Fatal(err)
		}
		again(s)
		s.noted(t)
		if st := s.stands(t); !st.Stopped {
			t.Fatalf("it stands %+v; want stopped, from Docker's events", st)
		}
		s.r.startLost(t.Context())
		if s.webRuns() {
			t.Fatal("started what someone stopped while the agent was down")
		}
	})
	t.Run("Docker was killed", func(t *testing.T) {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		s.connected()
		s.noted(t)
		s.e.DaemonKilled()
		again(s)
		s.noted(t)
		s.r.startLost(t.Context())
		if !s.webRuns() {
			t.Fatalf("not started: it stands %+v, and the journal says %+v", s.stands(t), s.said(webRef))
		}
	})
	t.Run("someone stopped it, and then Docker restarted", func(t *testing.T) {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		s.connected()
		s.noted(t)
		if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
			t.Fatal(err)
		}
		s.e.DaemonKilled() // and with it, what said someone stopped it
		again(s)
		s.noted(t)
		st := s.stands(t)
		if st.Unsure == "" || !strings.Contains(st.Unsure, "wasn't running when Docker last started") {
			t.Fatalf("it stands %+v; want unsure", st)
		}
		s.r.startLost(t.Context())
		s.presence.set(false, time.Now().Add(-time.Hour))
		s.pass(t)
		if s.webRuns() {
			t.Fatal("started one that may have been stopped by someone")
		}
		if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "can't tell") {
			t.Errorf("the journal says %+v", got)
		}
	})
}

// One that ends by itself, where its policy leaves it, isn't started, and isn't
// made again once it's gone: a job that finished doesn't run again.
func TestWhatEndedByItselfStaysEnded(t *testing.T) {
	s := newStand(t)
	job := &resourcev1.Container{Image: "nginx:1.27", Restart: resourcev1.RestartPolicy_RESTART_POLICY_NO}
	s.run(t, webRef, job, nil)
	s.connected()
	s.e.Exit("web", 0)
	s.noted(t)
	if st := s.stands(t); !st.Stopped {
		t.Fatalf("it stands %+v; want ended, which is kept as stopped", st)
	}
	s.presence.set(false, time.Now().Add(-time.Hour))
	s.e.Remove("web")
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("ran a finished job again")
	}
}

// A kill on its way to a removal, as docker rm -f makes, shows for a moment as a
// stopped container. One list isn't enough to settle a stop, so it's made again.
func TestAKillOnItsWayToARemovalIsntAStop(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.noted(t)
	s.e.Kill("web")
	s.lookOnce(t) // the one list that lands between the kill and the removal
	s.e.Remove("web")
	s.lookOnce(t)
	if st := s.stands(t); st.Down() {
		t.Fatalf("it stands %+v after one look at a container on its way out", st)
	}
	s.pass(t)
	if !s.webRuns() {
		t.Fatalf("not made again: %+v", s.said(webRef))
	}
}

// One Docker couldn't start isn't a stop: it's left for the agent to start while
// the control plane is away, and read in full once, not at every list.
func TestAFailedStartIsReadOnceAndIsntAStop(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.e.FailedToStart("web", "driver failed programming external connectivity: port is already allocated")
	id := s.e.Container("web").ID
	reads := 0
	s.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/"+id+"/json") {
			reads++
		}
		return nil
	})
	for range 5 {
		s.noted(t)
	}
	if reads != 1 {
		t.Errorf("read in full %d times over ten lists, want once", reads)
	}
	if st := s.stands(t); st.Down() {
		t.Fatalf("it stands %+v; a failed start isn't a stop", st)
	}
	s.e.Intercept(nil)
	s.presence.set(false, time.Now().Add(-time.Hour))
	s.pass(t)
	if !s.webRuns() {
		t.Fatalf("not started while away: %+v", s.said(webRef))
	}
}

// Docker not answering settles nothing: the next list asks again.
func TestDockerNotAnsweringSettlesNothing(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.e.DaemonKilled()
	s.e.EventsFail = true
	s.noted(t)
	if st := s.stands(t); st.Down() {
		t.Fatalf("it stands %+v with Docker's events unread", st)
	}
	s.e.EventsFail = false
	s.noted(t)
	if st := s.stands(t); !st.Lost {
		t.Fatalf("it stands %+v once Docker answered; want lost", st)
	}
}

// The other ways a container reads stopped with nobody having stopped it, and
// what the rule makes of each (ADR 0054, "What it doesn't tell apart").

// A server shutting down: Docker stops each container while it still answers,
// saying of each what it says of a person's stop, after the container's start.
// Two lists in that time take it for someone's stop. That's wrong, and it doesn't
// last: the agent starts nothing for it, and once Docker has started it again at
// boot it reads as running, with nothing on Activity.
func TestAShutdownsStopIsForgottenAtBoot(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.noted(t)
	s.e.ShuttingDown()
	s.noted(t)
	if st := s.stands(t); !st.Stopped {
		t.Fatalf("during the shutdown it stands %+v; the rule is known to read this as a stop", st)
	}
	s.r.startLost(t.Context())
	if s.webRuns() {
		t.Fatal("the agent started a container during a shutdown")
	}
	s.e.Reboot()
	again := newReconciler(s.r.cfg, s.presence, s.held) // the agent, started at boot
	s.r = again
	s.noted(t)
	if st := s.stands(t); st.Down() {
		t.Fatalf("after the boot, running, it stands %+v", st)
	}
	if !s.webRuns() {
		t.Fatal("Docker didn't bring it back at boot")
	}
	if entries, _, _ := s.journal.Pending(); len(entries) != 0 {
		t.Errorf("Activity is told %+v of a shutdown and a boot", entries)
	}
}

// docker restart by hand stops and starts in one. Lists that land between the two
// take it for a stop, and the next one, with it running, forgets that.
func TestARestartByHandLeavesNothingBehind(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.noted(t)
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	s.noted(t) // two lists in the gap, which a slow stop can leave
	if err := s.e.Client().StartContainer(t.Context(), "web"); err != nil {
		t.Fatal(err)
	}
	s.noted(t)
	if st := s.stands(t); st.Down() {
		t.Fatalf("restarted and running, it stands %+v", st)
	}
	if entries, _, _ := s.journal.Pending(); len(entries) != 0 {
		t.Errorf("Activity is told %+v of a restart by hand", entries)
	}
}

// A container that's made and never started, outside any step, as docker create
// by hand leaves one, hasn't stopped: the rule says nothing of it, and the agent
// neither starts it nor writes anything down.
func TestOneMadeAndNeverStartedIsntAStop(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.e.NeverStarted("web")
	for range 3 {
		s.noted(t)
	}
	if st := s.stands(t); st.Down() {
		t.Fatalf("made and never started, it stands %+v", st)
	}
	s.r.startLost(t.Context())
	s.presence.set(false, time.Now().Add(-time.Hour))
	s.pass(t)
	if s.webRuns() {
		t.Fatal("the agent started a container someone made and didn't start")
	}
	if entries, _, _ := s.journal.Pending(); len(entries) != 0 {
		t.Errorf("Activity is told %+v", entries)
	}
}

// Two containers answering to one service, as a twin made by hand: the service
// runs if either does, whichever the list has last, so a stopped twin doesn't
// have the one that runs taken for stopped.
func TestAStoppedTwinDoesntHideOneThatRuns(t *testing.T) {
	for _, twinLast := range []bool{true, false} {
		s := newStand(t)
		s.run(t, webRef, nginx, nil)
		s.connected()
		list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
		if err != nil || len(list) != 1 {
			t.Fatalf("listed %d containers, %v", len(list), err)
		}
		twin := list[0]
		twin.ID, twin.State = "twin-made-by-hand", "exited"
		both := []docker.ContainerSummary{twin, list[0]}
		if twinLast {
			both = []docker.ContainerSummary{list[0], twin}
		}
		asked := 0
		s.e.Intercept(func(r *http.Request) error {
			if strings.Contains(r.URL.Path, twin.ID) {
				asked++
			}
			return nil
		})
		for range 3 {
			s.r.noted(t.Context(), time.Now(), both)
		}
		if st := s.stands(t); st.Down() {
			t.Fatalf("with the twin last=%t, the service stands %+v though one of its containers runs", twinLast, st)
		}
		// The twin isn't here to be read, so what shows the service was taken
		// for one that runs is that nothing was asked of the twin.
		if asked != 0 {
			t.Errorf("with the twin last=%t, Docker was asked about the stopped twin %d times, as if the service had stopped", twinLast, asked)
		}
	}
}

// A mark outlasts a step that didn't start the container, and a fill, as how it
// stands is a look's to say: lost stays lost, and is still started.
func TestALostMarkOutlastsAFill(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.noted(t)
	s.e.DaemonKilled()
	s.noted(t)
	rec, _, _ := s.store.Read(webRef)
	if !rec.Lost {
		t.Fatalf("the record says %+v", rec)
	}
	rec.Lost = false // a fill says what should run, not how it stands
	if err := s.store.Keep(rec); err != nil {
		t.Fatal(err)
	}
	if st := s.stands(t); !st.Lost {
		t.Fatalf("after a fill it stands %+v; want lost still", st)
	}
	s.r.startLost(t.Context())
	if !s.webRuns() {
		t.Fatal("not started after a fill")
	}
}

// A service deployed more than once has a container numbered past 1, and Docker
// killed under it is started all the same. On a real engine the first try at the
// rule left exactly such a service down: the list was read for number 1 only,
// and the fake's stand never deploys twice (the chaos tests' away/kill-docker).
func TestWhatDockerStoppedIsStartedWhateverItsNumber(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.connected()
	s.e.DaemonKilled()
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil || len(list) != 1 {
		t.Fatalf("listed %d containers, %v", len(list), err)
	}
	// As the list has it after eight rollouts: the same container, numbered 9.
	list[0].Labels = maps.Clone(list[0].Labels)
	list[0].Labels[docker.LabelNumber] = "9"
	s.r.noted(t.Context(), time.Now(), list)
	s.r.noted(t.Context(), time.Now(), list)
	if st := s.stands(t); !st.Lost {
		t.Fatalf("numbered 9, it stands %+v; want lost", st)
	}
	s.r.startLost(t.Context())
	if !s.webRuns() {
		t.Fatalf("not started: the journal says %+v", s.said(webRef))
	}
}

// More than one container answers to a service, none runs, and nothing says
// someone stopped any: as after Docker was killed with a failed rollout's leftover
// beside the one that ran. The agent can't say which is the service, so it starts
// none, the leftover least of all, and says so once. A stop someone made of the
// one that ran is still known for a stop, whichever stands now.
func TestWithMoreThanOneAndNoStopKnownNothingIsStarted(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	ran := s.e.Container("web").ID
	s.revision = "2"
	left := s.another(t, "2", "failed")
	s.connected()
	s.noted(t)
	s.e.DaemonKilled()
	s.noted(t)
	st := s.stands(t)
	if st.Unsure == "" || st.Lost || st.Stopped || !strings.Contains(st.Unsure, "more than one container") {
		t.Fatalf("it stands %+v; want unsure, for more than one container", st)
	}
	s.r.startLost(t.Context())
	s.presence.set(false, time.Now().Add(-time.Hour))
	s.pass(t)
	s.nothingStarted(t, ran, left)

	stopped := newStand(t)
	stopped.run(t, webRef, nginx, nil)
	live := stopped.e.Container("web").ID
	stopped.revision = "2"
	stopped.another(t, "2", "failed")
	stopped.connected()
	if err := stopped.e.Client().StopContainer(t.Context(), live, time.Second); err != nil {
		t.Fatal(err)
	}
	stopped.noted(t)
	if st := stopped.stands(t); !st.Stopped {
		t.Fatalf("someone stopped the one that ran, and it stands %+v", st)
	}
}
