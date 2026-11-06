// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const recreate = 2 * time.Second

// server is a fake machine: Docker and a firewall behind its agent.
type server struct {
	docker   *containertest.Fake
	firewall *firewall.Fake
	agent    resource.Agent
	online   atomic.Bool
}

type fleet map[string]*server

func (f fleet) Agent(id string) (resource.Agent, bool) {
	s, ok := f[id]
	if !ok {
		return nil, false
	}
	return s.agent, s.online.Load()
}

func setup(t *testing.T, b store.Backend, ids ...string) (*Engine, fleet, *store.DB) {
	t.Helper()
	e, f, db, _ := setupAt(t, b, ids...)
	return e, f, db
}

// setupAt is setup that also says where the store is, for storetest.
func setupAt(t *testing.T, b store.Backend, ids ...string) (*Engine, fleet, *store.DB, string) {
	t.Helper()
	db, location := storetest.Open(t, b)
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
			s := &server{docker: containertest.NewFake(), firewall: firewall.NewFake()}
			s.online.Store(true)
			s.agent = resource.Local{Drivers: resource.NewRegistry(
				&container.Driver{Runtime: s.docker, RecreateDowntime: recreate},
				&firewall.Driver{Backend: s.firewall},
				&service.Driver{Containers: container.Driver{Runtime: s.docker, RecreateDowntime: recreate}},
			), Server: id, Values: secretstest.Keeper(db, testKinds())}
			f[id] = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return newEngine(db, f), f, db, location
}

// newEngine is an engine over db and the fleet f, as a control plane that
// starts again makes one.
func newEngine(db *store.DB, f fleet) *Engine { return newEngineWith(db, f) }

// newEngineWith is newEngine reaching the servers through agents.
func newEngineWith(db *store.DB, agents Agents) *Engine {
	kinds := testKinds()
	return New(db, kinds, agents, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler))
}

