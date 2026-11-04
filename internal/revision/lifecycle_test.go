// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// stored reads a change as the store has it.
func stored(t *testing.T, db *store.DB, changeID string) store.Change {
	t.Helper()
	var c store.Change
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		c, err = tx.GetChange(t.Context(), changeID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// reverting lists the changes that undo or roll back a change.
func reverting(t *testing.T, db *store.DB, changeID string) []store.Change {
	t.Helper()
	var out []store.Change
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		out, err = tx.Reverting(t.Context(), []string{changeID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func steps(t *testing.T, c store.Change) []StepState {
	t.Helper()
	p, err := decodeProgress(c.Progress)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]StepState, len(p.Steps))
	for i := range p.Steps {
		out[i] = p.Steps[i].State
	}
	return out
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// finishesOnTheNextStart starts the engine again over the same store, as a
// control plane that starts again does, and checks it finishes the change.
func finishesOnTheNextStart(t *testing.T, db *store.DB, f fleet, changeID string, rules int) {
	t.Helper()
	next := newEngine(db, f)
	if err := next.Resume(t.Context()); err != nil {
		t.Fatalf("Resume on the next start = %v", err)
	}
	next.Wait()
	c := stored(t, db, changeID)
	if c.State != store.ChangeApplied || f["srv_A"].firewall.Len() != rules {
		t.Errorf("after the next start the change is %s with %d rules on srv_A, want applied with %d", c.State, f["srv_A"].firewall.Len(), rules)
	}
	for i, s := range steps(t, c) {
		if s != StepDone {
			t.Errorf("after the next start step %d is %s, want done", i, s)
		}
	}
}

// A step still running when the control plane's grace runs out is
// cancelled, and its change stays applying, neither failed nor rolled back.
// Stopped, the engine starts nothing more; the next start finishes it.
func TestAStopCancelsAStepThatOutlastsTheGrace(t *testing.T) {
	storetest.Each(t, testAStopCancelsAStepThatOutlastsTheGrace)
}

func testAStopCancelsAStepThatOutlastsTheGrace(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	plain := f["srv_A"].agent
	started := make(chan struct{}, 1)
	f["srv_A"].agent = hooked{Agent: plain, apply: func(ctx context.Context, step resource.Step, _ proto.Message) error {
		if step.Ref.Name != "slow" {
			return nil
		}
		started <- struct{}{}
		<-ctx.Done() // longer than the grace
		return ctx.Err()
	}}
	p := propose(t, e, "Open http and slow", rule("srv_A", "http", 80), rule("srv_A", "slow", 8080))
	if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	<-started
	grace, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := e.Stop(grace); err != nil {
		t.Fatalf("Stop = %v", err)
	}

	c := stored(t, db, p.Change)
	if c.State != store.ChangeApplying || len(reverting(t, db, p.Change)) != 0 {
		t.Fatalf("a change stopped partway is %s, with %d rollbacks; want it applying, for the next start", c.State, len(reverting(t, db, p.Change)))
	}
	if s := steps(t, c); len(s) != 2 || s[0] != StepDone || s[1] != StepQueued {
		t.Errorf("a change stopped partway has steps %v, want done, then queued", s)
	}
	other := propose(t, e, "Open https", rule("srv_A", "https", 443))
	if err := e.Apply(t.Context(), other.Change); !errors.Is(err, ErrStopping) {
		t.Errorf("applying once stopped = %v, want ErrStopping", err)
	}
	if err := e.Resume(t.Context()); err != nil || stored(t, db, p.Change).State != store.ChangeApplying {
		t.Errorf("Resume once stopped = %v, and ran the change", err)
	}

	f["srv_A"].agent = plain
	finishesOnTheNextStart(t, db, f, p.Change, 2)
}

// A call that fails because the control plane is stopping, as when it
// closes the agent listener under the step, interrupts the change: it
// isn't failed, and isn't rolled back.
func TestACallLostToTheStopInterruptsTheChange(t *testing.T) {
	storetest.Each(t, testACallLostToTheStopInterruptsTheChange)
}

func testACallLostToTheStopInterruptsTheChange(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	plain := f["srv_A"].agent
	started, drop := make(chan struct{}, 1), make(chan struct{})
	f["srv_A"].agent = hooked{Agent: plain, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		if step.Ref.Name != "slow" {
			return nil
		}
		started <- struct{}{}
		<-drop
		return fmt.Errorf("%w: the agent listener closed", resource.ErrLost)
	}}
	p := propose(t, e, "Open http and slow", rule("srv_A", "http", 80), rule("srv_A", "slow", 8080))
	if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop(context.Background()) }()
	waitUntil(t, "the engine is stopping", e.isStopping)
	close(drop)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if c := stored(t, db, p.Change); c.State != store.ChangeApplying || len(reverting(t, db, p.Change)) != 0 {
		t.Fatalf("a change whose call was lost to the stop is %s, with %d rollbacks; want it applying", c.State, len(reverting(t, db, p.Change)))
	}
	f["srv_A"].agent = plain
	finishesOnTheNextStart(t, db, f, p.Change, 2)
}

// A step under way when the control plane stops finishes, and the change
// stops there: it starts no new step, and the next start runs the rest.
func TestAStopLetsTheStepUnderWayFinish(t *testing.T) {
	storetest.Each(t, testAStopLetsTheStepUnderWayFinish)
}

func testAStopLetsTheStepUnderWayFinish(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	plain := f["srv_A"].agent
	started, release := make(chan struct{}, 1), make(chan struct{})
	f["srv_A"].agent = hooked{Agent: plain, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		if step.Ref.Name == "first" {
			started <- struct{}{}
			<-release
		}
		return nil
	}}
	p := propose(t, e, "Open two ports", rule("srv_A", "first", 80), rule("srv_A", "second", 81))
	if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop(context.Background()) }()
	waitUntil(t, "the engine is stopping", e.isStopping)
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop = %v", err)
	}
	c := stored(t, db, p.Change)
	if s := steps(t, c); c.State != store.ChangeApplying || len(s) != 2 || s[0] != StepDone || s[1] != StepQueued || f["srv_A"].firewall.Len() != 1 {
		t.Fatalf("after the stop the change is %s with steps %v and %d rules, want applying, the first step done", c.State, s, f["srv_A"].firewall.Len())
	}
	f["srv_A"].agent = plain
	finishesOnTheNextStart(t, db, f, p.Change, 2)
}

