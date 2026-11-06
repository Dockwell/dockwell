// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	controlhealth "github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Restart reaches the API by a service's name or ID, as a change with a plan to
// apply, and makes no revision.
func TestRestartThroughTheAPI(t *testing.T) { storetest.Each(t, testRestartThroughTheAPI) }

func testRestartThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	made := apply(t, c, propose(t, c, "Run web", serviceEdit("web", "nginx:1.27", nil)))
	waitChange(t, c, made.GetId(), "web runs", func(ch *dockwellv1.Change) bool { return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED })
	id := made.GetPlan().GetResources()[0].GetId()
	runs := hz1.docker.Runs

	res, err := c.RestartService(t.Context(), connect.NewRequest(&dockwellv1.RestartServiceRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	ch := res.Msg.GetChange()
	if ch.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED || ch.GetSummary() != "Restart web" || !ch.GetRestart() {
		t.Errorf("the restart reads %v", ch)
	}
	if hz1.docker.Runs != runs {
		t.Error("planning a restart ran something")
	}
	steps := ch.GetPlan().GetServers()[0].GetSteps()
	if len(steps) != 1 || steps[0].GetAction() != dockwellv1.PlanAction_PLAN_ACTION_REPLACE || steps[0].GetRestart() != ch.GetId() || len(steps[0].GetChanges()) != 0 {
		t.Errorf("its steps are %v", steps)
	}
	if ch.GetPlan().GetSummary().GetReplace() != 1 {
		t.Errorf("its summary is %v", ch.GetPlan().GetSummary())
	}
	applied := apply(t, c, ch)
	done := waitChange(t, c, applied.GetId(), "the restart ends", func(ch *dockwellv1.Change) bool { return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED })
	if done.GetSteps()[0].GetMessage() != "Restarted service web on hz-1" {
		t.Errorf("its step says %q", done.GetSteps()[0].GetMessage())
	}
	if hz1.docker.Runs != runs+1 {
		t.Errorf("the restart ran %d containers", hz1.docker.Runs-runs)
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()})); reasonOf(err) != "cant_undo" {
		t.Errorf("undoing a restart = %v, want it refused", err)
	}

	// By name, as the CLI's first argument is, with a summary of its own.
	named, err := c.RestartService(t.Context(), connect.NewRequest(&dockwellv1.RestartServiceRequest{Id: "web", Summary: "Pick up the new certificate"}))
	if err != nil || named.Msg.GetChange().GetSummary() != "Pick up the new certificate" {
		t.Errorf("by name = %v, %v", named, err)
	}
	for name, req := range map[string]*dockwellv1.RestartServiceRequest{
		"no name":        {},
		"no such ID":     {Id: "svc_01JD6Z3R8M9QF2K7T4XW5BNC0V"},
		"no such name":   {Id: "ghost"},
		"another's kind": {Id: "prj_01JD6Z3R8M9QF2K7T4XW5BNC0V"},
	} {
		if _, err := c.RestartService(t.Context(), connect.NewRequest(req)); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	// A read-only token may not plan one, as it may not plan anything.
	reader := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	if _, err := reader.RestartService(t.Context(), connect.NewRequest(&dockwellv1.RestartServiceRequest{Id: id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("with a read-only token = %v", err)
	}
	// And over REST, as any plan.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.origin+"/v1/services/"+id+":restart", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	httpRes, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer httpRes.Body.Close()
	body, _ := io.ReadAll(httpRes.Body)
	var out struct {
		Change struct {
			Restart bool `json:"restart"`
		} `json:"change"`
	}
	if httpRes.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || !out.Change.Restart {
		t.Errorf("over REST = %d: %s", httpRes.StatusCode, body)
	}
}

// What an agent says of its services reaches the API, a read-only token reads it,
// and it says nothing an agent didn't.
func TestHealthThroughTheAPI(t *testing.T) { storetest.Each(t, testHealthThroughTheAPI) }

func testHealthThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	made := apply(t, c, propose(t, c, "Run web", serviceEdit("web", "nginx:1.27", nil)))
	waitChange(t, c, made.GetId(), "web runs", func(ch *dockwellv1.Change) bool { return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED })
	res := made.GetPlan().GetResources()[0]
	project := docker.Project(res.GetResource().GetScope())

	h := dockwellv1connect.NewHealthServiceClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	list := func() *dockwellv1.ServiceHealth {
		t.Helper()
		got, err := h.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Msg.GetServices()) != 1 {
			t.Fatalf("it lists %v", got.Msg.GetServices())
		}
		return got.Msg.GetServices()[0]
	}
	if s := list(); s.GetState() != dockwellv1.HealthState_HEALTH_STATE_UNKNOWN {
		t.Errorf("with nothing said, it reads %v", s.GetState())
	}
	f.agents.ReportHealth(hz1.id.ServerID, &agentv1.Health{ScannedAt: timestamppb.Now(), ReportedAt: timestamppb.Now(), Instances: []*dockwellv1.InstanceHealth{{
		Project: project, Service: "web", Number: 3, Container: project + "-web-3", State: dockwellv1.HealthState_HEALTH_STATE_HEALTHY,
		Check: dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_HTTP, Since: timestamppb.Now(),
		Last: &dockwellv1.HealthResult{Passed: true, Output: "GET /healthz on port 8080 answered 200 OK", Time: timestamppb.Now()},
	}}})
	s := list()
	if s.GetState() != dockwellv1.HealthState_HEALTH_STATE_HEALTHY || s.GetId() != res.GetId() || s.GetServers()[0].GetInstances()[0].GetNumber() != 3 ||
		!strings.Contains(s.GetServers()[0].GetInstances()[0].GetLast().GetOutput(), "200 OK") {
		t.Errorf("it reads %v", s)
	}
	hz1.stop()
	waitConnected(t, f, hz1.id.ServerID, false)
	s = list()
	if s.GetState() != dockwellv1.HealthState_HEALTH_STATE_UNKNOWN || !s.GetServers()[0].GetStale() {
		t.Errorf("with its agent gone, it reads %v, want unknown and stale", s)
	}
}