// testKinds are the kinds the control plane knows. Its drivers only read
// specs; they never reach a server.
func testKinds() *resource.Registry {
	return resource.NewRegistry(&container.Driver{RecreateDowntime: recreate}, &firewall.Driver{}, &service.Driver{},
		variable.Type{}, variable.SecretType{})
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
	c, err := s.docker.Inspect(context.Background(), container.Target{Name: "web"})
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
	if c, _ := f["srv_A"].docker.Inspect(t.Context(), container.Target{Name: "web"}); c != nil {
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
	f["srv_B"].online.Store(false)

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

// plannedBefore records a planned change as an earlier version did: its
// fingerprint hashes specs as they are, its items aren't sealed, and
// nothing rotates. It returns the change, as the store keeps it, with its
// plan and items.
func plannedBefore(t *testing.T, e *Engine, db *store.DB, summary string, edits ...Edit) (store.Change, *Plan, []item) {
	t.Helper()
	items, err := e.items(t.Context(), edits, false)
	if err != nil {
		t.Fatal(err)
	}
	changeID := "chg_before_" + strings.ReplaceAll(strings.ToLower(summary), " ", "_")
	plan, err := e.plan(t.Context(), changeID, items, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.plain == plan.Fingerprint {
		t.Fatal("the plain fingerprint is the keyed one, so the test can't tell which applying checks")
	}
	c := store.Change{
		ID: changeID, Summary: summary, Actor: "usr_test", State: store.ChangePlanned, Fingerprint: plan.plain,
		FingerprintScheme: fingerprintPlain, CreatedAt: time.Now(), ProposedVia: ViaPanel, Servers: serversOf(plan),
	}
	plan.Change, plan.Summary = c.ID, summary
	if c.Plan, err = json.Marshal(plan); err != nil {
		t.Fatal(err)
	}
	for i := range items {
		it := &items[i]
		c.Items = append(c.Items, store.ChangeItem{
			ResourceID: it.id, Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, BaseRevision: it.base, Spec: it.encoded, Servers: it.servers,
		})
	}
	if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error { return tx.CreateChange(t.Context(), c) }); err != nil {
		t.Fatal(err)
	}
	return c, plan, items
}

// leftApplying records a change as an earlier version left it when it
// stopped partway: planned as plannedBefore has it, its revisions committed
// as they were then, in the clear, and its steps still to run.
func leftApplying(t *testing.T, e *Engine, db *store.DB, summary string, edits ...Edit) string {
	t.Helper()
	c, plan, items := plannedBefore(t, e, db, summary, edits...)
	record, err := json.Marshal(newProgress(plan, items))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error {
		for i := range items {
			it := &items[i]
			rev, err := tx.AddRevision(t.Context(), store.NewRevision{
				Kind: string(it.ref.Kind), Scope: it.ref.Scope, Name: it.ref.Name, ID: it.id, Spec: it.encoded, Change: c.ID, Position: i, At: now,
			})
			if err != nil {
				return err
			}
			if err := tx.PlaceRevision(t.Context(), rev.ID, rev.Revision, it.servers, now); err != nil {
				return err
			}
		}
		return tx.StartApplying(t.Context(), c.ID, c.Actor, c.ProposedVia, record, now)
	}); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// A change planned before ADR 0022 has a fingerprint that hashed every
// spec as it is, and applying it now digests some specs with a key. It
// still applies while its plan is the one reviewed, read as it was
// fingerprinted then, and runs its steps on the servers; one whose plan
// moved says it was planned by an earlier version, rather than that the
// servers changed. One that asks for a container kept its environment
// unsealed, which is no longer read: it says so, and to propose it again.
func TestAChangePlannedBeforeTheUpgradeStillApplies(t *testing.T) {
	storetest.Each(t, testAChangePlannedBeforeTheUpgradeStillApplies)
}

func testAChangePlannedBeforeTheUpgradeStillApplies(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))

	held, plan, _ := plannedBefore(t, e, db, "Open http", rule("srv_A", "http", 80))
	if len(plan.Steps()) != 1 || plan.Steps()[0].Action != resource.ActionCreate {
		t.Fatalf("the change plans %+v, want a step on its server", plan.Steps())
	}
	if err := e.Apply(t.Context(), held.ID); err != nil {
		t.Errorf("applying a change planned before the upgrade, whose plan holds = %v, want it applied", err)
	}
	if c := stored(t, db, held.ID); c.State != store.ChangeApplied || c.FingerprintScheme != fingerprintPlain {
		t.Errorf("after applying, the change is %s under scheme %d, want applied under %d", c.State, c.FingerprintScheme, fingerprintPlain)
	}
	if r, err := f["srv_A"].firewall.Rule(t.Context(), "http"); err != nil || r == nil {
		t.Errorf("the step of a change planned before the upgrade didn't run: %v, %v", r, err)
	}

	moved, _, _ := plannedBefore(t, e, db, "Open https", rule("srv_A", "https", 443))
	if err := f["srv_A"].firewall.Put(t.Context(), "https", rule("srv_A", "https", 443).Spec.(*resourcev1.FirewallRule)); err != nil {
		t.Fatal(err)
	}
	err := e.Apply(t.Context(), moved.ID)
	if !errors.Is(err, ErrPlanChanged) || !strings.Contains(err.Error(), "was planned by an earlier version of Dockwell") ||
		strings.Contains(err.Error(), "the servers changed since") {
		t.Errorf("applying a change planned before the upgrade, whose plan moved = %v, want ErrPlanChanged saying it was planned before", err)
	}

	unsealed, _, _ := plannedBefore(t, e, db, "Upgrade web", web("nginx:1.28"))
	err = e.Apply(t.Context(), unsealed.ID)
	if !errors.Is(err, ErrPlanChanged) || !strings.Contains(err.Error(), "kept what it asks of container web in srv_A unencrypted") ||
		!strings.Contains(err.Error(), "Propose the change again") {
		t.Errorf("applying a change planned before the upgrade, with a container's environment in the clear = %v, want it refused", err)
	}
	if image(t, f["srv_A"]) != "nginx:1.27" {
		t.Error("a change whose spec was stored in the clear ran")
	}
}

// A change an earlier version left applying finishes after the upgrade,
// under the scheme it was planned with: the next start runs the steps that
// remain, and those the server already has need nothing. One holding a
// spec of a kind that may hold secrets, in the clear, is refused and stays
// on record, says why, and holds up no other.
func TestAChangeLeftApplyingFinishesAfterTheUpgrade(t *testing.T) {
	storetest.Each(t, testAChangeLeftApplyingFinishesAfterTheUpgrade)
}

