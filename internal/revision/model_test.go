// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func tree(t *testing.T, db *store.DB) scope.Tree {
	t.Helper()
	var tr scope.Tree
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		tr, err = scope.Load(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// production is the hidden default environment.
func production(t *testing.T, db *store.DB) string {
	t.Helper()
	_, env, err := tree(t, db).Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	return env.ID
}

// app is a service named web in env.
func app(env, image string, servers ...string) Edit {
	return Edit{
		Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: "web"},
		Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image}}},
		Servers: servers,
	}
}

func environment(project, name string) Edit {
	return Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: name}, Spec: &resourcev1.Environment{}}
}

// deployments lists a resource's deployments as "server@revision=state",
// newest first.
func deployments(t *testing.T, db *store.DB, ref resource.Ref) []string {
	t.Helper()
	var out []string
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(ref.Kind), ref.Scope, ref.Name)
		if err != nil {
			t.Fatal(err)
		}
		deps, err := tx.Deployments(t.Context(), r.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := range deps {
			d := &deps[i]
			out = append(out, d.Server+"@"+string(rune('0'+d.Revision))+"="+d.State)
		}
		return nil
	})
	return out
}

func running(t *testing.T, s *server, ref resource.Ref) string {
	t.Helper()
	c, err := s.docker.Inspect(t.Context(), service.Target(ref))
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		return ""
	}
	return c.GetImage()
}

func TestSetupMakesTheHiddenDefaultsOnce(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		for i, want := range []bool{true, false} {
			made, err := Setup(ctx, db, slog.New(slog.DiscardHandler))
			if err != nil || made != want {
				t.Fatalf("Setup #%d = %t, %v; want %t", i+1, made, err, want)
			}
		}
		tr := tree(t, db)
		if tr.Organization.Name != scope.DefaultOrganization || len(tr.Projects) != 1 || tr.Projects[0].Name != scope.DefaultProject ||
			len(tr.Projects[0].Environments) != 1 || tr.Projects[0].Environments[0].Name != scope.DefaultEnvironment {
			t.Fatalf("after setup: %+v", tr)
		}
		p, env := tr.Projects[0], tr.Projects[0].Environments[0]
		if !strings.HasPrefix(tr.Organization.ID, "org_") || !strings.HasPrefix(p.ID, "prj_") || !strings.HasPrefix(env.ID, "env_") {
			t.Errorf("IDs should say what they name: %s, %s, %s", tr.Organization.ID, p.ID, env.ID)
		}
		if s := tr.Shown(""); s != (scope.Shown{}) {
			t.Errorf("with one of each, nothing shows: %+v", s)
		}
		// The defaults are revisions of a change like any other.
		_ = db.View(ctx, func(tx *store.Tx) error {
			r, err := tx.GetResourceByID(ctx, env.ID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := tx.GetChange(ctx, r.ChangeID)
			if err != nil || c.State != store.ChangeApplied || c.Actor != SystemActor || len(c.Items) != 2 || c.Fingerprint == "" {
				t.Errorf("the setup change = %+v, %v", c, err)
			}
			return nil
		})
	})
}

func TestAServiceRunsOnTheOnlyServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A")
		env := production(t, db)
		web := app(env, "nginx:1.27")
		p := propose(t, e, "Run web", web)
		if len(p.Servers) != 1 || p.Servers[0].Server != "srv_A" || p.Steps()[0].Action != resource.ActionCreate {
			t.Fatalf("plan: %+v", p)
		}
		if c := change(t, p.Steps(), "app.image"); c.To != "nginx:1.27" {
			t.Errorf("image change: %+v", c)
		}
		apply(t, e, p)
		if got := running(t, f["srv_A"], web.Ref); got != "nginx:1.27" {
			t.Fatalf("on srv_A: %q", got)
		}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_A@1=live"}) {
			t.Errorf("deployments: %v", d)
		}

		// Editing keeps it where it is, and the new revision replaces the
		// old one there.
		apply(t, e, propose(t, e, "Upgrade web", app(env, "nginx:1.28")))
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_A@2=live", "srv_A@1=superseded"}) {
			t.Errorf("deployments after an upgrade: %v", d)
		}
		if drift, err := e.Drift(t.Context(), "srv_A"); err != nil || len(drift) != 0 {
			t.Errorf("drift: %+v, %v", drift, err)
		}
	})
}

