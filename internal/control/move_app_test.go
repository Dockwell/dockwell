// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	var oldID string
	for _, r := range first.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(volume.Kind) && r.GetResource().GetName() == "data" {
			oldID = r.GetId()
		}
	}
	// A backup from before the move goes with the volume.
	bc := backupClient(b, b.origin)
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: oldID}))
	if err != nil {
		t.Fatal(err)
	}
	before := waitBackup(t, bc, made.Msg.GetBackup().GetId())
	if before.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
		t.Fatalf("the backup before the move: %v", before)
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
	// Its backup from before is the moved volume's, listed and restored where it went.
	var movedID string
	for _, r := range m.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(volume.Kind) && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			movedID = r.GetId()
		}
	}
	listed, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{EnvironmentId: to}))
	if err != nil {
		t.Fatal(err)
	}
	if bs := listed.Msg.GetBackups(); len(bs) != 1 || bs[0].GetId() != before.GetId() || bs[0].GetVolumeId() != movedID {
		t.Fatalf("where it went, the backups are %v", bs)
	}
	if err := os.WriteFile(filepath.Join(e.VolumeDir(moved), "orders.db"), []byte("changed since"), 0o600); err != nil {
		t.Fatal(err)
	}
	restoring, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: before.GetId()}))
	if err != nil {
		t.Fatalf("restoring the backup from before where it went: %v", err)
	}
	applyReviewed(t, ch, restoring.Msg.GetChange())
	if got, _ := os.ReadFile(filepath.Join(e.VolumeDir(moved), "orders.db")); string(got) != "the orders" {
		t.Errorf("restored where it went, the volume holds %q", got)
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
	// And its backups are the old volume's again.
	if back, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{VolumeId: oldID})); err != nil || len(back.Msg.GetBackups()) != 1 || back.Msg.GetBackups()[0].GetEnvironmentId() != env {
		t.Errorf("undone, the old volume's backups are %v, %v", back.Msg.GetBackups(), err)
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
	deleteData := func(anyway bool, keepIn string) (*dockwellv1.Change, error) {
		res, err := ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete data", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "data"}, Delete: true, WithContents: true, DeleteShared: anyway, KeepSharedIn: keepIn,
		}}}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	// Deleting it anyway plans the server's deletion, which the blog then lacks.
	anyway, err := deleteData(true, "")
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

	// Where the server goes may be picked, as the panel's picker does, by an
	// environment's ID; never the project being deleted.
	if _, err := deleteData(false, "data"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("keeping the server in the project deleted = %v, want it refused", err)
	}
	picked, err := deleteData(false, appEnv)
	if err != nil {
		t.Fatalf("keeping the server with the blog = %v", err)
	}
	with := false
	for _, r := range picked.GetPlan().GetResources() {
		ref := r.GetResource()
		with = with || ref.GetKind() == "service" && ref.GetName() == "db" && ref.GetScope() == appEnv && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE
	}
	if !with {
		t.Errorf("the server doesn't go with the blog: %v", picked.GetPlan().GetResources())
	}
	if _, err := ch.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: picked.GetId()})); err != nil {
		t.Fatal(err)
	}

	kept, err := deleteData(false, "")
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

const crossingStack = `
services:
  web:
    image: ghcr.io/acme/shop:1.4
    networks: [front]
  reporter:
    image: ghcr.io/acme/worker:1.0
    networks: [side]
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes: [db-data:/var/lib/postgresql/data]
    networks: [back]
volumes:
  db-data: {}
networks:
  front: {}
  side: {}
  back: {}
`