func testAChangeLeftApplyingFinishesAfterTheUpgrade(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	id := leftApplying(t, e, db, "Open the web", rule("srv_A", "http", 80), rule("srv_A", "https", 443))
	// The first step ran before it stopped.
	if err := f["srv_A"].firewall.Put(t.Context(), "http", rule("srv_A", "http", 80).Spec.(*resourcev1.FirewallRule)); err != nil {
		t.Fatal(err)
	}
	upgraded := newEngine(db, f)
	if err := upgraded.Resume(t.Context()); err != nil {
		t.Fatalf("Resume after the upgrade = %v", err)
	}
	upgraded.Wait()
	if c := stored(t, db, id); c.State != store.ChangeApplied || c.FingerprintScheme != fingerprintPlain {
		t.Errorf("after resuming, the change is %s under scheme %d, want applied under %d", c.State, c.FingerprintScheme, fingerprintPlain)
	}
	for _, name := range []string{"http", "https"} {
		if r, err := f["srv_A"].firewall.Rule(t.Context(), name); err != nil || r == nil {
			t.Errorf("after resuming, %s isn't on the server: %v, %v", name, r, err)
		}
	}

	stuck := leftApplying(t, e, db, "Run web", web("nginx:1.27"))
	other := leftApplying(t, e, db, "Open ssh", rule("srv_A", "ssh", 22))
	upgraded = newEngine(db, f)
	err := upgraded.Resume(t.Context())
	if !errors.Is(err, secrets.ErrUnsealed) {
		t.Errorf("Resume with a container stored in the clear = %v, want it to say so", err)
	}
	upgraded.Wait()
	if c := stored(t, db, stuck); c.State != store.ChangeApplying {
		t.Errorf("the change holding a spec in the clear is %s, want it left applying", c.State)
	}
	if c := stored(t, db, other); c.State != store.ChangeApplied {
		t.Errorf("the change beside it is %s, want applied", c.State)
	}
	if got, _ := f["srv_A"].docker.Inspect(t.Context(), container.HostTarget(resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"})); got != nil {
		t.Errorf("a spec stored in the clear ran: %v", got)
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
	if err := hz1.docker.Healthy(t.Context(), container.Target{Name: "web"}); err != nil {
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

// The window holds as an undo applies, as it does when it's proposed. A rollback a
// person asks for names the change it reverts as an undo does, and has no time limit.
func TestAPlannedUndoHasAWindowToo(t *testing.T) { storetest.Each(t, testAPlannedUndoHasAWindowToo) }

func testAPlannedUndoHasAWindowToo(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A")
	by := By{Actor: "usr_test"}
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	next := propose(t, e, "Move on", web("nginx:1.28"))
	apply(t, e, next)
	undo, err := e.Undo(t.Context(), next.Change, by)
	if err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return time.Now().Add(UndoWindow + time.Hour) }
	if err := e.Apply(t.Context(), undo.Change); !errors.Is(err, ErrCantUndo) || !strings.Contains(err.Error(), "changes can be undone for 7 days") {
		t.Errorf("applying an undo past the window = %v, want ErrCantUndo", err)
	}

	var id string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(container.Kind), "srv_A", "web")
		id = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	back, err := e.RollBackTo(t.Context(), id, 1, func(store.Resource, proto.Message) (string, []Note) { return "Roll web back", nil }, by)
	if err != nil {
		t.Fatal(err)
	}
	if back.Reverts != next.Change {
		t.Fatalf("the rollback reverts %q, want %s, so this proves nothing", back.Reverts, next.Change)
	}
	if err := e.Apply(t.Context(), back.Change); err != nil {
		t.Errorf("applying a rollback past the undo window = %v, want it applied", err)
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

// hooked wraps an agent to watch or disturb each step it applies, and
// each plan it makes.
type hooked struct {
	resource.Agent
	before  func(resource.Step)
	applied *[]string
	// apply runs before each step, and plan before each plan; an error
	// stands for the agent's.
	apply func(ctx context.Context, step resource.Step, desired proto.Message) error
	plan  func(ctx context.Context, desired []resource.Desired) error
}

func (h hooked) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	if h.plan != nil {
		if err := h.plan(ctx, desired); err != nil {
			return nil, err
		}
	}
	return h.Agent.Plan(ctx, desired)
}

func (h hooked) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if h.before != nil {
		h.before(step)
	}
	if h.apply != nil {
		if err := h.apply(ctx, step, desired); err != nil {
			return err
		}
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
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { f["srv_B"].online.Store(false) }}
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

	f["srv_B"].online.Store(true)
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		if c, _ := tx.GetChange(t.Context(), failed.Rollback); c.State != store.ChangeApplied {
			t.Errorf("rollback is %s after srv_B returned, want applied", c.State)
		}
		return nil
	})
	if c, _ := f["srv_B"].docker.Inspect(t.Context(), container.Target{Name: "web"}); c != nil {
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
