// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	agentfiles "github.com/dockwell/dockwell/internal/agent/files"
	agentupdates "github.com/dockwell/dockwell/internal/agent/updates"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// gate is a firewall whose rule "slow" takes until its call is cancelled,
// or until it is released, to put in place, so a test can drop the stream
// while a step runs.
type gate struct {
	*firewall.Fake
	started chan struct{}
	release chan struct{}
}

func (g *gate) Put(ctx context.Context, name string, rule *resourcev1.FirewallRule) error {
	if name == "slow" {
		g.started <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.release:
		}
	}
	return g.Fake.Put(ctx, name, rule)
}

// testServer is a server with an agent, whose drivers run Docker and a
// firewall in memory.
type testServer struct {
	id      *enroll.Identity
	docker  *containertest.Fake
	fw      *gate
	drivers *resource.Registry
	logs    agent.LogReader
	// client is the Docker its agent lists containers from, when it has one.
	client *docker.Client
	stop   func()
	// hello changes what its agent says of itself, to stand in for another version.
	hello func(*agentv1.Hello)
	// probe answers what a website's server is asked of what it sends to, as a
	// test asks nothing beyond this machine. Nil answers everything.
	probe func(context.Context, *agentv1.ProbeCall) *agentv1.ProbeResult
	// health says how its services are, when set.
	health agent.HealthReporter
	// disk reads its files. dataDir is what that takes for Dockwell's directory.
	disk    *agentfiles.Server
	dataDir string
	// backups takes its backups, when it has Docker.
	backups *backup.Runner
	// updates reads its package updates, when set.
	updates *agentupdates.Reader
	// onePanel reads what 1Panel keeps on it, when set.
	onePanel func(context.Context) (*agentv1.ReadOnePanelResult, error)
}

// addServer enrolls a server, hz-1, and connects its agent with fake
// drivers.
// With no drivers, its agent has none, as in this build.
func (f *fullServer) addServer(t *testing.T, withDrivers bool) *testServer {
	t.Helper()
	return f.addNamedServer(t, "hz-1", withDrivers)
}

// addNamedServer is addServer for a server with another name.
func (f *fullServer) addNamedServer(t *testing.T, name string, withDrivers bool) *testServer {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:     enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		docker: containertest.NewFake(), fw: &gate{Fake: firewall.NewFake(), started: make(chan struct{}, 4), release: make(chan struct{})},
		dataDir: t.TempDir(),
	}
	s.disk = agentfiles.New(s.dataDir, nil, nil)
	if withDrivers {
		containers := container.Driver{Runtime: s.docker, RecreateDowntime: 2 * time.Second}
		s.drivers = resource.NewRegistry(&containers, &firewall.Driver{Backend: s.fw}, &service.Driver{Containers: containers})
	}
	s.connect(t, f)
	return s
}

// connect runs the server's agent until stop, and waits for it.
func (s *testServer) connect(t *testing.T, f *fullServer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// It says nothing of the world, whatever machine the test runs on.
	collector := collect.New()
	collector.Metadata, collector.STUN = collect.ClosedMetadata, []string{}
	cfg := agent.Config{
		Collector: collector,
		Logger:    slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: s.id, Build: buildinfo.Read(), Drivers: s.drivers, Logs: s.logs,
		Docker: s.client, Hello: s.hello, Health: s.health, Files: s.disk, Updates: s.updates, Probe: s.probe, OnePanel: s.onePanel,
	}
	if cfg.Probe == nil {
		cfg.Probe = answersEverything
	}
	if s.backups != nil {
		cfg.Backups = backup.Calls{Runner: s.backups}
	}
	if s.client != nil {
		cfg.Databases = database.Reader{Client: s.client}
	}
	go func() {
		_ = agent.Run(ctx, cfg)
		close(done)
	}()
	var once sync.Once
	s.stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(s.stop)
	waitConnected(t, f, s.id.ServerID, true)
}

// answersEverything stands in for a probe that found everything answering.
func answersEverything(_ context.Context, call *agentv1.ProbeCall) *agentv1.ProbeResult {
	if call.GetPort() != 0 {
		return &agentv1.ProbeResult{Answered: true, Said: fmt.Sprintf("something listens on port %d", call.GetPort())}
	}
	return &agentv1.ProbeResult{Answered: true, Said: "200 OK"}
}

func waitConnected(t *testing.T, f *fullServer, server string, connected bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := f.agents.Agent(server); ok == connected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s's agent to be connected: %t", server, connected)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recorder keeps every response body a client reads, to search them.
type recorder struct {
	next connect.HTTPClient
	mu   sync.Mutex
	buf  bytes.Buffer
}

type lockedWriter struct{ r *recorder }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	return w.r.buf.Write(p)
}

func (r *recorder) Do(req *http.Request) (*http.Response, error) {
	res, err := r.next.Do(req)
	if err == nil {
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(res.Body, lockedWriter{r}), res.Body}
	}
	return res, err
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// signedIn claims the install and returns a browser with a session.
func signedIn(t *testing.T, f *fullServer) *browser {
	t.Helper()
	_, port, _ := net.SplitHostPort(f.api)
	b := newBrowser(t, "http://localhost:"+port, f.api, nil)
	link, err := f.auth.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	b.claim(t, link.Token)
	return b
}

func changeClient(c connect.HTTPClient, origin string) dockwellv1connect.ChangeServiceClient {
	return dockwellv1connect.NewChangeServiceClient(c, origin, connect.WithProtoJSON())
}

// webEdit runs a container named web on the only server, which it leaves
// to be filled in.
func webEdit(image string, env map[string]string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "web"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Container{Container: &resourcev1.Container{
			Image: image, Env: env, Ports: []*resourcev1.PortMapping{{HostPort: 80, ContainerPort: 8080}},
		}}},
	}
}

// ruleEdit opens a port on the only server.
func ruleEdit(name string, port uint32) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(firewall.Kind), Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: &resourcev1.FirewallRule{Port: port}}},
	}
}

func propose(t *testing.T, c dockwellv1connect.ChangeServiceClient, summary string, edits ...*dockwellv1.Edit) *dockwellv1.Change {
	t.Helper()
	res, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: summary, Edits: edits}))
	if err != nil {
		t.Fatalf("ProposeChange %q: %v", summary, err)
	}
	return res.Msg.GetChange()
}

// watched is what a watch of a change received.
type watched struct {
	msgs []*dockwellv1.WatchChangeResponse
	err  error
}

// last is the change as the watch saw it last.
func (w watched) last() *dockwellv1.Change {
	for i := len(w.msgs) - 1; i >= 0; i-- {
		if c := w.msgs[i].GetChange(); c != nil {
			return c
		}
	}
	return nil
}