// The interleaving that lost a wakeup: a rollback marks its step on srv_A
// waiting, srv_A reconnects while the rollback still runs on srv_B, and the
// Resume its connection asks for skips the rollback as running. Once the
// run ends, Resume runs again, and the rollback finishes.
func TestNoResumeIsLostWhileARollbackRuns(t *testing.T) {
	storetest.Each(t, testNoResumeIsLostWhileARollbackRuns)
}

func testNoResumeIsLostWhileARollbackRuns(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	apply(t, e, propose(t, e, "Open http", rule("srv_A", "http", 80), rule("srv_B", "http", 80)))
	rollingBack, release := make(chan struct{}, 1), make(chan struct{})
	f["srv_B"].agent = hooked{Agent: f["srv_B"].agent, apply: func(_ context.Context, step resource.Step, desired proto.Message) error {
		rule, _ := desired.(*resourcev1.FirewallRule)
		switch {
		case step.Ref.Name == "boom":
			f["srv_A"].online.Store(false) // srv_A drops as the change fails
			return errors.New("boom can't be opened")
		case rule.GetPort() == 80: // the rollback, on srv_B
			rollingBack <- struct{}{}
			<-release
		}
		return nil
	}}
	p := propose(t, e, "Move http to 81", rule("srv_A", "http", 81), rule("srv_B", "http", 81), rule("srv_B", "boom", 9))
	if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	<-rollingBack
	f["srv_A"].online.Store(true)
	if err := e.Resume(t.Context()); err != nil {
		t.Fatalf("Resume while the rollback runs = %v", err)
	}
	close(release)
	e.Wait()

	rollbacks := reverting(t, db, p.Change)
	if len(rollbacks) != 1 {
		t.Fatalf("%s has %d rollbacks, want 1", p.Change, len(rollbacks))
	}
	if c := stored(t, db, rollbacks[0].ID); c.State != store.ChangeApplied {
		t.Errorf("the rollback is %s once its run ended with srv_A back; srv_A's reconnect was lost", c.State)
	}
	if r, _ := f["srv_A"].firewall.Rule(t.Context(), "http"); r.GetPort() != 80 {
		t.Errorf("srv_A's http rule is on port %d, want 80 again", r.GetPort())
	}
}

