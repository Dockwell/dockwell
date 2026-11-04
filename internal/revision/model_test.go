// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
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
		for _, d := range deps {
			out = append(out, d.Server+"@"+string(rune('0'+d.Revision))+"="+d.State)
		}
		return nil
	})
	return out
}

func running(t *testing.T, s *server, ref resource.Ref) string {
	t.Helper()
	c, err := s.docker.Inspect(t.Context(), service.ContainerName(ref))
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
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "there are 2 servers, so say which web runs on: srv_A, srv_B") {
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

		undo, err := e.Undo(t.Context(), move.Change, "usr_test")
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

		undo, err := e.Undo(t.Context(), p.Change, "usr_test")
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
		del := func(kind resource.Kind, scopeID, name string) Edit {
			return Edit{Ref: resource.Ref{Kind: kind, Scope: scopeID, Name: name}}
		}
		refuse := func(why string, want string, edits ...Edit) {
			t.Helper()
			_, err := e.Propose(t.Context(), Proposal{Summary: why, Actor: "usr_test", Edits: edits})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
				t.Errorf("%s = %v, want ErrInvalid saying %q", why, err, want)
			}
		}
		refuse("deleting the only environment", "production is the last environment in project default",
			del(scope.EnvironmentKind, project, "production"))
		refuse("deleting the only project", "default still holds production",
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