// watch follows a change until its watch ends. It returns once the watch
// has sent the change as it stands, so nothing after is missed.
func watch(t *testing.T, c dockwellv1connect.ChangeServiceClient, changeID string) <-chan watched {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	stream, err := c.WatchChange(ctx, connect.NewRequest(&dockwellv1.WatchChangeRequest{Id: changeID}))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !stream.Receive() {
		cancel()
		t.Fatalf("WatchChange %s sent nothing: %v", changeID, stream.Err())
	}
	out := make(chan watched, 1)
	go func() {
		defer cancel()
		w := watched{msgs: []*dockwellv1.WatchChangeResponse{stream.Msg()}}
		for stream.Receive() {
			w.msgs = append(w.msgs, stream.Msg())
		}
		w.err = stream.Err()
		_ = stream.Close()
		out <- w
	}()
	return out
}

func waitWatch(t *testing.T, ch <-chan watched) watched {
	t.Helper()
	select {
	case w := <-ch:
		if w.err != nil {
			t.Fatalf("the watch ended with %v", w.err)
		}
		return w
	case <-time.After(20 * time.Second):
		t.Fatal("the watch didn't end")
	}
	return watched{}
}

func apply(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId()})); err != nil {
		t.Fatalf("ApplyChange %s: %v", ch.GetSummary(), err)
	}
	return waitWatch(t, w).last()
}

func reasonOf(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Meta().Get(authapi.ReasonKey)
	}
	return ""
}

// choiceOf is the Choice detail of an error, for a client to ask which.
func choiceOf(err error) *dockwellv1.Choice {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return nil
	}
	for _, d := range cerr.Details() {
		if v, err := d.Value(); err == nil {
			if c, ok := v.(*dockwellv1.Choice); ok {
				return c
			}
		}
	}
	return nil
}

// The engine end to end through the API: propose and get a plan, apply it
// while a second client watches each step, change it, undo it, and
// discard a plan.
func TestPlanApplyAndUndoThroughTheAPI(t *testing.T) {
	storetest.Each(t, testPlanApplyAndUndoThroughTheAPI)
}

func testPlanApplyAndUndoThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	observer := changeClient(newBrowserLike(b), b.origin) // a second client, such as the CLI

	// With one server, a container needs no server named: it is the only one.
	planned := propose(t, c, "Run web", webEdit("nginx:1.27", map[string]string{"API_KEY": "s3cret-value"}))
	plan := planned.GetPlan()
	if planned.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED || planned.GetProposedVia() != dockwellv1.Surface_SURFACE_PANEL ||
		planned.GetProposedBy().GetName() != "Mira Chen" || planned.GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER {
		t.Errorf("proposed change = %v", planned)
	}
	if len(plan.GetResources()) != 1 || plan.GetResources()[0].GetAction() != dockwellv1.PlanAction_PLAN_ACTION_CREATE ||
		plan.GetResources()[0].GetResource().GetScope() != hz1.id.ServerID || plan.GetSummary().GetCreate() != 1 {
		t.Errorf("plan = %v, want one container to create on hz-1", plan)
	}
	if len(plan.GetServers()) != 1 || plan.GetServers()[0].GetServerName() != "hz-1" || len(plan.GetServers()[0].GetSteps()) != 1 ||
		plan.GetServers()[0].GetSteps()[0].GetResourceId() != plan.GetResources()[0].GetId() {
		t.Errorf("plan's servers = %v, want one step on hz-1", plan.GetServers())
	}
	if plan.GetFingerprint() == "" || plan.GetUndoWindow().AsDuration() != revision.UndoWindow || plan.GetDestructive() {
		t.Errorf("plan = %v, want a fingerprint, 7 days to undo, and nothing destructive", plan)
	}
	if hz1.docker.Runs != 0 {
		t.Fatal("planning ran a container")
	}

	// The same proposal against the same state plans the same, with a
	// fingerprint of its own, as a container is a kind that may hold secrets.
	again := propose(t, c, "Run web", webEdit("nginx:1.27", map[string]string{"API_KEY": "s3cret-value"}))
	if again.GetPlan().GetFingerprint() == plan.GetFingerprint() || again.GetId() == planned.GetId() ||
		!proto.Equal(again.GetPlan().GetSummary(), plan.GetSummary()) || len(again.GetPlan().GetServers()) != len(plan.GetServers()) {
		t.Errorf("planning again gave %v, want %v with a fingerprint of its own", again.GetPlan(), plan)
	}
	discarded, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: again.GetId()}))
	if err != nil || discarded.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_DISCARDED {
		t.Fatalf("DiscardChange = %v, %v", discarded, err)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: again.GetId()})); reasonOf(err) != "not_planned" {
		t.Errorf("applying a discarded change = %v, want not_planned", err)
	}

	// A second client watches while the first applies.
	w := watch(t, observer, planned.GetId())
	res, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: planned.GetId(), Fingerprint: plan.GetFingerprint()}))
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Msg.GetChange().GetState(); s != dockwellv1.ChangeState_CHANGE_STATE_APPLYING && s != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("ApplyChange returned the change %v", s)
	}
	seen := waitWatch(t, w)
	checkProgress(t, seen, 1)
	done := seen.last()
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || done.GetAppliedVia() != dockwellv1.Surface_SURFACE_PANEL ||
		done.GetAppliedBy().GetName() != "Mira Chen" || done.GetApplyTime() == nil ||
		done.GetUndoDeadline().AsTime().Sub(done.GetApplyTime().AsTime()) != revision.UndoWindow {
		t.Errorf("the applied change = %v", done)
	}
	if got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"}); got.GetImage() != "nginx:1.27" {
		t.Errorf("hz-1 runs %v, want nginx:1.27", got)
	}

	// A change of image recreates the container: a replace, counted on its
	// own.
	update := propose(t, c, "Update web", webEdit("nginx:1.28", map[string]string{"API_KEY": "s3cret-value"}))
	if s := update.GetPlan().GetSummary(); s.GetReplace() != 1 || s.GetChange() != 0 || update.GetPlan().GetServers()[0].GetDowntime().AsDuration() != 2*time.Second {
		t.Errorf("updating the image plans %v, downtime %v, want one replace of 2s", s, update.GetPlan().GetServers()[0].GetDowntime())
	}
	if got := apply(t, c, update); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the update = %v", got)
	}

	// Undo plans putting the old image back, and applying it does.
	undoRes, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: update.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	undo := undoRes.Msg.GetChange()
	if undo.GetReverts() != update.GetId() || undo.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED || undo.GetPlan().GetSummary().GetReplace() != 1 {
		t.Errorf("the undo = %v", undo)
	}
	if got := apply(t, c, undo); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo = %v", got)
	}
	if got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"}); got.GetImage() != "nginx:1.27" {
		t.Errorf("after the undo hz-1 runs %v, want nginx:1.27", got)
	}
	got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: update.GetId()}))
	if err != nil || got.Msg.GetChange().GetUndoneBy() != undo.GetId() {
		t.Errorf("GetChange of the undone change = %v, %v, want it undone by %s", got, err, undo.GetId())
	}
	if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: planned.GetId()})); reasonOf(err) != "cant_undo" {
		t.Errorf("undoing a change whose resource changed since = %v, want cant_undo", err)
	}

	// Changes list newest first, a page at a time; the defaults' change is
	// the oldest.
	var all []*dockwellv1.Change
	token := ""
	for range 10 {
		page, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Msg.GetChanges()...)
		if token = page.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(all) != 5 || all[0].GetId() != undo.GetId() || all[4].GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
		t.Errorf("ListChanges gave %d changes, newest %s; want 5, newest %s, oldest by Dockwell", len(all), all[0].GetId(), undo.GetId())
	}

	// The audit log has each step for the timeline: the change, who, from
	// where, and how it ended.
	var events []store.Event
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err = tx.ListEvents(t.Context(), 100)
		return err
	})
	want := map[string]string{planned.GetId(): "change.applied", again.GetId(): "change.discarded", undo.GetId(): "change.applied"}
	for i := range events {
		e := &events[i]
		if action, ok := want[e.Subject]; ok && e.Action == action {
			if !strings.HasPrefix(e.Actor, "user:") || e.Surface != revision.ViaPanel {
				t.Errorf("event %s %s by %q via %q, want a user via the panel", e.Action, e.Subject, e.Actor, e.Surface)
			}
			delete(want, e.Subject)
		}
	}
	if len(want) > 0 {
		t.Errorf("the audit log lacks %v", want)
	}
}

