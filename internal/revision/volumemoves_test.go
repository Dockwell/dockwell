// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// movingAgent is a server's agent that moves a volume's data, as this version's
// does, unless it's from before.
type movingAgent struct {
	resource.Local
	old bool
}

func (a movingAgent) VolumeMoves() bool { return !a.old }

// moveBench is a server whose agent moves volumes' data in place, as one that sees
// Docker's own folders does, with web and its volume data in production, and the
// environment staging.
type moveBench struct {
	e             *Engine
	engine        *dockertest.Engine
	agents        onDocker
	prod, staging string
}

func newMoveBench(t *testing.T, b store.Backend) *moveBench {
	t.Helper()
	db, _ := storetest.Open(t, b)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_A", Name: "srv_A", CreatedAt: now, CertSerial: "srv_A", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	engine := dockertest.New(t)
	engine.Mountpoints = true
	engine.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	engine.Registry["nginx:broken"] = dockertest.Image{Exits: true, Config: docker.Config{Cmd: []string{"nginx"}}}
	client, kept := engine.Client(), &volume.Kept{}
	helper := filepath.Join(t.TempDir(), "dockwell")
	if err := os.WriteFile(helper, []byte("#!agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &backup.Runner{Client: client, Root: t.TempDir(), Binary: helper}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Deleted: kept}}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, variable.Type{}, variable.SecretType{})
	agents := onDocker{"srv_A": movingAgent{Local: resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
		&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept}, Moves: runner}, &network.Driver{Client: client}), Server: "srv_A", Values: secretstest.Keeper(db, kinds)}}}
	x := &moveBench{e: New(db, kinds, agents, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler)), engine: engine, agents: agents}
	x.prod = production(t, db)
	apply(t, x.e, propose(t, x.e, "Add staging", environment(tree(t, db).Projects[0].ID, "staging")))
	_, env, err := tree(t, db).Resolve("", "staging")
	if err != nil {
		t.Fatal(err)
	}
	x.staging = env.ID
	withData(t, x.e, x.prod)
	if err := os.WriteFile(filepath.Join(x.dir(x.prod), "orders.db"), []byte("the orders"), 0o600); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *moveBench) dir(env string) string {
	return x.engine.VolumeDir(volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}).Name)
}

func (x *moveBench) holds(env string) string {
	got, _ := os.ReadFile(filepath.Join(x.dir(env), "orders.db"))
	return string(got)
}

// move is web and its volume, from one environment to another.
func move(from, to, image string) []Edit {
	web := uses2(to)
	web.Spec.(*resourcev1.Service).GetApp().Image = image //nolint:forcetypeassert // uses2's
	moved := data(to, "srv_A")
	moved.MoveFrom = resource.Ref{Kind: volume.Kind, Scope: from, Name: "data"}
	return []Edit{
		{Ref: resource.Ref{Kind: service.Kind, Scope: from, Name: "web"}},
		{Ref: resource.Ref{Kind: volume.Kind, Scope: from, Name: "data"}},
		moved, web,
	}
}

// Moving an app to another environment moves its volume's data into a volume of the
// new name, in place, with nothing mounting it: the old service goes before the data
// moves, and the old volume, empty, after. Undoing it moves the data back the same
// way (ADR 0084, #572).
func TestAMoveTakesTheDataAlongAndUndoBringsItBack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		x := newMoveBench(t, b)
		p := propose(t, x.e, "Move web", move(x.prod, x.staging, "nginx:1.27")...)
		if got, want := stepKinds(p), []string{"delete service", "create volume", "create service", "delete volume"}; !slices.Equal(got, want) {
			t.Errorf("steps = %v, want %v", got, want)
		}
		steps := p.Steps()
		if m := steps[1].VolumeMove; m == nil || m.How != resource.MoveInPlace || steps[1].Ref.Scope != x.staging {
			t.Errorf("the new volume's step = %+v", steps[1])
		}
		apply(t, x.e, p)
		if x.holds(x.staging) != "the orders" || x.holds(x.prod) != "" {
			t.Fatalf("moved, staging holds %q and production %q", x.holds(x.staging), x.holds(x.prod))
		}

		undo, err := x.e.Undo(t.Context(), p.Change, By{Actor: "usr_test", Via: ViaPanel})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := stepKinds(undo), []string{"delete service", "create volume", "create service", "delete volume"}; !slices.Equal(got, want) {
			t.Errorf("the undo's steps = %v, want %v", got, want)
		}
		if m := undo.Steps()[1].VolumeMove; m == nil || m.How != resource.MoveInPlace {
			t.Errorf("the undo's volume step = %+v", undo.Steps()[1])
		}
		apply(t, x.e, undo)
		if x.holds(x.prod) != "the orders" || x.holds(x.staging) != "" {
			t.Errorf("undone, production holds %q and staging %q", x.holds(x.prod), x.holds(x.staging))
		}
	})
}