// plannedByDockwell lists the changes Dockwell made that are planned.
func plannedByDockwell(t *testing.T, db *store.DB) []string {
	t.Helper()
	var out []string
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		ids, err := tx.ChangesIn(t.Context(), store.ChangePlanned)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if c, _ := tx.GetChange(t.Context(), id); c.Actor == SystemActor {
				out = append(out, id)
			}
		}
		return nil
	})
	return out
}

// A rollback commits whatever the servers say: srv_B coming back while it
// is planned can't leave it planned for good. It records the plan it runs
// with, srv_B's part from its last known state, which waits for Resume.
func TestARollbackIsNeverLeftPlanned(t *testing.T) {
	storetest.Each(t, testARollbackIsNeverLeftPlanned)
}

func testARollbackIsNeverLeftPlanned(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	var rollbackPlans atomic.Int32
	f["srv_A"].agent = hooked{
		Agent:  f["srv_A"].agent,
		before: func(resource.Step) { f["srv_B"].online.Store(false) },
		plan: func(_ context.Context, desired []resource.Desired) error {
			// A plan that deletes http is the rollback's. srv_B is back by
			// the second.
			if desired[0].Spec == nil && rollbackPlans.Add(1) == 2 {
				f["srv_B"].online.Store(true)
			}
			return nil
		},
	}
	onB := web("nginx:1.27")
	onB.Ref.Scope = "srv_B"
	p := propose(t, e, "Open http and run web", rule("srv_A", "http", 80), onB)
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" {
		t.Fatalf("apply = %v, want it failed and rolled back", err)
	}
	if planned := plannedByDockwell(t, db); len(planned) != 0 {
		t.Fatalf("Dockwell left %v planned", planned)
	}
	rb, err := e.Change(t.Context(), failed.Rollback)
	if err != nil {
		t.Fatal(err)
	}
	var offline bool
	for _, s := range rb.Plan.Servers {
		offline = offline || s.Server == "srv_B" && s.Offline
	}
	if rb.State != store.ChangeApplying || !offline || rb.Fingerprint != rb.Plan.Fingerprint {
		t.Errorf("the rollback is %s with plan %+v; want it applying with the plan it ran, srv_B planned offline", rb.State, rb.Plan)
	}
	f["srv_B"].online.Store(true)
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if c := stored(t, db, failed.Rollback); c.State != store.ChangeApplied || f["srv_A"].firewall.Len() != 0 {
		t.Errorf("after srv_B came back the rollback is %s, with %d rules on srv_A", c.State, f["srv_A"].firewall.Len())
	}
}

// A rollback commits even when an agent can't plan it: what it does there
// is planned from the server's last known state, and Resume plans it
// against the server once the run ends.
func TestARollbackCommitsWhenAnAgentCantPlanIt(t *testing.T) {
	storetest.Each(t, testARollbackCommitsWhenAnAgentCantPlanIt)
}

func testARollbackCommitsWhenAnAgentCantPlanIt(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	var failedOnce atomic.Bool
	f["srv_A"].agent = hooked{
		Agent: f["srv_A"].agent,
		apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
			if step.Ref.Name == "boom" {
				return errors.New("boom can't be opened")
			}
			return nil
		},
		plan: func(_ context.Context, desired []resource.Desired) error {
			if desired[0].Spec == nil && failedOnce.CompareAndSwap(false, true) {
				return errors.New("can't read the firewall just now")
			}
			return nil
		},
	}
	p := propose(t, e, "Open http and boom", rule("srv_A", "http", 80), rule("srv_A", "boom", 9))
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" || !errors.Is(failed.RollbackErr, ErrPending) {
		t.Fatalf("apply = %v, want it failed, with a rollback that waits", err)
	}
	e.Wait() // srv_A was there all along: Resume plans it again, and runs it
	if c := stored(t, db, failed.Rollback); c.State != store.ChangeApplied || f["srv_A"].firewall.Len() != 0 {
		t.Errorf("the rollback is %s, with %d rules on srv_A; want it applied, with none", c.State, f["srv_A"].firewall.Len())
	}
}

// A resource's action in a plan comes from its steps on every server: a
// replace anywhere is a replace, a delete everywhere a delete, a create
// everywhere a create, and anything else a change. The counts follow.
func TestAPlanCountsARecreateAnywhereAsAReplace(t *testing.T) {
	storetest.Each(t, testAPlanCountsARecreateAnywhereAsAReplace)
}

