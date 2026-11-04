// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const recreate = 2 * time.Second

// server is a fake machine: Docker and a firewall behind its agent.
type server struct {
	docker   *container.Fake
	firewall *firewall.Fake
	agent    resource.Agent
	online   bool
}

type fleet map[string]*server

func (f fleet) Agent(id string) (resource.Agent, bool) {
	s, ok := f[id]
	if !ok {
		return nil, false
	}
	return s.agent, s.online
}

func setup(t *testing.T, b store.Backend, ids ...string) (*Engine, fleet, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, b)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f := fleet{}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
				return err
			}
			s := &server{docker: container.NewFake(), firewall: firewall.NewFake(), online: true}
			s.agent = resource.Local{Drivers: resource.NewRegistry(
				&container.Driver{Runtime: s.docker, RecreateDowntime: recreate},
				&firewall.Driver{Backend: s.firewall},
				&service.Driver{Containers: container.Driver{Runtime: s.docker, RecreateDowntime: recreate}},
			)}
			f[id] = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The control plane's drivers only read specs; they never reach a server.
	kinds := resource.NewRegistry(&container.Driver{RecreateDowntime: recreate}, &firewall.Driver{}, &service.Driver{})
	return New(db, kinds, f, slog.New(slog.DiscardHandler)), f, db
}

func web(image string) Edit {
	return Edit{
		Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"},
		Spec: &resourcev1.Container{
			Image: image,
			Env:   map[string]string{"API_KEY": "s3cret-value"},
			Ports: []*resourcev1.PortMapping{{HostPort: 80, ContainerPort: 8080}},
		},
	}
}

func rule(server, name string, port uint32, sources ...string) Edit {
	return Edit{
		Ref:  resource.Ref{Kind: firewall.Kind, Scope: server, Name: name},
		Spec: &resourcev1.FirewallRule{Port: port, Sources: sources, Comment: "the web app"},
	}
}