// checkProgress checks a watch of an applied change: the change as it
// stood, then applying with its steps queued, then each step running and
// done in turn, then the change applied.
func checkProgress(t *testing.T, w watched, steps int) {
	t.Helper()
	var states []string
	for _, m := range w.msgs {
		if c := m.GetChange(); c != nil {
			states = append(states, c.GetState().String())
			continue
		}
		states = append(states, m.GetStep().GetStep().GetState().String())
	}
	want := []string{"CHANGE_STATE_PLANNED", "CHANGE_STATE_APPLYING"}
	for range steps {
		want = append(want, "STEP_STATE_RUNNING", "STEP_STATE_DONE")
	}
	want = append(want, "CHANGE_STATE_APPLIED")
	if strings.Join(states, " ") != strings.Join(want, " ") {
		t.Errorf("the watch saw %v, want %v", states, want)
		for _, m := range w.msgs {
			t.Logf("%v", m)
		}
	}
	for _, m := range w.msgs {
		if c := m.GetChange(); c != nil && c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLYING {
			if len(c.GetSteps()) != steps || c.GetSteps()[0].GetState() != dockwellv1.StepState_STEP_STATE_QUEUED {
				t.Errorf("the applying change's steps = %v, want %d queued", c.GetSteps(), steps)
			}
		}
		if s := m.GetStep().GetStep(); s != nil && s.GetState() == dockwellv1.StepState_STEP_STATE_DONE &&
			(!strings.HasPrefix(s.GetMessage(), "Created") && !strings.HasPrefix(s.GetMessage(), "Replaced") || s.GetFinishTime() == nil) {
			t.Errorf("a done step says %q, finished %v", s.GetMessage(), s.GetFinishTime())
		}
	}
}

// waitChange polls a change until cond holds.
func waitChange(t *testing.T, c dockwellv1connect.ChangeServiceClient, changeID, what string, cond func(*dockwellv1.Change) bool) *dockwellv1.Change {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: changeID}))
		if err == nil && cond(res.Msg.GetChange()) {
			return res.Msg.GetChange()
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s: %v, %v", what, res, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newBrowserLike is a second client signed in as the same person, as the
// CLI would be beside the panel.
func newBrowserLike(b *browser) *browser {
	c := *b
	return &c
}

// Projects and environments run on no server, so their changes apply end
// to end with no agent: this is what works in production before M3. A new
// project gets its production environment in the same change, and the
// hidden defaults keep working until there is a choice.
func TestProjectsApplyAndUndoWithNoServer(t *testing.T) {
	storetest.Each(t, testProjectsApplyAndUndoWithNoServer)
}

func testProjectsApplyAndUndoWithNoServer(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)

	// The only project needs no name for a new environment to live in.
	staging := propose(t, c, "Add staging", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "staging"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{DisplayName: "Staging"}}},
	})
	if r := staging.GetPlan().GetResources(); len(r) != 1 || r[0].GetResource().GetKind() != "environment" || !strings.HasPrefix(r[0].GetResource().GetScope(), "prj_") {
		t.Errorf("a new environment plans %v, want it in the default project", r)
	}
	if got := apply(t, c, staging); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || len(got.GetSteps()) != 0 {
		t.Fatalf("adding an environment = %v", got)
	}

	shop := propose(t, c, "Add the shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Shop"}}},
	})
	plan := shop.GetPlan()
	if len(plan.GetResources()) != 2 || plan.GetResources()[0].GetResource().GetKind() != "project" ||
		plan.GetResources()[1].GetResource().GetName() != "production" || plan.GetSummary().GetCreate() != 2 || len(plan.GetServers()) != 0 {
		t.Errorf("a new project plans %v, want it and its production environment, on no server", plan)
	}
	applied := apply(t, c, shop)
	if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("adding a project = %v", applied)
	}

	// Two projects: an environment must now say which.
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add qa", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Name: "qa"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	}}}))
	if reasonOf(err) != "choice" || !strings.Contains(err.Error(), "default and shop") {
		t.Errorf("an environment with no project once there are two = %v, want a choice naming them", err)
	}
	if c := choiceOf(err); c.GetLevel() != "project" || c.GetField() != "edits[0].resource.scope" || !slices.Equal(c.GetOptions(), []string{"default", "shop"}) {
		t.Errorf("the choice's detail = %v, want the projects, for the environment's scope", c)
	}
	// REST carries the detail in its google.rpc.Status.
	status, reason, body := callREST(t, b, http.MethodPost, "/v1/changes", nil,
		`{"summary": "Add qa", "edits": [{"resource": {"name": "qa"}, "spec": {"environment": {}}}]}`)
	if status != http.StatusBadRequest || reason != "choice" || !strings.Contains(body, "dockwell.v1.Choice") || !strings.Contains(body, `"edits[0].resource.scope"`) {
		t.Errorf("the choice over REST = %d %s %s, want the detail in the status", status, reason, body)
	}
	// Naming the project works, by name.
	propose(t, c, "Add qa", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Scope: "shop", Name: "qa"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	})

	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: shop.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if s := undo.Msg.GetChange().GetPlan().GetSummary(); s.GetDestroy() != 2 {
		t.Errorf("undoing a new project plans %v, want it and its environment destroyed", s)
	}
	if got := apply(t, c, undo.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing the project = %v", got)
	}
	var projects []store.Resource
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		org, _ := tx.Organization(t.Context())
		projects, err = tx.ListResources(t.Context(), org.ID, "project")
		return err
	})
	if len(projects) != 1 || projects[0].Name != "default" {
		t.Errorf("after the undo the projects are %v, want only default", projects)
	}
}