func testAPlanCountsARecreateAnywhereAsAReplace(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A", "srv_B")
	env := production(t, db)
	apply(t, e, propose(t, e, "Run web on both", app(env, "nginx:1.27", "srv_A", "srv_B")))
	api := app(env, "nginx:1.27", "srv_A", "srv_B")
	api.Ref.Name = "api"
	for _, tc := range []*struct {
		what   string
		edit   Edit
		action resource.Action
		counts Counts
	}{
		{"a new image, and off srv_B: recreated on srv_A, deleted on srv_B", app(env, "nginx:1.28", "srv_A"), resource.ActionReplace, Counts{Replace: 1}},
		{"kept on srv_A alone: deleted on srv_B, yet it still runs", app(env, "nginx:1.27", "srv_A"), resource.ActionUpdate, Counts{Change: 1}},
		{"deleted: off every server", Edit{Ref: app(env, "", "").Ref}, resource.ActionDelete, Counts{Destroy: 1}},
		{"new, on both servers", api, resource.ActionCreate, Counts{Create: 1}},
	} {
		p := propose(t, e, tc.what, tc.edit)
		if got := p.Resources[0].Action; got != tc.action || p.Counts() != tc.counts {
			t.Errorf("%s plans %s, counted %+v; want %s, %+v", tc.what, got, p.Counts(), tc.action, tc.counts)
		}
	}
}

// A change that touches a resource another change is applying is busy,
// whatever its plan would say now: the claim is checked before planning.
func TestBusyIsCheckedBeforePlanning(t *testing.T) {
	storetest.Each(t, testBusyIsCheckedBeforePlanning)
}

func testBusyIsCheckedBeforePlanning(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	webRunning, slowRunning := make(chan struct{}, 1), make(chan struct{}, 1)
	webGo, slowGo := make(chan struct{}), make(chan struct{})
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		switch step.Ref.Name {
		case "web":
			webRunning <- struct{}{}
			<-webGo
		case "slow":
			slowRunning <- struct{}{}
			<-slowGo
		}
		return nil
	}}
	upgrade := propose(t, e, "Upgrade web and open slow", web("nginx:1.28"), rule("srv_A", "slow", 8080))
	if err := e.Start(t.Context(), upgrade.Change, By{Actor: "user:usr_mira"}); err != nil {
		t.Fatal(err)
	}
	<-webRunning
	// Planned now, web is to be replaced. By the time it applies, the
	// upgrade has replaced it, so a fresh plan would differ.
	again := propose(t, e, "Upgrade web", web("nginx:1.28"))
	close(webGo)
	<-slowRunning
	if err := e.Apply(t.Context(), again.Change); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), upgrade.Change) {
		t.Errorf("applying while %s still runs = %v, want ErrBusy naming it", upgrade.Change, err)
	}
	close(slowGo)
	e.Wait()
}

// Planning asks agents outside the commit lock: a slow agent holds up the
// changes that touch its server, and no others.
func TestASlowAgentHoldsUpOnlyItsOwnChanges(t *testing.T) {
	storetest.Each(t, testASlowAgentHoldsUpOnlyItsOwnChanges)
}

func testASlowAgentHoldsUpOnlyItsOwnChanges(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A", "srv_B")
	slow := propose(t, e, "Open http on srv_A", rule("srv_A", "http", 80))
	other := propose(t, e, "Open http on srv_B", rule("srv_B", "http", 80))
	planning, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, plan: func(ctx context.Context, _ []resource.Desired) error {
		planning <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- e.Apply(context.Background(), slow.Change) }()
	<-planning
	applied := make(chan error, 1)
	go func() { applied <- e.Apply(context.Background(), other.Change) }()
	select {
	case err := <-applied:
		if err != nil {
			t.Errorf("applying on srv_B while srv_A's agent plans = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("applying on srv_B waited for srv_A's agent to plan")
	}
	unblock()
	if err := <-done; err != nil {
		t.Errorf("applying on srv_A = %v", err)
	}
	e.Wait()
}

// Watchers hear that a change failed only once the store has it: a failure
// the store can't record leaves the change applying there, for Resume.
func TestAFailureIsPublishedOnlyOnceRecorded(t *testing.T) {
	storetest.Each(t, testAFailureIsPublishedOnlyOnceRecorded)
}

func testAFailureIsPublishedOnlyOnceRecorded(t *testing.T, backend store.Backend) {
	e, f, db, location := setupAt(t, backend, "srv_A")
	restore := func() {}
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		if step.Ref.Name == "boom" {
			restore = storetest.FailUpdates(t, location, "changes", "state") // as a full disk would
			return errors.New("boom can't be opened")
		}
		return nil
	}}
	p := propose(t, e, "Open http and boom", rule("srv_A", "http", 80), rule("srv_A", "boom", 9))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates := e.Watch(ctx, p.Change)
	if err := e.Apply(t.Context(), p.Change); err == nil {
		t.Fatal("apply succeeded")
	}
	restore()
	if c := stored(t, db, p.Change); c.State != store.ChangeApplying {
		t.Fatalf("the change is %s, want still applying", c.State)
	}
	for {
		select {
		case u := <-updates:
			if u.State == store.ChangeFailed {
				t.Fatal("watchers heard the change failed, though the store doesn't say so")
			}
		default:
			return
		}
	}
}

