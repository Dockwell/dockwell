// SPDX-License-Identifier: AGPL-3.0-only

package health

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	agenthealth "github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const (
	unknown    = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
	noCheck    = dockwellv1.HealthState_HEALTH_STATE_NO_CHECK
	starting   = dockwellv1.HealthState_HEALTH_STATE_STARTING
	healthy    = dockwellv1.HealthState_HEALTH_STATE_HEALTHY
	unhealthy  = dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY
	notRunning = dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING
	paused     = dockwellv1.HealthState_HEALTH_STATE_PAUSED
)

// reports is what servers last said, as Agents keeps it.
type reports struct {
	mu        sync.Mutex
	said      map[string][]*dockwellv1.InstanceHealth
	connected map[string]bool
}

func (r *reports) Health(server string) (agenthealth.Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	said, ok := r.said[server]
	return agenthealth.Report{Said: ok, Instances: said}, r.connected[server]
}

type agents struct{ local resource.Local }

func (a agents) Agent(string) (resource.Agent, bool) { return a.local, true }

// world is an install with a service web on the server srv_A.
type world struct {
	db      *store.DB
	engine  *revision.Engine
	env     string
	project string
	webID   string
}

func newWorld(t *testing.T, b store.Backend) *world {
	t.Helper()
	db, _ := storetest.Open(t, b)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_A", Name: "hz-1", CreatedAt: now, CertSerial: "A", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	containers := container.Driver{Runtime: containertest.NewFake()}
	kinds := resource.NewRegistry(&service.Driver{})
	engine := revision.New(db, kinds, agents{resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: containers})}}, secretstest.Keeper(db, kinds), logger)
	var env string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		_, e, err := tree.Resolve("", "")
		env = e.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w := &world{db: db, engine: engine, env: env, project: docker.Project(env)}
	w.webID = w.addService(t, "web")
	return w
}

func (w *world) addService(t *testing.T, name string) string {
	t.Helper()
	ref := resource.Ref{Kind: service.Kind, Scope: w.env, Name: name}
	p, err := w.engine.Propose(t.Context(), revision.Proposal{Summary: "Run " + name, Actor: "user:usr_1", Edits: []revision.Edit{{
		Ref: ref, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.engine.Apply(t.Context(), p.Change); err != nil {
		t.Fatal(err)
	}
	return p.Resources[0].ID
}

func (w *world) instance(name string, number int32, state dockwellv1.HealthState, output string) *dockwellv1.InstanceHealth {
	return &dockwellv1.InstanceHealth{
		Project: w.project, Service: name, Number: number, Container: docker.InstanceName(w.project, name, int(number)), State: state,
		Since: timestamppb.New(time.Now().Add(-time.Minute)), Last: &dockwellv1.HealthResult{Output: output, Time: timestamppb.Now()},
	}
}

func (w *world) list(t *testing.T, h *Handler, req *dockwellv1.ListHealthRequest) []*dockwellv1.ServiceHealth {
	t.Helper()
	res, err := h.ListHealth(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetServices()
}

// It says what the agent last said, and no more: nothing it can't know, and never
// "healthy" for what nobody checked.
func TestWhatTheAPISaysIsWhatTheAgentSaid(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		r := &reports{said: map[string][]*dockwellv1.InstanceHealth{}, connected: map[string]bool{"srv_A": true}}
		h := New(w.db, r, slog.New(slog.DiscardHandler))
		one := func() *dockwellv1.ServiceHealth {
			t.Helper()
			got := w.list(t, h, &dockwellv1.ListHealthRequest{})
			if len(got) != 1 {
				t.Fatalf("it lists %v", got)
			}
			return got[0]
		}
		if s := one(); s.GetState() != unknown || s.GetId() != w.webID || s.GetResource().GetName() != "web" || s.GetServers()[0].GetServerName() != "hz-1" {
			t.Errorf("before an agent says anything, it reads %v, want unknown", s)
		}
		for state, want := range map[dockwellv1.HealthState]dockwellv1.HealthState{
			noCheck: noCheck, starting: starting, healthy: healthy, unhealthy: unhealthy, notRunning: notRunning,
		} {
			r.mu.Lock()
			r.said["srv_A"] = []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "what it said")}
			r.mu.Unlock()
			s := one()
			if s.GetState() != want || s.GetServers()[0].GetState() != want || s.GetServers()[0].GetInstances()[0].GetLast().GetOutput() != "what it said" {
				t.Errorf("an agent saying %v reads as %v", state, s)
			}
			if s.GetServers()[0].GetSince() == nil {
				t.Errorf("it doesn't say since when: %v", s)
			}
		}
		r.mu.Lock()
		r.said["srv_A"] = []*dockwellv1.InstanceHealth{w.instance("other", 1, healthy, "")}
		r.mu.Unlock()
		if s := one(); s.GetState() != notRunning {
			t.Errorf("a server with no container of it reads as %v, want not running", s.GetState())
		}
		// An agent that has gone: what it said stays, marked, and the state goes.
		r.mu.Lock()
		r.said["srv_A"] = []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")}
		r.connected["srv_A"] = false
		r.mu.Unlock()
		s := one()
		if s.GetState() != unknown || !s.GetServers()[0].GetStale() || s.GetServers()[0].GetInstances()[0].GetLast().GetOutput() != "ok" {
			t.Errorf("with its agent gone, it reads %v, want unknown, with what it last said marked stale", s)
		}
	})
}

// While a new container takes over, or fails to, what serves is the better of the
// two: a deploy that failed its check doesn't read as the service going down.
func TestAServiceIsAsWellAsItsBestContainer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		r := &reports{said: map[string][]*dockwellv1.InstanceHealth{"srv_A": {
			w.instance("web", 1, healthy, "ok"), w.instance("web", 2, unhealthy, "503"),
		}}, connected: map[string]bool{"srv_A": true}}
		h := New(w.db, r, slog.New(slog.DiscardHandler))
		got := w.list(t, h, &dockwellv1.ListHealthRequest{})
		if len(got) != 1 || got[0].GetState() != healthy || len(got[0].GetServers()[0].GetInstances()) != 2 {
			t.Errorf("it reads %v", got)
		}
	})
}

