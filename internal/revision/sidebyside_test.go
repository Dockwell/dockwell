// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// together holds each call it gates until n of them are in, as servers whose steps
// go side by side all are; it gives up after a while, as steps that go one after
// another never get there.
type together struct {
	mu     sync.Mutex
	n, in  int
	all    chan struct{}
	failOn string
	order  []string
}

func newTogether(n int) *together { return &together{n: n, all: make(chan struct{})} }

func (g *together) arrive(_ context.Context, server string) error {
	g.mu.Lock()
	g.in++
	g.order = append(g.order, server)
	if g.in == g.n {
		close(g.all)
	}
	g.mu.Unlock()
	select {
	case <-g.all:
	case <-time.After(5 * time.Second):
		return errors.New("the other server's step never started alongside this one")
	}
	if server == g.failOn {
		return errors.New("the step failed on " + server)
	}
	return nil
}

// hostsDo is what servers' hosts do as a fleet action's steps reach them: a unit's
// action, and an agent's upgrade (ADR 0094).
type hostsDo struct {
	unit, upgrade func(ctx context.Context, server string) error
}

// fakeUnits plans any action on any unit, and takes it as do.unit says.
type fakeUnits struct {
	server string
	do     *hostsDo
}

func (u fakeUnits) PlanUnit(_ context.Context, name string, action resource.UnitAction) (*resource.Unit, bool, error) {
	return &resource.Unit{Name: name, Action: action}, true, nil
}

func (u fakeUnits) ApplyUnit(ctx context.Context, _ resource.Unit, _ func(string)) error {
	return u.do.unit(ctx, u.server)
}

// fakeUpgrader is an agent at 0.9.3, whose upgrade goes as do.upgrade says.
type fakeUpgrader struct {
	server string
	do     *hostsDo
}

func (u fakeUpgrader) Own() string { return "0.9.3" }

func (u fakeUpgrader) Upgrade(ctx context.Context, _ resource.AgentUpgradeAsked) error {
	return u.do.upgrade(ctx, u.server)
}

func (fakeUpgrader) Forget(string) {}

// onHosts is an engine over the servers ids, whose hosts do as do says, with how
// to start another on the same store and servers, as a control plane that starts
// again does, the store, and where it is.
func onHosts(t *testing.T, do *hostsDo, ids ...string) (e *Engine, again func() *Engine, db *store.DB, location string) {
	t.Helper()
	_, f, db, location := setupAt(t, store.SQLite, ids...)
	kinds := testKinds().With(&host.Driver{})
	for _, id := range ids {
		f[id].agent = resource.Local{Drivers: resource.NewRegistry(&host.Driver{Units: fakeUnits{id, do}, Upgrader: fakeUpgrader{id, do}}), Server: id}
	}
	again = func() *Engine {
		e := New(db, kinds, f, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler))
		t.Cleanup(e.Wait)
		return e
	}
	return again(), again, db, location
}

// restartNginx restarts nginx on server, as a fleet action does.
func restartNginx(server string) Edit {
	return Edit{Ref: resource.Ref{Kind: host.Kind, Scope: server, Name: host.Name}, Unit: &resource.UnitAsked{Name: "nginx.service", Action: resource.UnitRestart}}
}

