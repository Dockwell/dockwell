// SPDX-License-Identifier: AGPL-3.0-only

package control

// What #304's first review found, as tests: an evaluator on a control plane with
// its nine default rules and one webhook, run by the test's clock against fakes of
// the fleet and of how apps are.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	controlhealth "github.com/dockwell/dockwell/internal/control/health"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
)

type evalFleet struct{ list func() []*dockwellv1.Server }

func (z evalFleet) List() []*dockwellv1.Server { return z.list() }

type noDomains struct{}

func (noDomains) Domain(string, string) (*dockwellv1.DomainStatus, bool) { return nil, false }

// placedApps says each app is placed on srv_zz, web-1, which is connected: how
// each stands is what Standings say.
type placedApps []string

func (p placedApps) Placed(context.Context) ([]*dockwellv1.ServiceHealth, error) {
	out := make([]*dockwellv1.ServiceHealth, len(p))
	for i, name := range p {
		out[i] = &dockwellv1.ServiceHealth{
			Id:       fmt.Sprintf("svc_01kat4zr6e3v0y5xbkzb5d%04d", i),
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_zz", Name: name},
			Servers:  []*dockwellv1.ServerHealth{{ServerId: "srv_zz", ServerName: "web-1", State: dockwellv1.HealthState_HEALTH_STATE_NO_CHECK}},
		}
	}
	return out, nil
}

func connectedAt(now time.Time, memPct uint64) []*dockwellv1.Server {
	return []*dockwellv1.Server{{
		Id: "srv_zz", Name: "web-1", Connection: dockwellv1.Connection_CONNECTION_CONNECTED,
		Stats: &dockwellv1.ServerStats{
			Time: timestamppb.New(now), MemoryUsedBytes: memPct, MemoryTotalBytes: 100,
			Disks: []*dockwellv1.DiskUsage{{MountPoint: "/", UsedBytes: 40, SizeBytes: 100}},
		},
	}}
}

// evalStand is a control plane with its nine default rules and one webhook at url,
// whose own evaluator stays still, so the test's runs by the test's clock alone.
func evalStand(t *testing.T, url string) (*fullServer, dockwellv1connect.ChangeServiceClient, *browser) {
	t.Helper()
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	for deadline := time.Now().Add(10 * time.Second); ; {
		res, err := sites.ListAlertRules(t.Context(), connect.NewRequest(&dockwellv1.ListAlertRulesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Msg.GetRules()) == 9 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add the ops webhook", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
			To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: url}},
		}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, made.Msg.GetChange())
	return f, c, b
}

// clock is the test's time, which every part of an evaluator reads.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func evaluatorOn(f *fullServer, at *clock) *alerts.Evaluator {
	return &alerts.Evaluator{
		DB: f.store, Keeper: f.keeper, Engine: f.engine, Domains: noDomains{}, Logger: slog.New(slog.DiscardHandler),
		Health: placedApps{}, Fleet: evalFleet{func() []*dockwellv1.Server { return connectedAt(at.Now(), 50) }},
		Now: at.Now, Sender: &alerts.Sender{},
	}
}

func tickOn(t *testing.T, e *alerts.Evaluator) {
	t.Helper()
	if err := e.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.Wait()
}