// Apply refuses a plan that no longer holds: a resource that changed
// since, or servers that no longer match.
func TestStaleAndChangedPlans(t *testing.T) { storetest.Each(t, testStaleAndChangedPlans) }

func testStaleAndChangedPlans(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)

	first := propose(t, c, "Open http", ruleEdit("http", 80))
	second := propose(t, c, "Open http on 8080", ruleEdit("http", 8080))
	apply(t, c, first)
	_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: second.GetId()}))
	if reasonOf(err) != "plan_stale" || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), first.GetId()) {
		t.Errorf("applying a plan made before another change = %v, want plan_stale naming %s", err, first.GetId())
	}

	// The server moved on its own: the plan no longer matches it.
	edit := propose(t, c, "Open http on 8080", ruleEdit("http", 8080))
	if err := hz1.fw.Delete(t.Context(), "http"); err != nil {
		t.Fatal(err)
	}
	_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: edit.GetId()}))
	if reasonOf(err) != "plan_changed" {
		t.Errorf("applying a plan the server no longer matches = %v, want plan_changed", err)
	}
	if _, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: "chg_nothing"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("GetChange of no change = %v, want not found", err)
	}
}

// A step that fails stops the change, and the steps already done are
// rolled back in a change of Dockwell's own, which the failed one names.
func TestAFailedApplyRollsBack(t *testing.T) { storetest.Each(t, testAFailedApplyRollsBack) }

func testAFailedApplyRollsBack(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	hz1.docker.Broken["nginx:broken"] = true
	c := changeClient(b, b.origin)

	failed := apply(t, c, propose(t, c, "Open http and run web", ruleEdit("http", 80), ruleEdit("https", 443),
		webEdit("nginx:broken", nil)))
	if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || failed.GetRolledBackBy() == "" || !strings.Contains(failed.GetError(), "isn't healthy") {
		t.Fatalf("a change with a broken image = %v, want it failed and rolled back", failed)
	}
	var states []dockwellv1.StepState
	for _, s := range failed.GetSteps() {
		states = append(states, s.GetState())
	}
	if len(states) != 3 || states[0] != dockwellv1.StepState_STEP_STATE_DONE || states[2] != dockwellv1.StepState_STEP_STATE_FAILED {
		t.Errorf("the failed change's steps are %v, want done, done, failed", states)
	}
	// The watch of the failed change ends once its rollback is on record;
	// the rollback has a watch of its own.
	rb := waitWatch(t, watch(t, c, failed.GetRolledBackBy())).last()
	if !rb.GetRollback() || rb.GetReverts() != failed.GetId() || rb.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED ||
		rb.GetAppliedVia() != dockwellv1.Surface_SURFACE_AUTOMATIC || rb.GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
		t.Errorf("the rollback = %v", rb)
	}
	if hz1.fw.Len() != 0 {
		t.Errorf("after the rollback hz-1 has %d rules, want none", hz1.fw.Len())
	}
}

// A stream that drops while a step runs fails the step, and the rollback
// waits for the server; when its agent reconnects, Resume finishes it.
func TestADroppedStreamResumesOnReconnect(t *testing.T) {
	storetest.Each(t, testADroppedStreamResumesOnReconnect)
}

func testADroppedStreamResumesOnReconnect(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)

	ch := propose(t, c, "Open http and slow", ruleEdit("http", 80), ruleEdit("slow", 8080))
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId()})); err != nil {
		t.Fatal(err)
	}
	<-hz1.fw.started // the slow rule's step is running
	hz1.stop()
	failed := waitWatch(t, w).last()
	if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(failed.GetError(), "went offline") || failed.GetRolledBackBy() == "" {
		t.Fatalf("a change whose stream dropped = %v, want it failed, with a rollback", failed)
	}
	if !slices.ContainsFunc(failed.GetSteps(), func(s *dockwellv1.StepProgress) bool {
		return s.GetState() == dockwellv1.StepState_STEP_STATE_FAILED && s.GetLost()
	}) {
		t.Errorf("a change whose stream dropped has steps %v, want the one it was on marked lost", failed.GetSteps())
	}
	rb := waitChange(t, c, failed.GetRolledBackBy(), "the rollback waits for hz-1", func(c *dockwellv1.Change) bool {
		return c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLYING && len(c.GetSteps()) > 0 &&
			c.GetSteps()[0].GetState() == dockwellv1.StepState_STEP_STATE_WAITING && strings.Contains(c.GetSteps()[0].GetMessage(), "reconnect")
	})
	if hz1.fw.Len() != 1 {
		t.Fatalf("hz-1 has %d rules while its agent is away, want http still there", hz1.fw.Len())
	}

	// The agent reconnects, and the rollback finishes.
	rw := watch(t, c, rb.GetId())
	hz1.connect(t, f)
	done := waitWatch(t, rw).last()
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the rollback after reconnecting = %v", done)
	}
	if hz1.fw.Len() != 0 {
		t.Errorf("after the rollback hz-1 has %d rules, want none", hz1.fw.Len())
	}
}

// Drift is listed as a diff from the latest revision, and can be reverted
// or adopted, each as a plan.
func TestDriftThroughTheAPI(t *testing.T) { storetest.Each(t, testDriftThroughTheAPI) }

func testDriftThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run web", webEdit("nginx:1.27", nil)))

	listDrift := func() *dockwellv1.ListDriftResponse {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	if d := listDrift(); len(d.GetDrift()) != 0 || len(d.GetUncheckedServers()) != 0 {
		t.Fatalf("drift right after applying = %v", d)
	}
	changed := func(image string) {
		got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"})
		got.Image = image
		_ = hz1.docker.Run(t.Context(), container.Target{Name: "web"}, got, resource.Decision{})
	}
	changed("nginx:hand-edit")
	d := listDrift().GetDrift()
	if len(d) != 1 || d[0].GetServerId() != hz1.id.ServerID || d[0].GetResource().GetName() != "web" || len(d[0].GetChanges()) != 1 ||
		d[0].GetChanges()[0].GetTo() != "nginx:hand-edit" {
		t.Fatalf("ListDrift = %v, want web's image", d)
	}
	revert, err := c.RevertDrift(t.Context(), connect.NewRequest(&dockwellv1.RevertDriftRequest{ServerId: hz1.id.ServerID, ResourceId: d[0].GetResourceId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, revert.Msg.GetChange())
	if got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"}); got.GetImage() != "nginx:1.27" {
		t.Errorf("after reverting, hz-1 runs %s", got.GetImage())
	}

	changed("nginx:keep-me")
	d = listDrift().GetDrift()
	adopt, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: hz1.id.ServerID, ResourceId: d[0].GetResourceId()}))
	if err != nil {
		t.Fatal(err)
	}
	if p := adopt.Msg.GetChange().GetPlan(); len(p.GetServers()) != 0 || p.GetSummary().GetChange() != 1 {
		t.Errorf("adopting plans %v, want a new revision and nothing to do on the server", p)
	}
	runs := hz1.docker.Runs
	apply(t, c, adopt.Msg.GetChange())
	if d := listDrift(); len(d.GetDrift()) != 0 || hz1.docker.Runs != runs {
		t.Errorf("after adopting, drift is %v and hz-1 ran %d containers, want none", d, hz1.docker.Runs-runs)
	}
	if _, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: hz1.id.ServerID, ResourceId: d[0].GetResourceId()})); reasonOf(err) != "no_drift" {
		t.Errorf("adopting drift that is gone = %v, want no_drift", err)
	}

	// A server that isn't connected isn't checked, and says so.
	hz1.stop()
	waitConnected(t, f, hz1.id.ServerID, false)
	if d := listDrift(); len(d.GetUncheckedServers()) != 1 || d.GetUncheckedServers()[0].GetReason() != "server_offline" {
		t.Errorf("drift with hz-1 away = %v, want it unchecked", d)
	}
}

// Secrets never leave the API: not in plans, progress, history, drift,
// audit events or logs, whichever call or protocol asks.
func TestSecretsNeverLeaveTheAPI(t *testing.T) { storetest.Each(t, testSecretsNeverLeaveTheAPI) }

func testSecretsNeverLeaveTheAPI(t *testing.T, backend store.Backend) {
	logs := &syncBuffer{}
	f := startFullLogging(t, backend, nil, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	rec := &recorder{next: b}
	c := changeClient(rec, b.origin)

	created := apply(t, c, propose(t, c, "Run web", webEdit("nginx:1.27", map[string]string{"API_KEY": "s3cret-one", "MODE": "live"})))
	updated := propose(t, c, "Rotate the key", webEdit("nginx:1.27", map[string]string{"API_KEY": "s3cret-two", "MODE": "live"}))
	var key *dockwellv1.FieldChange
	for _, ch := range updated.GetPlan().GetServers()[0].GetSteps()[0].GetChanges() {
		if ch.GetPath() == "env[API_KEY]" {
			key = ch
		}
	}
	if key == nil || !key.GetSensitive() || key.GetOp() != dockwellv1.FieldOp_FIELD_OP_MODIFIED || key.GetFrom() != "" || key.GetTo() != "" {
		t.Errorf("the plan shows the key's change as %v, want it modified, sensitive, with no values", key)
	}
	apply(t, c, updated)
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: updated.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, undo.Msg.GetChange())

	got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"})
	got.Env["API_KEY"] = "s3cret-by-hand"
	_ = hz1.docker.Run(t.Context(), container.Target{Name: "web"}, got, resource.Decision{})
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{ServerId: hz1.id.ServerID}))
	if err != nil || len(drift.Msg.GetDrift()) != 1 {
		t.Fatalf("ListDrift = %v, %v", drift, err)
	}
	adopt, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: hz1.id.ServerID, ResourceId: drift.Msg.GetDrift()[0].GetResourceId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, adopt.Msg.GetChange())
	if _, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: created.GetId()})); err != nil {
		t.Fatal(err)
	}
	// REST, and the gRPC-Web protocol, carry the same messages.
	for _, path := range []string{"/v1/changes", "/v1/changes/" + created.GetId(), "/v1/drift"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+path, http.NoBody)
		res, err := rec.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %v, %v", path, res, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	grpcWeb := dockwellv1connect.NewChangeServiceClient(rec, b.origin, connect.WithGRPCWeb())
	if _, err := grpcWeb.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: updated.GetId()})); err != nil {
		t.Fatal(err)
	}

	responses := rec.String()
	if !strings.Contains(responses, "env[API_KEY]") || !strings.Contains(responses, "MODE") {
		t.Fatal("the recorded responses don't hold the plans; the test is reading the wrong thing")
	}
	var events []store.Event
	var stored []store.Change
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		events, _ = tx.ListEvents(t.Context(), 1000)
		stored, _ = tx.ListChanges(t.Context(), store.ChangePage{}, 100)
		return nil
	})
	var history strings.Builder
	for i := range events {
		e := &events[i]
		history.WriteString(e.Actor + e.Action + e.Subject + e.Detail + "\n")
	}
	for i := range stored {
		ch := &stored[i]
		history.Write(ch.Plan)
		history.Write(ch.Progress)
		history.WriteString(ch.Error + ch.Summary + "\n")
	}
	for name, text := range map[string]string{"API responses": responses, "audit log and change history": history.String(), "logs": logs.String()} {
		if strings.Contains(text, "s3cret") {
			i := strings.Index(text, "s3cret")
			t.Errorf("a secret is in the %s: …%s…", name, text[max(0, i-80):min(len(text), i+40)])
		}
	}
}

// A plan that may cut off access, or destroys data, can't be undone fully.
// Applying it needs the fingerprint of the plan reviewed, as the API's typed
// confirmation, and a signed-in session: no API token may apply it.
func TestDestructivePlansNeedASessionAndAConfirmation(t *testing.T) {
	storetest.Each(t, testDestructivePlansNeedASessionAndAConfirmation)
}