// rollOut proposes edits over servers in waves of batch, stopping at a failure.
func rollOut(t *testing.T, e *Engine, batch int, servers []string, edits ...Edit) *Plan {
	t.Helper()
	p, err := e.Propose(t.Context(), Proposal{Summary: "Roll out", Actor: "usr_test", Edits: edits, Rollout: &Rollout{Servers: servers, Batch: batch}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stepStates is each server's step state on change's record.
func stepStates(t *testing.T, db *store.DB, change string) map[string]StepState {
	t.Helper()
	progress, err := decodeProgress(stored(t, db, change).Progress)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]StepState{}
	for i := range progress.Steps {
		out[progress.Steps[i].Server] = progress.Steps[i].State
	}
	return out
}

// on is onA's container, on server.
func on(server string) Edit {
	e := onA()
	e.Ref.Scope = server
	return e
}

// A wave's servers go side by side (ADR 0094): their steps are under way at once.
func TestAWavesServersGoSideBySide(t *testing.T) {
	g := newTogether(2)
	e, _, _, _ := onHosts(t, &hostsDo{unit: g.arrive}, "srv_A", "srv_B")
	p := rollOut(t, e, 2, []string{"srv_A", "srv_B"}, restartNginx("srv_A"), restartNginx("srv_B"))
	if p.Waves == nil || len(p.Waves.Order) != 1 {
		t.Fatalf("the waves are %+v, want one of both servers", p.Waves)
	}
	if err := e.Apply(t.Context(), p.Change); err != nil {
		t.Fatalf("applying = %v", err)
	}
}

// A step that fails in a wave lets the steps under way beside it end, and starts no
// other: the next wave is skipped, and the change fails with that step.
func TestAFailureInAWaveSkipsWhatHasntStarted(t *testing.T) {
	g := newTogether(2)
	g.failOn = "srv_A"
	e, _, db, _ := onHosts(t, &hostsDo{unit: g.arrive}, "srv_A", "srv_B", "srv_C")
	p := rollOut(t, e, 2, []string{"srv_A", "srv_B", "srv_C"}, restartNginx("srv_A"), restartNginx("srv_B"), restartNginx("srv_C"))
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) {
		t.Fatalf("applying = %v, want the change failed", err)
	}
	g.mu.Lock()
	order := slices.Clone(g.order)
	g.mu.Unlock()
	if slices.Contains(order, "srv_C") {
		t.Errorf("the next wave's server was called after a failure: %v", order)
	}
	want := map[string]StepState{"srv_A": StepFailed, "srv_B": StepDone, "srv_C": StepSkipped}
	if got := stepStates(t, db, p.Change); !maps.Equal(got, want) {
		t.Errorf("the steps are %v, want %v", got, want)
	}
}

// A wave's servers that each find a newer image on a pull make one revision of it
// between them, as servers one after another do: what each step found goes on the
// change's record one at a time, though the steps go side by side.
func TestAWavesPullsThatFindANewerImageMakeOneRevision(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A", "srv_B")
	p, err := s.e.Propose(t.Context(), Proposal{
		Summary: "Deploy", Actor: "usr_test",
		Edits:   []Edit{{Ref: s.ref, Spec: shopSpec(), Base: 1, Pull: true, Run: true}},
		Rollout: &Rollout{Servers: []string{"srv_A", "srv_B"}, Batch: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Waves == nil || len(p.Waves.Order) != 1 {
		t.Fatalf("the waves are %+v, want one of both servers", p.Waves)
	}
	apply(t, s.e, p)
	s.wantNewerRunning(t, p.Change)
}

// span is when one step's call to a server's agent ran.
type span struct {
	server     string
	action     resource.Action
	start, end time.Time
}

// timed is a server's agent that keeps when each of its calls ran, slowed by wait.
type timed struct {
	resource.Agent
	server string
	wait   time.Duration
	mu     *sync.Mutex
	spans  *[]span
}

func (a timed) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	start := time.Now()
	time.Sleep(a.wait)
	err := a.Agent.Apply(ctx, step, desired)
	a.mu.Lock()
	*a.spans = append(*a.spans, span{a.server, step.Action, start, time.Now()})
	a.mu.Unlock()
	return err
}

// A wave whose steps take something off a server, as a move between servers does,
// goes in the queue's order, as a change without waves does: what's made on the new
// server is up before what ran on the old one goes.
func TestAWaveThatTakesSomethingOffAServerGoesInOrder(t *testing.T) {
	e, f, db := setup(t, store.SQLite, "srv_A", "srv_B")
	ref := resource.Ref{Kind: service.Kind, Scope: production(t, db), Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}
	apply(t, e, propose(t, e, "Make", Edit{Ref: ref, Spec: spec, Servers: []string{"srv_A"}}))
	var mu sync.Mutex
	var spans []span
	for s, wait := range map[string]time.Duration{"srv_A": 0, "srv_B": 300 * time.Millisecond} {
		f[s].agent = timed{Agent: f[s].agent, server: s, wait: wait, mu: &mu, spans: &spans}
	}
	p, err := e.Propose(t.Context(), Proposal{
		Summary: "Move", Actor: "usr_test",
		Edits:   []Edit{{Ref: ref, Spec: spec, Base: 1, Servers: []string{"srv_B"}}},
		Rollout: &Rollout{Servers: []string{"srv_A", "srv_B"}, Batch: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Waves == nil || len(p.Waves.Order) != 1 {
		t.Fatalf("the waves are %+v, want one of both servers", p.Waves)
	}
	apply(t, e, p)
	var made, gone *span
	for i := range spans {
		switch sp := &spans[i]; {
		case sp.server == "srv_B" && sp.action == resource.ActionCreate:
			made = sp
		case sp.server == "srv_A" && sp.action == resource.ActionDelete:
			gone = sp
		}
	}
	if made == nil || gone == nil {
		t.Fatalf("the calls were %+v, want a create on srv_B and a delete on srv_A", spans)
	}
	if gone.start.Before(made.end) {
		t.Errorf("srv_A's copy went %s before srv_B's was up", made.end.Sub(gone.start))
	}
}

// A wave whose steps aren't a host's alone goes in the queue's order, as something on
// one server may need another's first, as a move or a link does: two servers'
// containers in one wave are made one after the other (ADR 0094, the review of slice
// 2, L2).
func TestAWaveOfStepsNotOnAHostGoesInOrder(t *testing.T) {
	e, f, _ := setup(t, store.SQLite, "srv_A", "srv_B")
	var mu sync.Mutex
	var spans []span
	for _, s := range []string{"srv_A", "srv_B"} {
		f[s].agent = timed{Agent: f[s].agent, server: s, wait: 200 * time.Millisecond, mu: &mu, spans: &spans}
	}
	p := rollOut(t, e, 2, []string{"srv_A", "srv_B"}, on("srv_A"), on("srv_B"))
	if p.Waves == nil || len(p.Waves.Order) != 1 {
		t.Fatalf("the waves are %+v, want one of both servers", p.Waves)
	}
	apply(t, e, p)
	if len(spans) != 2 {
		t.Fatalf("the calls were %+v, want one on each server", spans)
	}
	if a, b := spans[0], spans[1]; a.start.Before(b.end) && b.start.Before(a.end) {
		t.Errorf("%s's step and %s's went side by side: %+v", a.server, b.server, spans)
	}
}

// A group of steps is of the wave under way: a step of a wave before it, as a delete
// that goes after every wave's builds, goes on its own (the review of slice 2, L2).
func TestAGroupIsOfTheWaveUnderWay(t *testing.T) {
	waves := []int{1, 1, 2, 2, 1, 2}
	at := func(i int) int { return waves[i] }
	for _, c := range []struct{ i, wave, want int }{{0, 1, 2}, {2, 2, 4}, {4, 2, 5}, {5, 2, 6}} {
		if got := waveGroup(len(waves), c.i, c.wave, at); got != c.want {
			t.Errorf("the group from %d in wave %d ends at %d, want %d", c.i, c.wave, got, c.want)
		}
	}
	if got := waveGroup(3, 0, 0, func(int) int { return 0 }); got != 1 {
		t.Errorf("with no waves, the group from 0 ends at %d, want 1", got)
	}
}

// A wave calls so many servers at once at most, however big its batch: each call
// carries what its step asks, as an upgrade's install script does (the review of
// slice 2, L3).
func TestAWaveCallsAtMostSoManyServersAtOnce(t *testing.T) {
	var mu sync.Mutex
	in, most := 0, 0
	unit := func(context.Context, string) error {
		mu.Lock()
		in++
		most = max(most, in)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		in--
		mu.Unlock()
		return nil
	}
	const most32 = 32
	ids := make([]string, most32+8)
	edits := make([]Edit, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("srv_%02d", i)
		edits[i] = restartNginx(ids[i])
	}
	e, _, _, _ := onHosts(t, &hostsDo{unit: unit}, ids...)
	p := rollOut(t, e, len(ids), ids, edits...)
	if p.Waves == nil || len(p.Waves.Order) != 1 {
		t.Fatalf("the waves are %+v, want one of every server", p.Waves)
	}
	apply(t, e, p)
	if most > most32 || most < 2 {
		t.Errorf("%d servers were called at once, want 2 to %d", most, most32)
	}
}

// A step that fails as the engine stops still fails the change, at once: the waves
// after it never run, when the control plane starts again or ever (ADR 0094, the
// review of slice 2, M2).
func TestAFailureAStopMeetsFailsTheChange(t *testing.T) {
	var e *Engine
	inA, inB := make(chan struct{}), make(chan struct{})
	var calledC atomic.Bool
	unit := func(ctx context.Context, server string) error {
		switch server {
		case "srv_A":
			close(inA)
			for !e.isStopping() {
				time.Sleep(5 * time.Millisecond)
			}
			return errors.New("nginx didn't restart")
		case "srv_B":
			close(inB)
			<-ctx.Done()
			return ctx.Err()
		}
		calledC.Store(true)
		return nil
	}
	var again func() *Engine
	var db *store.DB
	e, again, db, _ = onHosts(t, &hostsDo{unit: unit}, "srv_A", "srv_B", "srv_C")
	p := rollOut(t, e, 2, []string{"srv_A", "srv_B", "srv_C"}, restartNginx("srv_A"), restartNginx("srv_B"), restartNginx("srv_C"))
	applied := make(chan error, 1)
	go func() { applied <- e.Apply(context.WithoutCancel(t.Context()), p.Change) }()
	for _, in := range []chan struct{}{inA, inB} {
		select {
		case <-in:
		case <-time.After(10 * time.Second):
			t.Fatal("the first wave's steps never started")
		}
	}
	grace, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = e.Stop(grace)
	select {
	case err := <-applied:
		t.Logf("applying: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the run never ended")
	}
	if c := stored(t, db, p.Change); c.State != store.ChangeFailed {
		t.Errorf("after a failure the stop met, the change is %s: %s", c.State, c.Error)
	}
	if err := again().Resume(t.Context()); err != nil {
		t.Logf("resuming: %v", err)
	}
	if calledC.Load() {
		t.Error("the next wave ran after a failure")
	}
}

// A change that stops at a failure, found applying with a failure on its record, as
// one whose failure couldn't be written, fails as it's resumed, before anything runs:
// the waves after it never do (ADR 0094, the review of slice 2, M2).
func TestAResumeOfAChangeWithAFailureOnRecordRunsNothing(t *testing.T) {
	var calledC atomic.Bool
	unit := func(_ context.Context, server string) error {
		switch server {
		case "srv_A":
			return errors.New("nginx didn't restart")
		case "srv_C":
			calledC.Store(true)
		}
		return nil
	}
	e, again, db, location := onHosts(t, &hostsDo{unit: unit}, "srv_A", "srv_B", "srv_C")
	p := rollOut(t, e, 2, []string{"srv_A", "srv_B", "srv_C"}, restartNginx("srv_A"), restartNginx("srv_B"), restartNginx("srv_C"))
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) {
		t.Fatalf("applying = %v, want the change failed", err)
	}
	// As if how it failed never reached the store: it reads applying, with the
	// failed step on record and the next wave's still to run.
	progress, err := decodeProgress(stored(t, db, p.Change).Progress)
	if err != nil {
		t.Fatal(err)
	}
	for i := range progress.Steps {
		if progress.Steps[i].Server == "srv_C" {
			progress.Steps[i].State, progress.Steps[i].Message = StepQueued, ""
		}
	}
	b, err := json.Marshal(progress)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, location, "UPDATE changes SET state = 'applying', progress = '"+strings.ReplaceAll(string(b), "'", "''")+"' WHERE id = '"+p.Change+"'")
	if err := again().Resume(t.Context()); err != nil {
		t.Logf("resuming: %v", err)
	}
	if calledC.Load() {
		t.Error("the next wave ran on a resume, after a failure on record")
	}
	if c := stored(t, db, p.Change); c.State != store.ChangeFailed {
		t.Errorf("resumed with a failure on record, the change is %s", c.State)
	}
}

// An upgrade asked beside a step that fails ends failed with the change, rather than
// wait for its server for good: no resume reads a failed change (ADR 0094, the review
// of slice 2, L4).
func TestAnUpgradeAskedBesideAFailureEndsFailed(t *testing.T) {
	askedB := make(chan struct{})
	upgrade := func(_ context.Context, server string) error {
		if server == "srv_B" {
			close(askedB)
			return nil
		}
		select {
		case <-askedB:
		case <-time.After(5 * time.Second):
		}
		return errors.New("the install script ended with status 1")
	}
	e, _, db, _ := onHosts(t, &hostsDo{upgrade: upgrade}, "srv_A", "srv_B")
	e.SetAgentRelease(AgentRelease{Version: "0.9.4", Script: []byte("#!/bin/sh\n")})
	version, sum, err := e.AgentReleaseOf()
	if err != nil {
		t.Fatal(err)
	}
	var edits []Edit
	for _, s := range []string{"srv_A", "srv_B"} {
		edits = append(edits, Edit{Ref: resource.Ref{Kind: host.Kind, Scope: s, Name: host.Name}, UpgradeAgent: &resource.AgentUpgradeAsked{Version: version, ScriptSHA256: sum}})
	}
	p := rollOut(t, e, 2, []string{"srv_A", "srv_B"}, edits...)
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) {
		t.Fatalf("applying = %v, want the change failed", err)
	}
	want := map[string]StepState{"srv_A": StepFailed, "srv_B": StepFailed}
	if got := stepStates(t, db, p.Change); !maps.Equal(got, want) {
		t.Errorf("the steps are %v, want %v: none waiting on a failed change", got, want)
	}
}

// A resumed run that fails names its server, though only that server had a step left:
// the change was over three (the review of slice 2, L5).
func TestAResumedRunNamesTheServerThatFailed(t *testing.T) {
	unit := func(_ context.Context, server string) error {
		if server == "srv_C" {
			return resource.Plainly("nginx didn't restart", errors.New("exit status 1"))
		}
		return nil
	}
	e, again, db, _ := onHosts(t, &hostsDo{unit: unit}, "srv_A", "srv_B", "srv_C")
	p, err := e.Propose(t.Context(), Proposal{
		Summary: "Roll out", Actor: "usr_test", Edits: []Edit{restartNginx("srv_A"), restartNginx("srv_B"), restartNginx("srv_C")},
		Rollout: &Rollout{Servers: []string{"srv_A", "srv_B", "srv_C"}, Batch: 2, Pause: 2 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() { applied <- e.Apply(context.WithoutCancel(t.Context()), p.Change) }()
	// The control plane stops in the pause before srv_C's wave.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if pr := e.progress(p.Change); pr != nil && slices.ContainsFunc(pr.Steps, func(st StepProgress) bool {
			return st.Server == "srv_C" && strings.Contains(st.Message, "after the pause")
		}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the change never paused before srv_C's wave")
		}
	}
	grace, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_ = e.Stop(grace)
	if err := <-applied; err != nil {
		t.Logf("applying: %v", err)
	}
	if err := again().Resume(t.Context()); err != nil {
		t.Logf("resuming: %v", err)
	}
	if c := stored(t, db, p.Change); c.State != store.ChangeFailed || !strings.HasPrefix(c.Error, "srv_C: ") {
		t.Errorf("the resumed change ended %s: %q, want failed naming srv_C", c.State, c.Error)
	}
}