func TestAServiceSaysWhereItRunsOnceThereIsAChoice(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		env := production(t, db)
		_, err := e.Propose(t.Context(), Proposal{Summary: "Run web", Actor: "usr_test", Edits: []Edit{app(env, "nginx:1.27")}})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "there are 2 servers, srv_A and srv_B, so say which web runs on") {
			t.Fatalf("a new service with two servers and no choice = %v", err)
		}
		web := app(env, "nginx:1.27", "srv_B", "srv_A", "srv_B")
		apply(t, e, propose(t, e, "Run web on both", web))
		for _, s := range []string{"srv_A", "srv_B"} {
			if got := running(t, f[s], web.Ref); got != "nginx:1.27" {
				t.Errorf("on %s: %q", s, got)
			}
		}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_A@1=live", "srv_B@1=live"}) {
			t.Errorf("deployments: %v", d)
		}
		// Once it runs somewhere, an edit that doesn't say keeps it there.
		p := propose(t, e, "Upgrade web", app(env, "nginx:1.28"))
		if len(p.Servers) != 2 {
			t.Errorf("an upgrade without servers should stay on both: %+v", p.Servers)
		}
		for _, bad := range [][]string{{}, {"srv_nope"}} {
			if _, err := e.Propose(t.Context(), Proposal{Summary: "x", Actor: "usr_test", Edits: []Edit{app(env, "nginx:1.28", bad...)}}); !errors.Is(err, ErrInvalid) {
				t.Errorf("placing on %v = %v, want ErrInvalid", bad, err)
			}
		}
	})
}

func TestMovingAServiceIsARevision(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		env := production(t, db)
		web := app(env, "nginx:1.27", "srv_A")
		apply(t, e, propose(t, e, "Run web", web))

		var applied []string
		for _, id := range []string{"srv_A", "srv_B"} {
			f[id].agent = hooked{Agent: f[id].agent, applied: &applied}
		}
		move := propose(t, e, "Move web", app(env, "nginx:1.27", "srv_B"))
		if move.Revisions != 1 {
			t.Errorf("a move with the same spec is still a revision: %+v", move)
		}
		apply(t, e, move)
		if len(applied) != 2 || !strings.HasPrefix(applied[0], "create web") || !strings.HasPrefix(applied[1], "delete web") {
			t.Errorf("order: %v; the new copy must run before the old one goes", applied)
		}
		if running(t, f["srv_A"], web.Ref) != "" || running(t, f["srv_B"], web.Ref) != "nginx:1.27" {
			t.Error("web didn't move to srv_B")
		}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_B@2=live", "srv_A@1=superseded"}) {
			t.Errorf("deployments after the move: %v", d)
		}

		undo, err := e.Undo(t.Context(), move.Change, By{Actor: "usr_test"})
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, undo)
		if running(t, f["srv_A"], web.Ref) != "nginx:1.27" || running(t, f["srv_B"], web.Ref) != "" {
			t.Error("undo didn't move web back to srv_A")
		}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_A@3=live", "srv_B@2=superseded", "srv_A@1=superseded"}) {
			t.Errorf("deployments after undo: %v", d)
		}
	})
}

func TestAFailedDeploymentSaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A")
		env := production(t, db)
		web := app(env, "nginx:1.27")
		apply(t, e, propose(t, e, "Run web", web))
		f["srv_A"].docker.Broken["nginx:broken"] = true
		err := e.Apply(t.Context(), propose(t, e, "Break web", app(env, "nginx:broken")).Change)
		var failed *FailedError
		if !errors.As(err, &failed) || failed.Rollback == "" || failed.RollbackErr != nil {
			t.Fatalf("apply = %v, want a failure that rolled back", err)
		}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_A@3=live", "srv_A@2=failed", "srv_A@1=superseded"}) {
			t.Errorf("deployments: %v", d)
		}
		if running(t, f["srv_A"], web.Ref) != "nginx:1.27" {
			t.Error("the rollback didn't bring nginx:1.27 back")
		}
	})
}

func TestServicesOfOneNameInTwoEnvironmentsShareAServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A")
		prod := production(t, db)
		project := tree(t, db).Projects[0].ID
		apply(t, e, propose(t, e, "Add staging", environment(project, "staging")))
		_, staging, err := tree(t, db).Resolve("", "staging")
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Run web twice", app(prod, "nginx:1.27"), app(staging.ID, "nginx:1.28")))
		if running(t, f["srv_A"], app(prod, "").Ref) != "nginx:1.27" || running(t, f["srv_A"], app(staging.ID, "").Ref) != "nginx:1.28" {
			t.Error("each environment's web should run, side by side")
		}
	})
}