// A change of health is on the timeline, with what the check said, for the
// service, its server, and everyone reading the timeline, as an event of its own.
func TestHealthOnTheTimeline(t *testing.T) { storetest.Each(t, testHealthOnTheTimeline) }

// A service that keeps changing is one event, which a watch sees change as it goes
// on: its time, and how often it changed since it began.
func TestAFlapIsOneEventThatChanges(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	made := apply(t, c, propose(t, c, "Run web", serviceEdit("web", "nginx:1.27", nil)))
	waitChange(t, c, made.GetId(), "web runs", func(ch *dockwellv1.Change) bool { return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED })
	project := docker.Project(made.GetPlan().GetResources()[0].GetResource().GetScope())

	now := time.Now()
	r := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	say := func(state dockwellv1.HealthState) {
		r.Heard(hz1.id.ServerID, []*dockwellv1.InstanceHealth{{Project: project, Service: "web", Number: 1, State: state}})
		now = now.Add(5 * time.Second)
		r.Record(t.Context())
	}
	w := watchEvents(t, eventClient(b, b.origin), &dockwellv1.WatchEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}})
	say(dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
	began := now
	for range 3 {
		say(dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING)
		say(dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
	}
	health := func(w *eventWatch) []*dockwellv1.Event {
		var out []*dockwellv1.Event
		for _, e := range w.latest {
			if e.GetHealth() != nil {
				out = append(out, e)
			}
		}
		return out
	}
	w.until("it keeps changing", func(w *eventWatch) bool { return len(health(w)) == 1 })
	first := health(w)[0]
	if first.GetHealth().GetState() != dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING || first.GetHealth().GetFlips() < 3 || !first.GetHealth().GetSince().AsTime().Equal(began) {
		t.Errorf("the event says %v, want not running, 3 changes or more, since %s", first.GetHealth(), began)
	}
	for range 6 {
		say(dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING)
		say(dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
	}
	w.until("its event changes", func(w *eventWatch) bool {
		got := health(w)
		return len(got) == 1 && got[0].GetHealth().GetFlips() >= 12
	})
	again := health(w)[0]
	if again.GetId() != first.GetId() || !again.GetTime().AsTime().After(first.GetTime().AsTime()) || !again.GetHealth().GetSince().AsTime().Equal(began) {
		t.Errorf("as it went on, the event went from %v to %v, want one event, told later, since %s", first, again, began)
	}
	if got := listEvents(t, eventClient(b, b.origin), &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}}); len(got) != 1 {
		t.Errorf("the timeline has %d health events, want one", len(got))
	}
}

func testHealthOnTheTimeline(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	made := apply(t, c, propose(t, c, "Run web", serviceEdit("web", "nginx:1.27", nil)))
	waitChange(t, c, made.GetId(), "web runs", func(ch *dockwellv1.Change) bool { return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED })
	res := made.GetPlan().GetResources()[0]
	project := docker.Project(res.GetResource().GetScope())

	now := time.Now()
	r := &controlhealth.Recorder{DB: f.store, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	r.Heard(hz1.id.ServerID, []*dockwellv1.InstanceHealth{{
		Project: project, Service: "web", Number: 1, State: dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY,
		Last: &dockwellv1.HealthResult{Output: "GET /healthz on port 8080 answered 503 Service Unavailable"},
	}})
	r.Record(t.Context())
	now = now.Add(controlhealth.Dwell)
	r.Record(t.Context())

	e := eventClient(b, b.origin)
	byKind := listEvents(t, e, &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}})
	if len(byKind) != 1 {
		t.Fatalf("the timeline's health events are %v", byKind)
	}
	ev := byKind[0]
	if ev.GetAction() != controlhealth.Action || ev.GetSubject().GetId() != res.GetId() || ev.GetSubject().GetName() != "web" || ev.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
		t.Errorf("the event is %v", ev)
	}
	h := ev.GetHealth()
	if h.GetState() != dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY || h.GetServerId() != hz1.id.ServerID || h.GetServerName() != "hz-1" || !strings.Contains(h.GetOutput(), "503") {
		t.Errorf("what it says is %v", h)
	}
	// It is told once it has lasted, and says when it came about.
	if since := h.GetSince().AsTime(); !since.Equal(now.Add(-controlhealth.Dwell)) || !ev.GetTime().AsTime().Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("it says it came about at %s, told at %s; want %s, told at %s", since, ev.GetTime().AsTime(), now.Add(-controlhealth.Dwell), now)
	}
	for name, req := range map[string]*dockwellv1.ListEventsRequest{
		"its service": {ResourceId: res.GetId(), Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}},
		"its server":  {ServerId: hz1.id.ServerID, Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}},
		"everything":  {},
	} {
		found := false
		for _, got := range listEvents(t, e, req) {
			found = found || got.GetId() == ev.GetId()
		}
		if !found {
			t.Errorf("reading %s, the event isn't there", name)
		}
	}
	if got := listEvents(t, e, &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_CHANGE}}); len(got) == 0 || got[0].GetId() == ev.GetId() {
		t.Errorf("changes alone read as %v", got)
	}
}