// An app in Docker's crash loop goes between restarting and up for a moment,
// again and again: app-down fires once, as the health recorder folds it, however
// it reads while up. One that starts slowly once never does.
func TestACrashLoopFiresAppDown(t *testing.T) {
	project := docker.Project("env_zz")
	for _, tc := range []struct {
		name     string
		up, down dockwellv1.HealthState
		want     int
	}{
		{"its check says starting while it's up", dockwellv1.HealthState_HEALTH_STATE_STARTING, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY, 1},
		{"it has no check, so it reads running while it's up", dockwellv1.HealthState_HEALTH_STATE_NO_CHECK, dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHook(t)
			f, _, _ := evalStand(t, h.srv.URL+"/hook")
			at := &clock{now: time.Now()}
			rec := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: at.Now}
			e := evaluatorOn(f, at)
			e.Health, e.Standings = placedApps{"api"}, rec
			// 30 minutes of a loop: up for 2 seconds, restarting for a minute.
			for s := range 30 * 60 {
				state := tc.down
				if (s % 62) < 2 {
					state = tc.up
				}
				rec.Heard("srv_zz", []*dockwellv1.InstanceHealth{{Project: project, Service: "api", State: state}})
				if s%15 == 0 {
					tickOn(t, e)
				}
				at.add(time.Second)
			}
			got := h.of("app-down")
			if len(got) != tc.want || (len(got) > 0 && !strings.HasPrefix(got[0], "firing: api ")) {
				t.Errorf("30 minutes of a crash loop sent %q, want one firing message", got)
			}
		})
	}

	t.Run("an app that starts slowly once isn't down", func(t *testing.T) {
		h := newHook(t)
		f, _, _ := evalStand(t, h.srv.URL+"/hook")
		at := &clock{now: time.Now()}
		rec := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: at.Now}
		e := evaluatorOn(f, at)
		e.Health, e.Standings = placedApps{"api"}, rec
		for s := range 10 * 60 {
			state := dockwellv1.HealthState_HEALTH_STATE_HEALTHY
			if s < 3*60 {
				state = dockwellv1.HealthState_HEALTH_STATE_STARTING
			}
			rec.Heard("srv_zz", []*dockwellv1.InstanceHealth{{Project: project, Service: "api", State: state}})
			if s%15 == 0 {
				tickOn(t, e)
			}
			at.add(time.Second)
		}
		if got := h.of("app-down"); len(got) != 0 {
			t.Errorf("an app that took 3 minutes to start sent %q", got)
		}
	})

	t.Run("an app that starts slowly after a stop-first deploy isn't down", func(t *testing.T) {
		h := newHook(t)
		f, _, _ := evalStand(t, h.srv.URL+"/hook")
		at := &clock{now: time.Now()}
		rec := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: at.Now}
		e := evaluatorOn(f, at)
		e.Health, e.Standings = placedApps{"api"}, rec
		in := func(id string, state dockwellv1.HealthState) []*dockwellv1.InstanceHealth {
			return []*dockwellv1.InstanceHealth{{Project: project, Service: "api", ContainerId: id, State: state}}
		}
		// Up for 5 minutes, stopped for 5 seconds, then 3 minutes starting in a new container.
		for s := range 20 * 60 {
			var list []*dockwellv1.InstanceHealth
			switch {
			case s < 300:
				list = in("old", dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
			case s < 305:
				list = in("old", dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING)
			case s < 485:
				list = in("new", dockwellv1.HealthState_HEALTH_STATE_STARTING)
			default:
				list = in("new", dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
			}
			rec.Heard("srv_zz", list)
			if s%15 == 0 {
				tickOn(t, e)
			}
			at.add(time.Second)
		}
		if got := h.of("app-down"); len(got) != 0 {
			t.Errorf("a deploy whose new container took 3 minutes to start sent %q", got)
		}
	})
}

// A one-shot job that exited 0, with a restart policy of no or on-failure, did its
// work: it isn't down. One that exited otherwise is.
func TestAOneShotJobThatFinishedIsntDown(t *testing.T) {
	h := newHook(t)
	f, _, _ := evalStand(t, h.srv.URL+"/hook")
	at := &clock{now: time.Now()}
	rec := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: at.Now}
	e := evaluatorOn(f, at)
	e.Health, e.Standings = placedApps{"migrate", "worker"}, rec
	project := docker.Project("env_zz")
	for range 60 { // 15 minutes
		rec.Heard("srv_zz", []*dockwellv1.InstanceHealth{
			{Project: project, Service: "migrate", State: dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, Finished: true},
			{Project: project, Service: "worker", State: dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING},
		})
		tickOn(t, e)
		at.add(alerts.Every)
	}
	got := h.of("app-down")
	if len(got) != 1 || !strings.Contains(got[0], "worker isn't running on web-1") || strings.Contains(strings.Join(got, " "), "migrate") {
		t.Errorf("a finished job and a stopped worker sent %q, want the worker alone", got)
	}
}