func TestListingNarrows(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		api := w.addService(t, "api")
		h := New(w.db, &reports{}, slog.New(slog.DiscardHandler))
		if got := w.list(t, h, &dockwellv1.ListHealthRequest{}); len(got) != 2 {
			t.Errorf("every service = %v", got)
		}
		if got := w.list(t, h, &dockwellv1.ListHealthRequest{Service: api}); len(got) != 1 || got[0].GetId() != api {
			t.Errorf("by ID = %v", got)
		}
		if got := w.list(t, h, &dockwellv1.ListHealthRequest{Service: "web"}); len(got) != 1 || got[0].GetId() != w.webID {
			t.Errorf("by name = %v", got)
		}
		if got := w.list(t, h, &dockwellv1.ListHealthRequest{Environment: w.env}); len(got) != 2 {
			t.Errorf("by environment = %v", got)
		}
		res, err := h.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{PageSize: 1}))
		if err != nil || len(res.Msg.GetServices()) != 1 || res.Msg.GetNextPageToken() == "" {
			t.Fatalf("a page of one = %v, %v", res, err)
		}
		next, err := h.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{PageSize: 1, PageToken: res.Msg.GetNextPageToken()}))
		if err != nil || len(next.Msg.GetServices()) != 1 || next.Msg.GetNextPageToken() != "" || next.Msg.GetServices()[0].GetId() == res.Msg.GetServices()[0].GetId() {
			t.Errorf("the next page = %v, %v", next, err)
		}
		if _, err := h.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{Environment: "nowhere"})); err == nil {
			t.Error("an environment that isn't there was taken")
		}
	})
}