// A move that fails after its data moved is rolled back with the data: it goes back
// into the old volume before the old service runs again on it.
func TestAMoveThatFailsTakesTheDataBack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		x := newMoveBench(t, b)
		p := propose(t, x.e, "Move web", move(x.prod, x.staging, "nginx:broken")...)
		err := x.e.Apply(t.Context(), p.Change)
		var failed *FailedError
		if !errors.As(err, &failed) || failed.Rollback == "" || failed.RollbackErr != nil {
			t.Fatalf("applying a move whose new service can't start = %v", err)
		}
		if x.holds(x.prod) != "the orders" || x.holds(x.staging) != "" {
			t.Errorf("rolled back, production holds %q and staging %q", x.holds(x.prod), x.holds(x.staging))
		}
		web := x.engine.Container(docker.ContainerName(docker.Project(x.prod), "web"))
		if web == nil || !web.State.Running {
			t.Errorf("web in production after the rollback = %+v", web)
		}
	})
}

// A move is refused where the agent is too old to make it, and one that doesn't
// delete what it moves from is refused as it's proposed.
func TestAMoveNeedsAnAgentThatMoves(t *testing.T) {
	x := newMoveBench(t, store.SQLite)
	edits := move(x.prod, x.staging, "nginx:1.27")
	if _, err := x.e.Propose(t.Context(), Proposal{Summary: "Move web", Actor: "usr_test", Edits: edits[2:]}); err == nil || !strings.Contains(err.Error(), "deletes data too") {
		t.Errorf("a move that keeps what it moves from = %v", err)
	}
	a := x.agents["srv_A"].(movingAgent) //nolint:forcetypeassert // newMoveBench's
	a.old = true
	x.agents["srv_A"] = a
	if _, err := x.e.Propose(t.Context(), Proposal{Summary: "Move web", Actor: "usr_test", Edits: edits}); err == nil || !strings.Contains(err.Error(), "too old to move a volume's data") {
		t.Errorf("a move on an agent from before = %v", err)
	}
}

// app is a service of env that mounts the volumes named and uses the variables named.
func appIn(env, name string, volumes []string, variables ...string) Edit {
	c := &resourcev1.Container{Image: "nginx:1.27"}
	for _, v := range volumes {
		c.Mounts = append(c.Mounts, &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: v}, Target: "/" + v})
	}
	svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
	for _, v := range variables {
		if svc.Variables == nil {
			svc.Variables = map[string]string{}
		}
		svc.Variables[v] = ""
	}
	return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: svc, Servers: []string{"srv_A"}}
}

func greeting(env string) Edit {
	return Edit{Ref: resource.Ref{Kind: variable.VariableKind, Scope: env, Name: "GREETING"}, Spec: &resourcev1.Variable{Value: "hello"}}
}

func refsOf(refs []resource.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = string(r.Kind) + " " + r.Name
	}
	return out
}