// Projects and environments are kinds too: making one is a change with a
// plan, and undo takes it away. The hidden defaults show only while there
// is a choice.
func TestAnEnvironmentIsAChangeAndShowsOnlyOnceThereIsAChoice(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		project := tree(t, db).Projects[0].ID
		p := propose(t, e, "Add staging", environment(project, "staging"))
		if p.Revisions != 1 || len(p.Servers) != 0 {
			t.Errorf("an environment is a revision with nothing to do on servers: %+v", p)
		}
		apply(t, e, p)
		tr := tree(t, db)
		if s := tr.Shown(""); s.Project || !s.Environment || s.Organization {
			t.Errorf("with two environments, only the environment shows: %+v", s)
		}
		var choose *scope.ChoiceError
		if _, _, err := tr.Resolve("", ""); !errors.As(err, &choose) || choose.Level != scope.EnvironmentKind ||
			!slices.Equal(choose.Options, []string{"production", "staging"}) {
			t.Errorf("Resolve without an environment = %v, want a choice of production or staging", err)
		}
		if _, env, err := tr.Resolve("default", "staging"); err != nil || env.Name != "staging" {
			t.Errorf("Resolve(default, staging) = %+v, %v", env, err)
		}

		undo, err := e.Undo(t.Context(), p.Change, By{Actor: "usr_test"})
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, undo)
		if s := tree(t, db).Shown(""); s.Environment {
			t.Error("with one environment again, it hides again")
		}
	})
}

func TestScopesGoOnlyOnceEmptyAndNeverTheLast(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		tr := tree(t, db)
		org, project, prod := tr.Organization.ID, tr.Projects[0].ID, tr.Projects[0].Environments[0].ID
		refuse := func(why string, want string, edits ...Edit) {
			t.Helper()
			refuseProposal(t, e, why, ErrInvalid, want, edits...)
		}
		refuse("deleting the only environment", "production is the last environment in project default",
			del(scope.EnvironmentKind, project, "production"))
		refuse("deleting the only project", "default is the last project in the organization",
			del(scope.ProjectKind, org, "default"))
		refuse("deleting the only project and its environment", "default is the last project in the organization",
			del(scope.ProjectKind, org, "default"), del(scope.EnvironmentKind, project, "production"))
		refuse("a bad name", "isn't a valid environment name", environment(project, "Staging!"))
		refuse("a service with nothing to run", "a service needs something to run",
			Edit{Ref: resource.Ref{Kind: service.Kind, Scope: prod, Name: "web"}, Spec: &resourcev1.Service{}})
		refuse("a service nowhere", "there's no environment env_nope", app("env_nope", "nginx:1"))
		refuse("a service in a project", "there's no environment "+project, app(project, "nginx:1"))
		refuse("an environment in the organization", "there's no project "+org, environment(org, "staging"))

		apply(t, e, propose(t, e, "Add staging with web", environment(project, "staging")))
		_, staging, _ := tree(t, db).Resolve("", "staging")
		apply(t, e, propose(t, e, "Run web in staging", app(staging.ID, "nginx:1.27")))
		refuse("deleting an environment that holds a service", "still holds web", del(scope.EnvironmentKind, project, "staging"))
		// Its contents can go in the same change.
		apply(t, e, propose(t, e, "Retire staging", del(service.Kind, staging.ID, "web"), del(scope.EnvironmentKind, project, "staging")))
		// And with staging back, production can go.
		apply(t, e, propose(t, e, "Add staging again", environment(project, "staging")))
		apply(t, e, propose(t, e, "Retire production", del(scope.EnvironmentKind, project, "production")))
		if _, env, err := tree(t, db).Resolve("", ""); err != nil || env.Name != "staging" {
			t.Errorf("with staging the only one, it is what an empty environment means: %+v, %v", env, err)
		}
	})
}

// A name that would only fail on the server is refused when it is
// proposed, and a container run by hand can't take a service's.
func TestNamesThatWouldFailOnTheServerAreRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		env := production(t, db)
		named := func(name string) Edit {
			ed := app(env, "nginx:1.27")
			ed.Ref.Name = name
			return ed
		}
		hijack := Edit{
			Ref:  resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: service.ContainerName(app(env, "").Ref)},
			Spec: &resourcev1.Container{Image: "nginx:1.27"},
		}
		for _, c := range []struct {
			edit Edit
			want string
		}{
			{named("My App"), `"My App" isn't a valid service name`},
			{named("api/v2"), `"api/v2" isn't a valid service name`},
			{hijack, "has the form of a service's container"},
		} {
			_, err := e.Propose(t.Context(), Proposal{Summary: "x", Actor: "usr_test", Edits: []Edit{c.edit}})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("proposing %s = %v, want ErrInvalid saying %q", c.edit.Ref, err, c.want)
			}
		}
	})
}