func propose(t *testing.T, e *Engine, summary string, edits ...Edit) *Plan {
	t.Helper()
	p, err := e.Propose(t.Context(), Proposal{Summary: summary, Actor: "usr_test", Edits: edits})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func apply(t *testing.T, e *Engine, p *Plan) {
	t.Helper()
	if err := e.Apply(t.Context(), p.Change); err != nil {
		t.Fatal(err)
	}
}

func change(t *testing.T, steps []resource.Step, path string) resource.Change {
	t.Helper()
	for i := range steps {
		for _, c := range steps[i].Changes {
			if c.Path == path {
				return c
			}
		}
	}
	t.Fatalf("no change to %s in %+v", path, steps)
	return resource.Change{}
}

func image(t *testing.T, s *server) string {
	t.Helper()
	c, err := s.docker.Inspect(context.Background(), "web")
	if err != nil || c == nil {
		t.Fatalf("no web container: %v", err)
	}
	return c.GetImage()
}

// A container and a firewall rule, unlike in every way that matters, go
// through the same plan, apply and undo.
func TestPlanApplyUndoAcrossKinds(t *testing.T) { storetest.Each(t, testPlanApplyUndoAcrossKinds) }

func testPlanApplyUndoAcrossKinds(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	hz1 := f["srv_A"]

	first := propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80))
	if len(first.Servers) != 1 || first.Servers[0].Server != "srv_A" || first.Revisions != 2 {
		t.Fatalf("plan: %+v", first)
	}
	steps := first.Steps()
	if steps[0].Action != resource.ActionCreate || steps[1].Action != resource.ActionCreate || first.Servers[0].Downtime != 0 {
		t.Errorf("creating should take no downtime: %+v", steps)
	}
	apply(t, e, first)
	if image(t, hz1) != "nginx:1.27" || hz1.firewall.Len() != 1 {
		t.Fatal("the first change didn't reach the server")
	}

	second := propose(t, e, "Upgrade web and limit it to the office",
		web("nginx:1.28"), rule("srv_A", "http", 80, "203.0.113.7"))
	sp := second.Servers[0]
	if sp.Steps[0].Action != resource.ActionReplace || sp.Steps[0].Effect != resource.EffectRecreate || sp.Steps[1].Action != resource.ActionUpdate {
		t.Errorf("steps: %+v", sp.Steps)
	}
	if sp.Downtime != recreate {
		t.Errorf("downtime %s, want the container's %s; the rule changes atomically", sp.Downtime, recreate)
	}
	if c := change(t, sp.Steps, "image"); c.From != "nginx:1.27" || c.To != "nginx:1.28" || c.Op != resource.OpModified {
		t.Errorf("image change: %+v", c)
	}
	if c := change(t, sp.Steps, "sources"); c.From != "" || c.To != "203.0.113.7/32" || c.Op != resource.OpAdded {
		t.Errorf("sources change, normalized to CIDR: %+v", c)
	}
	apply(t, e, second)

	undo, err := e.Undo(t.Context(), second.Change, By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if undo.Reverts != second.Change || len(undo.Steps()) != 2 || undo.Servers[0].Downtime != recreate {
		t.Errorf("undo plan: %+v", undo)
	}
	apply(t, e, undo)
	r, _ := hz1.firewall.Rule(t.Context(), "http")
	if image(t, hz1) != "nginx:1.27" || len(r.GetSources()) != 0 {
		t.Errorf("after undo: %s, sources %v", image(t, hz1), r.GetSources())
	}

	// History only grows: undo wrote third revisions rather than rewinding.
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		res, err := tx.GetResource(t.Context(), string(container.Kind), "srv_A", "web")
		if err != nil || res.Revision != 3 || res.ChangeID != undo.Change {
			t.Errorf("web: revision %d by %s (%v), want 3 by the undo", res.Revision, res.ChangeID, err)
		}
		return nil
	})

	// The first change can't be undone now: web moved on since.
	if _, err := e.Undo(t.Context(), first.Change, By{Actor: "usr_test"}); !errors.Is(err, ErrCantUndo) {
		t.Errorf("undoing a change something later changed again = %v, want ErrCantUndo", err)
	}
}

func TestDeletesRunLastAndUndoBringsThemBack(t *testing.T) {
	storetest.Each(t, testDeletesRunLastAndUndoBringsThemBack)
}

func testDeletesRunLastAndUndoBringsThemBack(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80)))

	remove := propose(t, e, "Retire web",
		Edit{Ref: resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: "http"}},
		Edit{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}},
		rule("srv_A", "maintenance", 8080))
	steps := remove.Steps()
	if steps[0].Ref.Name != "maintenance" || steps[1].Ref.Name != "web" || steps[2].Ref.Name != "http" {
		t.Errorf("order: creates first, then deletes in reverse: %v, %v, %v", steps[0].Ref, steps[1].Ref, steps[2].Ref)
	}
	apply(t, e, remove)
	if c, _ := f["srv_A"].docker.Inspect(t.Context(), "web"); c != nil {
		t.Fatal("web is still running")
	}

	undo, err := e.Undo(t.Context(), remove.Change, By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, undo)
	httpRule, _ := f["srv_A"].firewall.Rule(t.Context(), "http")
	maintenance, _ := f["srv_A"].firewall.Rule(t.Context(), "maintenance")
	if image(t, f["srv_A"]) != "nginx:1.27" || httpRule == nil || maintenance != nil {
		t.Errorf("after undoing: image %s, http %v, maintenance %v; want web and http back, maintenance gone",
			image(t, f["srv_A"]), httpRule, maintenance)
	}
}

func TestSecretsStayOutOfPlans(t *testing.T) { storetest.Each(t, testSecretsStayOutOfPlans) }