func TestStatesAddUp(t *testing.T) {
	for _, tc := range []struct {
		in   []dockwellv1.HealthState
		want dockwellv1.HealthState
	}{
		{nil, unknown},
		{[]dockwellv1.HealthState{healthy, healthy}, healthy},
		{[]dockwellv1.HealthState{healthy, noCheck}, noCheck},
		{[]dockwellv1.HealthState{healthy, unknown}, unknown},
		{[]dockwellv1.HealthState{healthy, starting}, starting},
		{[]dockwellv1.HealthState{healthy, unhealthy, notRunning}, unhealthy},
		{[]dockwellv1.HealthState{unknown, notRunning}, notRunning},
		{[]dockwellv1.HealthState{healthy, paused}, paused},
		{[]dockwellv1.HealthState{unknown, paused}, paused},
		{[]dockwellv1.HealthState{paused, notRunning}, notRunning},
		{[]dockwellv1.HealthState{paused, unhealthy}, unhealthy},
	} {
		if got := Worst(tc.in); got != tc.want {
			t.Errorf("the worst of %v = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := Best(nil); got != notRunning {
		t.Errorf("the best of nothing = %v", got)
	}
	// While one takes over from another, what serves is what that server reads as, and
	// a paused one serves nothing.
	for _, tc := range []struct {
		in   []dockwellv1.HealthState
		want dockwellv1.HealthState
	}{
		{[]dockwellv1.HealthState{paused, healthy}, healthy},
		{[]dockwellv1.HealthState{paused, unhealthy}, unhealthy},
		{[]dockwellv1.HealthState{paused, notRunning}, paused},
	} {
		list := make([]*dockwellv1.InstanceHealth, len(tc.in))
		for i, state := range tc.in {
			list[i] = &dockwellv1.InstanceHealth{State: state}
		}
		if got := Best(list); got != tc.want {
			t.Errorf("the best of %v = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A paused service serves nothing, so the timeline tells of it, and of its coming back.
func TestTheTimelineHearsOfAPause(t *testing.T) {
	w := newWorld(t, store.SQLite)
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	r := w.newRecorder(c)
	say := func(state dockwellv1.HealthState) {
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "")})
	}
	say(healthy)
	r.Record(t.Context())
	say(paused)
	c.tick(Dwell)
	r.Record(t.Context())
	got := w.events(t)
	if len(got) != 1 || got[0].State != paused {
		t.Fatalf("after a pause lasted, the timeline has %v", got)
	}
	c.tick(time.Minute)
	say(healthy)
	c.tick(Dwell)
	r.Record(t.Context())
	if got = w.events(t); len(got) != 2 || got[1].State != healthy || got[1].Previous != paused {
		t.Fatalf("after it came back, the timeline has %v", got)
	}
}

// clock is a Recorder's time.
type clock struct{ now time.Time }

func (c *clock) tick(d time.Duration) { c.now = c.now.Add(d) }

// events reads what the timeline has of a service, oldest first.
func (w *world) events(t *testing.T) []Told {
	t.Helper()
	told, _ := w.eventsAt(t)
	return told
}

// eventsAt is web's events on srv_A, oldest first, and when each was last told.
func (w *world) eventsAt(t *testing.T) ([]Told, []time.Time) {
	t.Helper()
	var out []Told
	var at []time.Time
	if err := w.db.View(t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListEvents(t.Context(), 1000)
		for i := len(list) - 1; i >= 0; i-- {
			if list[i].Action != Action || list[i].Subject != w.webID || list[i].Target != "srv_A" {
				continue
			}
			told, ok := ReadTold(list[i].Detail)
			if !ok {
				t.Errorf("an event's detail reads %q", list[i].Detail)
			}
			if list[i].Actor != "dockwell" || list[i].Surface != store.SurfaceAutomatic {
				t.Errorf("an event is by %s, from %s", list[i].Actor, list[i].Surface)
			}
			out = append(out, told)
			at = append(at, list[i].At)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out, at
}

func (w *world) newRecorder(c *clock) *Recorder {
	return &Recorder{DB: w.db, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return c.now }}
}

// The timeline hears when a service turns unhealthy and comes back, and hears
// nothing of a service that has always been fine.
func TestTheTimelineHearsAChangeOfState(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		r := w.newRecorder(c)
		ctx := t.Context()
		say := func(state dockwellv1.HealthState, output string) {
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, output)})
		}
		say(healthy, "ok")
		r.Record(ctx)
		c.tick(time.Hour)
		r.Record(ctx)
		if got := w.events(t); len(got) != 0 {
			t.Fatalf("a service that has always been fine has %v", got)
		}
		say(unhealthy, "GET /healthz on port 8080 answered 503 Service Unavailable")
		r.Record(ctx)
		if got := w.events(t); len(got) != 0 {
			t.Fatalf("a state that has just come is on the timeline: %v", got)
		}
		c.tick(Dwell)
		r.Record(ctx)
		got := w.events(t)
		if len(got) != 1 || got[0].State != unhealthy || !strings.Contains(got[0].Output, "503") {
			t.Fatalf("after it lasted, the timeline has %v", got)
		}
		c.tick(time.Minute)
		say(healthy, "ok")
		c.tick(Dwell)
		r.Record(ctx)
		got = w.events(t)
		if len(got) != 2 || got[1].State != healthy || got[1].Previous != unhealthy {
			t.Fatalf("after it came back, the timeline has %v", got)
		}
		c.tick(time.Hour)
		r.Record(ctx)
		if got := w.events(t); len(got) != 2 {
			t.Errorf("nothing new makes %v", got)
		}
	})
}