// Moving an app takes what only it uses, its volume with its data, and copies a value
// another app reads too, which stays; the other app runs on where it was (ADR 0084,
// #572).
func TestMovingAnAppTakesWhatOnlyItUses(t *testing.T) {
	x := newMoveBench(t, store.SQLite)
	apply(t, x.e, propose(t, x.e, "A greeting", greeting(x.prod), appIn(x.prod, "web", []string{"data"}, "GREETING"), appIn(x.prod, "worker", nil, "GREETING")))
	edits, report, err := x.e.PlanAppMove(t.Context(), AppMove{From: x.prod, To: x.staging, Services: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Blocks) > 0 {
		t.Fatalf("blocked: %+v", report.Blocks)
	}
	if got, want := refsOf(report.Moves), []string{"service web", "volume data"}; !slices.Equal(got, want) {
		t.Errorf("moves %v, want %v", got, want)
	}
	if got, want := refsOf(report.Copies), []string{"variable GREETING"}; !slices.Equal(got, want) {
		t.Errorf("copies %v, want %v", got, want)
	}
	apply(t, x.e, propose(t, x.e, "Move web", edits...))
	if x.holds(x.staging) != "the orders" || x.holds(x.prod) != "" {
		t.Errorf("moved, staging holds %q and production %q", x.holds(x.staging), x.holds(x.prod))
	}
	for _, name := range []string{docker.ContainerName(docker.Project(x.staging), "web"), docker.ContainerName(docker.Project(x.prod), "worker")} {
		if c := x.engine.Container(name); c == nil || !c.State.Running {
			t.Errorf("%s isn't running: %+v", name, c)
		}
	}
	if c := x.engine.Container(docker.ContainerName(docker.Project(x.prod), "web")); c != nil {
		t.Errorf("web still runs in production: %+v", c)
	}
}

// What would part the app from what stays holds the move back, each with the
// service to move too; moving that too clears it.
func TestMovingAnAppStopsAtWhatItShares(t *testing.T) {
	x := newMoveBench(t, store.SQLite)
	apply(t, x.e, propose(t, x.e, "A worker", appIn(x.prod, "worker", []string{"data"})))
	edits, report, err := x.e.PlanAppMove(t.Context(), AppMove{From: x.prod, To: x.staging, Services: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 0 || len(report.Blocks) != 1 || report.Blocks[0].MoveToo != "worker" || !strings.Contains(report.Blocks[0].Why, "worker's too") {
		t.Fatalf("edits %d, blocks %+v", len(edits), report.Blocks)
	}
	edits, report, err = x.e.PlanAppMove(t.Context(), AppMove{From: x.prod, To: x.staging, Services: []string{"web", "worker"}})
	if err != nil || len(report.Blocks) > 0 {
		t.Fatalf("moving both = %v, %+v", err, report)
	}
	apply(t, x.e, propose(t, x.e, "Move web and worker", edits...))
	if x.holds(x.staging) != "the orders" {
		t.Errorf("moved, staging holds %q", x.holds(x.staging))
	}
	// Into an environment that has one of the names already, it's held back.
	_, report, err = x.e.PlanAppMove(t.Context(), AppMove{From: x.staging, To: x.prod, Services: []string{"web", "worker"}})
	if err != nil || len(report.Blocks) != 0 {
		t.Fatalf("moving back = %v, %+v", err, report)
	}
	apply(t, x.e, propose(t, x.e, "A clash", appIn(x.prod, "web", nil)))
	_, report, err = x.e.PlanAppMove(t.Context(), AppMove{From: x.staging, To: x.prod, Services: []string{"web", "worker"}})
	if err != nil || len(report.Blocks) != 1 || !strings.Contains(report.Blocks[0].Why, "named web where the app goes") {
		t.Errorf("moving onto a name that's taken = %v, %+v", err, report)
	}
}

// An app moves into a project the move makes, named for it, in the same change.
func TestMovingAnAppIntoANewProject(t *testing.T) {
	x := newMoveBench(t, store.SQLite)
	edits, report, err := x.e.PlanAppMove(t.Context(), AppMove{From: x.prod, Services: []string{"web"}, NewProject: "web"})
	if err != nil || len(report.Blocks) > 0 {
		t.Fatalf("planning = %v, %+v", err, report)
	}
	apply(t, x.e, propose(t, x.e, "Move web to a project of its own", edits...))
	var env string
	for _, ed := range edits {
		if ed.Ref.Kind == service.Kind && ed.Spec != nil {
			env = ed.Ref.Scope
		}
	}
	if env == x.prod || env == "" {
		t.Fatalf("web went to %q", env)
	}
	if x.holds(env) != "the orders" {
		t.Errorf("the new project's volume holds %q", x.holds(env))
	}
}