func testSecretsStayOutOfPlans(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A")
	p := propose(t, e, "Run web", web("nginx:1.27"))
	c := change(t, p.Steps(), "env[API_KEY]")
	if !c.Sensitive || c.From != "" || c.To != "" || c.Op != resource.OpAdded {
		t.Errorf("a secret's change: %+v", c)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		rec, err := tx.GetChange(t.Context(), p.Change)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(rec.Plan, []byte("s3cret")) {
			t.Error("the recorded plan holds the secret")
		}
		return nil
	})
}

func TestAPlanCoversEveryServerItTouches(t *testing.T) {
	storetest.Each(t, testAPlanCoversEveryServerItTouches)
}

func testAPlanCoversEveryServerItTouches(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A", "srv_B")
	f["srv_B"].online = false

	p := propose(t, e, "Open the API on both", rule("srv_A", "api", 8443), rule("srv_B", "api", 8443))
	if len(p.Servers) != 2 || p.Servers[0].Offline || !p.Servers[1].Offline {
		t.Fatalf("servers: %+v", p.Servers)
	}
	if err := e.Apply(t.Context(), p.Change); !errors.Is(err, ErrOffline) {
		t.Errorf("applying with a server offline = %v, want ErrOffline", err)
	}
	if f["srv_A"].firewall.Len() != 0 {
		t.Error("part of the change ran although the plan couldn't apply whole")
	}
}

func TestApplyRefusesAStalePlan(t *testing.T) { storetest.Each(t, testApplyRefusesAStalePlan) }

func testApplyRefusesAStalePlan(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	a := propose(t, e, "Upgrade web", web("nginx:1.28"))
	b := propose(t, e, "Upgrade web further", web("nginx:1.29"))
	apply(t, e, a)
	if err := e.Apply(t.Context(), b.Change); !errors.Is(err, ErrStale) {
		t.Errorf("applying a plan made before another change = %v, want ErrStale", err)
	}
	if err := e.Apply(t.Context(), a.Change); !errors.Is(err, ErrNotPlanned) {
		t.Errorf("applying twice = %v, want ErrNotPlanned", err)
	}
}

func TestApplyRefusesWhenTheServerMovedSinceThePlan(t *testing.T) {
	storetest.Each(t, testApplyRefusesWhenTheServerMovedSinceThePlan)
}

func testApplyRefusesWhenTheServerMovedSinceThePlan(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	p := propose(t, e, "Upgrade web", web("nginx:1.28"))
	f["srv_A"].docker.Edit("web", func(c *resourcev1.Container) { c.Image = "nginx:1.26" })
	if err := e.Apply(t.Context(), p.Change); !errors.Is(err, ErrPlanChanged) {
		t.Errorf("applying after the server changed = %v, want ErrPlanChanged", err)
	}
}

func TestAFailedApplyRollsBack(t *testing.T) { storetest.Each(t, testAFailedApplyRollsBack) }

func testAFailedApplyRollsBack(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	hz1 := f["srv_A"]
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	hz1.docker.Broken["nginx:broken"] = true

	p := propose(t, e, "Deploy a broken build", rule("srv_A", "http", 80), web("nginx:broken"))
	err := e.Apply(t.Context(), p.Change)
	var failed *FailedError
	if !errors.As(err, &failed) || failed.Step.Name != "web" || failed.Rollback == "" || failed.RollbackErr != nil {
		t.Fatalf("apply = %v, want a failure at web that rolled back", err)
	}
	if image(t, hz1) != "nginx:1.27" || hz1.firewall.Len() != 0 {
		t.Errorf("after rollback: image %s, %d rules; want 1.27 and none", image(t, hz1), hz1.firewall.Len())
	}
	if err := hz1.docker.Healthy(t.Context(), "web"); err != nil {
		t.Errorf("web isn't healthy after rollback: %v", err)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		bad, _ := tx.GetChange(t.Context(), p.Change)
		back, _ := tx.GetChange(t.Context(), failed.Rollback)
		if bad.State != store.ChangeFailed || back.State != store.ChangeApplied || back.Reverts != p.Change || back.Actor != SystemActor {
			t.Errorf("states: failed change %s, rollback %s reverting %q by %s", bad.State, back.State, back.Reverts, back.Actor)
		}
		return nil
	})
}