// A short gap is the change's to tell: a stop-first deploy's few seconds, a check
// that fails once.
func TestAShortGapIsntNews(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		r := w.newRecorder(c)
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")})
		r.Record(t.Context())
		c.tick(time.Hour)
		for _, state := range []dockwellv1.HealthState{notRunning, starting, healthy} {
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "")})
			c.tick(10 * time.Second)
			r.Record(t.Context())
		}
		c.tick(time.Hour)
		r.Record(t.Context())
		if got := w.events(t); len(got) != 0 {
			t.Errorf("a deploy's gap made %v", got)
		}
		// While a new one takes over, the old one goes on being what counts.
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok"), w.instance("web", 2, unhealthy, "503")})
		c.tick(time.Hour)
		r.Record(t.Context())
		if got := w.events(t); len(got) != 0 {
			t.Errorf("a new one failing its check, with the old one serving, made %v", got)
		}
	})
}

// A service that keeps changing doesn't flood: it is one event, which changes as it
// goes on, saying what it keeps falling to and how often since it began, and how
// it settles is told once it has.
func TestAFlappingServiceDoesntFlood(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		start := time.Unix(1_800_000_000, 0)
		c := &clock{now: start}
		r := w.newRecorder(c)
		state := healthy
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "ok")})
		r.Record(t.Context())
		// It flips every 40 s for an hour, long enough each time to be told.
		const flips = 90
		var last time.Time
		for range flips {
			if state == healthy {
				state = unhealthy
			} else {
				state = healthy
			}
			last = c.now
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "out")})
			for range 8 {
				c.tick(5 * time.Second)
				r.Record(t.Context())
			}
		}
		got, at := w.eventsAt(t)
		// Its first two changes are told as they come, then it keeps changing.
		if len(got) != 3 {
			t.Fatalf("an hour of flapping made %d events, want 3: %+v", len(got), got)
		}
		flap := got[2]
		if flap.State != unhealthy || flap.Flips < flips-2 || flap.Since == nil || !flap.Since.Equal(start) {
			t.Errorf("the event of an hour's flapping says %+v, want unhealthy, %d flips, since %s", flap, flips, start)
		}
		if late := c.now.Sub(at[2]); late > flapEvery+5*time.Second {
			t.Errorf("the event of an hour's flapping was last told %s ago, want within %s", late, flapEvery)
		}
		// Once it has settled, that is told, as when it settled: healthy, as it was last.
		settled := last
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")})
		for range 2 * FlapWindow / (5 * time.Second) {
			c.tick(5 * time.Second)
			r.Record(t.Context())
		}
		got = w.events(t)
		if len(got) != 4 || got[3].State != healthy || got[3].Previous != unhealthy || !got[3].Since.Equal(settled) || got[3].Flips != 0 {
			t.Errorf("after it settled, the timeline has %+v, want it healthy since %s", got, settled)
		}
	})
}

