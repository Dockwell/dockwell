// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
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

// lookups counts how often the engine looks up each server's agent. Each
// Resume pass looks up every server of each change it finds applying, so
// the count says how many passes ran.
type lookups struct {
	fleet
	mu sync.Mutex
	n  map[string]int
}

func (l *lookups) Agent(id string) (resource.Agent, bool) {
	l.mu.Lock()
	l.n[id]++
	l.mu.Unlock()
	return l.fleet.Agent(id)
}

func (l *lookups) of(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n[id]
}

// waitingForB fails a change as srv_B drops, so that its rollback waits for
// srv_B: web goes back to nginx:1.27 there, which srv_B still runs.
func waitingForB(t *testing.T, e *Engine, f fleet) (rollback string) {
	t.Helper()
	onB := web("nginx:1.27")
	onB.Ref.Scope = "srv_B"
	apply(t, e, propose(t, e, "Run web on srv_B", onB))
	plain := f["srv_A"].agent
	f["srv_A"].agent = hooked{Agent: plain, before: func(resource.Step) { f["srv_B"].online.Store(false) }}
	defer func() { f["srv_A"].agent = plain }()
	onB.Spec.(*resourcev1.Container).Image = "nginx:1.28"
	p := propose(t, e, "Open http and upgrade web", rule("srv_A", "http", 80), onB)
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) || !errors.Is(failed.RollbackErr, ErrPending) {
		t.Fatalf("apply as srv_B drops = %v, want it failed, with a rollback that waits for srv_B", err)
	}
	return failed.Rollback
}

// Resume skips a change that runs here, and runs again once that change
// lets go: not each time some other change lets go. A rollback that waits
// for a server that is away lets go at once, every pass, and used to take
// the wakeup meant for the running change, so Resume went round and round
// until that change ended.
func TestResumeWaitsForTheChangeItSkipped(t *testing.T) {
	storetest.Each(t, testResumeWaitsForTheChangeItSkipped)
}

func testResumeWaitsForTheChangeItSkipped(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	// The running change is older than the rollback, so each pass meets it
	// first, then lets the rollback go.
	slow := propose(t, e, "Open slow", rule("srv_A", "slow", 8080))
	time.Sleep(2 * time.Millisecond)
	rollback := waitingForB(t, e, f)

	agents := &lookups{fleet: f, n: map[string]int{}}
	e = newEngineWith(db, agents)
	running, release := make(chan struct{}, 1), make(chan struct{})
	plain := f["srv_A"].agent
	f["srv_A"].agent = hooked{Agent: plain, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		if step.Ref.Name == "slow" {
			running <- struct{}{}
			<-release
		}
		return nil
	}}
	if err := e.Start(t.Context(), slow.Change, By{Actor: "user:usr_mira"}); err != nil {
		t.Fatal(err)
	}
	<-running
	// Two servers connect while slow runs: each Resume skips slow, and
	// finds srv_B away.
	for range 2 {
		if err := e.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := agents.of("srv_B"); n > 2 {
		t.Errorf("Resume looked for srv_B %d times while slow ran, want 2: one for each Resume, and none in a loop", n)
	}

	// Once slow ends, Resume runs again, and srv_B is back by then.
	f["srv_B"].online.Store(true)
	close(release)
	e.Wait()
	if c := stored(t, db, rollback); c.State != store.ChangeApplied {
		t.Errorf("the rollback is %s once slow ended with srv_B back; Resume didn't run again", c.State)
	}
	if n := agents.of("srv_B"); n > 4 {
		t.Errorf("Resume looked for srv_B %d times in all, want at most 4", n)
	}
}

// A pass of Resume that gets nowhere doesn't ask for another. Here srv_B is
// connected, yet the rollback's call never reaches its agent, as when the
// stream it went to had just ended: the step waits, and the agent's
// reconnect resumes it, not the run itself, round and round.
func TestAResumeThatGetsNowhereDoesntGoAgain(t *testing.T) {
	storetest.Each(t, testAResumeThatGetsNowhereDoesntGoAgain)
}

