// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
)

const moveStack = `
services:
  web:
    image: ghcr.io/acme/shop:1.4
    volumes: [data:/data]
    networks: [front]
  worker:
    image: ghcr.io/acme/worker:1.0
    networks: [back]
volumes:
  data: {}
networks:
  front: {}
  back: {}
`

// Moving an app to a project of its own takes its volume, with its data, and its
// route and its network, in one plan with undo; the worker beside it, on a network of
// its own, stays. A service that shares
// its volume holds the move back, naming itself to move too, and plans nothing
// (ADR 0084, #572).
func TestMovingAnAppThroughTheAPI(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	ch := changeClient(b, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), moveStack, nil, false)
	if got := apply(t, ch, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the stack: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	apply(t, ch, propose(t, ch, "A domain", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "www"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "shop.example.com", Service: "web", Port: 80}}},
	}))
	old := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}).Name
	if err := os.WriteFile(filepath.Join(e.VolumeDir(old), "orders.db"), []byte("the orders"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	res, err := svc.MoveService(t.Context(), connect.NewRequest(&dockwellv1.MoveServiceRequest{Id: "web", NewProject: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	var moves []string
	for _, r := range m.GetMoves() {
		moves = append(moves, r.GetKind()+" "+r.GetName())
	}
	if want := []string{"network front", "route www", "service web", "volume data"}; !slices.Equal(moves, want) || len(m.GetBlocks()) != 0 || m.GetChange() == nil {
		t.Fatalf("moves %v, blocks %v, change %v", moves, m.GetBlocks(), m.GetChange() != nil)
	}
	if m.GetChange().GetSummary() != "Move web to shop" {
		t.Errorf("the plan reads %q", m.GetChange().GetSummary())
	}
	// Its domain goes from one environment to another, which a person confirms.
	applyReviewed(t, ch, m.GetChange())
	var to string
	for _, r := range m.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			to = r.GetResource().GetScope()
		}
	}
	moved := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: to, Name: "data"}).Name
	if got, _ := os.ReadFile(filepath.Join(e.VolumeDir(moved), "orders.db")); string(got) != "the orders" {
		t.Errorf("the moved volume holds %q", got)
	}
	if r := ing.routes[resource.Ref{Kind: route.Kind, Scope: to, Name: "www"}]; r.GetDomain() != "shop.example.com" {
		t.Errorf("the route where it went is %v; routes %v", r, ing.routes)
	}
	if web := e.Newest(docker.Project(to), "web"); web == nil {
		t.Error("web doesn't run where it went")
	}
	if worker := e.Newest(docker.Project(env), "worker"); worker == nil {
		t.Error("the worker left with it")
	}

	// Undone, it's all back, data and all.
	undone, err := ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: m.GetChange().GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, ch, undone.Msg.GetChange())
	if got, _ := os.ReadFile(filepath.Join(e.VolumeDir(old), "orders.db")); string(got) != "the orders" {
		t.Errorf("undone, the old volume holds %q", got)
	}

	// A worker that mounts its volume too holds it back.
	apply(t, ch, propose(t, ch, "The worker reads the data", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: env, Name: "worker"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "ghcr.io/acme/worker:1.0", Networks: []string{"back"}, Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
		}}}}},
	}))
	res, err = svc.MoveService(t.Context(), connect.NewRequest(&dockwellv1.MoveServiceRequest{Id: "web", NewProject: "shop-two"}))
	if err != nil {
		t.Fatal(err)
	}
	if bl := res.Msg.GetBlocks(); len(bl) != 1 || bl[0].GetMoveToo() != "worker" || res.Msg.GetChange() != nil || !strings.Contains(bl[0].GetWhy(), "Move worker too") {
		t.Errorf("with the worker on its volume, the move reads %v", res.Msg)
	}
}