// A deploy or a restart that stops what ran first leaves a gap a scan can catch.
// Going down and coming back in one container is what keeps changing means: coming
// back in another is someone replacing it, so however often that happens it isn't
// news, and a crash soon after one is told as the crash it is.
func TestAReplaceIsntAServiceChanging(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		start := time.Unix(1_800_000_000, 0)
		c := &clock{now: start}
		r := w.newRecorder(c)
		say := func(state dockwellv1.HealthState, container string) {
			i := w.instance("web", 1, state, "")
			i.ContainerId = container
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{i})
			r.Record(t.Context())
		}
		wait := func(d time.Duration) {
			for end := c.now.Add(d); c.now.Before(end); {
				c.tick(5 * time.Second)
				r.Record(t.Context())
			}
		}
		say(healthy, "aaa")
		// Two restarts a minute apart, with a scan in each gap.
		wait(time.Minute)
		say(notRunning, "aaa")
		wait(5 * time.Second)
		say(healthy, "bbb")
		wait(time.Minute)
		say(notRunning, "bbb")
		wait(5 * time.Second)
		say(healthy, "ccc")
		wait(time.Minute)
		if got := w.events(t); len(got) != 0 {
			t.Fatalf("two restarts a minute apart made %+v, want nothing", got)
		}
		// A crash two minutes on is news, from when it happened.
		crashed := c.now
		say(notRunning, "ccc")
		wait(Dwell + 5*time.Second)
		got := w.events(t)
		if len(got) != 1 || got[0].State != notRunning || got[0].Flips != 0 || got[0].Since == nil || !got[0].Since.Equal(crashed) {
			t.Errorf("a crash after two restarts made %+v, want it not running since %s, and no count of changes", got, crashed)
		}
	})
}

// Going down and coming back in one container is what a service that keeps changing
// does, whatever its agent says of it, and an older agent, which says no ID, counts
// as it did.
func TestAFlapInOneContainerStillCounts(t *testing.T) {
	for _, container := range []string{"aaa", ""} {
		t.Run("container "+container, func(t *testing.T) {
			w := newWorld(t, store.SQLite)
			c := &clock{now: time.Unix(1_800_000_000, 0)}
			r := w.newRecorder(c)
			say := func(state dockwellv1.HealthState) {
				i := w.instance("web", 1, state, "")
				i.ContainerId = container
				r.Heard("srv_A", []*dockwellv1.InstanceHealth{i})
				c.tick(5 * time.Second)
				r.Record(t.Context())
			}
			say(healthy)
			say(notRunning)
			say(healthy)
			say(notRunning)
			if got := w.events(t); len(got) != 1 || got[0].Flips != 3 {
				t.Errorf("down and back in one container made %+v, want one event of 3 changes", got)
			}
		})
	}
}

// Once it has stopped changing, how it ended up is told, as that's what is so now.
// One that ended up down had an event saying it keeps changing, with not running as
// what it kept falling to, and that stayed its last word.
func TestAFlapThatEndsDownSaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		r := w.newRecorder(c)
		say := func(state dockwellv1.HealthState) {
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "")})
			c.tick(5 * time.Second)
			r.Record(t.Context())
		}
		say(healthy)
		say(notRunning)
		say(healthy)
		say(notRunning)
		got := w.events(t)
		if len(got) != 1 || got[0].Flips != 3 {
			t.Fatalf("flapping made %+v", got)
		}
		down := *got[0].Since
		for range 2 * FlapWindow / (5 * time.Second) {
			say(notRunning)
		}
		got = w.events(t)
		if len(got) != 2 || got[1].State != notRunning || got[1].Flips != 0 {
			t.Fatalf("after it stayed down, the timeline has %+v, want it told as not running, and no longer changing", got)
		}
		if !got[1].Since.After(down) {
			t.Errorf("it says it has been down since %s, which is when it began to change", got[1].Since)
		}
		// And that's said once.
		for range 2 * FlapWindow / (5 * time.Second) {
			say(notRunning)
		}
		if got = w.events(t); len(got) != 2 {
			t.Errorf("staying down made %+v", got)
		}
	})
}