func testAResumeThatGetsNowhereDoesntGoAgain(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	rollback := waitingForB(t, e, f)
	var calls atomic.Int32
	plain := f["srv_B"].agent
	f["srv_B"].agent = hooked{Agent: plain, apply: func(context.Context, resource.Step, proto.Message) error {
		calls.Add(1)
		return fmt.Errorf("%w: srv_B's agent disconnected before the call reached it", resource.ErrNotConnected)
	}}
	// srv_B's own web moved on to 1.28 meanwhile, so the rollback has a
	// step there.
	f["srv_B"].docker.Edit("web", func(c *resourcev1.Container) { c.Image = "nginx:1.28" })
	f["srv_B"].online.Store(true)
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		// Fatal, so the loop's next pass finds the store closed and ends.
		t.Fatalf("the rollback's step was sent %d times in 300 ms, want once: Resume went round again with nothing done", n)
	}
	e.Wait()
	if c := stored(t, db, rollback); c.State != store.ChangeApplying {
		t.Fatalf("the rollback is %s, want it still applying, waiting for srv_B", c.State)
	}
	// The agent reconnects, and Resume runs as it does then.
	f["srv_B"].agent = plain
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := stored(t, db, rollback); c.State != store.ChangeApplied || image(t, f["srv_B"]) != "nginx:1.27" {
		t.Errorf("after srv_B reconnects the rollback is %s, with %s on srv_B", c.State, image(t, f["srv_B"]))
	}
}

// No wakeup is lost when Resume skips a change because another holds one of
// its resources: once the holder lets go, Resume runs again. Here a revert
// of drift, which makes no revision, holds web on srv_B as it plans, before
// it is on record as applying, while a rollback that puts web back waits.
func TestResumeRunsAgainOnceTheHolderLetsGo(t *testing.T) {
	storetest.Each(t, testResumeRunsAgainOnceTheHolderLetsGo)
}

func testResumeRunsAgainOnceTheHolderLetsGo(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	rollback := waitingForB(t, e, f)
	f["srv_B"].online.Store(true)
	f["srv_B"].docker.Edit("web", func(c *resourcev1.Container) { c.Env["DEBUG"] = "1" })
	drift, err := e.Drift(t.Context(), "srv_B")
	if err != nil || len(drift) != 1 {
		t.Fatalf("drift on srv_B = %v, %v", drift, err)
	}
	revert, err := e.Revert(t.Context(), drift[0], By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	planning, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	f["srv_B"].agent = hooked{Agent: f["srv_B"].agent, plan: func(context.Context, []resource.Desired) error {
		once.Do(func() { planning <- struct{}{}; <-release })
		return nil
	}}
	applied := make(chan error, 1)
	go func() { applied <- e.Apply(context.Background(), revert.Change) }()
	<-planning
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := stored(t, db, rollback); c.State != store.ChangeApplying {
		t.Fatalf("the rollback is %s while the revert holds web, want applying", c.State)
	}
	close(release)
	if err := <-applied; err != nil {
		t.Fatalf("applying the revert = %v", err)
	}
	e.Wait()
	if c := stored(t, db, rollback); c.State != store.ChangeApplied {
		t.Errorf("the rollback is %s once the revert let go of web; Resume didn't run again", c.State)
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

// A rollback whose stream drops while a step runs waits for its server, as
// for one that isn't connected, rather than failing for good: Resume plans
// the step against the server again, and finds it done or not.
func TestARollbackWaitsWhenItsStreamDrops(t *testing.T) {
	storetest.Each(t, testARollbackWaitsWhenItsStreamDrops)
}

func testARollbackWaitsWhenItsStreamDrops(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	var dropped atomic.Bool
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		switch {
		case step.Ref.Name == "boom":
			return errors.New("boom can't be opened")
		case step.Action == resource.ActionDelete && dropped.CompareAndSwap(false, true):
			// The rollback's step: the stream drops while it runs.
			return fmt.Errorf("%w: srv_A's agent disconnected before it answered, so the step may or may not have run", resource.ErrLost)
		}
		return nil
	}}
	p := propose(t, e, "Open http and boom", rule("srv_A", "http", 80), rule("srv_A", "boom", 9))
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) || failed.Rollback == "" || !errors.Is(failed.RollbackErr, ErrPending) {
		t.Fatalf("apply = %v, want it failed, with a rollback that waits for srv_A", err)
	}
	e.Wait() // srv_A is back: Resume plans the rollback's step again, and runs it
	if c := stored(t, db, failed.Rollback); c.State != store.ChangeApplied || f["srv_A"].firewall.Len() != 0 {
		t.Errorf("the rollback is %s, with %d rules on srv_A; want it applied, with none", c.State, f["srv_A"].firewall.Len())
	}
}

