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

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// An app's own database, on a server another project runs, is the app's to back up
// and restore from its page, with a role in its project alone: a dump of that one
// database as its owner, and a restore of it alone, nothing stopped, its user kept
// out while it loads. The server's whole restore stays the server's, with a role in
// every project that shares it or an admin, a plan that names what each loses, and
// its name typed (ADR 0084).
func TestAnAppBacksUpAndRestoresItsOwnDatabase(t *testing.T) {
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
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	data := apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	var volumeID string
	for _, r := range data.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "volume" {
			volumeID = r.GetId()
		}
	}
	cat := catalogClient(b)
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	choice := ""
	for _, o := range got.Msg.GetNeeds() {
		if o.GetService() == "db" {
			choice = o.GetChoice()
		}
	}
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", Needs: map[string]string{"db": choice}})
	if done := apply(t, ch, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", done.GetState(), done.GetError())
	}
	var dbEnv, appEnv string
	for _, r := range installed.GetPlan().GetResources() {
		switch r.GetResource().GetKind() {
		case "database.user":
			dbEnv = r.GetResource().GetScope()
		case "link":
			appEnv = r.GetResource().GetScope()
		}
	}
	for _, env := range []string{dbEnv, appEnv} {
		protectEnv(t, ch, f, env, resourcev1.Protection_PROTECTION_OPEN)
	}
	bc := backupClient(b, b.origin)
	whole, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	waitBackup(t, bc, whole.Msg.GetBackup().GetId())

	// The blog's developer, who holds nothing of data's project.
	hold := aMember(t, f)
	blogDeveloper := store.Grant{ProjectID: envProject(t, f, appEnv), Role: store.RoleDeveloper}
	hold(blogDeveloper)
	made, err := cat.BackUpCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.BackUpCatalogNeedRequest{Id: "blog", Need: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	var one *dockwellv1.Backup
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		list, err := cat.ListCatalogNeedBackups(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogNeedBackupsRequest{Id: "blog", Need: "db"}))
		if err != nil {
			t.Fatal(err)
		}
		if bs := list.Msg.GetBackups(); len(bs) == 1 && bs[0].GetState() != dockwellv1.BackupState_BACKUP_STATE_RUNNING {
			one = bs[0]
			break
		}
	}
	if one == nil || one.GetId() != made.Msg.GetBackup().GetId() || one.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || one.GetDatabaseName() != "blog" {
		list, err := cat.ListCatalogNeedBackups(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogNeedBackupsRequest{Id: "blog", Need: "db"}))
		t.Fatalf("the blog's database backed up as %v (made %v; listed %v, %v)", one, made.Msg.GetBackup(), list, err)
	}
	// Where data's server runs is data's project's to name, not the blog's developer's.
	if one.GetEnvironmentId() != "" || one.GetVolume() != "" || one.GetVolumeId() != "" || one.GetServerName() != "" {
		t.Errorf("the blog's developer reads where data's server runs: %v", one)
	}
	pg.mu.Lock()
	dumpedAsOwner := slices.ContainsFunc(pg.said, func(s string) bool { return strings.Contains(s, "pg_dump") && strings.HasSuffix(s, " blog blog") })
	pg.mu.Unlock()
	if !dumpedAsOwner {
		t.Error("the blog's database wasn't dumped as its owner")
	}
	// Its whole server's backups aren't the blog's to restore, nor this one by that way.
	if _, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: whole.Msg.GetBackup().GetId()})); err == nil {
		t.Error("the blog's developer restored data's whole server")
	}

	// A backup taken for the blog's link of a database it doesn't have now isn't
	// restored into the one it has.
	stray := "bkp_01KAT4ZR6E3V0Y5XBKZB5D9Q7N"
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		b, err := tx.GetBackup(t.Context(), one.GetId())
		if err != nil {
			return err
		}
		b.ID, b.OneDatabase = stray, "shop"
		return tx.CreateBackup(t.Context(), b)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.RestoreCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.RestoreCatalogNeedRequest{Id: "blog", Need: "db", BackupId: stray})); connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "this app's database is blog now") {
		t.Errorf("restoring a backup of another database into the blog's = %v", err)
	}

	restored, err := cat.RestoreCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.RestoreCatalogNeedRequest{Id: "blog", Need: "db", BackupId: one.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	change := restored.Msg.GetChange()
	step := change.GetPlan().GetServers()[0].GetSteps()[0].GetRestore()
	if step.GetDatabaseName() != "blog" || len(step.GetStops()) != 0 {
		t.Errorf("the plan says %v", step)
	}
	applyReviewed(t, ch, change)
	pg.mu.Lock()
	loaded := pg.loaded["blog"]
	said := strings.Join(pg.said, "\n")
	pg.mu.Unlock()
	if !strings.Contains(loaded, "CREATE TABLE posts") || !strings.Contains(loaded, `SET ROLE "blog";`) {
		t.Errorf("the blog's database loaded %q", loaded)
	}
	if out, in := strings.Index(said, `ALTER ROLE "blog" NOLOGIN`), strings.LastIndex(said, `ALTER ROLE "blog" LOGIN;`); out < 0 || in < out {
		t.Error("the blog's user wasn't kept out while it loaded, then let in")
	}

	// The blog's developer puts its database on a schedule of its own, in the blog's
	// project, which dumps it alone each day and keeps the newest one.
	daily := propose(t, ch, "Back up the blog's database daily", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "backup.policy", Scope: appEnv, Name: "db-daily"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupPolicy{BackupPolicy: &resourcev1.BackupPolicy{
			Link: "db", Every: resourcev1.BackupPolicy_EVERY_DAY, At: "03:00", Keep: 1,
		}}},
	})
	if done := apply(t, ch, daily); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the schedule ended %v: %s", done.GetState(), done.GetError())
	}
	runs := f.server.backups
	now := time.Now()
	listed := func() []*dockwellv1.Backup {
		t.Helper()
		list, err := cat.ListCatalogNeedBackups(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogNeedBackupsRequest{Id: "blog", Need: "db"}))
		if err != nil {
			t.Fatal(err)
		}
		return list.Msg.GetBackups()
	}
	seen := ""
	for day := 1; day <= 2; day++ {
		runs.Now = func() time.Time { return now.Add(time.Duration(day) * 25 * time.Hour) }
		if err := runs.RunDue(t.Context()); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			scheduled := slices.DeleteFunc(listed(), func(b *dockwellv1.Backup) bool { return b.GetPolicyId() == "" })
			if len(scheduled) == 1 && scheduled[0].GetId() != seen && scheduled[0].GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE && scheduled[0].GetDatabaseName() == "blog" {
				// The day before's went, as the schedule keeps one.
				seen = scheduled[0].GetId()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("day %d, the blog's scheduled backups are %v", day, scheduled)
			}
		}
	}

	// An agent from before backs up the whole volume, whatever it's asked: neither
	// Back up now nor the schedule asks it (the run's error is the one given before
	// asking), the run is recorded as failed, and the day before's stays, as a run
	// that fails takes nothing away.
	server := f.lastDocker
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	server.hello = func(h *agentv1.Hello) { h.OneDatabaseBackups = false }
	server.connect(t, f)
	if _, err := cat.BackUpCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.BackUpCatalogNeedRequest{Id: "blog", Need: "db"})); reasonOf(err) != "agent_too_old" ||
		!strings.Contains(err.Error(), "hz-1's agent backs up a whole volume, not one database alone") {
		t.Errorf("backing up the blog's database on an agent from before = %v", err)
	}
	runs.Now = func() time.Time { return now.Add(3 * 25 * time.Hour) }
	if err := runs.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		scheduled := slices.DeleteFunc(listed(), func(b *dockwellv1.Backup) bool { return b.GetPolicyId() == "" })
		failed := slices.ContainsFunc(scheduled, func(b *dockwellv1.Backup) bool {
			return b.GetState() == dockwellv1.BackupState_BACKUP_STATE_FAILED && strings.Contains(b.GetError(), "backs up a whole volume")
		})
		kept := slices.ContainsFunc(scheduled, func(b *dockwellv1.Backup) bool {
			return b.GetId() == seen && b.GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE
		})
		if failed && kept {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("on an agent from before, the blog's scheduled backups are %v", scheduled)
		}
	}
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	server.hello = nil
	server.connect(t, f)
	runs.Now = nil

	// An upgrade of the blog, which changes its project and so is an admin's, dumps its
	// database on data's server alone first, for the blog, which lists it.
	hold(onInstall(store.RoleAdmin))
	if list, err := cat.ListCatalogNeedBackups(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogNeedBackupsRequest{Id: "blog", Need: "db"})); err != nil ||
		len(list.Msg.GetBackups()) == 0 || list.Msg.GetBackups()[0].GetEnvironmentId() != dbEnv || list.Msg.GetBackups()[0].GetServerName() == "" {
		t.Errorf("an admin lists the blog's database's backups as %v, %v", list, err)
	}
	next := strings.NewReplacer(`release: 1`, `release: 2`, `version: "1.0"`, `version: "1.1"`, "blog:1@sha256:"+digest1, "blog:1@sha256:"+digest2).Replace(blogEntry)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest2] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	up, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var first *dockwellv1.BackupFirst
	for _, sp := range up.Msg.GetChange().GetPlan().GetServers() {
		for _, step := range sp.GetSteps() {
			if step.GetResource().GetScope() == dbEnv && step.GetBackupFirst() != nil {
				first = step.GetBackupFirst()
			}
		}
	}
	if first.GetDatabaseName() != "blog" || first.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_DUMP {
		t.Fatalf("the upgrade dumps first %v", first)
	}
	if notes := notesOf(up.Msg.GetChange(), "backup_first"); len(notes) != 1 || !strings.Contains(notes[0], "dumps its database alone where another project runs it") {
		t.Errorf("the upgrade says %q", notes)
	}
	if done := apply(t, ch, up.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", done.GetState(), done.GetError())
	}
	if !slices.ContainsFunc(listed(), func(b *dockwellv1.Backup) bool {
		return b.GetId() == first.GetBackupId() && b.GetDatabaseName() == "blog"
	}) {
		t.Errorf("the blog's backups don't list the dump before its upgrade: %v", listed())
	}

	// An app not from the catalog, in a project of its own linked to a database of its
	// own on data's server, updated to a newer image, by hand as an automatic update
	// would, dumps the database it reads alone first too.
	apply(t, ch, propose(t, ch, "Add the tools project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "tools"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Tools"}}},
	}))
	var toolsEnv string
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		org, _ := tx.Organization(t.Context())
		p, err := tx.GetResource(t.Context(), "project", org.ID, "tools")
		if err != nil {
			return err
		}
		env, err := tx.GetResource(t.Context(), "environment", p.ID, "production")
		toolsEnv = env.ID
		return err
	})
	protectEnv(t, ch, f, toolsEnv, resourcev1.Protection_PROTECTION_OPEN)
	apply(t, ch, propose(t, ch, "Run a worker on the shared database",
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: dbEnv, Name: "TOOLS_DB_PASSWORD"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "tools-own"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "database.user", Scope: dbEnv, Name: "tools"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: &resourcev1.DatabaseUser{Service: "db", Password: "TOOLS_DB_PASSWORD"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: toolsEnv, Name: "db"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Database", Service: "db", MeetsEnvironment: dbEnv, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, Version: "17",
				Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
				DatabaseName: "tools", User: "tools",
				Sets: []*resourcev1.LinkSet{{Name: "DB_PASSWORD", Part: resourcev1.LinkPart_LINK_PART_PASSWORD}},
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: toolsEnv, Name: "DB_PASSWORD"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: dbEnv, Secret: true, Name: "TOOLS_DB_PASSWORD", Link: "db"}},
			}}}},
		},
		// A second database of its own on the same server, which it reads too.
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: dbEnv, Name: "TOOLS_LOGS_PASSWORD"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "tools-logs-own"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "database.user", Scope: dbEnv, Name: "tools_logs"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: &resourcev1.DatabaseUser{Service: "db", Password: "TOOLS_LOGS_PASSWORD"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: toolsEnv, Name: "logs"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Logs", Service: "db", MeetsEnvironment: dbEnv, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, Version: "17",
				Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
				DatabaseName: "tools_logs", User: "tools_logs",
				Sets: []*resourcev1.LinkSet{{Name: "LOGS_PASSWORD", Part: resourcev1.LinkPart_LINK_PART_PASSWORD}},
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: toolsEnv, Name: "LOGS_PASSWORD"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: dbEnv, Secret: true, Name: "TOOLS_LOGS_PASSWORD", Link: "logs"}},
			}}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: toolsEnv, Name: "worker"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4"}},
				Secrets: map[string]string{"WORKER_DB_PASSWORD": "DB_PASSWORD", "WORKER_LOGS_PASSWORD": "LOGS_PASSWORD"},
			}}},
		},
	))
	e.Registry["ghcr.io/acme/shop:1.5"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	updated, err := svc.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: "worker", Project: "tools", Image: "ghcr.io/acme/shop:1.5"}))
	if err != nil {
		t.Fatal(err)
	}
	var dumped *dockwellv1.BackupFirst
	for _, sp := range updated.Msg.GetChange().GetPlan().GetServers() {
		for _, step := range sp.GetSteps() {
			if step.GetResource().GetScope() == dbEnv && step.GetBackupFirst() != nil {
				dumped = step.GetBackupFirst()
			}
		}
	}
	// Both its databases on that server, each alone, in the one step, each under a
	// backup ID of its own.
	if dumped == nil || len(dumped.GetAlso()) != 1 || dumped.GetAlso()[0].GetBackupId() == dumped.GetBackupId() ||
		!slices.Equal(slices.Sorted(slices.Values([]string{dumped.GetDatabaseName(), dumped.GetAlso()[0].GetDatabaseName()})), []string{"tools", "tools_logs"}) {
		t.Fatalf("the worker's update dumps first %v", dumped)
	}
	if notes := strings.Join(notesOf(updated.Msg.GetChange(), "backup_first_database"), "\n"); !strings.Contains(notes, "dumps the database tools and tools_logs alone first") {
		t.Errorf("the worker's update says %q", notes)
	}
	if done := apply(t, ch, updated.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the worker's update ended %v: %s", done.GetState(), done.GetError())
	}
	for _, d := range append([]*dockwellv1.DatabaseDump{{BackupId: dumped.GetBackupId(), DatabaseName: dumped.GetDatabaseName()}}, dumped.GetAlso()...) {
		bs, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{VolumeId: volumeID, DatabaseName: d.GetDatabaseName()}))
		if err != nil || !slices.ContainsFunc(bs.Msg.GetBackups(), func(b *dockwellv1.Backup) bool { return b.GetId() == d.GetBackupId() }) {
			t.Errorf("the dump of %s before the worker's update isn't kept: %v, %v", d.GetDatabaseName(), bs, err)
		}
	}

	// Data's developer alone can't restore the whole server the blog shares.
	hold(store.Grant{ProjectID: envProject(t, f, dbEnv), Role: store.RoleDeveloper})
	_, err = bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: whole.Msg.GetBackup().GetId(), Confirm: "db"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "takes back what 2 apps of other projects wrote to it since") {
		t.Errorf("data's developer restoring the shared server: %v", err)
	}
	// An admin types its name, and the plan names what the blog loses.
	hold(onInstall(store.RoleAdmin))
	_, err = bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: whole.Msg.GetBackup().GetId()}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "Restoring db whole takes back what blog and tools wrote to it since") || !strings.Contains(err.Error(), "Type db to confirm") {
		t.Errorf("restoring the shared server unconfirmed: %v", err)
	}
	confirmed, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: whole.Msg.GetBackup().GetId(), Confirm: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	if notes := notesOf(confirmed.Msg.GetChange(), "restore_linked_apps"); len(notes) != 1 || !strings.Contains(notes[0], "Restoring db whole takes back what blog and tools wrote to it since") {
		t.Errorf("the whole restore's plan says %q", notes)
	}
	// And a backup of the blog's database alone isn't restored that way.
	if _, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: one.GetId()})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("restoring the blog's database through the server: %v", err)
	}
}

// protectEnv sets an environment's protection, as its owner.
func protectEnv(t *testing.T, ch dockwellv1connect.ChangeServiceClient, f *fullServer, env string, p resourcev1.Protection) {
	t.Helper()
	var name, project string
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(t.Context(), env)
		if err == nil {
			name, project = r.Name, r.Scope
		}
		return err
	})
	apply(t, ch, propose(t, ch, "Protect "+name, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: project, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{Protection: p}}},
	}))
}