// An event is dated when its state came about, as well as Dwell lets the timeline
// know, however soon after the last it comes: an outage soon after a recovery
// reaches the timeline once it has lasted, not minutes later.
func TestAHealthEventIsToldWhenItHappened(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		start := time.Unix(1_800_000_000, 0)
		c := &clock{now: start}
		r := w.newRecorder(c)
		say := func(state dockwellv1.HealthState) {
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "")})
		}
		wait := func(d time.Duration) {
			for end := c.now.Add(d); c.now.Before(end); {
				c.tick(5 * time.Second)
				r.Record(t.Context())
			}
		}
		say(healthy)
		r.Record(t.Context())
		var came []time.Time
		for _, state := range []dockwellv1.HealthState{notRunning, healthy} {
			came = append(came, c.now)
			say(state)
			wait(time.Minute)
		}
		got, at := w.eventsAt(t)
		if len(got) != 2 {
			t.Fatalf("a stop and a recovery a minute apart made %+v", got)
		}
		for i := range got {
			if got[i].Since == nil || !got[i].Since.Equal(came[i]) {
				t.Errorf("event %d says it came about at %v, want %s", i, got[i].Since, came[i])
			}
			if late := at[i].Sub(came[i]); late > Dwell+5*time.Second {
				t.Errorf("event %d reached the timeline %s after it came about, want within %s", i, late, Dwell+5*time.Second)
			}
			if got[i].Flips != 0 {
				t.Errorf("event %d says it changed %d times, with nothing before it", i, got[i].Flips)
			}
		}
		// An outage a minute after the recovery is the third change in five minutes: it
		// keeps changing, and that is told at once.
		down := c.now
		say(notRunning)
		wait(10 * time.Second)
		got, at = w.eventsAt(t)
		if len(got) != 3 || got[2].State != notRunning || at[2].Sub(down) > 5*time.Second || got[2].Flips != 3 || !got[2].Since.Equal(came[0]) {
			t.Errorf("an outage soon after a recovery made %+v, at %v, want it told within 5 s as changing 3 times since %s", got, at, came[0])
		}
	})
}

// A control plane that starts again neither repeats what the timeline has, nor
// misses what ended while it was away.
func TestARestartNeitherRepeatsNorMisses(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		first := w.newRecorder(c)
		first.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, unhealthy, "503")})
		first.Record(t.Context())
		c.tick(Dwell)
		first.Record(t.Context())
		if got := w.events(t); len(got) != 1 {
			t.Fatalf("before the restart, the timeline has %v", got)
		}

		// It is still unhealthy: nothing new.
		second := w.newRecorder(c)
		second.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, unhealthy, "503")})
		second.Record(t.Context())
		c.tick(time.Hour)
		second.Record(t.Context())
		if got := w.events(t); len(got) != 1 {
			t.Errorf("a restart with nothing new makes %v", got)
		}

		// It recovered while the control plane was away: that is news.
		third := w.newRecorder(c)
		third.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")})
		third.Record(t.Context())
		c.tick(Dwell + time.Second)
		third.Record(t.Context())
		got := w.events(t)
		if len(got) != 2 || got[1].State != healthy || got[1].Previous != unhealthy {
			t.Errorf("recovering while it was away makes %v", got)
		}
	})
}

// A service with no container left doesn't run; one that has gone from the store
// is forgotten, with no event.
func TestWhatHasGoneIsForgotten(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		r := w.newRecorder(c)
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok"), w.instance("ghost", 1, unhealthy, "503")})
		r.Record(t.Context())
		c.tick(time.Hour)
		r.Heard("srv_A", nil)
		c.tick(Dwell)
		r.Record(t.Context())
		got := w.events(t)
		if len(got) != 1 || got[0].State != notRunning || !strings.Contains(got[0].Output, "no container") {
			t.Errorf("a service with no container left has %v", got)
		}
		r.mu.Lock()
		_, ghost := r.tracked[key{"srv_A", w.project, "ghost"}]
		n := len(r.tracked)
		r.mu.Unlock()
		if ghost || n != 1 {
			t.Errorf("what isn't in the store is still tracked: %d entries", n)
		}
	})
}

// A server speaks for what runs on it. One that says a service placed elsewhere
// is down has said nothing the timeline should repeat, whatever has been done to
// its agent.
func TestAServerCantSpeakForAServiceItDoesntRun(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		now := time.Now()
		if err := w.db.Update(t.Context(), func(tx *store.Tx) error {
			return tx.CreateServer(t.Context(), store.Server{ID: "srv_B", Name: "hz-2", CreatedAt: now, CertSerial: "B", CertNotAfter: now})
		}); err != nil {
			t.Fatal(err)
		}
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		r := w.newRecorder(c)
		r.Heard("srv_B", []*dockwellv1.InstanceHealth{w.instance("web", 1, unhealthy, "the sky is falling")})
		r.Record(t.Context())
		c.tick(time.Hour)
		r.Record(t.Context())
		// Of any server, as what is in question is an event naming the wrong one.
		if err := w.db.View(t.Context(), func(tx *store.Tx) error {
			list, err := tx.ListEvents(t.Context(), 1000)
			for i := range list {
				if list[i].Action == Action {
					t.Errorf("the timeline has what another server said of web: %+v", list[i])
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, unhealthy, "503")})
		r.Record(t.Context())
		c.tick(Dwell)
		r.Record(t.Context())
		if got := w.events(t); len(got) != 1 || !strings.Contains(got[0].Output, "503") {
			t.Errorf("the timeline should have what web's own server said, and has %v", got)
		}
	})
}

