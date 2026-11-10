// SPDX-License-Identifier: AGPL-3.0-only

package health

import (
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// An app is down from when it went bad until it has stayed good for Dwell,
// whatever it goes through between, and keeps going down while it keeps changing.
// A paused app and a job that did its work aren't down.
func TestHowAServiceStandsForAlerts(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := &Recorder{Now: func() time.Time { return now }}
	hear := func(state dockwellv1.HealthState, finished bool) {
		r.Heard("srv_a", []*dockwellv1.InstanceHealth{{Project: "p", Service: "api", State: state, Finished: finished}})
	}
	stand := func() Standing { return r.Standing("srv_a", "p", "api") }
	if st := stand(); st.Heard {
		t.Errorf("a service never heard stands as %+v", st)
	}
	hear(dockwellv1.HealthState_HEALTH_STATE_HEALTHY, false)
	if st := stand(); !st.Heard || st.Down {
		t.Errorf("a healthy service stands as %+v", st)
	}
	now = now.Add(time.Minute)
	went := now
	hear(dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY, false)
	now = now.Add(time.Minute)
	hear(dockwellv1.HealthState_HEALTH_STATE_STARTING, false)
	if st := stand(); !st.Down || !st.Since.Equal(went) {
		t.Errorf("starting again after it went bad stands as %+v, want down since %v", st, went)
	}
	hear(dockwellv1.HealthState_HEALTH_STATE_HEALTHY, false)
	now = now.Add(Dwell / 2)
	if st := stand(); !st.Down {
		t.Errorf("healthy for less than Dwell stands as %+v, want still down", st)
	}
	now = now.Add(Dwell)
	if st := stand(); st.Down {
		t.Errorf("healthy for Dwell stands as %+v, want up", st)
	}
	hear(dockwellv1.HealthState_HEALTH_STATE_PAUSED, false)
	if st := stand(); st.Down {
		t.Errorf("a paused service stands as %+v", st)
	}
	hear(dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, true)
	if st := stand(); st.Down || !st.Finished {
		t.Errorf("a job that did its work stands as %+v", st)
	}
	// One that keeps going between good and bad is down while it does.
	for range 4 {
		now = now.Add(40 * time.Second)
		hear(dockwellv1.HealthState_HEALTH_STATE_HEALTHY, false)
		now = now.Add(5 * time.Second)
		hear(dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY, false)
	}
	now = now.Add(40 * time.Second)
	hear(dockwellv1.HealthState_HEALTH_STATE_HEALTHY, false)
	if st := stand(); !st.Down || !st.Flapping {
		t.Errorf("a service that keeps changing, healthy for 40 seconds now, stands as %+v", st)
	}
}

// A service that comes back in other containers than it went down in was
// replaced, by a deploy or a restart, so a slow start after a stop-first deploy
// isn't down. Docker's own restarts keep the container, so a crash loop is, on one
// container or several.
func TestAReplaceIsntAFallButARestartIs(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := &Recorder{Now: func() time.Time { return now }}
	hear := func(list ...*dockwellv1.InstanceHealth) {
		for _, i := range list {
			i.Project, i.Service = "p", "api"
		}
		r.Heard("srv_a", list)
	}
	in := func(id string, state dockwellv1.HealthState) *dockwellv1.InstanceHealth {
		return &dockwellv1.InstanceHealth{ContainerId: id, State: state}
	}
	stand := func() Standing { return r.Standing("srv_a", "p", "api") }
	const (
		healthy  = dockwellv1.HealthState_HEALTH_STATE_HEALTHY
		starting = dockwellv1.HealthState_HEALTH_STATE_STARTING
		stopped  = dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING
	)

	// Stop-first: the old one stops, a scan catches no container, and the new one
	// takes three minutes to start.
	hear(in("old", healthy))
	now = now.Add(time.Minute)
	hear(in("old", stopped))
	now = now.Add(3 * time.Second)
	hear()
	now = now.Add(2 * time.Second)
	for range 18 {
		hear(in("new", starting))
		if st := stand(); st.Down {
			t.Fatalf("a new container starting after a deploy stands as %+v", st)
		}
		now = now.Add(10 * time.Second)
	}
	hear(in("new", healthy))
	now = now.Add(Dwell)

	// A crash loop keeps its container.
	went := now
	hear(in("new", stopped))
	now = now.Add(time.Minute)
	hear(in("new", starting))
	if st := stand(); !st.Down || !st.Since.Equal(went) {
		t.Errorf("a container Docker restarts stands as %+v, want down since %v", st, went)
	}

	// Two replicas that both crash, whichever says how the service is.
	hear(in("new", healthy), in("two", healthy))
	now = now.Add(Dwell)
	went = now
	hear(in("new", stopped), in("two", stopped))
	now = now.Add(10 * time.Second)
	hear(in("new", starting), in("two", stopped))
	now = now.Add(10 * time.Second)
	hear(in("new", stopped), in("two", starting))
	if st := stand(); !st.Down || st.Since.After(went) {
		t.Errorf("two replicas Docker restarts stand as %+v, want down since %v or before", st, went)
	}
}

// A job that finished is still finished once its container is removed, as docker
// system prune does. Run again, a job that fails before anyone sees it running is
// down, though its state reads as it did.
func TestAFinishedJobRunAgain(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := &Recorder{Now: func() time.Time { return now }}
	hear := func(id string, state dockwellv1.HealthState, finished bool) {
		r.Heard("srv_a", []*dockwellv1.InstanceHealth{{Project: "p", Service: "job", ContainerId: id, State: state, Finished: finished}})
	}
	stand := func() Standing { return r.Standing("srv_a", "p", "job") }
	hear("c1", dockwellv1.HealthState_HEALTH_STATE_NO_CHECK, false)
	now = now.Add(time.Minute)
	hear("c1", dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, true)
	now = now.Add(time.Minute)
	r.Heard("srv_a", nil) // pruned
	now = now.Add(10 * time.Minute)
	if st := stand(); st.Down || !st.Finished {
		t.Errorf("a finished job whose container was removed stands as %+v", st)
	}

	hear("c1", dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, true)
	now = now.Add(time.Minute)
	went := now
	hear("c2", dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, false)
	now = now.Add(5 * time.Minute)
	if st := stand(); !st.Down || !st.Since.Equal(went) || st.Finished {
		t.Errorf("a job run again that failed at once stands as %+v, want down since %v", st, went)
	}
}