// A retried apply that loses the race to the first try reads that the
// change is applied: its plan isn't to blame, and proposing the edits again
// would only make another change. Here the retry reaches a second engine
// over the same store, as a second control plane would, and plans while
// the first try applies.
func TestARetriedApplyThatLostTheRaceIsntBlamedOnThePlan(t *testing.T) {
	storetest.Each(t, testARetriedApplyThatLostTheRaceIsntBlamedOnThePlan)
}

func testARetriedApplyThatLostTheRaceIsntBlamedOnThePlan(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	p := propose(t, e, "Open http", rule("srv_A", "http", 80))
	planning, release := make(chan struct{}, 1), make(chan struct{})
	var first atomic.Bool
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, plan: func(context.Context, []resource.Desired) error {
		if first.CompareAndSwap(false, true) {
			planning <- struct{}{}
			<-release
		}
		return nil
	}}
	retry := newEngine(db, f)
	retried := make(chan error, 1)
	go func() { retried <- retry.Apply(context.Background(), p.Change) }()
	<-planning
	apply(t, e, p)
	close(release)
	err := <-retried
	if !errors.Is(err, ErrNotPlanned) || errors.Is(err, ErrPlanChanged) || errors.Is(err, ErrStale) {
		t.Fatalf("the retry that lost the race = %v, want ErrNotPlanned, not a plan to propose again", err)
	}
	if want := p.Change + " is applied already, so it can't be applied again. Undo it, or propose a new change"; err.Error() != want {
		t.Errorf("the retry says %q, want %q", err, want)
	}
}

// Watchers hear that a step ended only once the store has it, as for a
// change: a step whose record can't be written isn't announced done.
func TestAStepIsAnnouncedOnlyOnceRecorded(t *testing.T) {
	storetest.Each(t, testAStepIsAnnouncedOnlyOnceRecorded)
}

func testAStepIsAnnouncedOnlyOnceRecorded(t *testing.T, backend store.Backend) {
	e, f, _, location := setupAt(t, backend, "srv_A")
	restore := func() {}
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
		switch step.Ref.Name {
		case "http":
			restore = storetest.FailUpdates(t, location, "changes", "progress") // as a full disk would
		case "https":
			restore()
		}
		return nil
	}}
	p := propose(t, e, "Open http and https", rule("srv_A", "http", 80), rule("srv_A", "https", 443))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates := e.Watch(ctx, p.Change)
	apply(t, e, p)
	var ended []int
	for {
		select {
		case u := <-updates:
			if u.Step != nil && u.Step.State.Final() {
				ended = append(ended, u.Index)
			}
			continue
		default:
		}
		break
	}
	if !slices.Equal(ended, []int{1}) {
		t.Errorf("watchers heard steps %v end, want only step 1: step 0's end never reached the store", ended)
	}
}

// A rollback's step that its agent couldn't plan says so while it waits,
// with the agent's reason: its server is connected, so it isn't waiting for
// it to reconnect.
func TestAWaitingStepSaysWhyItWaits(t *testing.T) { storetest.Each(t, testAWaitingStepSaysWhyItWaits) }