// A service flapping faster than Dwell has no state that lasts long enough to be
// told. It is told as what it keeps falling to, in one event, with how often.
func TestAServiceFlappingFasterThanTheDwellIsTold(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		start := time.Unix(1_800_000_000, 0)
		c := &clock{now: start}
		r := w.newRecorder(c)
		state := healthy
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, "ok")})
		r.Record(t.Context())
		for range 360 { // every 20 s, for two hours
			if state == healthy {
				state = unhealthy
			} else {
				state = healthy
			}
			say := "ok"
			if state == unhealthy {
				say = "GET /healthz on port 8080 answered 503"
			}
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, state, say)})
			for range 4 {
				c.tick(5 * time.Second)
				r.Record(t.Context())
			}
		}
		got := w.events(t)
		if len(got) != 1 {
			t.Fatalf("two hours of a service down every other 20 s made %d events, want one: %+v", len(got), got)
		}
		// Whichever way it happens to be as it's told, what it says is what it keeps falling to.
		if got[0].State != unhealthy || !strings.Contains(got[0].Output, "503") || got[0].Flips < 350 || !got[0].Since.Equal(start) {
			t.Errorf("the event says %+v, want what it keeps falling to, and how often since %s", got[0], start)
		}
		// Once it settles, that is told too.
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")})
		for range FlapWindow/(5*time.Second) + 1 {
			c.tick(5 * time.Second)
			r.Record(t.Context())
		}
		if all := w.events(t); len(all) != 2 || all[1].State != healthy {
			t.Errorf("after it settled, the timeline has %+v", all)
		}
	})
}

// A state that says neither good nor bad, as an agent that can't read Docker, isn't
// a flip: it is nobody knowing.
func TestNotKnowingIsntAFlip(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		c := &clock{now: time.Unix(1_800_000_000, 0)}
		r := w.newRecorder(c)
		r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")})
		r.Record(t.Context())
		for range 40 {
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, "")})
			c.tick(20 * time.Second)
			r.Heard("srv_A", []*dockwellv1.InstanceHealth{w.instance("web", 1, healthy, "ok")})
			c.tick(20 * time.Second)
			r.Record(t.Context())
		}
		if got := w.events(t); len(got) != 0 {
			t.Errorf("not knowing made %v", got)
		}
	})
}