func TestDriftIsFoundThenRevertedOrAdopted(t *testing.T) {
	storetest.Each(t, testDriftIsFoundThenRevertedOrAdopted)
}

func testDriftIsFoundThenRevertedOrAdopted(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	hz1 := f["srv_A"]
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80)))
	if d, err := e.Drift(t.Context(), "srv_A"); err != nil || len(d) != 0 {
		t.Fatalf("drift right after applying: %v, %v", d, err)
	}

	// Someone runs docker by hand.
	hz1.docker.Edit("web", func(c *resourcev1.Container) { c.Env["DEBUG"] = "1" })
	drift, err := e.Drift(t.Context(), "srv_A")
	if err != nil || len(drift) != 1 || drift[0].Ref.Name != "web" {
		t.Fatalf("drift: %+v, %v", drift, err)
	}
	if c := drift[0].Changes[0]; c.Path != "env[DEBUG]" || c.Op != resource.OpAdded || !c.Sensitive {
		t.Errorf("drift change: %+v", c)
	}
	revert, err := e.Revert(t.Context(), drift[0], By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if s := revert.Steps(); len(s) != 1 || s[0].Action != resource.ActionReplace || s[0].Changes[0].Op != resource.OpRemoved || revert.Revisions != 0 {
		t.Errorf("revert plan: %+v", revert)
	}
	apply(t, e, revert)
	if d, _ := e.Drift(t.Context(), "srv_A"); len(d) != 0 {
		t.Errorf("drift after reverting: %+v", d)
	}

	// Someone changes the restart policy and the team wants to keep it.
	hz1.docker.Edit("web", func(c *resourcev1.Container) { c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS })
	drift, _ = e.Drift(t.Context(), "srv_A")
	adopt, err := e.Adopt(t.Context(), drift[0], By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(adopt.Steps()) != 0 || adopt.Revisions != 1 || adopt.Empty() {
		t.Errorf("adopting changes the record, not the server: %+v", adopt)
	}
	runs := hz1.docker.Runs
	apply(t, e, adopt)
	if d, _ := e.Drift(t.Context(), "srv_A"); len(d) != 0 || hz1.docker.Runs != runs {
		t.Errorf("after adopting: drift %+v, %d new runs", d, hz1.docker.Runs-runs)
	}

	// Someone deletes the rule.
	_ = hz1.firewall.Delete(t.Context(), "http")
	drift, _ = e.Drift(t.Context(), "srv_A")
	if len(drift) != 1 || !drift[0].Missing || drift[0].Ref.Name != "http" {
		t.Errorf("a deleted rule: %+v", drift)
	}
}

func TestUndoHasAWindow(t *testing.T) { storetest.Each(t, testUndoHasAWindow) }

func testUndoHasAWindow(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	p := propose(t, e, "Run web", web("nginx:1.27"))
	apply(t, e, p)
	e.now = func() time.Time { return time.Now().Add(UndoWindow + time.Hour) }
	if _, err := e.Undo(t.Context(), p.Change, By{Actor: "usr_test"}); !errors.Is(err, ErrCantUndo) {
		t.Errorf("undo after the window = %v, want ErrCantUndo", err)
	}
}

func TestProposalsAreChecked(t *testing.T) { storetest.Each(t, testProposalsAreChecked) }

func testProposalsAreChecked(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	cases := map[string][]Edit{
		"no image":          {{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}, Spec: &resourcev1.Container{}}},
		"the wrong type":    {{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}, Spec: &resourcev1.FirewallRule{Port: 80}}},
		"an unknown kind":   {{Ref: resource.Ref{Kind: "volume", Scope: "srv_A", Name: "data"}, Spec: &resourcev1.FirewallRule{}}},
		"nothing to delete": {{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}}},
		"the same twice":    {web("nginx:1"), web("nginx:2")},
		"a bad source":      {rule("srv_A", "http", 80, "the office")},
	}
	for name, edits := range cases {
		if _, err := e.Propose(t.Context(), Proposal{Summary: name, Actor: "usr_test", Edits: edits}); err == nil {
			t.Errorf("%s: proposed without error", name)
		}
	}
}

