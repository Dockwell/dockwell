// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The same proposal against the same state plans the same, though each
// plan gives new resources IDs of their own; and planning changes nothing
// on the servers. Its fingerprint is the same too, unless it edits a kind
// that may hold secrets: that is digested for each change alone, so
// comparing two changes' fingerprints can't confirm they set one value.
func TestPlansAreReproducibleAndChangeNothing(t *testing.T) {
	storetest.Each(t, testPlansAreReproducibleAndChangeNothing)
}

func testPlansAreReproducibleAndChangeNothing(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	org := tree(t, db).Organization.ID
	edits := func() []Edit {
		return []Edit{
			{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}},
			web("nginx:1.27"),
			rule("srv_A", "http", 80),
		}
	}
	first := propose(t, e, "Open the shop", edits()...)
	second := propose(t, e, "Open the shop", edits()...)
	if first.Fingerprint == second.Fingerprint {
		t.Error("two changes that set one container's environment have one fingerprint")
	}
	// Planned the same, with each new resource's own ID, which the new
	// project's environment lives in.
	same := func(p *Plan) *Plan {
		c := *p
		c.Change, c.Fingerprint, c.plain = "", "", ""
		c.Resources = slices.Clone(p.Resources)
		for i := range c.Resources {
			c.Resources[i].ID = ""
			if c.Resources[i].Ref.Kind == scope.EnvironmentKind {
				c.Resources[i].Ref.Scope = ""
			}
		}
		return &c
	}
	if a, b := same(first), same(second); !reflect.DeepEqual(a, b) {
		t.Errorf("the same proposal planned\n%+v, then\n%+v", a, b)
	}
	if first.Resources[0].ID == second.Resources[0].ID {
		t.Error("two plans gave a new project the same ID")
	}
	if a, b := propose(t, e, "Open", edits()[2]), propose(t, e, "Open", edits()[2]); a.Fingerprint != b.Fingerprint {
		t.Errorf("the same firewall rule planned %s, then %s", a.Fingerprint, b.Fingerprint)
	}
	if f["srv_A"].docker.Runs != 0 || f["srv_A"].firewall.Len() != 0 {
		t.Error("planning changed the server")
	}
	// A different state plans differently.
	apply(t, e, first)
	if third := propose(t, e, "Open the shop", edits()[1:]...); third.Fingerprint == first.Fingerprint {
		t.Error("a plan against a server that has everything already has the first plan's fingerprint")
	}
}

// A plan says what it does to each resource as a whole, and sums them up
// for the plan sheet: a replace on its own, never among the changes.
func TestAPlanSumsUpEachResource(t *testing.T) { storetest.Each(t, testAPlanSumsUpEachResource) }

func testAPlanSumsUpEachResource(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80), rule("srv_A", "ssh", 22)))
	p := propose(t, e, "Upgrade web, limit http, drop ssh, add https",
		web("nginx:1.28"), rule("srv_A", "http", 80, "203.0.113.7"), Edit{Ref: rule("srv_A", "ssh", 22).Ref}, rule("srv_A", "https", 443),
		environment(tree(t, db).Projects[0].ID, "staging"))
	if c := p.Counts(); c != (Counts{Create: 2, Change: 1, Replace: 1, Destroy: 1}) {
		t.Errorf("counts = %+v, want 2 created, 1 changed, 1 replaced, 1 destroyed", c)
	}
	actions := map[string]resource.Action{}
	for i := range p.Resources {
		actions[p.Resources[i].Ref.Name] = p.Resources[i].Action
	}
	if actions["web"] != resource.ActionReplace || actions["http"] != resource.ActionUpdate || actions["ssh"] != resource.ActionDelete ||
		actions["https"] != resource.ActionCreate || actions["staging"] != resource.ActionCreate {
		t.Errorf("actions = %v", actions)
	}
	if p.Servers[0].Name != "srv_A" || p.Destructive() {
		t.Errorf("server plan = %+v, destructive %t", p.Servers[0], p.Destructive())
	}
}

// A change applied in the background reports each step as it goes, to any
// number of watchers, and records it.
func TestStartReportsEachStep(t *testing.T) { storetest.Each(t, testStartReportsEachStep) }

func testStartReportsEachStep(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	p := propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a, b := e.Watch(ctx, p.Change), e.Watch(ctx, p.Change)
	if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan Update{a, b} {
		var seen []string
		for u := range ch {
			if u.Step != nil {
				seen = append(seen, u.Step.State.String())
				continue
			}
			seen = append(seen, u.State)
			if u.State == store.ChangeApplied {
				break
			}
		}
		if got := strings.Join(seen, " "); got != "applying running done running done applied" {
			t.Errorf("a watcher saw %s", got)
		}
	}
	e.Wait()
	r, err := e.Change(t.Context(), p.Change)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != store.ChangeApplied || r.AppliedBy != "user:usr_mira" || r.AppliedVia != ViaPanel || len(r.Progress.Steps) != 2 ||
		r.Progress.Steps[1].State != StepDone || r.Progress.Steps[1].Message != "Created firewall.rule http on srv_A" {
		t.Errorf("the recorded change = %+v", r)
	}
	if d := r.UndoDeadline(); d.Sub(r.AppliedAt) != UndoWindow {
		t.Errorf("undo deadline %v, applied at %v", d, r.AppliedAt)
	}
}