// An app that reads a database beside it, through a link of its environment, as the
// user it has there, moves on its own where another app reads that database too: the
// database stays, and the moved app reaches it where it is, on a link network of its
// own where it answers to its own name, reading its user's password from there, as a
// share across projects has it (ADR 0084, #572).
func TestMovingAnAppLeavesItsSharedDatabaseLinked(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	imported, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "shop", Compose: crossingStack, Variables: map[string]string{"DB_PASSWORD": "the-administrators"}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, ch, imported.Msg.GetChange())
	var env string
	for _, r := range imported.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			env = r.GetResource().GetScope()
		}
	}
	ref := func(kind, name string) *dockwellv1.ResourceRef {
		return &dockwellv1.ResourceRef{Kind: kind, Scope: env, Name: name}
	}
	link := func(name, host string, sets ...*resourcev1.LinkSet) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: ref("link", name), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
			Label: "Database", Service: "db", Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, User: host,
			Accepts: []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}}, Sets: sets,
		}}}}
	}
	service := func(name, image string, networks []string, variables, secrets map[string]string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: ref("service", name), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image, Networks: networks}}, Variables: variables, Secrets: secrets,
		}}}}
	}
	apply(t, ch, propose(t, ch, "Link web and the reporter to db",
		&dockwellv1.Edit{Resource: ref("secret", "WEB_PASSWORD"), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "a-password-for-web"}}}},
		&dockwellv1.Edit{Resource: ref("database.user", "web"), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: &resourcev1.DatabaseUser{Service: "db", Password: "WEB_PASSWORD"}}}},
		&dockwellv1.Edit{Resource: ref("variable", "DB_HOST"), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "db"}}}},
		&dockwellv1.Edit{Resource: ref("variable", "REPORT_HOST"), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "db"}}}},
		link("db", "web", &resourcev1.LinkSet{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}, &resourcev1.LinkSet{Name: "WEB_PASSWORD", Part: resourcev1.LinkPart_LINK_PART_PASSWORD}),
		link("reports", "", &resourcev1.LinkSet{Name: "REPORT_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}),
		service("web", "ghcr.io/acme/shop:1.4", []string{"front"}, map[string]string{"DB_HOST": ""}, map[string]string{"WEB_PASSWORD": ""}),
		service("reporter", "ghcr.io/acme/worker:1.0", []string{"side"}, map[string]string{"REPORT_HOST": ""}, nil),
	))

	// A dump of web's database alone, taken for its link, follows the link.
	linkID := func(scope string) string {
		t.Helper()
		var id string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), "link", scope, "db")
			id = r.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	now := time.Now()
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateBackup(t.Context(), store.Backup{ID: "bkp_web", EnvironmentID: env, Volume: "db-data", VolumeID: "res_db_data", ServerID: f.lastDocker.id.ServerID, State: store.BackupDone, StartedAt: now, OneDatabase: "web", ForLink: linkID(env)})
	}); err != nil {
		t.Fatal(err)
	}
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	res, err := svc.MoveService(t.Context(), connect.NewRequest(&dockwellv1.MoveServiceRequest{Id: "web", Project: "shop", NewProject: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	if len(m.GetBlocks()) != 0 || len(m.GetCrosses()) != 1 || m.GetCrosses()[0].GetName() != "db" || m.GetChange() == nil {
		t.Fatalf("the move reads blocks %v, crosses %v", m.GetBlocks(), m.GetCrosses())
	}
	for _, r := range m.GetMoves() {
		if r.GetName() == "db" && r.GetKind() == "service" || r.GetKind() == "database.user" {
			t.Errorf("the move takes %v, which stays", r)
		}
	}
	if crosses := notesOf(m.GetChange(), "move_crosses"); len(crosses) != 1 {
		t.Errorf("the plan says %v", m.GetChange().GetPlan().GetNotes())
	}
	applyReviewed(t, ch, m.GetChange())
	var to string
	for _, r := range m.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			to = r.GetResource().GetScope()
		}
	}
	web := e.Newest(docker.Project(to), "web")
	if web == nil {
		t.Fatalf("web doesn't run where it went: %v", e.Containers())
	}
	read := map[string]string{}
	for _, kv := range web.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		read[k] = v
	}
	if read["DB_HOST"] != "db" || read["WEB_PASSWORD"] != "a-password-for-web" {
		t.Errorf("web reads %v", read)
	}
	linkNet := docker.ScopedName(docker.Project(to), "db-link")
	if _, ok := web.NetworkSettings.Networks[linkNet]; !ok {
		t.Errorf("web isn't on its link network: %v", web.NetworkSettings.Networks)
	}
	if db := e.Newest(docker.Project(env), "db"); db == nil || db.NetworkSettings.Networks[linkNet] == nil || !slices.Contains(db.NetworkSettings.Networks[linkNet].Aliases, "db") {
		t.Errorf("db isn't on web's link network as db: %v", db)
	}
	if reporter := e.Newest(docker.Project(env), "reporter"); reporter == nil {
		t.Error("the reporter left")
	}
	var dump store.Backup
	if err := f.store.View(t.Context(), func(tx *store.Tx) (err error) {
		dump, err = tx.GetBackup(t.Context(), "bkp_web")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if dump.ForLink != linkID(to) {
		t.Errorf("the dump taken for web's link is %s's, not the moved link's %s", dump.ForLink, linkID(to))
	}
}

// A developer of one project moving an app learns nothing of projects they don't
// hold: a place elsewhere reads as one that isn't there, a choice names only their
// own, and making a project for it is an admin's, said before its name is read (the
// review of #572, its M1).
func TestMovingAnAppSaysNothingOfOthersProjects(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	envOf := map[string]string{}
	for _, project := range []string{"shop", "hidden"} {
		res, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: project, Compose: moveStack}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, ch, res.Msg.GetChange())
		for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
			if r.GetResource().GetKind() == "service" {
				envOf[project] = r.GetResource().GetScope()
			}
		}
	}
	aMember(t, f)(store.Grant{ProjectID: envProject(t, f, envOf["shop"]), Role: store.RoleDeveloper})
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	move := func(req *dockwellv1.MoveServiceRequest) error {
		t.Helper()
		req.Id, req.Project = "web", "shop"
		_, err := svc.MoveService(t.Context(), connect.NewRequest(req))
		return err
	}
	for _, to := range []*dockwellv1.MoveServiceRequest{{ToProject: "nosuch"}, {ToProject: "hidden"}, {ToEnvironment: envOf["hidden"]}} {
		err := move(to)
		if connect.CodeOf(err) != connect.CodeNotFound || strings.Contains(err.Error(), "default") || strings.Contains(err.Error(), envOf["hidden"]) && to.GetToEnvironment() == "" {
			t.Errorf("moving to %v = %v, want not found, naming no other project", to, err)
		}
		if to.GetToProject() == "nosuch" && strings.Contains(err.Error(), "hidden") {
			t.Errorf("moving to nosuch names hidden: %v", err)
		}
	}
	if err := move(&dockwellv1.MoveServiceRequest{NewProject: "hidden"}); connect.CodeOf(err) != connect.CodePermissionDenied || strings.Contains(err.Error(), "already") {
		t.Errorf("a developer making a project = %v, want refused before its name is read", err)
	}
}