func TestAProposalDoesntChangeTheCallersSpec(t *testing.T) {
	storetest.Each(t, testAProposalDoesntChangeTheCallersSpec)
}

func testAProposalDoesntChangeTheCallersSpec(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	ed := rule("srv_A", "http", 80, "203.0.113.7")
	before := proto.Clone(ed.Spec)
	propose(t, e, "Open http", ed)
	if !proto.Equal(before, ed.Spec) {
		t.Error("Propose normalized the caller's spec in place")
	}
}

func TestARuleThatBlocksSSHFromEverywhereIsFlagged(t *testing.T) {
	storetest.Each(t, testARuleThatBlocksSSHFromEverywhereIsFlagged)
}

func testARuleThatBlocksSSHFromEverywhereIsFlagged(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	block := Edit{
		Ref:  resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: "no-ssh"},
		Spec: &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY},
	}
	p := propose(t, e, "Close SSH", block)
	if w := p.Steps()[0].Warnings; len(w) != 1 || w[0] != resource.WarningMayLockOut {
		t.Errorf("warnings: %v", w)
	}
}

// hooked wraps an agent to watch or disturb each step it applies.
type hooked struct {
	resource.Agent
	before  func(resource.Step)
	applied *[]string
}

func (h hooked) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if h.before != nil {
		h.before(step)
	}
	if err := ctx.Err(); err != nil {
		return err // an agent that honours cancellation
	}
	if h.applied != nil {
		*h.applied = append(*h.applied, step.Action.String()+" "+step.Ref.Name+" on "+step.Ref.Scope)
	}
	return h.Agent.Apply(ctx, step, desired)
}

func TestBuildsRunOnEveryServerBeforeAnyDelete(t *testing.T) {
	storetest.Each(t, testBuildsRunOnEveryServerBeforeAnyDelete)
}

func testBuildsRunOnEveryServerBeforeAnyDelete(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A", "srv_B")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	var applied []string
	for _, id := range []string{"srv_A", "srv_B"} {
		f[id].agent = hooked{Agent: f[id].agent, applied: &applied}
	}
	moved := web("nginx:1.27")
	moved.Ref.Scope = "srv_B"
	apply(t, e, propose(t, e, "Move web to srv_B", Edit{Ref: web("").Ref}, moved))
	if len(applied) != 2 || applied[0] != "create web on srv_B" || applied[1] != "delete web on srv_A" {
		t.Errorf("order: %v; the new web must run before the old one goes", applied)
	}
}

func TestACancelledApplyStillFinishes(t *testing.T) {
	storetest.Each(t, testACancelledApplyStillFinishes)
}

func testACancelledApplyStillFinishes(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { cancel() }}
	p := propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80))
	if err := e.Apply(ctx, p.Change); err != nil {
		t.Fatalf("apply after the caller left = %v", err)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		if c, _ := tx.GetChange(t.Context(), p.Change); c.State != store.ChangeApplied {
			t.Errorf("state %s, want applied", c.State)
		}
		return nil
	})
	if f["srv_A"].firewall.Len() != 1 {
		t.Error("the second step didn't run")
	}
}

func TestAServerThatDropsMidApplyIsRolledBackWhenItReturns(t *testing.T) {
	storetest.Each(t, testAServerThatDropsMidApplyIsRolledBackWhenItReturns)
}

