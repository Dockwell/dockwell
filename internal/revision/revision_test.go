// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/store"
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

func setup(t *testing.T, ids ...string) (*Engine, fleet, *store.DB) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
			)}
			f[id] = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The control plane's drivers only read specs; they never reach a server.
	kinds := resource.NewRegistry(&container.Driver{RecreateDowntime: recreate}, &firewall.Driver{})
	return New(db, kinds, f, slog.New(slog.DiscardHandler)), f, db
}

func web(image string) Edit {
	return Edit{
		Ref: resource.Ref{Kind: container.Kind, Server: "srv_A", Name: "web"},
		Spec: &resourcev1.Container{
			Image: image,
			Env:   map[string]string{"API_KEY": "s3cret-value"},
			Ports: []*resourcev1.PortMapping{{HostPort: 80, ContainerPort: 8080}},
		},
	}
}

func rule(server, name string, port uint32, sources ...string) Edit {
	return Edit{
		Ref:  resource.Ref{Kind: firewall.Kind, Server: server, Name: name},
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
func TestPlanApplyUndoAcrossKinds(t *testing.T) {
	e, f, db := setup(t, "srv_A")
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

	undo, err := e.Undo(t.Context(), second.Change, "usr_test")
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
	if _, err := e.Undo(t.Context(), first.Change, "usr_test"); !errors.Is(err, ErrCantUndo) {
		t.Errorf("undoing a change something later changed again = %v, want ErrCantUndo", err)
	}
}

func TestDeletesRunLastAndUndoBringsThemBack(t *testing.T) {
	e, f, _ := setup(t, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80)))

	remove := propose(t, e, "Retire web",
		Edit{Ref: resource.Ref{Kind: firewall.Kind, Server: "srv_A", Name: "http"}},
		Edit{Ref: resource.Ref{Kind: container.Kind, Server: "srv_A", Name: "web"}},
		rule("srv_A", "maintenance", 8080))
	steps := remove.Steps()
	if steps[0].Ref.Name != "maintenance" || steps[1].Ref.Name != "web" || steps[2].Ref.Name != "http" {
		t.Errorf("order: creates first, then deletes in reverse: %v, %v, %v", steps[0].Ref, steps[1].Ref, steps[2].Ref)
	}
	apply(t, e, remove)
	if c, _ := f["srv_A"].docker.Inspect(t.Context(), "web"); c != nil {
		t.Fatal("web is still running")
	}

	undo, err := e.Undo(t.Context(), remove.Change, "usr_test")
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

func TestSecretsStayOutOfPlans(t *testing.T) {
	e, _, db := setup(t, "srv_A")
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
	e, f, _ := setup(t, "srv_A", "srv_B")
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

func TestApplyRefusesAStalePlan(t *testing.T) {
	e, _, _ := setup(t, "srv_A")
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
	e, f, _ := setup(t, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	p := propose(t, e, "Upgrade web", web("nginx:1.28"))
	f["srv_A"].docker.Edit("web", func(c *resourcev1.Container) { c.Image = "nginx:1.26" })
	if err := e.Apply(t.Context(), p.Change); !errors.Is(err, ErrPlanChanged) {
		t.Errorf("applying after the server changed = %v, want ErrPlanChanged", err)
	}
}

func TestAFailedApplyRollsBack(t *testing.T) {
	e, f, db := setup(t, "srv_A")
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
	e, f, _ := setup(t, "srv_A")
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
	revert, err := e.Revert(t.Context(), drift[0], "usr_test")
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
	adopt, err := e.Adopt(t.Context(), drift[0], "usr_test")
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

func TestUndoHasAWindow(t *testing.T) {
	e, _, _ := setup(t, "srv_A")
	p := propose(t, e, "Run web", web("nginx:1.27"))
	apply(t, e, p)
	e.now = func() time.Time { return time.Now().Add(UndoWindow + time.Hour) }
	if _, err := e.Undo(t.Context(), p.Change, "usr_test"); !errors.Is(err, ErrCantUndo) {
		t.Errorf("undo after the window = %v, want ErrCantUndo", err)
	}
}

func TestProposalsAreChecked(t *testing.T) {
	e, _, _ := setup(t, "srv_A")
	cases := map[string][]Edit{
		"no image":          {{Ref: resource.Ref{Kind: container.Kind, Server: "srv_A", Name: "web"}, Spec: &resourcev1.Container{}}},
		"the wrong type":    {{Ref: resource.Ref{Kind: container.Kind, Server: "srv_A", Name: "web"}, Spec: &resourcev1.FirewallRule{Port: 80}}},
		"an unknown kind":   {{Ref: resource.Ref{Kind: "volume", Server: "srv_A", Name: "data"}, Spec: &resourcev1.FirewallRule{}}},
		"nothing to delete": {{Ref: resource.Ref{Kind: container.Kind, Server: "srv_A", Name: "web"}}},
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
	e, _, _ := setup(t, "srv_A")
	ed := rule("srv_A", "http", 80, "203.0.113.7")
	before := proto.Clone(ed.Spec)
	propose(t, e, "Open http", ed)
	if !proto.Equal(before, ed.Spec) {
		t.Error("Propose normalized the caller's spec in place")
	}
}

func TestARuleThatBlocksSSHFromEverywhereIsFlagged(t *testing.T) {
	e, _, _ := setup(t, "srv_A")
	block := Edit{
		Ref:  resource.Ref{Kind: firewall.Kind, Server: "srv_A", Name: "no-ssh"},
		Spec: &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY},
	}
	p := propose(t, e, "Close SSH", block)
	if w := p.Steps()[0].Warnings; len(w) != 1 || w[0] != resource.WarningMayLockOut {
		t.Errorf("warnings: %v", w)
	}
}