// A change planned before its environment went can't apply after.
func TestApplyRefusesAServiceWhoseEnvironmentWent(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		project := tree(t, db).Projects[0].ID
		apply(t, e, propose(t, e, "Add staging", environment(project, "staging")))
		_, staging, _ := tree(t, db).Resolve("", "staging")
		later := propose(t, e, "Run web in staging", app(staging.ID, "nginx:1.27"))
		apply(t, e, propose(t, e, "Retire staging", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}}))
		if err := e.Apply(t.Context(), later.Change); !errors.Is(err, ErrInvalid) {
			t.Errorf("applying into a deleted environment = %v, want ErrInvalid", err)
		}
	})
}

// Deleting a project is one edit: its environments go with it, in the plan and in an
// undo, as a project can't be left without one and an environment can't go alone.
func TestDeletingAProjectTakesItsEnvironments(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		deleted := func(p *Plan) []string {
			t.Helper()
			var out []string
			for i := range p.Resources {
				if r := &p.Resources[i]; r.Action != resource.ActionDelete {
					t.Errorf("%s plans %s, want it deleted", r.Ref, r.Action)
				} else {
					out = append(out, string(r.Ref.Kind)+" "+r.Ref.Name)
				}
			}
			slices.Sort(out)
			return out
		}

		apply(t, e, propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}))
		shop, _, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		retire := propose(t, e, "Retire shop", del(scope.ProjectKind, org, "shop"))
		if got, want := deleted(retire), []string{"environment production", "project shop"}; !slices.Equal(got, want) {
			t.Errorf("deleting a project with one environment plans %v, want %v", got, want)
		}
		apply(t, e, retire)
		if _, err := tree(t, db).Project("shop"); err == nil {
			t.Error("after deleting it, shop is still there")
		}
		if s := tree(t, db).Shown(""); s.Project {
			t.Error("with one project again, it hides again")
		}
		apply(t, e, undo(t, e, retire))
		back, prod, err := tree(t, db).Resolve("shop", "")
		if err != nil || back.ID != shop.ID {
			t.Fatalf("after undoing it, shop = %+v, %v; want it back with its environment", back, err)
		}

		// Every environment goes, not only the last, however the edit names them.
		apply(t, e, propose(t, e, "Add staging", environment(shop.ID, "staging")))
		both := propose(t, e, "Retire shop", del(scope.ProjectKind, org, "shop"))
		if got, want := deleted(both), []string{"environment production", "environment staging", "project shop"}; !slices.Equal(got, want) {
			t.Errorf("deleting a project with two environments plans %v, want %v", got, want)
		}
		if err := e.Discard(t.Context(), both.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}
		byName := propose(t, e, "Retire shop by name", del(scope.EnvironmentKind, shop.ID, "staging"), del(scope.ProjectKind, org, "shop"))
		if got, want := deleted(byName), []string{"environment production", "environment staging", "project shop"}; !slices.Equal(got, want) {
			t.Errorf("naming one of its environments as well plans %v, want %v", got, want)
		}
		if err := e.Discard(t.Context(), byName.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		// What it holds keeps it, and the refusal says what and where, as deleting
		// each takes its own command.
		_, staging, err := tree(t, db).Resolve("shop", "staging")
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Run web and api", named("web", prod.ID, "nginx:1.27"), named("api", prod.ID, "nginx:1.27"), named("db", staging.ID, "nginx:1.27")))
		refuseProposal(t, e, "deleting a project that runs something", ErrInvalid,
			"project shop still holds service api and service web in production and service db in staging, so it stays. Delete them first, with dockwell services delete api --project shop --environment production, dockwell services delete web --project shop --environment production and dockwell services delete db --project shop --environment staging, or in the same change",
			del(scope.ProjectKind, org, "shop"))
		// Deleting them in the same change is enough.
		apply(t, e, propose(t, e, "Stop web and api", del(service.Kind, prod.ID, "web"), del(service.Kind, prod.ID, "api")))
		apply(t, e, propose(t, e, "Retire shop with what's left", del(service.Kind, staging.ID, "db"), del(scope.ProjectKind, org, "shop")))
		if _, err := tree(t, db).Project("shop"); err == nil {
			t.Error("after deleting it with its last service, shop is still there")
		}
	})
}