func testAServerThatDropsMidApplyIsRolledBackWhenItReturns(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A", "srv_B")
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { f["srv_B"].online = false }}
	onB := web("nginx:1.27")
	onB.Ref.Scope = "srv_B"
	p := propose(t, e, "Open http and run web", rule("srv_A", "http", 80), onB)

	err := e.Apply(t.Context(), p.Change)
	var failed *FailedError
	if !errors.As(err, &failed) || !errors.Is(err, ErrOffline) || !errors.Is(failed.RollbackErr, ErrPending) {
		t.Fatalf("apply = %v, want a failure for srv_B with the rollback pending", err)
	}
	if f["srv_A"].firewall.Len() != 0 {
		t.Error("srv_A's rule wasn't rolled back")
	}
	// Desired state is already back, before srv_B returns.
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		r, _ := tx.GetResource(t.Context(), string(container.Kind), "srv_B", "web")
		if r.Spec != nil || r.ChangeID != failed.Rollback {
			t.Errorf("web on srv_B: spec %v by %s, want deleted by the rollback", r.Spec, r.ChangeID)
		}
		c, _ := tx.GetChange(t.Context(), failed.Rollback)
		if c.State != store.ChangeApplying {
			t.Errorf("rollback is %s, want applying until srv_B returns", c.State)
		}
		return nil
	})
	if err := e.Resume(t.Context()); err != nil {
		t.Fatalf("resume while srv_B is away = %v", err)
	}

	f["srv_B"].online = true
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		if c, _ := tx.GetChange(t.Context(), failed.Rollback); c.State != store.ChangeApplied {
			t.Errorf("rollback is %s after srv_B returned, want applied", c.State)
		}
		return nil
	})
	if c, _ := f["srv_B"].docker.Inspect(t.Context(), "web"); c != nil {
		t.Error("web runs on srv_B although its change was rolled back")
	}
}

func TestWhatAServerReportsIsNormalized(t *testing.T) {
	storetest.Each(t, testWhatAServerReportsIsNormalized)
}

func testWhatAServerReportsIsNormalized(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	spec := web("nginx:1.27")
	spec.Spec.(*resourcev1.Container).Ports = []*resourcev1.PortMapping{{HostPort: 443, ContainerPort: 8443}, {HostPort: 80, ContainerPort: 8080}}
	apply(t, e, propose(t, e, "Run web", spec))
	// Docker reports ports in its own order and leaves out defaults.
	f["srv_A"].docker.Edit("web", func(c *resourcev1.Container) {
		c.Ports[0], c.Ports[1] = c.Ports[1], c.Ports[0]
		c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED
		c.Ports[0].Protocol = resourcev1.Protocol_PROTOCOL_UNSPECIFIED
	})
	if d, err := e.Drift(t.Context(), "srv_A"); err != nil || len(d) != 0 {
		t.Errorf("drift from the server's own order and defaults: %+v, %v", d, err)
	}
	if p := propose(t, e, "Nothing", spec); !p.Empty() {
		t.Errorf("planning the same spec again: %+v", p)
	}
}

func TestTheUndoWindowIsWordedExactly(t *testing.T) {
	storetest.Each(t, testTheUndoWindowIsWordedExactly)
}

func testTheUndoWindowIsWordedExactly(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	p := propose(t, e, "Run web", web("nginx:1.27"))
	apply(t, e, p)
	e.now = func() time.Time { return time.Now().Add(UndoWindow + time.Hour + time.Minute) }
	_, err := e.Undo(t.Context(), p.Change, By{Actor: "usr_test"})
	if err == nil || !strings.Contains(err.Error(), "applied 7 days and 1 hour ago, and changes can be undone for 7 days") {
		t.Errorf("message: %v", err)
	}
}

func TestBlockingSSHForEveryAddressIsFlaggedToo(t *testing.T) {
	storetest.Each(t, testBlockingSSHForEveryAddressIsFlaggedToo)
}

func testBlockingSSHForEveryAddressIsFlaggedToo(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	block := Edit{
		Ref:  resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: "no-ssh"},
		Spec: &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY, Sources: []string{"0.0.0.0/0", "::/0"}},
	}
	if w := propose(t, e, "Close SSH", block).Steps()[0].Warnings; len(w) != 1 {
		t.Errorf("warnings: %v", w)
	}
}