// Deleting what another project's app uses too stops that app, which needs a
// developer's role where its link is, whatever else the plan holds: here the app
// reads no value of the server's, so nothing else in the plan asks for one (the
// review of #572, its M3). Refused, it names neither the app nor its project.
func TestDeletingWhatOthersUseNeedsARoleWhereTheyAre(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	serviceEnv := func(c *dockwellv1.Change) string {
		for _, r := range c.GetPlan().GetResources() {
			if r.GetResource().GetKind() == "service" {
				return r.GetResource().GetScope()
			}
		}
		t.Fatalf("no service in %v", c.GetPlan().GetResources())
		return ""
	}
	app, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "reporting", Compose: moveStack}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, ch, app.Msg.GetChange())
	data, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "data", Compose: moveStack}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, ch, data.Msg.GetChange())
	dataProject := envProject(t, f, serviceEnv(data.Msg.GetChange()))
	apply(t, ch, propose(t, ch, "A staging environment", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: dataProject, Name: "staging"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	}))
	staging, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "data", Environment: "staging", Compose: databaseStack, Variables: map[string]string{"DB_PASSWORD": "the-administrators"}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, ch, staging.Msg.GetChange())
	stagingEnv, appEnv := serviceEnv(staging.Msg.GetChange()), serviceEnv(app.Msg.GetChange())
	apply(t, ch, propose(t, ch, "Reach staging's database",
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: appEnv, Name: "REPORT_HOST"}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "db"}}}},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: appEnv, Name: "reports"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Database", Service: "db", MeetsEnvironment: stagingEnv, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL,
				Accepts: []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
				Sets:    []*resourcev1.LinkSet{{Name: "REPORT_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
			}}},
		}))

	hold := aMember(t, f)
	deleteStaging := func() error {
		_, err := ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete staging", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: dataProject, Name: "staging"}, Delete: true, WithContents: true, DeleteShared: true,
		}}}))
		return err
	}
	hold(store.Grant{ProjectID: dataProject, Role: store.RoleDeveloper})
	if err := deleteStaging(); connect.CodeOf(err) != connect.CodePermissionDenied || strings.Contains(err.Error(), "reports") || strings.Contains(err.Error(), "reporting") {
		t.Errorf("a developer of data alone deleting what reporting uses = %v, want refused, naming neither", err)
	}
	hold(store.Grant{ProjectID: dataProject, Role: store.RoleDeveloper}, store.Grant{ProjectID: envProject(t, f, appEnv), Role: store.RoleDeveloper})
	if err := deleteStaging(); err != nil {
		t.Errorf("a developer of both deleting it = %v", err)
	}
}

// A copy sent before the app moved is fetched after it, with its server's copy gone:
// it opens as what it was sealed as, its environment then, and lands as the moved
// volume's (the review of #572, its N1).
func TestACopySentBeforeAMoveIsFetchedAfterIt(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	if b.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || b.GetCopyError() != "" {
		t.Fatalf("the copy before the move: %v", b)
	}
	svc := dockwellv1connect.NewServiceServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	res, err := svc.MoveService(t.Context(), connect.NewRequest(&dockwellv1.MoveServiceRequest{Id: "web", NewProject: "elsewhere"}))
	if err != nil || len(res.Msg.GetBlocks()) != 0 {
		t.Fatalf("moving web = %v, blocks %v", err, res.Msg.GetBlocks())
	}
	applyReviewed(t, s.c, res.Msg.GetChange())
	moved := s.get(t, b.GetId())
	if moved.GetEnvironmentId() == b.GetEnvironmentId() {
		t.Fatalf("the backup stayed in %s", moved.GetEnvironmentId())
	}
	s.lost(t, moved)
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the fetch to end", func() bool { return s.said(t, "backup.fetched") != nil || s.said(t, "backup.fetch_failed") != nil })
	if failed := s.said(t, "backup.fetch_failed"); failed != nil {
		t.Fatalf("fetching the copy sent before the move: %v", failed)
	}
	if got := s.get(t, b.GetId()); got.GetFetchError() != "" || got.GetEnvironmentId() != moved.GetEnvironmentId() {
		t.Errorf("fetched, the backup is %v", got)
	}
}