// A plan to delete a project is checked again as it applies, so what arrived after
// it was planned isn't left with nowhere to live.
func TestDeletingAProjectThatFilledSincePlanningIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		apply(t, e, propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}))
		shop, prod, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		retire := propose(t, e, "Retire shop", del(scope.ProjectKind, org, "shop"))
		apply(t, e, propose(t, e, "Run web", app(prod.ID, "nginx:1.27")))
		if err := e.Apply(t.Context(), retire.Change); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "still holds service web in production") {
			t.Errorf("applying with a service arrived = %v, want a refusal naming it", err)
		}

		// So is an environment made since, which the plan never named.
		apply(t, e, propose(t, e, "Stop web", del(service.Kind, prod.ID, "web")))
		again := propose(t, e, "Retire shop again", del(scope.ProjectKind, org, "shop"))
		apply(t, e, propose(t, e, "Add staging", environment(shop.ID, "staging")))
		if err := e.Apply(t.Context(), again.Change); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "still holds environment staging") {
			t.Errorf("applying with an environment arrived = %v, want a refusal naming it", err)
		}
		if _, err := tree(t, db).Project("shop"); err != nil {
			t.Errorf("a refused delete took shop anyway: %v", err)
		}
	})
}

// named is a service named name in env.
func named(name, env, image string, servers ...string) Edit {
	ed := app(env, image, servers...)
	ed.Ref.Name = name
	return ed
}

func del(kind resource.Kind, scopeID, name string) Edit {
	return Edit{Ref: resource.Ref{Kind: kind, Scope: scopeID, Name: name}}
}

func refuseProposal(t *testing.T, e *Engine, why string, want error, says string, edits ...Edit) {
	t.Helper()
	_, err := e.Propose(t.Context(), Proposal{Summary: why, Actor: "usr_test", Edits: edits})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), says) {
		t.Errorf("%s = %v, want %v saying %q", why, err, want, says)
	}
}

// removedOnly reports a server plan for a removed server: no steps.
func removedOnly(sp ServerPlan, server string) bool {
	return sp.Server == server && sp.Removed && !sp.Offline && len(sp.Steps) == 0
}

func undo(t *testing.T, e *Engine, p *Plan) *Plan {
	t.Helper()
	u, err := e.Undo(t.Context(), p.Change, By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// remove takes a server out of the fleet, as revoking it does.
func remove(t *testing.T, db *store.DB, f fleet, server string) {
	t.Helper()
	if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.RevokeServer(t.Context(), server, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	f[server].online.Store(false)
}

// One change can't delete a scope and keep something in it, and it can
// bring a scope back with what lives in it.
func TestAChangeIsCheckedAsAWhole(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		project := tree(t, db).Projects[0].ID
		apply(t, e, propose(t, e, "Add staging", environment(project, "staging")))
		_, staging, _ := tree(t, db).Resolve("", "staging")

		refuseProposal(t, e, "deleting staging while making web in it", ErrInvalid,
			"this change deletes environment staging, so service web has nowhere to live. Keep staging, or leave web out",
			del(scope.EnvironmentKind, project, "staging"), app(staging.ID, "nginx:1.27"))
		apply(t, e, propose(t, e, "Run web in staging", app(staging.ID, "nginx:1.27")))
		refuseProposal(t, e, "deleting staging while upgrading web in it", ErrInvalid,
			"this change deletes environment staging, so service web has nowhere to live. Keep staging, or delete web too",
			app(staging.ID, "nginx:1.28"), del(scope.EnvironmentKind, project, "staging"))

		shop := propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: tree(t, db).Organization.ID, Name: "shop"}, Spec: &resourcev1.Project{}})
		apply(t, e, shop)
		p, prod, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		org := tree(t, db).Organization.ID
		refuseProposal(t, e, "deleting shop while making an environment in it", ErrInvalid, "this change deletes project shop, so environment staging has nowhere to live",
			del(scope.EnvironmentKind, p.ID, prod.Name), del(scope.ProjectKind, org, "shop"), environment(p.ID, "staging"))

		// Retiring shop deletes it and its environment; undoing that brings
		// both back in one change, the environment in the project it makes.
		retire := propose(t, e, "Retire shop", del(scope.EnvironmentKind, p.ID, prod.Name), del(scope.ProjectKind, org, "shop"))
		apply(t, e, retire)
		apply(t, e, undo(t, e, retire))
		if back, env, err := tree(t, db).Resolve("shop", ""); err != nil || back.ID != p.ID || env.ID != prod.ID {
			t.Errorf("after undoing the retirement, shop = %+v, %+v, %v; want it back with its environment", back, env, err)
		}
	})
}