func testAWaitingStepSaysWhyItWaits(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	var plans atomic.Int32
	release := make(chan struct{})
	f["srv_A"].agent = hooked{
		Agent: f["srv_A"].agent,
		apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
			if step.Ref.Name == "boom" {
				return errors.New("boom can't be opened")
			}
			return nil
		},
		plan: func(_ context.Context, desired []resource.Desired) error {
			if desired[0].Spec != nil {
				return nil // the change's own plans; the rollback's delete
			}
			switch plans.Add(1) {
			case 1:
				return errors.New("can't read the firewall just now")
			case 2:
				<-release // Resume's, once the rollback's first run lets go
			}
			return nil
		},
	}
	p := propose(t, e, "Open http and boom", rule("srv_A", "http", 80), rule("srv_A", "boom", 9))
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) || !errors.Is(failed.RollbackErr, ErrPending) {
		t.Fatalf("apply = %v, want it failed, with a rollback that waits", err)
	}
	rb, err := decodeProgress(stored(t, db, failed.Rollback).Progress)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Waiting for srv_A's agent, which couldn't plan this step: can't read the firewall just now"
	for i := range rb.Steps {
		if s := &rb.Steps[i]; s.State != StepWaiting || s.Message != want {
			t.Errorf("the rollback's step %s is %s, saying %q; want it waiting, saying %q", s.Ref.Name, s.State, s.Message, want)
		}
	}
	close(release)
	e.Wait()
	if c := stored(t, db, failed.Rollback); c.State != store.ChangeApplied {
		t.Errorf("the rollback is %s once its agent could plan it, want applied", c.State)
	}
}

// A rollback commits only while nothing it puts back moved since it read
// it, as every change does: a newer revision committed meanwhile, by any
// path, makes the rollback stale rather than being written over.
func TestARollbackDoesntWriteOverANewerRevision(t *testing.T) {
	storetest.Each(t, testARollbackDoesntWriteOverANewerRevision)
}

func testARollbackDoesntWriteOverANewerRevision(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Open http", rule("srv_A", "http", 80)))
	plain := f["srv_A"].agent
	var failed, raced atomic.Bool
	f["srv_A"].agent = hooked{
		Agent: plain,
		apply: func(_ context.Context, step resource.Step, desired proto.Message) error {
			if desired.(*resourcev1.FirewallRule).GetPort() == 8080 {
				failed.Store(true)
				return errors.New("the port is taken")
			}
			return nil
		},
		plan: func(ctx context.Context, desired []resource.Desired) error {
			// The rollback plans with no lock held; a newer revision of
			// http commits meanwhile.
			if !failed.Load() || raced.Swap(true) {
				return nil
			}
			newer, err := resource.Encode(&resourcev1.FirewallRule{Port: 9090, Comment: "the web app"})
			if err != nil {
				return err
			}
			if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
				if err := tx.CreateChange(ctx, store.Change{ID: "chg_newer", Summary: "Move http", Actor: "usr_other", State: store.ChangePlanned,
					Plan: []byte("{}"), CreatedAt: time.Now(), ProposedVia: ViaAPI}); err != nil {
					return err
				}
				_, err := tx.AddRevision(ctx, store.NewRevision{Kind: string(firewall.Kind), Scope: "srv_A", Name: "http", Spec: newer, Change: "chg_newer", At: time.Now()})
				return err
			}); err != nil {
				t.Errorf("can't commit a newer revision of http: %v", err)
			}
			return nil
		},
	}
	p := propose(t, e, "Move http to 8080", rule("srv_A", "http", 8080))
	err := e.Apply(t.Context(), p.Change)
	var ferr *FailedError
	if !errors.As(err, &ferr) || !raced.Load() {
		t.Fatalf("Apply = %v, raced %t; want the change to fail, and a newer revision to commit as it rolls back", err, raced.Load())
	}
	if !errors.Is(ferr.RollbackErr, ErrStale) || ferr.Rollback != "" {
		t.Errorf("the rollback = %q, %v; want it refused as out of date", ferr.Rollback, ferr.RollbackErr)
	}
	var latest store.Resource
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		latest, err = tx.GetResource(t.Context(), string(firewall.Kind), "srv_A", "http")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if latest.ChangeID != "chg_newer" || latest.Revision != 3 {
		t.Errorf("http's latest revision is %d, from %s; want 3, the newer one, kept", latest.Revision, latest.ChangeID)
	}
}