func testDestructivePlansNeedASessionAndAConfirmation(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServer(t, true)
	c := changeClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)

	// A token may propose and apply what can be undone.
	open := propose(t, script, "Open http", ruleEdit("http", 80))
	if open.GetProposedVia() != dockwellv1.Surface_SURFACE_API || open.GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_TOKEN ||
		open.GetProposedBy().GetName() != "CI TOKEN_SCOPE_READ_WRITE" {
		t.Errorf("a change a token proposed = %v", open)
	}
	if got := apply(t, script, open); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || got.GetAppliedVia() != dockwellv1.Surface_SURFACE_API {
		t.Errorf("a token's apply = %v", got)
	}
	// The CLI says it is the CLI, and its changes record it.
	cli := dockwellv1connect.NewChangeServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin,
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("User-Agent", auth.CLIUserAgent+"v0.4.0")
				return next(ctx, req)
			}
		})))
	if got := propose(t, cli, "Open https", ruleEdit("https", 443)); got.GetProposedVia() != dockwellv1.Surface_SURFACE_CLI {
		t.Errorf("a change the CLI proposed came from %v, want the CLI", got.GetProposedVia())
	}

	block := propose(t, script, "Close SSH", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "ssh"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: &resourcev1.FirewallRule{
			Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY,
		}}},
	})
	fp := block.GetPlan().GetFingerprint()
	if !block.GetPlan().GetDestructive() || block.GetPlan().GetServers()[0].GetSteps()[0].GetWarnings()[0] != dockwellv1.PlanWarning_PLAN_WARNING_MAY_LOCK_OUT {
		t.Fatalf("blocking SSH plans %v, want it destructive, warning of a lockout", block.GetPlan())
	}
	_, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: block.GetId(), Fingerprint: fp}))
	if reasonOf(err) != authapi.ReasonNeedsSession || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a token applying a destructive plan = %v, want needs_session", err)
	}
	_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: block.GetId()}))
	if reasonOf(err) != "needs_confirmation" || !strings.Contains(err.Error(), fp) {
		t.Errorf("applying a destructive plan without its fingerprint = %v, want needs_confirmation", err)
	}
	_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: block.GetId(), Fingerprint: "0123"}))
	if reasonOf(err) != "plan_changed" {
		t.Errorf("applying with another plan's fingerprint = %v, want plan_changed", err)
	}
	w := watch(t, c, block.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: block.GetId(), Fingerprint: fp})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a confirmed destructive plan = %v", got)
	}
}

// In this build agents have no drivers, so a kind that runs on a server
// fails at planning, saying the server's agent can't run it.
func TestAKindNoAgentRunsFailsAtPlanning(t *testing.T) {
	storetest.Each(t, testAKindNoAgentRunsFailsAtPlanning)
}

func testAKindNoAgentRunsFailsAtPlanning(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServer(t, false)
	c := changeClient(b, b.origin)
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run web", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Name: "web"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}},
		}}},
	}}}))
	if reasonOf(err) != "no_driver" || !strings.Contains(err.Error(), "the agent on hz-1 has no driver for service resources") {
		t.Errorf("planning a service on an agent with no drivers = %v, want no_driver", err)
	}
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Nothing", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	}}}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "is for a service, but its spec is a project") {
		t.Errorf("an edit whose kind and spec disagree = %v, want invalid", err)
	}
}

// syncBuffer is a buffer logs can be written to from many goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Stopping the control plane mid-apply leaves the change applying, for the
// next start to finish: the API stops first, then the engine, which cancels
// the step that outlasts its grace, then the agent listener. The stop
// doesn't fail the change, and nothing rolls it back.
func TestAStopMidApplyLeavesTheChangeForTheNextStart(t *testing.T) {
	storetest.Each(t, testAStopMidApplyLeavesTheChangeForTheNextStart)
}

func testAStopMidApplyLeavesTheChangeForTheNextStart(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	c := changeClient(b, b.origin)
	ch := propose(t, c, "Open http and slow", ruleEdit("http", 80), ruleEdit("slow", 8080))
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId()})); err != nil {
		t.Fatal(err)
	}
	<-hz1.fw.started // the slow rule's step is running
	if err := f.stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var stopped store.Change
	var rollbacks []store.Change
	if err := f.store.View(t.Context(), func(tx *store.Tx) (err error) {
		if stopped, err = tx.GetChange(t.Context(), ch.GetId()); err != nil {
			return err
		}
		rollbacks, err = tx.Reverting(t.Context(), []string{ch.GetId()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stopped.State != store.ChangeApplying || len(rollbacks) != 0 {
		t.Fatalf("after a stop mid-apply the change is %s, with %d rollbacks; want it applying, for the next start", stopped.State, len(rollbacks))
	}

	// The next start, with hz-1's agent back, finishes it.
	close(hz1.fw.release)
	kinds := resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{})
	next := revision.New(f.store, kinds, localAgents{hz1.id.ServerID: resource.Local{Drivers: hz1.drivers}}, secretstest.Keeper(f.store, kinds),
		slog.New(slog.DiscardHandler))
	if err := next.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	next.Wait()
	r, err := next.Change(t.Context(), ch.GetId())
	if err != nil {
		t.Fatal(err)
	}
	if r.State != store.ChangeApplied || hz1.fw.Len() != 2 {
		t.Errorf("after the next start the change is %s with %d rules on hz-1, want applied with 2", r.State, hz1.fw.Len())
	}
}

// localAgents reaches each server's drivers in this process, as a control
// plane whose agents have reconnected does.
type localAgents map[string]resource.Agent

func (a localAgents) Agent(server string) (resource.Agent, bool) {
	local, ok := a[server]
	return local, ok
}

// A server whose drift can't be checked is listed with why, and the others
// report theirs all the same.
func TestDriftReportsEachServerItCantCheck(t *testing.T) {
	storetest.Each(t, testDriftReportsEachServerItCantCheck)
}

func testDriftReportsEachServerItCantCheck(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	hz2 := f.addNamedServer(t, "hz-2", true)
	c := changeClient(b, b.origin)
	on := func(server string) *dockwellv1.Edit {
		e := webEdit("nginx:1.27", nil)
		e.Resource.Scope = server
		return e
	}
	apply(t, c, propose(t, c, "Run web on both", on("hz-1"), on("hz-2")))

	// hz-2's agent comes back without its drivers, so it can't read web.
	hz2.stop()
	waitConnected(t, f, hz2.id.ServerID, false)
	hz2.drivers = nil
	hz2.connect(t, f)
	got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"})
	got.Image = "nginx:hand-edit"
	_ = hz1.docker.Run(t.Context(), container.Target{Name: "web"}, got, resource.Decision{})

	res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatalf("ListDrift with one server that can't be checked = %v, want the others' drift", err)
	}
	if d := res.Msg.GetDrift(); len(d) != 1 || d[0].GetServerId() != hz1.id.ServerID {
		t.Errorf("drift = %v, want web on hz-1", d)
	}
	u := res.Msg.GetUncheckedServers()
	if len(u) != 1 || u[0].GetServerId() != hz2.id.ServerID || u[0].GetServerName() != "hz-2" || u[0].GetReason() != "no_driver" ||
		!strings.Contains(u[0].GetMessage(), "hz-2") {
		t.Errorf("unchecked servers = %v, want hz-2, for want of a driver", u)
	}

	// One that isn't connected says so.
	hz2.stop()
	waitConnected(t, f, hz2.id.ServerID, false)
	res, err = c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Msg.GetUncheckedServers(); len(u) != 1 || u[0].GetReason() != "server_offline" {
		t.Errorf("unchecked servers with hz-2 away = %v, want it offline", u)
	}
}

// A row Dockwell won't read costs the server that runs it, not the fleet: a build
// from main between the Docker driver and sealing could leave one, and the last check
// of #70 had drift fail for every server on it, with "try again". What reads the row
// says which it is and what to do, and the log has it too, as the review of #137 found
// the refusal had taken it out. A row that can't be opened goes the same way.
func TestARowItWontReadCostsOneServersDrift(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		t.Run("not sealed", func(t *testing.T) {
			spec := `x'0a0a'`
			if backend == store.Postgres {
				spec = `'\x0a0a'`
			}
			testARowItWontReadCostsOneServersDrift(t, backend, func(string) string { return spec },
				"Restore the database from a backup. On a development install, start again with an empty data directory")
		})
		t.Run("sealed for another row", func(t *testing.T) {
			testARowItWontReadCostsOneServersDrift(t, backend, func(other string) string {
				return `(SELECT r.spec FROM revisions r WHERE r.resource_id IN (SELECT id FROM resources WHERE scope = '` + other + `' AND kind = 'container'))`
			}, "Restore the database and the key file from the same backup")
		})
	})
}