// A new project starts with an environment, made in the same change, so no
// project is ever without one.
func TestANewProjectStartsWithAnEnvironment(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		shop := Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{DisplayName: "Shop"}}
		p := propose(t, e, "Add shop", shop)
		if p.Revisions != 2 || len(p.Servers) != 0 {
			t.Errorf("a new project is two revisions, the project and its environment, with nothing on servers: %+v", p)
		}
		apply(t, e, p)
		project, env, err := tree(t, db).Resolve("shop", "")
		if err != nil || env.Name != scope.DefaultEnvironment || env.Project != project.ID || project.DisplayName != "Shop" {
			t.Fatalf("shop = %+v, %+v, %v; want it with its production", project, env, err)
		}
		if s := tree(t, db).Shown("shop"); !s.Project || s.Environment {
			t.Errorf("with two projects of one environment each, only the project shows: %+v", s)
		}

		// Undo takes both away, and leaves the default as it was.
		apply(t, e, undo(t, e, p))
		if tr := tree(t, db); len(tr.Projects) != 1 || tr.Projects[0].Name != scope.DefaultProject {
			t.Errorf("after undo: %+v", tr.Projects)
		}
		// Made again, shop continues its history, and so does production.
		apply(t, e, propose(t, e, "Add shop again", shop))
		_, again, err := tree(t, db).Resolve("shop", "")
		if err != nil || again.ID != env.ID {
			t.Errorf("shop's production again = %+v, %v; want %s back", again, err, env.ID)
		}
		var rev int
		_ = db.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResourceByID(t.Context(), again.ID)
			rev = r.Revision
			return err
		})
		if rev != 3 {
			t.Errorf("production's revision = %d, want 3: made, deleted by the undo, made again", rev)
		}
	})
}

// The last environment, or the last project, can be replaced in the
// change that makes its successor.
func TestTheLastOneCanBeReplacedInOneChange(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		tr := tree(t, db)
		org, project := tr.Organization.ID, tr.Projects[0].ID
		apply(t, e, propose(t, e, "Replace production with live", environment(project, "live"), del(scope.EnvironmentKind, project, "production")))
		if _, env, err := tree(t, db).Resolve("", ""); err != nil || env.Name != "live" {
			t.Errorf("the only environment = %+v, %v; want live", env, err)
		}

		shop := Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}
		apply(t, e, propose(t, e, "Replace default with shop", shop, del(scope.EnvironmentKind, project, "live"), del(scope.ProjectKind, org, "default")))
		if p, env, err := tree(t, db).Resolve("", ""); err != nil || p.Name != "shop" || env.Name != scope.DefaultEnvironment {
			t.Errorf("the only project = %+v, %+v, %v; want shop with its production", p, env, err)
		}
		refuseProposal(t, e, "deleting the last project", ErrInvalid, "shop is the last project in the organization, which needs one. Make another first, or in the same change",
			del(scope.EnvironmentKind, tree(t, db).Projects[0].ID, "production"), del(scope.ProjectKind, org, "shop"))
	})
}

// A server that was removed takes no steps: a service can move off it, or
// be deleted from it, and the plan says it is gone. Nothing is placed on
// it, by an edit, an undo or a rollback.
func TestAServiceCanLeaveARemovedServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		env := production(t, db)
		web, api := app(env, "nginx:1.27", "srv_B"), named("api", env, "api:1", "srv_B")
		apply(t, e, propose(t, e, "Run web and api on srv_B", web, api))
		remove(t, db, f, "srv_B")

		refuseProposal(t, e, "placing a service on a removed server", ErrInvalid, "srv_B was removed from the fleet, so db can't run there",
			named("db", env, "postgres:18", "srv_B"))
		// An edit that doesn't say keeps a service on its servers still in
		// the fleet; with none, it goes on the only server.
		p := propose(t, e, "Upgrade web", app(env, "nginx:1.28"))
		if len(p.Servers) != 2 || p.Servers[0].Server != "srv_A" || p.Servers[0].Steps[0].Action != resource.ActionCreate ||
			!removedOnly(p.Servers[1], "srv_B") {
			t.Errorf("an edit of a service whose server was removed: %+v", p.Servers)
		}

		move := propose(t, e, "Move web to srv_A", app(env, "nginx:1.27", "srv_A"))
		apply(t, e, move)
		if running(t, f["srv_A"], web.Ref) != "nginx:1.27" {
			t.Error("web didn't start on srv_A")
		}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, []string{"srv_A@2=live", "srv_B@1=superseded"}) {
			t.Errorf("web's deployments after the move: %v", d)
		}
		if _, err := e.Undo(t.Context(), move.Change, By{Actor: "usr_test"}); !errors.Is(err, ErrCantUndo) ||
			!strings.Contains(err.Error(), "web ran on srv_B, which was removed from the fleet, so there's nowhere to put it back. Edit web directly instead") {
			t.Errorf("undoing the move = %v, want ErrCantUndo saying there's nowhere to put web back", err)
		}

		gone := propose(t, e, "Delete api", del(service.Kind, env, "api"))
		if len(gone.Servers) != 1 || !removedOnly(gone.Servers[0], "srv_B") || len(gone.Steps()) != 0 {
			t.Errorf("deleting a service from a removed server plans no step: %+v", gone.Servers)
		}
		apply(t, e, gone)
		if d := deployments(t, db, api.Ref); !slices.Equal(d, []string{"srv_B@1=superseded"}) {
			t.Errorf("api's deployments after the delete: %v", d)
		}
		if _, err := e.Undo(t.Context(), gone.Change, By{Actor: "usr_test"}); !errors.Is(err, ErrCantUndo) {
			t.Errorf("undoing api's deletion = %v, want ErrCantUndo", err)
		}
	})
}

