// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// A backup restores into a new database as the step that makes its service does:
// the step plans the restore, loads the backup into the new volume, and only then
// starts the container, on what the backup holds. The original runs on as it was,
// the same container, never stopped. A step for a service that's here already
// doesn't restore into it (ADR 0106).
func TestABackupRestoresIntoANewDatabaseAsItsServiceIsMade(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17.2", pgImage("17"))
	client := e.Client()
	ctx := t.Context()
	const restored = "env_01restored"
	from := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	into := resource.Ref{Kind: volume.Kind, Scope: restored, Name: "data"}
	for _, v := range []resource.Ref{from, into} {
		if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{Name: volume.TargetOf(v).Name}); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(t.TempDir(), "dockwell")
	if err := os.WriteFile(helper, []byte("#!agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: 2 * time.Second, ListenWait: 2 * time.Second}
	runner := &backup.Runner{Client: client, Root: t.TempDir(), Binary: helper, Work: 1000}
	local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: rt}, Movers: runner})}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "postgres:17.2", Env: map[string]string{"POSTGRES_PASSWORD": "example"},
		Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/postgresql/data"}},
	}}}
	deploy := func(want resource.Desired) {
		t.Helper()
		steps, err := local.Plan(ctx, []resource.Desired{want})
		if err != nil {
			t.Fatal(err)
		}
		if err := local.Apply(ctx, steps[0], want.Spec); err != nil {
			t.Fatal(err)
		}
	}
	original := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	deploy(resource.Desired{Ref: original, Spec: spec})
	write := func(v resource.Ref, sql string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(e.VolumeDir(volume.TargetOf(v).Name), "data.sql"), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(v resource.Ref) string {
		data, _ := os.ReadFile(filepath.Join(e.VolumeDir(volume.TargetOf(v).Name), "data.sql"))
		return string(data)
	}
	write(from, "CREATE TABLE orders (id int);\n")
	backupID := id.New(id.Backup)
	if _, err := runner.Take(ctx, backup.Take{ID: backupID, Ref: from}); err != nil {
		t.Fatal(err)
	}
	write(from, "CREATE TABLE orders (id int);\nCREATE TABLE since (id int);\n")
	name := docker.ContainerName(volume.TargetOf(from).Project, "db")
	before, err := client.InspectContainer(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	source := &resource.RestoreSource{Backup: backupID, From: from, Change: "chg_restore"}
	// Not into the original, nor any service that's here.
	if _, err := local.Plan(ctx, []resource.Desired{{Ref: original, Spec: spec, Restore: source}}); err == nil || !strings.Contains(err.Error(), "the environment the backup was taken in") {
		t.Errorf("restoring into a service that's here = %v", err)
	}

	db := resource.Ref{Kind: service.Kind, Scope: restored, Name: "db"}
	want := resource.Desired{Ref: db, Spec: spec, Restore: source}
	steps, err := local.Plan(ctx, []resource.Desired{want})
	if err != nil {
		t.Fatal(err)
	}
	r := steps[0].Restore
	if steps[0].Action != resource.ActionCreate || r == nil || !r.IntoNew || r.Backup != backupID || r.Method != resource.BackupDump || len(r.Stops) != 0 {
		t.Fatalf("the plan is %s with %+v", steps[0].Action, r)
	}
	// What a call from the control plane carries: the step's plan without where the
	// backup is, and that in what the change restores.
	step := steps[0]
	plan := *step.Restore
	plan.From = resource.Ref{}
	step.Restore = &plan
	if err := local.Apply(ctx, step, want.Spec); err == nil || !strings.Contains(err.Error(), "Plan the restore again") {
		t.Errorf("a step that doesn't say where the backup is = %v", err)
	}
	applyCtx := resource.WithRestore(resource.WithDeploy(ctx, resource.Deploy{Revision: 1, Change: "chg_restore"}), *source)
	if err := local.Apply(applyCtx, step, want.Spec); err != nil {
		t.Fatal(err)
	}
	if got := read(into); got != "CREATE TABLE orders (id int);\n\n-- PostgreSQL database cluster dump complete\n" {
		t.Errorf("the new database holds %q", got)
	}
	made, err := client.InspectContainer(ctx, docker.ContainerName(volume.TargetOf(into).Project, "db"))
	if err != nil || !made.State.Running {
		t.Errorf("the new database = %+v, %v", made, err)
	}
	after, err := client.InspectContainer(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.State.StartedAt != before.State.StartedAt || !after.State.Running || e.Started(name) != 1 {
		t.Errorf("the original's container went from %s at %s to %s at %s, started %d times", before.ID, before.State.StartedAt, after.ID, after.State.StartedAt, e.Started(name))
	}
	if got := read(from); !strings.Contains(got, "since") {
		t.Errorf("the original holds %q", got)
	}

	// Its note is gone once the step made the database, so nothing reads it after.
	note := filepath.Join(runner.Root, restored, "data", "restore-into-chg_restore.json")
	if _, err := os.Stat(note); !os.IsNotExist(err) {
		t.Errorf("the restore's note is left after its step: %v", err)
	}

	// A resume of the change, once its step made the new database, as a restart of
	// the control plane or a dropped agent leaves it: the step goes on where the
	// restore finished, and nothing loads again (the review's M1).
	made, err = client.InspectContainer(ctx, docker.ContainerName(volume.TargetOf(into).Project, "db"))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := local.Plan(ctx, []resource.Desired{want})
	if err != nil {
		t.Fatalf("resuming after the restore finished: %v", err)
	}
	if r := resumed[0].Restore; r == nil || !r.IntoNew || resumed[0].Action == resource.ActionCreate {
		t.Fatalf("the resume plans %s with %+v", resumed[0].Action, r)
	}
	if resumed[0].Action != resource.ActionNone {
		if err := local.Apply(applyCtx, resumed[0], want.Spec); err != nil {
			t.Errorf("going on after a finished restore: %v", err)
		}
	}
	if again, err := client.InspectContainer(ctx, made.ID); err != nil || !again.State.Running || !again.State.StartedAt.Equal(made.State.StartedAt) {
		t.Errorf("the resume touched the new database: %+v, %v", again, err)
	}

	// Where its note says the load hadn't finished, the step is applied to fail, and
	// says so, so the change rolls back.
	if err := os.WriteFile(note, []byte(`{"change":"chg_restore","stage":"filling"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, err = local.Plan(ctx, []resource.Desired{want})
	if err != nil {
		t.Fatalf("resuming after a restore cut short: %v", err)
	}
	if resumed[0].Action == resource.ActionNone || resumed[0].Restore == nil {
		t.Fatalf("the resume of an unfinished restore plans %s with %+v, which wouldn't fail", resumed[0].Action, resumed[0].Restore)
	}
	err = local.Apply(applyCtx, resumed[0], want.Spec)
	if plain, _ := resource.Explain(err); err == nil || !strings.Contains(plain, "didn't finish loading into db before Dockwell stopped, so this change rolls back") {
		t.Errorf("going on after an unfinished restore = %v (%q)", err, plain)
	}
	// Another change's restore never counts as this one's.
	other := *source
	other.Change = "chg_other"
	if _, err := local.Plan(ctx, []resource.Desired{{Ref: db, Spec: spec, Restore: &other}}); err == nil || !strings.Contains(err.Error(), "is here already") {
		t.Errorf("another change restoring into the new database = %v", err)
	}
	if got := read(from); !strings.Contains(got, "since") {
		t.Errorf("the original holds %q", got)
	}
}