// Deleting a project whose PostgreSQL an app of another project shares keeps that app
// working: the server moves to a project of its own, named for it, with its users and
// their passwords, and the app's link follows it, in the deletion's one plan. Deleting
// it anyway is a choice the deletion says (ADR 0084, #572).
func TestDeletingAProjectThatSharesKeepsWhatOthersUse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blogEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	imported, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "data", Compose: databaseStack, Variables: map[string]string{"DB_PASSWORD": "the-administrators"}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, ch, imported.Msg.GetChange())
	cat := catalogClient(b)
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetWhy() == "" {
			choice = o.GetChoice()
		}
	}
	if choice == "" {
		t.Fatalf("the PostgreSQL isn't offered: %v", got.Msg.GetNeeds())
	}
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", Needs: map[string]string{"db": choice}})
	if got := apply(t, ch, installed); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var appEnv string
	for _, r := range installed.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			appEnv = r.GetResource().GetScope()
		}
	}
	deleteData := func(anyway bool) (*dockwellv1.Change, error) {
		res, err := ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete data", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "data"}, Delete: true, WithContents: true, DeleteShared: anyway,
		}}}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	// Deleting it anyway plans the server's deletion, which the blog then lacks.
	anyway, err := deleteData(true)
	if err != nil {
		t.Fatalf("deleting it anyway = %v", err)
	}
	goes := false
	for _, r := range anyway.GetPlan().GetResources() {
		goes = goes || r.GetResource().GetKind() == "service" && r.GetResource().GetName() == "db" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_DELETE
	}
	if !goes {
		t.Errorf("deleting it anyway keeps the server: %v", anyway.GetPlan().GetResources())
	}
	if _, err := ch.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: anyway.GetId()})); err != nil {
		t.Fatal(err)
	}

	kept, err := deleteData(false)
	if err != nil {
		t.Fatal(err)
	}
	var to string
	follows := false
	for _, r := range kept.GetPlan().GetResources() {
		ref := r.GetResource()
		if ref.GetKind() == "service" && ref.GetName() == "db" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			to = ref.GetScope()
		}
		follows = follows || ref.GetKind() == "link" && ref.GetScope() == appEnv && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_UPDATE
	}
	if to == "" || !follows {
		t.Fatalf("the deletion keeps the server in %q, its link follows %t: %v", to, follows, kept.GetPlan().GetResources())
	}
	applyReviewed(t, ch, kept)
	if c := e.Newest(docker.Project(to), "db"); c == nil {
		t.Error("the server doesn't run where it went")
	}
	if c := e.Newest(docker.Project(appEnv), "app"); c == nil {
		t.Error("the blog stopped")
	}

	// Deleting the server's own project anyway names the blog, dumps the blog's
	// database alone first, while the server still runs, as the blog's backup, and
	// holds the blog's password at what it reads now.
	res, err := ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete db", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "db"}, Delete: true, WithContents: true, DeleteShared: true,
	}}}))
	if err != nil {
		t.Fatalf("deleting the server's project anyway = %v", err)
	}
	gone := res.Msg.GetChange()
	if breaks := notesOf(gone, "breaks_shared"); len(breaks) != 1 || !strings.Contains(breaks[0], "the app in blog loses db") {
		t.Errorf("the plan says %v", gone.GetPlan().GetNotes())
	}
	dumps, held := false, false
	for _, srv := range gone.GetPlan().GetServers() {
		for _, st := range srv.GetSteps() {
			if st.GetResource().GetKind() == "volume" && st.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_DELETE && st.GetBackupFirst().GetDatabaseName() == "blog" {
				dumps = true
			}
		}
	}
	for _, r := range gone.GetPlan().GetResources() {
		held = held || r.GetResource().GetScope() == appEnv && r.GetResource().GetKind() == "secret" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_UPDATE
	}
	if !dumps || !held {
		t.Fatalf("the deletion dumps the blog's database first %t, holds its password %t: %v", dumps, held, gone.GetPlan())
	}
	applyReviewed(t, ch, gone)
	pg.mu.Lock()
	dumped := slices.ContainsFunc(pg.said, func(s string) bool { return strings.Contains(s, "pg_dump") && strings.HasSuffix(s, " blog blog") })
	pg.mu.Unlock()
	if !dumped {
		t.Error("the blog's database wasn't dumped before its server went")
	}
	// Kept as the blog's, of its one database, though its server is gone.
	list, err := backupClient(b, b.origin).ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{DatabaseName: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	if bs := list.Msg.GetBackups(); len(bs) == 0 || bs[0].GetDatabaseName() != "blog" || bs[0].GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
		t.Errorf("the blog's backups are %v", bs)
	}
	if c := e.Newest(docker.Project(to), "db"); c != nil {
		t.Error("the server still runs")
	}
}