func testARowItWontReadCostsOneServersDrift(t *testing.T, backend store.Backend, spec func(other string) string, fix string) {
	logs := &syncBuffer{}
	f := startFullLogging(t, backend, nil, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelError})))
	b := signedIn(t, f)
	hz1 := f.addServer(t, true)
	hz2 := f.addNamedServer(t, "hz-2", true)
	c := changeClient(b, b.origin)
	on := func(server string) *dockwellv1.Edit {
		e := webEdit("nginx:1.27", nil)
		e.Resource.Scope = server
		return e
	}
	apply(t, c, propose(t, c, "Run web on both", on("hz-1"), on("hz-2")))
	got, _ := hz1.docker.Inspect(t.Context(), container.Target{Name: "web"})
	got.Image = "nginx:hand-edit"
	_ = hz1.docker.Run(t.Context(), container.Target{Name: "web"}, got, resource.Decision{})

	drop := `DROP TRIGGER revisions_never_change`
	if backend == store.Postgres {
		drop += ` ON revisions`
	}
	storetest.Exec(t, f.db, drop)
	storetest.Exec(t, f.db, `UPDATE revisions SET spec = `+spec(hz1.id.ServerID)+` WHERE resource_id IN (SELECT id FROM resources WHERE scope = '`+hz2.id.ServerID+`' AND kind = 'container')`)

	res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatalf("ListDrift with a row it won't read on one server = %v, want the others' drift", err)
	}
	if d := res.Msg.GetDrift(); len(d) != 1 || d[0].GetServerId() != hz1.id.ServerID {
		t.Errorf("drift = %v, want web on hz-1", d)
	}
	u := res.Msg.GetUncheckedServers()
	if len(u) != 1 || u[0].GetServerName() != "hz-2" || u[0].GetReason() != "spec_unreadable" || !strings.Contains(u[0].GetMessage(), fix) {
		t.Errorf("unchecked servers = %v, want hz-2, saying what to do", u)
	}
	if !strings.Contains(logs.String(), fix) {
		t.Errorf("after drift, the log doesn't say what to do: %s", logs.String())
	}
	seen := len(logs.String())
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Change web", Edits: []*dockwellv1.Edit{on("hz-2")}}))
	if err == nil || !strings.Contains(err.Error(), fix) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("planning from that row = %v, want it refused, saying what to do", err)
	}
	if !strings.Contains(logs.String()[seen:], fix) {
		t.Errorf("after planning, the log doesn't say what to do: %s", logs.String()[seen:])
	}
}

// Once there are two projects, an edit names an environment's project
// beside it, by name; while there is one, it needn't.
func TestAnEditNamesAnEnvironmentsProject(t *testing.T) {
	storetest.Each(t, testAnEditNamesAnEnvironmentsProject)
}

func testAnEditNamesAnEnvironmentsProject(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServer(t, true)
	c := changeClient(b, b.origin)
	svc := func(project string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Scope: "production", Name: "web"},
			Project:  project,
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}},
			}}},
		}
	}
	// One project: the environment's name is enough.
	if ch := propose(t, c, "Run web", svc("")); len(ch.GetPlan().GetResources()) != 1 {
		t.Fatalf("a service in production plans %v", ch.GetPlan())
	}
	apply(t, c, propose(t, c, "Add the shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	}))

	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run web", Edits: []*dockwellv1.Edit{svc("")}}))
	if reasonOf(err) != "choice" || !strings.Contains(err.Error(), "default and shop") {
		t.Errorf("an environment named without its project once there are two = %v, want a choice naming them", err)
	}
	if c := choiceOf(err); c.GetLevel() != "project" || c.GetField() != "edits[0].project" || !slices.Equal(c.GetOptions(), []string{"default", "shop"}) {
		t.Errorf("the choice's detail = %v, want the projects, for the edit's project", c)
	}
	ch := propose(t, c, "Run web in the shop", svc("shop"))
	var shopProduction string
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		_, env, err := tree.Resolve("shop", "production")
		shopProduction = env.ID
		return err
	})
	if r := ch.GetPlan().GetResources(); len(r) != 1 || r[0].GetResource().GetScope() != shopProduction || shopProduction == "" {
		t.Errorf("a service in shop's production plans %v, want it in %s", r, shopProduction)
	}
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run web", Edits: []*dockwellv1.Edit{svc("nowhere")}}))
	if reasonOf(err) != "not_found" || !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("an environment in a project that isn't there = %v, want not_found", err)
	}
	withProject := ruleEdit("http", 80)
	withProject.Project = "shop"
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Open http", Edits: []*dockwellv1.Edit{withProject}}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "names a project") {
		t.Errorf("a firewall rule that names a project = %v, want it refused", err)
	}
}

// A change that gives a container more of its server, such as a server
// path or Docker's socket, needs a signed-in person and the fingerprint,
// as a destructive one does, and its plan lists what it reaches. One that
// keeps a container's access as it is, such as a new image tag, a token
// applies, so deploys from CI keep working. The control plane decides
// from the specs, and a path no container may mount is refused when the
// change is proposed.
func TestHostAccessNeedsAPerson(t *testing.T) { storetest.Each(t, testHostAccessNeedsAPerson) }