// A connected agent isn't a checked service: stats keep its stream open whatever its
// monitor is doing. What it said stands for as long as it says, and no longer.
func TestWhatIsTooOldToStandReadsAsNotKnown(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		now := time.Now()
		base := func() *dockwellv1.InstanceHealth {
			i := w.instance("web", 1, healthy, "GET /healthz on port 8080 answered 200 OK")
			i.Check = dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_HTTP
			return i
		}
		for name, tc := range map[string]struct {
			instance func() *dockwellv1.InstanceHealth
			report   agenthealth.Report
			want     dockwellv1.HealthState
			stale    bool
		}{
			"a fresh result": {base, agenthealth.Report{Said: true}, healthy, false},
			"a day-old result, with nothing said of how long it's good for": {func() *dockwellv1.InstanceHealth {
				i := base()
				i.Last.Time = timestamppb.New(now.Add(-24 * time.Hour))
				return i
			}, agenthealth.Report{Said: true}, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, true},
			"past what it says it's good for": {func() *dockwellv1.InstanceHealth {
				i := base()
				i.ConfirmedAt, i.GoodFor = timestamppb.New(now.Add(-2*time.Minute)), durationpb.New(time.Minute)
				return i
			}, agenthealth.Report{Said: true}, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, true},
			"within what it says it's good for": {func() *dockwellv1.InstanceHealth {
				i := base()
				i.ConfirmedAt, i.GoodFor = timestamppb.New(now.Add(-2*time.Minute)), durationpb.New(time.Hour)
				return i
			}, agenthealth.Report{Said: true}, healthy, false},
			"a server's clock a day off, and a fresh report": {func() *dockwellv1.InstanceHealth {
				i := base()
				i.ConfirmedAt, i.GoodFor = timestamppb.New(now.Add(-24*time.Hour-time.Second)), durationpb.New(time.Minute)
				return i
			}, agenthealth.Report{Said: true, ReportedAt: now.Add(-24 * time.Hour), ReceivedAt: now}, healthy, false},
			"a report that stopped coming": {base, agenthealth.Report{Said: true, ReportedAt: now.Add(-time.Hour), ReceivedAt: now.Add(-time.Hour)}, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, true},
		} {
			t.Run(name, func(t *testing.T) {
				tc.report.Instances = []*dockwellv1.InstanceHealth{tc.instance()}
				h := New(w.db, oneReport{tc.report}, slog.New(slog.DiscardHandler))
				got := w.list(t, h, &dockwellv1.ListHealthRequest{})[0]
				s := got.GetServers()[0]
				if got.GetState() != tc.want || s.GetStale() != tc.stale {
					t.Errorf("it reads %s, stale=%t, want %s, stale=%t", got.GetState(), s.GetStale(), tc.want, tc.stale)
				}
			})
		}
		// With nothing read yet, it hasn't said that nothing runs.
		h := New(w.db, oneReport{agenthealth.Report{}}, slog.New(slog.DiscardHandler))
		if got := w.list(t, h, &dockwellv1.ListHealthRequest{})[0]; got.GetState() != dockwellv1.HealthState_HEALTH_STATE_UNKNOWN {
			t.Errorf("an agent that hasn't read Docker reads as %s", got.GetState())
		}
		// Having read and found none is different.
		h = New(w.db, oneReport{agenthealth.Report{Said: true}}, slog.New(slog.DiscardHandler))
		if got := w.list(t, h, &dockwellv1.ListHealthRequest{})[0]; got.GetState() != dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING {
			t.Errorf("an agent that read Docker and found none reads as %s", got.GetState())
		}
	})
}

// An agent that can't read Docker is there, and says so: what only Docker can say
// isn't known, and what it last found isn't that nothing runs.
func TestAnAgentThatCantReadDockerSaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		w := newWorld(t, b)
		blind := time.Now().Add(-2 * time.Minute)
		unknown := w.instance("web", 1, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, "")
		for name, report := range map[string]agenthealth.Report{
			"with what it found": {Said: true, Instances: []*dockwellv1.InstanceHealth{unknown}, DockerUnreadableSince: blind},
			"having found none":  {Said: true, DockerUnreadableSince: blind},
			// As an agent started with Docker down, which has never read it.
			"having read nothing": {DockerUnreadableSince: blind},
		} {
			t.Run(name, func(t *testing.T) {
				h := New(w.db, oneReport{report}, slog.New(slog.DiscardHandler))
				got := w.list(t, h, &dockwellv1.ListHealthRequest{})[0]
				s := got.GetServers()[0]
				if got.GetState() != dockwellv1.HealthState_HEALTH_STATE_UNKNOWN || s.GetStale() || !s.GetDockerUnreadableSince().AsTime().Equal(blind) {
					t.Errorf("it reads %s, stale=%t, Docker unreadable since %v; want unknown, not stale, since %s", got.GetState(), s.GetStale(), s.GetDockerUnreadableSince(), blind)
				}
			})
		}
		// Once it is gone, all of it is stale, which says more.
		h := New(w.db, goneReport{agenthealth.Report{Said: true, Instances: []*dockwellv1.InstanceHealth{unknown}, DockerUnreadableSince: blind}}, slog.New(slog.DiscardHandler))
		if s := w.list(t, h, &dockwellv1.ListHealthRequest{})[0].GetServers()[0]; !s.GetStale() || s.GetDockerUnreadableSince() != nil {
			t.Errorf("with its agent gone, it reads stale=%t, Docker unreadable since %v; want stale, and nothing of Docker", s.GetStale(), s.GetDockerUnreadableSince())
		}
	})
}

// goneReport is every server's agent having said the same, and gone since.
type goneReport struct{ report agenthealth.Report }

func (o goneReport) Health(string) (agenthealth.Report, bool) { return o.report, false }

// oneReport is every server's agent having said the same, and connected.
type oneReport struct{ report agenthealth.Report }

func (o oneReport) Health(string) (agenthealth.Report, bool) { return o.report, true }