// A watcher that falls behind is dropped, and lets go of everything it
// held then, though its ctx goes on: a watch of the timeline that falls
// behind again and again, and watches again each time with the same ctx,
// holds one watcher, not one for each time.
func TestADroppedWatcherLetsGo(t *testing.T) {
	e, _, _ := setup(t, store.SQLite, "srv_A")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	before := runtime.NumGoroutine()
	const times = 20
	for range times {
		updates := e.WatchAll(ctx)
		for range watcherBuffer + 1 {
			e.published("chg_busy", store.ChangeApplying)
		}
		for range updates {
			// Drain until the engine drops this watcher and closes it.
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+times/2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after %d dropped watchers, from %d before: dropped watchers wait for their ctx", runtime.NumGoroutine(), times, before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if n := len(e.watchers[everyChange]); n != 0 {
		t.Errorf("%d watchers left, want none", n)
	}
}

// A planned change can be discarded; nothing else can.
func TestDiscard(t *testing.T) { storetest.Each(t, testDiscard) }

func testDiscard(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	p := propose(t, e, "Run web", web("nginx:1.27"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	w := e.Watch(ctx, p.Change)
	if err := e.Discard(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaAPI}); err != nil {
		t.Fatal(err)
	}
	if u := <-w; u.State != store.ChangeDiscarded {
		t.Errorf("a watcher saw %+v, want discarded", u)
	}
	if err := e.Apply(t.Context(), p.Change); !errors.Is(err, ErrNotPlanned) {
		t.Errorf("applying a discarded change = %v, want ErrNotPlanned", err)
	}
	if err := e.Discard(t.Context(), p.Change, By{}); !errors.Is(err, ErrNotPlanned) {
		t.Errorf("discarding twice = %v, want ErrNotPlanned", err)
	}
	if err := e.Discard(t.Context(), "chg_nothing", By{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("discarding no change = %v, want ErrNotFound", err)
	}
	if f["srv_A"].docker.Runs != 0 {
		t.Error("a discarded change ran")
	}
}

// Resume carries a change's progress over: what ran before the stop stays
// done, what remains runs, and the record says so.
func TestResumeCarriesProgressOver(t *testing.T) { storetest.Each(t, testResumeCarriesProgressOver) }

func testResumeCarriesProgressOver(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A", "srv_B")
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { f["srv_B"].online.Store(false) }}
	onB := web("nginx:1.27")
	onB.Ref.Scope = "srv_B"
	p := propose(t, e, "Open http and run web", rule("srv_A", "http", 80), onB)
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) {
		t.Fatalf("apply = %v", err)
	}
	r, _ := e.Change(t.Context(), p.Change)
	if r.RolledBackBy != failed.Rollback || r.Progress.Steps[0].State != StepDone || r.Progress.Steps[1].State != StepFailed {
		t.Errorf("the failed change = %+v", r)
	}
	rb, _ := e.Change(t.Context(), failed.Rollback)
	var waiting int
	for i := range rb.Progress.Steps {
		if rb.Progress.Steps[i].State == StepWaiting {
			waiting++
		}
	}
	if rb.State != store.ChangeApplying || waiting != 1 {
		t.Errorf("the rollback while srv_B is away = %+v, want one step waiting", rb)
	}
	f["srv_B"].online.Store(true)
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	rb, _ = e.Change(t.Context(), failed.Rollback)
	for i := range rb.Progress.Steps {
		if s := &rb.Progress.Steps[i]; s.State != StepDone {
			t.Errorf("after resuming, step %+v isn't done", s)
		}
	}
}

// Two changes never run steps on one resource at once: one that makes no
// revision, such as reverting drift, waits for the change applying it.
func TestAResourceRunsOneChangeAtATime(t *testing.T) {
	storetest.Each(t, testAResourceRunsOneChangeAtATime)
}

func testAResourceRunsOneChangeAtATime(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	hold := make(chan struct{})
	running := make(chan struct{}, 1)
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { running <- struct{}{}; <-hold }}
	upgrade := propose(t, e, "Upgrade web", web("nginx:1.28"))
	if err := e.Start(t.Context(), upgrade.Change, By{Actor: "user:usr_mira"}); err != nil {
		t.Fatal(err)
	}
	<-running
	// The same spec again makes no revision, so only the guard stops it.
	again := propose(t, e, "Upgrade web", web("nginx:1.28"))
	if err := e.Apply(t.Context(), again.Change); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), upgrade.Change) {
		t.Errorf("applying while another change runs on web = %v, want ErrBusy naming %s", err, upgrade.Change)
	}
	close(hold)
	e.Wait()
	other := propose(t, e, "Open http", rule("srv_A", "http", 80))
	f["srv_A"].agent = f["srv_A"].agent.(hooked).Agent
	if err := e.Apply(t.Context(), other.Change); err != nil {
		t.Errorf("applying once the other change ended = %v", err)
	}
}