// Drift names a server by its name, which is what people read.
func TestDriftNamesTheServer(t *testing.T) { storetest.Each(t, testDriftNamesTheServer) }

func testDriftNamesTheServer(t *testing.T, backend store.Backend) {
	e, f, _, location := setupAt(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Open http", rule("srv_A", "http", 80)))
	storetest.Exec(t, location, `UPDATE servers SET name = 'hz-9' WHERE id = 'srv_A'`)
	f["srv_A"].agent = cantObserve{f["srv_A"].agent}
	var agent *AgentError
	if _, err := e.Drift(t.Context(), "srv_A"); !errors.As(err, &agent) || agent.Server != "hz-9" || !strings.Contains(err.Error(), "on hz-9") {
		t.Errorf("drift with an agent that can't read the server = %v, want it to name hz-9", err)
	}
	f["srv_A"].online.Store(false)
	if _, err := e.Drift(t.Context(), "srv_A"); !errors.Is(err, ErrOffline) || !strings.Contains(err.Error(), "hz-9") {
		t.Errorf("drift with hz-9 away = %v, want it to name hz-9", err)
	}
}

type cantObserve struct{ resource.Agent }

func (cantObserve) Observe(context.Context, []resource.Ref) ([]proto.Message, error) {
	return nil, &resource.ServerError{Server: "hz-9", Err: errors.New("can't read the firewall")}
}

// An agent's own failure names its server once, however it is wrapped.
func TestAnAgentsFailureNamesItsServerOnce(t *testing.T) {
	err := &AgentError{Server: "hz-1", Doing: "plan", Err: &resource.ServerError{Server: "hz-1", Err: errors.New("can't read the firewall")}}
	if got := err.Error(); got != "can't plan on hz-1: can't read the firewall" {
		t.Errorf("message = %q", got)
	}
}

// A list of changes has each plan in brief, and what rolled each back or
// undid it, read for the page at once.
func TestChangesListPlansInBrief(t *testing.T) { storetest.Each(t, testChangesListPlansInBrief) }

func testChangesListPlansInBrief(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	env := production(t, db)
	opened := propose(t, e, "Open http", rule("srv_A", "http", 80))
	apply(t, e, opened)
	undo, err := e.Undo(t.Context(), opened.Change, By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, undo)
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		if step.Ref.Name == "boom" {
			return errors.New("boom can't be opened")
		}
		return nil
	}}
	boom := propose(t, e, "Run web and boom", app(env, "nginx:1.27"), rule("srv_A", "boom", 9))
	var failed *FailedError
	if err := e.Apply(t.Context(), boom.Change); !errors.As(err, &failed) {
		t.Fatalf("apply = %v", err)
	}
	records, err := e.Changes(t.Context(), store.ChangePage{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{opened.Change: "undone by " + undo.Change, boom.Change: "rolled back by " + failed.Rollback}
	for _, r := range records {
		full, err := e.Change(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Plan != nil || r.Brief != full.Plan.Brief() || r.Brief != full.Brief {
			t.Errorf("%s is listed with plan %v, brief %+v; want no plan, and the brief %+v", r.ID, r.Plan, r.Brief, full.Plan.Brief())
		}
		var got string
		switch {
		case r.UndoneBy != "":
			got = "undone by " + r.UndoneBy
		case r.RolledBackBy != "":
			got = "rolled back by " + r.RolledBackBy
		}
		if w, ok := want[r.ID]; ok && got != w || !ok && got != "" {
			t.Errorf("%s is listed %q, want %q", r.ID, got, w)
		}
	}
}