// Undo puts a spec back on the servers still in the fleet.
func TestUndoSkipsARemovedServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		env := production(t, db)
		web := app(env, "nginx:1.27", "srv_A", "srv_B")
		apply(t, e, propose(t, e, "Run web on both", web))
		upgrade := propose(t, e, "Upgrade web", app(env, "nginx:1.28"))
		apply(t, e, upgrade)
		remove(t, db, f, "srv_B")

		u := undo(t, e, upgrade)
		if len(u.Servers) != 2 || u.Servers[0].Server != "srv_A" || !removedOnly(u.Servers[1], "srv_B") {
			t.Errorf("undo plan: %+v", u.Servers)
		}
		apply(t, e, u)
		if running(t, f["srv_A"], web.Ref) != "nginx:1.27" {
			t.Error("undo didn't put nginx:1.27 back on srv_A")
		}
		want := []string{"srv_A@3=live", "srv_A@2=superseded", "srv_B@2=superseded", "srv_A@1=superseded", "srv_B@1=superseded"}
		if d := deployments(t, db, web.Ref); !slices.Equal(d, want) {
			t.Errorf("deployments after undo: %v, want %v", d, want)
		}
	})
}

// A rollback waiting for a server finishes once that server is removed,
// rather than waiting for it forever.
func TestARollbackStopsWaitingForARemovedServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { f["srv_B"].online.Store(false) }}
		onB := web("nginx:1.27")
		onB.Ref.Scope = "srv_B"
		err := e.Apply(t.Context(), propose(t, e, "Open http and run web", onA(), onB).Change)
		var failed *FailedError
		if !errors.As(err, &failed) || !errors.Is(failed.RollbackErr, ErrPending) {
			t.Fatalf("apply = %v, want a failure with the rollback waiting for srv_B", err)
		}
		remove(t, db, f, "srv_B")
		if err := e.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = db.View(t.Context(), func(tx *store.Tx) error {
			if c, _ := tx.GetChange(t.Context(), failed.Rollback); c.State != store.ChangeApplied {
				t.Errorf("the rollback is %s once srv_B was removed, want applied", c.State)
			}
			return nil
		})
	})
}

// failingDelete holds a step that deletes a kind until it's released, then fails it.
type failingDelete struct {
	resource.Agent
	kind             resource.Kind
	reached, release chan struct{}
}

func (f *failingDelete) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if step.Ref.Kind == f.kind && step.Action == resource.ActionDelete {
		close(f.reached)
		<-f.release
		return errors.New("the engine won't do it")
	}
	return f.Agent.Apply(ctx, step, desired)
}