// A job that finished stays quiet once docker system prune removes its container.
// Run again, a job that fails before the agent sees it running fires.
func TestAFinishedJobThatsRunAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		again []*dockwellv1.InstanceHealth
		want  string
	}{
		{"pruned", nil, ""},
		{"run again, failing at once", []*dockwellv1.InstanceHealth{{ContainerId: "c2", State: dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING}}, "firing: migrate isn't running on web-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHook(t)
			f, _, _ := evalStand(t, h.srv.URL+"/hook")
			at := &clock{now: time.Now()}
			rec := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: at.Now}
			e := evaluatorOn(f, at)
			e.Health, e.Standings = placedApps{"migrate"}, rec
			project := docker.Project("env_zz")
			for i := range 80 { // 20 minutes: it finishes at once, and at 5 minutes this
				list := []*dockwellv1.InstanceHealth{{ContainerId: "c1", State: dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, Finished: true}}
				if i >= 20 {
					list = tc.again
				}
				for _, in := range list {
					in.Project, in.Service = project, "migrate"
				}
				rec.Heard("srv_zz", list)
				tickOn(t, e)
				at.add(alerts.Every)
			}
			got := h.of("app-down")
			if tc.want == "" && len(got) != 0 || tc.want != "" && (len(got) != 1 || !strings.HasPrefix(got[0], tc.want)) {
				t.Errorf("a finished job %s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// Memory that stands over its rule's 90% and dips under it now and then is one
// alert: once it's open, a dip into the band under the line keeps it open, and one
// further under has to last the rule's 10 minutes to resolve it.
func TestMemoryThatDipsStaysOneAlert(t *testing.T) {
	h := newHook(t)
	f, _, _ := evalStand(t, h.srv.URL+"/hook")
	at := &clock{now: time.Now()}
	tick := 0
	e := evaluatorOn(f, at)
	e.Fleet = evalFleet{func() []*dockwellv1.Server {
		pct := uint64(92)
		switch {
		case tick < 60: // over for 15 minutes, so it opens
		case tick%44 == 21:
			pct = 89 // in the band
		case tick%44 == 43:
			pct = 70 // under it, for one sample
		}
		return connectedAt(at.Now(), pct)
	}}
	for tick = range 480 { // 2 hours
		tickOn(t, e)
		at.add(alerts.Every)
	}
	if got := h.of("memory-high"); len(got) != 1 || !strings.HasPrefix(got[0], "firing:") {
		t.Errorf("2 hours of memory over 90%% with dips sent %q, want one firing message", got)
	}
}

// The band under the line is for resolving alone: memory that's over its rule's
// 90% for a moment, then under it in the band, isn't over for the rule's 10
// minutes when it goes over again an hour later.
func TestMemoryInTheBandIsntOver(t *testing.T) {
	h := newHook(t)
	f, _, _ := evalStand(t, h.srv.URL+"/hook")
	at := &clock{now: time.Now()}
	tick := 0
	e := evaluatorOn(f, at)
	e.Fleet = evalFleet{func() []*dockwellv1.Server {
		pct := uint64(88)
		if tick == 0 || tick == 240 {
			pct = 91
		}
		return connectedAt(at.Now(), pct)
	}}
	for tick = range 300 { // 75 minutes
		tickOn(t, e)
		at.add(alerts.Every)
	}
	if got := h.of("memory-high"); len(got) != 0 {
		t.Errorf("memory over 90%% for a moment twice, an hour apart, sent %q", got)
	}
}

// hold holds each request until its context ends, as a far end that never
// answers, and counts them.
type hold struct{ calls atomic.Int32 }

func (b *hold) RoundTrip(r *http.Request) (*http.Response, error) {
	b.calls.Add(1)
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// A message stays in the outbox until the far end takes it: a stop cuts a send
// short within StopGrace, and the next start sends it again. A far end that
// failed is tried again, past a minute, and gets it once it answers.
func TestTheOutboxDeliversWhatAStopCutShort(t *testing.T) {
	h := newHook(t)
	f, _, _ := evalStand(t, h.srv.URL+"/hook")
	at := &clock{now: time.Now()}
	away := func() []*dockwellv1.Server {
		return []*dockwellv1.Server{{Id: "srv_zz", Name: "web-1", Connection: dockwellv1.Connection_CONNECTION_UNREACHABLE, LastSeenTime: timestamppb.New(at.Now().Add(-time.Hour))}}
	}
	// The far end refuses the first try.
	h.mu.Lock()
	h.code = http.StatusServiceUnavailable
	h.mu.Unlock()
	first := evaluatorOn(f, at)
	first.Fleet = evalFleet{away}
	for range 11 { // fires at 2 minutes, queued and tried at 2.5
		tickOn(t, first)
		at.add(alerts.Every)
	}
	if got := h.of("server-unreachable"); len(got) != 1 {
		t.Fatalf("the first try reached the far end %d times", len(got))
	}

	// The control plane restarts and tries again, but stops while the far end
	// holds the request.
	at.add(time.Minute)
	held := &hold{}
	second := evaluatorOn(f, at)
	second.Fleet, second.Sender = evalFleet{away}, &alerts.Sender{Client: &http.Client{Transport: held}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { second.Run(ctx); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); held.calls.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if held.calls.Load() == 0 {
		t.Fatal("the restarted control plane didn't try again what was in its outbox")
	}
	stopped := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(alerts.StopGrace + 5*time.Second):
		t.Fatalf("a far end that never answers held the stop past %v", alerts.StopGrace+5*time.Second)
	}
	if took := time.Since(stopped); took > alerts.StopGrace+2*time.Second {
		t.Errorf("the stop took %v, want about %v at most", took, alerts.StopGrace)
	}

	// It starts again, and the far end answers now.
	h.mu.Lock()
	h.code = http.StatusOK
	h.mu.Unlock()
	at.add(time.Minute)
	third := evaluatorOn(f, at)
	third.Fleet = evalFleet{away}
	tickOn(t, third)
	if got := h.of("server-unreachable"); len(got) != 2 || got[1] != "firing: web-1 can't be reached" {
		t.Errorf("after the restarts, the far end got %q, want the firing message again", got)
	}
	// And only once: what it took is done.
	at.add(time.Minute)
	tickOn(t, third)
	if got := h.of("server-unreachable"); len(got) != 2 {
		t.Errorf("a message the far end took went again: %q", got)
	}
}

// A far end that's down is tried for a day, and given up then, with the channel's
// deliveries saying so; one that comes back within it gets what it missed.
func TestAChannelThatStaysDownIsTriedForADay(t *testing.T) {
	h := newHook(t)
	f, _, _ := evalStand(t, h.srv.URL+"/hook")
	h.mu.Lock()
	h.code = http.StatusBadGateway
	h.mu.Unlock()
	at := &clock{now: time.Now()}
	e := evaluatorOn(f, at)
	e.Fleet = evalFleet{func() []*dockwellv1.Server {
		return []*dockwellv1.Server{{Id: "srv_zz", Name: "web-1", Connection: dockwellv1.Connection_CONNECTION_UNREACHABLE, LastSeenTime: timestamppb.New(at.Now().Add(-time.Hour))}}
	}}
	for range 11 {
		tickOn(t, e)
		at.add(alerts.Every)
	}
	began := at.Now()
	for at.Now().Sub(began) < 25*time.Hour {
		tickOn(t, e)
		step := time.Minute
		if at.Now().Sub(began) > time.Hour {
			step = 5 * time.Minute // tries are 15 minutes apart by then
		}
		at.add(step)
	}
	tries := len(h.of("server-unreachable"))
	if tries < 90 || tries > 120 {
		t.Errorf("a far end down for a day was tried %d times, want about every 15 minutes", tries)
	}
	var deliveries []store.AlertDelivery
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		deliveries, err = tx.RecentDeliveries(t.Context(), 50)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 || !strings.HasPrefix(deliveries[0].Said, "Dockwell gave up after") || deliveries[1].Said != "502 Bad Gateway" {
		t.Errorf("the channel's deliveries are %+v, want its first failure and when Dockwell gave up", deliveries)
	}
	at.add(time.Hour)
	tickOn(t, e)
	if got := len(h.of("server-unreachable")); got != tries {
		t.Errorf("a message given up went again: %d tries, then %d", tries, got)
	}
}

// A message that went gets its resolved one, however the alert ends: under a mute,
// and when its rule is removed, which the message says.
func TestAResolvedMessageFollowsWhatWentOut(t *testing.T) {
	h := newHook(t)
	f, c, b := evalStand(t, h.srv.URL+"/hook")
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	at := &clock{now: time.Now()}
	reachable := false
	e := evaluatorOn(f, at)
	e.Fleet = evalFleet{func() []*dockwellv1.Server {
		if reachable {
			return connectedAt(at.Now(), 50)
		}
		return []*dockwellv1.Server{{Id: "srv_zz", Name: "web-1", Connection: dockwellv1.Connection_CONNECTION_UNREACHABLE, LastSeenTime: timestamppb.New(at.Now().Add(-time.Hour))}}
	}}
	for range 11 {
		tickOn(t, e)
		at.add(alerts.Every)
	}
	if _, err := sites.MuteAlerts(t.Context(), connect.NewRequest(&dockwellv1.MuteAlertsRequest{Rule: "server-unreachable", Duration: "1h", Reason: "moving it"})); err != nil {
		t.Fatal(err)
	}
	reachable = true
	for range 12 { // 3 minutes, under the mute
		tickOn(t, e)
		at.add(alerts.Every)
	}
	if got := h.of("server-unreachable"); len(got) != 2 || got[1] != "resolved: Resolved: web-1 can't be reached" {
		t.Errorf("under a mute, the far end got %q, want the resolved message as well", got)
	}

	// Its rule removed, an alert that went out is closed, and the far end told why.
	reachable = false
	if _, err := sites.UnmuteAlerts(t.Context(), connect.NewRequest(&dockwellv1.UnmuteAlertsRequest{Rule: "server-unreachable"})); err != nil {
		t.Fatal(err)
	}
	for range 11 {
		tickOn(t, e)
		at.add(alerts.Every)
	}
	gone, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Stop watching servers", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.RuleKind), Name: "server-unreachable"}, Delete: true,
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, gone.Msg.GetChange())
	tickOn(t, e)
	h.mu.Lock()
	last := h.got[len(h.got)-1]
	h.mu.Unlock()
	if text, _ := last["text"].(string); last["status"] != "resolved" || !strings.Contains(text, "Its rule was removed") {
		t.Errorf("when its rule was removed, the far end last got %v", last)
	}
}

// The first of a rule's alerts waits 30 seconds for others to join it, counted
// from the oldest that hasn't gone: apps that go down 20 seconds apart don't hold
// it back.
func TestTheFirstOfManyIsntHeldBack(t *testing.T) {
	h := newHook(t)
	f, _, _ := evalStand(t, h.srv.URL+"/hook")
	at := &clock{now: time.Now()}
	start := at.Now()
	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("app-%02d", i)
	}
	e := evaluatorOn(f, at)
	e.Health = placedApps(names)
	e.Standings = staggered{start: start, every: 20 * time.Second, now: at.Now}
	var firstSent time.Duration
	for range 80 { // 20 minutes
		tickOn(t, e)
		if firstSent == 0 && len(h.of("app-down")) > 0 {
			firstSent = at.Now().Sub(start)
		}
		at.add(alerts.Every)
	}
	// The first app goes down at once and fires 2 minutes later.
	if firstSent == 0 || firstSent > 2*time.Minute+alerts.GroupWait+alerts.Every {
		t.Errorf("the first of 30 apps going down 20 seconds apart was sent %v in, want within %v", firstSent, 2*time.Minute+alerts.GroupWait+alerts.Every)
	}
}

// staggered says app-NN went down NN times every after start.
type staggered struct {
	start time.Time
	every time.Duration
	now   func() time.Time
}

func (s staggered) Standing(_, _, service string) controlhealth.Standing {
	var n int
	_, _ = fmt.Sscanf(service, "app-%d", &n)
	since := s.start.Add(time.Duration(n) * s.every)
	if s.now().Before(since) {
		return controlhealth.Standing{Heard: true, State: dockwellv1.HealthState_HEALTH_STATE_NO_CHECK}
	}
	return controlhealth.Standing{Heard: true, Down: true, Since: since, State: dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING}
}