func testHostAccessNeedsAPerson(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServer(t, true)
	c := changeClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	web := func(image string, mounts ...*resourcev1.Mount) *dockwellv1.Edit {
		e := webEdit(image, nil)
		e.Spec.GetContainer().Mounts = mounts
		return e
	}
	config := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app/config"}, Target: "/etc/app", ReadOnly: true}

	// What reaches no more of the server than Docker keeps for it, a token
	// applies.
	apply(t, script, propose(t, script, "Run web", web("nginx:1.27")))

	mount := propose(t, script, "Mount the config", web("nginx:1.27", config))
	p := mount.GetPlan()
	step := p.GetServers()[0].GetSteps()[0]
	want := &dockwellv1.HostAccess{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/srv/app/config", ReadOnly: true}
	if !p.GetHostAccess() || len(step.GetHostAccess()) != 1 || !proto.Equal(step.GetHostAccess()[0], want) || len(p.GetResources()[0].GetHostAccess()) != 1 {
		t.Fatalf("mounting a server path plans %v, want it to list the path, and need a person", p)
	}
	_, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: mount.GetId(), Fingerprint: p.GetFingerprint()}))
	if reasonOf(err) != authapi.ReasonNeedsSession || !strings.Contains(err.Error(), "web access to its server: the server path /srv/app/config (read-only)") ||
		!strings.Contains(err.Error(), "Sign in to the panel and apply it there: the CLI uses an API token, even after dockwell login") {
		t.Errorf("a token applying host access = %v, want needs_session, saying what and where a person applies it", err)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: mount.GetId()})); reasonOf(err) != "needs_confirmation" {
		t.Errorf("a person applying host access without the fingerprint = %v, want needs_confirmation", err)
	}
	w := watch(t, c, mount.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: mount.GetId(), Fingerprint: p.GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a person applying host access with the fingerprint = %v", got)
	}

	// A new image tag keeps the access as it is: a token applies it.
	tag := propose(t, script, "Deploy 1.28", web("nginx:1.28", config))
	if tag.GetPlan().GetHostAccess() || len(tag.GetPlan().GetServers()[0].GetSteps()[0].GetHostAccess()) > 0 {
		t.Errorf("a new tag plans host access: %v", tag.GetPlan())
	}
	if got := apply(t, script, tag); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a token's deploy of a new tag = %v", got)
	}

	// Read-write where it was read-only, or Docker's socket, is more.
	rw := proto.CloneOf(config)
	rw.ReadOnly = false
	sock := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/var/run/docker.sock"}, Target: "/var/run/docker.sock"}
	for name, edit := range map[string]*dockwellv1.Edit{"read-write": web("nginx:1.28", rw), "socket": web("nginx:1.28", config, sock)} {
		if got := propose(t, script, name, edit); !got.GetPlan().GetHostAccess() {
			t.Errorf("%s plans no host access: %v", name, got.GetPlan())
		}
	}

	// A path no container may mount is refused as the change is proposed.
	root := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/"}, Target: "/host"}
	_, err = script.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Mount the server", Edits: []*dockwellv1.Edit{web("nginx:1.28", root)}}))
	if err == nil || !strings.Contains(err.Error(), "the server path / mounted at /host is the server's whole file system") {
		t.Errorf("proposing a mount of / = %v, want it refused", err)
	}
}

// What a Compose file leaves once its services go keeps its project, and the panel
// offers to delete that in one plan. A volume going is data going, so that needs a
// person and their confirmation, as any data loss does, and one change takes it all.
func TestAProjectGoesWithWhatsLeftInOneChange(t *testing.T) {
	storetest.Each(t, testAProjectGoesWithWhatsLeft)
}

func testAProjectGoesWithWhatsLeft(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	c := changeClient(b, b.origin)
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	res, err := imports.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{
		Compose: "name: shop\nservices:\n  web:\n    image: nginx:1.27\n    volumes: [\"data:/data\"]\nvolumes:\n  data: {}\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, res.Msg.GetChange())
	var env string
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			env = r.GetResource().GetScope()
		}
	}
	applyConfirmed(t, c, propose(t, c, "Delete web", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: env, Name: "web"}, Delete: true}))

	// Alone, it's refused by what's left, as a token or the CLI meets it.
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete shop", Edits: []*dockwellv1.Edit{
		{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "shop"}, Delete: true},
	}}))
	for _, says := range []string{"volume data in production", "dockwell volumes delete data --project shop", "dockwell networks delete default --project shop"} {
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("deleting what still holds a volume: %v, want it to say %q", err, says)
		}
	}

	all := propose(t, c, "Delete the project shop",
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "volume", Scope: env, Name: "data"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "network", Scope: env, Name: "default"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "shop"}, Delete: true},
	)
	var named []string
	for _, r := range all.GetPlan().GetResources() {
		named = append(named, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	slices.Sort(named)
	if want := []string{"environment production", "network default", "project shop", "volume data"}; !slices.Equal(named, want) {
		t.Errorf("the plan names %v, want %v", named, want)
	}
	if !all.GetPlan().GetDestructive() {
		t.Errorf("a plan that deletes a volume isn't destructive: %v", all.GetPlan())
	}
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	if _, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: all.GetId(), Fingerprint: all.GetPlan().GetFingerprint()})); reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token applying what destroys data = %v", err)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: all.GetId()})); err == nil {
		t.Error("a plan that destroys data applied with nothing confirming it")
	}
	projects := func() []string {
		t.Helper()
		var out []string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			tree, err := scope.Load(t.Context(), tx)
			for _, p := range tree.Projects {
				out = append(out, p.Name)
			}
			slices.Sort(out)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// A step that fails puts everything back: the project isn't left half gone.
	e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/networks/") {
			return errors.New("the engine won't do it")
		}
		return nil
	})
	w := watch(t, c, all.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: all.GetId(), Fingerprint: all.GetPlan().GetFingerprint()})); err == nil {
		waitWatch(t, w)
	}
	e.Intercept(nil)
	// The project is back before its rollback ends, and a change on what's in it
	// waits for that, so this waits for the rollback, as a person sees it end.
	waitChange(t, c, all.GetId(), "its rollback to end", func(ch *dockwellv1.Change) bool {
		return ch.GetRolledBackBy() != "" && ch.GetRollbackStatus().GetState() == dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED
	})
	if got := projects(); !slices.Equal(got, []string{"default", "shop"}) {
		t.Fatalf("after a failed step the projects are %v, want shop put back", got)
	}

	// Planned again, it goes, and undo brings back all of it, as a deleted volume is kept.
	again := propose(t, c, "Delete the project shop",
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "volume", Scope: env, Name: "data"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "network", Scope: env, Name: "default"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "shop"}, Delete: true},
	)
	applyConfirmed(t, c, again)
	if got := projects(); !slices.Equal(got, []string{"default"}) {
		t.Fatalf("the projects are %v", got)
	}
	back, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: again.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, c, back.Msg.GetChange())
	if got := projects(); !slices.Equal(got, []string{"default", "shop"}) {
		t.Errorf("after undoing it the projects are %v", got)
	}
}