// A scope can't go while a change to what's in it is still applying: that change may
// fail, and its rollback would have nowhere to put things back, so what it was
// deleting would stay on the server with nothing managing it. It says which change.
func TestAScopeWaitsForWhatsApplyingInIt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		apply(t, e, propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}))
		shop, _, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Add staging", environment(shop.ID, "staging")))
		_, staging, err := tree(t, db).Resolve("shop", "staging")
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Run web", app(staging.ID, "nginx:1.27", "srv_A")))
		fd := &failingDelete{Agent: f["srv_A"].agent, kind: service.Kind, reached: make(chan struct{}), release: make(chan struct{})}
		f["srv_A"].agent = fd
		// Planned before, so what's refused at apply is held to it as well.
		retire := propose(t, e, "Retire shop", del(service.Kind, staging.ID, "web"), del(scope.ProjectKind, org, "shop"))
		if err := e.Discard(t.Context(), retire.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}
		stop := propose(t, e, "Stop web", del(service.Kind, staging.ID, "web"))
		done := make(chan error, 1)
		go func() { done <- e.Apply(t.Context(), stop.Change) }()
		select {
		case <-fd.reached:
		case <-time.After(10 * time.Second):
			t.Fatal("the delete step never began")
		}
		says := "chg_"
		refuseProposal(t, e, "deleting a project while a change in it applies", ErrBusy, "Stop web", del(scope.ProjectKind, org, "shop"))
		refuseProposal(t, e, "deleting a project while a change in it applies", ErrBusy, says, del(scope.ProjectKind, org, "shop"))
		refuseProposal(t, e, "deleting an environment while a change in it applies", ErrBusy, "Stop web", del(scope.EnvironmentKind, shop.ID, "staging"))
		// An environment beside it isn't what's applying.
		other := propose(t, e, "Add qa", environment(shop.ID, "qa"))
		apply(t, e, other)
		apply(t, e, propose(t, e, "Retire qa", del(scope.EnvironmentKind, shop.ID, "qa")))

		close(fd.release)
		if err := <-done; err == nil {
			t.Error("the step that fails didn't")
		}
		e.Wait()
		if _, err := tree(t, db).Project("shop"); err != nil {
			t.Fatalf("shop went: %v", err)
		}
		if _, env, err := tree(t, db).Resolve("shop", "staging"); err != nil || env.ID != staging.ID {
			t.Fatalf("after the failure, staging is %+v, %v", env, err)
		}
		list := func() []store.Resource {
			var out []store.Resource
			if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
				out, err = tx.ListResources(t.Context(), staging.ID, string(service.Kind))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if got := list(); len(got) != 1 || got[0].Name != "web" {
			t.Errorf("the rollback didn't put web back: %+v", got)
		}
	})
}

// What a failed change made goes with its rollback, the project too: that rollback
// isn't held up by the change it's undoing, which is still applying when it plans.
func TestARollbackTakesTheProjectItsChangeMade(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		shopID := id.New(id.Project)
		envID := id.New(id.Environment)
		f["srv_A"].agent = &failingCreate{Agent: f["srv_A"].agent}
		p := propose(t, e, "Add shop with web",
			Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}, ID: shopID},
			Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: shopID, Name: "production"}, Spec: &resourcev1.Environment{}, ID: envID},
			app(envID, "nginx:1.27", "srv_A"))
		if err := e.Apply(t.Context(), p.Change); err == nil {
			t.Fatal("the step that fails didn't")
		}
		e.Wait()
		if _, err := tree(t, db).Project("shop"); err == nil {
			t.Error("the rollback left the project its change made")
		}
	})
}

type failingCreate struct{ resource.Agent }

func (f *failingCreate) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if step.Ref.Kind == service.Kind && step.Action == resource.ActionCreate {
		return errors.New("the engine won't do it")
	}
	return f.Agent.Apply(ctx, step, desired)
}

// An earlier deletion can't come back once its project has gone, as what it lived in
// hasn't. That's said by name, with what brings it back, as undo is how what a deletion
// kept comes back.
func TestUndoingADeletionInAProjectThatWentSaysWhatToUndo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		apply(t, e, propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}))
		_, prod, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Run web", app(prod.ID, "nginx:1.27", "srv_A")))
		stop := propose(t, e, "Stop web", del(service.Kind, prod.ID, "web"))
		apply(t, e, stop)
		retire := propose(t, e, "Retire shop", del(scope.ProjectKind, org, "shop"))
		apply(t, e, retire)

		_, err = e.Undo(t.Context(), stop.Change, By{Actor: "usr_test"})
		if err == nil {
			t.Fatal("undo was planned")
		}
		if strings.Contains(err.Error(), prod.ID) {
			t.Errorf("it names the environment by its ID: %v", err)
		}
		for _, says := range []string{"environment production of project shop went in " + retire.Change, "Undo " + retire.Change + " first"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("%v doesn't say %q", err, says)
			}
		}
		// What it says works.
		apply(t, e, undo(t, e, retire))
		apply(t, e, undo(t, e, stop))
	})
}

// An undo takes what its change made and no more, so what a later change made in the
// project keeps it, and the refusal says what and how to deal with it.
func TestUndoingAProjectLeavesWhatALaterChangeMade(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		org := tree(t, db).Organization.ID
		add := propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}})
		apply(t, e, add)
		shop, _, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Add staging", environment(shop.ID, "staging")))
		_, err = e.Undo(t.Context(), add.Change, By{Actor: "usr_test"})
		if err == nil {
			t.Fatal("undoing Add shop took what a later change made")
		}
		for _, says := range []string{"project shop still holds environment staging, so it stays", "Delete it first, with dockwell environments delete staging --project shop"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("%v doesn't say %q", err, says)
			}
		}
		if _, env, err := tree(t, db).Resolve("shop", "staging"); err != nil || env.Name != "staging" {
			t.Errorf("staging is %+v, %v", env, err)
		}
	})
}
