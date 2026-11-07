// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"io"
	"net"
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
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func backupClient(c connect.HTTPClient, origin string) dockwellv1connect.BackupServiceClient {
	return dockwellv1connect.NewBackupServiceClient(c, origin, connect.WithProtoJSON())
}

// webWithData runs a service with a volume, and says the volume's ID and where
// its bytes are.
func webWithData(t *testing.T, e *dockertest.Engine, c dockwellv1connect.ChangeServiceClient) (string, string) {
	t.Helper()
	web := runImage("ghcr.io/acme/shop:1.4")
	web.GetSpec().GetService().GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/srv/data"}}
	done := apply(t, c, propose(t, c, "Run web", web, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(volume.Kind), Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{}}},
	}))
	var id string
	var ref resource.Ref
	for _, r := range done.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(volume.Kind) {
			id = r.GetId()
			ref = resource.Ref{Kind: volume.Kind, Scope: r.GetResource().GetScope(), Name: "data"}
		}
	}
	return id, e.VolumeDir(volume.TargetOf(ref).Name)
}

func waitBackup(t *testing.T, bc dockwellv1connect.BackupServiceClient, id string) *dockwellv1.Backup {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if b := res.Msg.GetBackup(); b.GetState() != dockwellv1.BackupState_BACKUP_STATE_RUNNING {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s is still running", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeData(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readData(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The heart of it, end to end through the API and the agent stream: back up, lose
// the data, restore it through a plan that says what it replaces, and undo that.
func TestABackupRestoresThroughAPlan(t *testing.T) {
	storetest.Each(t, testABackupRestoresThroughAPlan)
}

func testABackupRestoresThroughAPlan(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")

	preview, err := bc.PreviewBackup(t.Context(), connect.NewRequest(&dockwellv1.PreviewBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	if p := preview.Msg; p.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_COPY_RUNNING || !slices.Equal(p.GetRunning(), []string{"web"}) {
		t.Errorf("preview = %v", p)
	}

	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Stop: true}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitBackup(t, bc, made.Msg.GetBackup().GetId())
	if backup.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || backup.GetSizeBytes() == 0 || backup.GetSha256() == "" ||
		backup.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_COPY || backup.GetVolume() != "data" || backup.GetServerName() != "hz-1" || backup.GetFinishedAt() == nil {
		t.Fatalf("backup = %v", backup)
	}
	listed, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetBackups()) != 1 || listed.Msg.GetBackups()[0].GetActor() == nil {
		t.Errorf("list = %v", listed.Msg)
	}

	writeData(t, dir, "orders.db", "a bad migration ate these")

	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	ch := planned.Msg.GetChange()
	if !ch.GetPlan().GetDestructive() {
		t.Error("a restore doesn't say it destroys data")
	}
	steps := ch.GetPlan().GetServers()[0].GetSteps()
	if len(steps) != 1 {
		t.Fatalf("steps = %v", steps)
	}
	step := steps[0]
	r := step.GetRestore()
	if r.GetBackupId() != backup.GetId() || r.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_COPY || r.GetSizeBytes() != backup.GetSizeBytes() ||
		!slices.Equal(r.GetStops(), []string{"web"}) || r.GetTakenAt() == nil {
		t.Errorf("the plan says %v", r)
	}
	if !step.GetDestroysData() || step.GetDowntime() == nil {
		t.Errorf("the step = %v", step)
	}

	// A token can plan it, and never apply it.
	token := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	if _, err := changeClient(token, b.origin).ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); reasonOf(err) != authapi.ReasonNeedsSession {
		t.Fatalf("a token applied a restore: %v", err)
	}
	// A person has to say which plan they reviewed.
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId()})); reasonOf(err) != "needs_confirmation" {
		t.Fatalf("a restore applied without its fingerprint: %v", err)
	}
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	done := waitWatch(t, w).last()
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the restore ended %v: %v", done.GetState(), done.GetError())
	}
	if got := readData(t, dir, "orders.db"); got != "the orders" {
		t.Errorf("orders.db = %q", got)
	}

	recent, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := recent.Msg.GetChanges()[0]; got.GetId() != ch.GetId() || got.GetUndoDeadline() == nil {
		t.Errorf("a restore offers no undo: %v", got)
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	u := undo.Msg.GetChange()
	if got := u.GetPlan().GetServers()[0].GetSteps()[0].GetRestore().GetUndoes(); got != ch.GetId() {
		t.Errorf("the undo says it undoes %q", got)
	}
	w = watch(t, c, u.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: u.GetId(), Fingerprint: u.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if end := waitWatch(t, w).last(); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %v", end.GetState(), end.GetError())
	}
	if got := readData(t, dir, "orders.db"); got != "a bad migration ate these" {
		t.Errorf("orders.db after undoing = %q", got)
	}
}

// Who may do what, as ADR 0033 has it.
func TestWhoMayBackUpAndDelete(t *testing.T) { storetest.Each(t, testWhoMayBackUpAndDelete) }

func testWhoMayBackUpAndDelete(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")

	read := backupClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	write := backupClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)

	if _, err := read.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a read-only token took a backup: %v", err)
	}
	made, err := write.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatalf("a read-write token can't take a backup: %v", err)
	}
	backup := waitBackup(t, read, made.Msg.GetBackup().GetId())
	if backup.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_COPY_RUNNING {
		t.Errorf("method = %v", backup.GetMethod())
	}
	if _, err := read.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a read-only token planned a restore: %v", err)
	}
	if _, err := write.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: backup.GetId()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a token deleted a backup: %v", err)
	}
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: backup.GetId()})); err != nil {
		t.Fatalf("a person can't delete a backup: %v", err)
	}
	if _, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: backup.GetId()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("it's still on record: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(f.lastDocker.backups.Root, "*", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("it's still on the server: %v", entries)
	}
}

// A backup that fails is on the timeline, and says why.
func TestAFailedBackupShowsOnTheTimeline(t *testing.T) {
	storetest.Each(t, testAFailedBackupShowsOnTheTimeline)
}

func testAFailedBackupShowsOnTheTimeline(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, _ := webWithData(t, e, c)
	good := f.lastDocker.backups.Binary
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitBackup(t, bc, made.Msg.GetBackup().GetId()); got.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
		t.Fatalf("backup = %v", got)
	}
	f.lastDocker.backups.Binary = filepath.Join(t.TempDir(), "gone")
	made, err = bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	failed := waitBackup(t, bc, made.Msg.GetBackup().GetId())
	f.lastDocker.backups.Binary = good
	if failed.GetState() != dockwellv1.BackupState_BACKUP_STATE_FAILED || failed.GetError() == "" {
		t.Fatalf("backup = %v", failed)
	}
	res, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range res.Msg.GetEvents() {
		actions = append(actions, e.GetAction())
		if e.GetKind() != dockwellv1.EventKind_EVENT_KIND_BACKUP || e.GetBackup().GetVolume() != "data" || e.GetBackup().GetServerName() != "hz-1" || e.GetActor() == nil {
			t.Errorf("event = %v", e)
		}
	}
	if !slices.Equal(actions, []string{"backup.failed", "backup.taken"}) {
		t.Errorf("the timeline says %v", actions)
	}
	if got := res.Msg.GetEvents()[0].GetBackup().GetError(); !strings.Contains(got, "agent's own binary") {
		t.Errorf("the failure says %q", got)
	}
	// By its volume too, as a service's page reads it.
	scoped, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{ResourceId: volumeID, Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Msg.GetEvents()) != 2 {
		t.Errorf("by its volume, the timeline has %v", scoped.Msg.GetEvents())
	}
}

// A database's dump stops nothing, and a restore of one takes in the app.
func TestADumpThroughTheAPI(t *testing.T) { storetest.Each(t, testADumpThroughTheAPI) }

func testADumpThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	bc := backupClient(b, b.origin)
	e.Registry["postgres:17"] = fakePostgres
	c := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	done := apply(t, ch, importCompose(t, c, shopStack, map[string]string{"DB_PASSWORD": "pw"}, false))
	var volumeID string
	var ref resource.Ref
	for _, r := range done.GetPlan().GetResources() {
		if r.GetResource().GetKind() == string(volume.Kind) {
			volumeID, ref = r.GetId(), resource.Ref{Kind: volume.Kind, Scope: r.GetResource().GetScope(), Name: r.GetResource().GetName()}
		}
	}
	dir := e.VolumeDir(volume.TargetOf(ref).Name)
	writeData(t, dir, "data.sql", "the orders")
	project := docker.Project(ref.Scope)
	starts := e.Started(docker.ContainerName(project, "db"))

	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitBackup(t, bc, made.Msg.GetBackup().GetId())
	if backup.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_DUMP || backup.GetDatabase() != "PostgreSQL" {
		t.Fatalf("backup = %v", backup)
	}
	if got := e.Started(docker.ContainerName(project, "db")); got != starts {
		t.Errorf("a dump restarted the database: %d, was %d", got, starts)
	}

	writeData(t, dir, "data.sql", "gone")
	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	change := planned.Msg.GetChange()
	r := change.GetPlan().GetServers()[0].GetSteps()[0].GetRestore()
	if !slices.Equal(r.GetStops(), []string{"db", "web", "worker"}) || r.GetDatabase() != "PostgreSQL" {
		t.Errorf("the plan says %v", r)
	}
	w := watch(t, ch, change.GetId())
	if _, err := ch.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if end := waitWatch(t, w).last(); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the restore ended %v: %v", end.GetState(), end.GetError())
	}
	if got := readData(t, dir, "data.sql"); !strings.Contains(got, "the orders") {
		t.Errorf("the database holds %q", got)
	}
	for _, service := range []string{"db", "web", "worker"} {
		if c := e.Container(docker.ContainerName(project, service)); c == nil || !c.State.Running {
			t.Errorf("%s isn't running", service)
		}
	}
}

// fakePostgres keeps its data as one file of SQL, and answers as the official
// image's tools do.
var fakePostgres = dockertest.Image{
	Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}},
	Exec: func(x dockertest.Exec) int {
		file := filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql")
		script := strings.Join(x.Cmd, " ")
		switch {
		case strings.Contains(script, "pg_dumpall"):
			data, err := os.ReadFile(file)
			if err != nil {
				return 1
			}
			_, _ = x.Stdout.Write(data)
			_, _ = io.WriteString(x.Stdout, "\n-- PostgreSQL database cluster dump complete\n")
		case strings.Contains(script, "psql"):
			data, err := io.ReadAll(x.Stdin)
			if err != nil || os.WriteFile(file, data, 0o600) != nil {
				return 1
			}
		}
		return 0
	},
}

// A backup can't be left waiting on a control plane that's gone: the next start fails
// it. And a stop as it starts lets go of the addresses a restart needs.
func TestAStartEndsWhatWasRunningAndAStopThenLetsGoOfItsAddresses(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, _ := webWithData(t, e, c)
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Stop: true}))
	if err != nil {
		t.Fatal(err)
	}
	id := made.Msg.GetBackup().GetId()
	waitBackup(t, bc, id)
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		row, err := tx.GetBackup(t.Context(), id)
		if err != nil {
			return err
		}
		row.State, row.FinishedAt = store.BackupRunning, nil
		return tx.FinishBackup(t.Context(), row)
	}); err != nil {
		t.Fatal(err)
	}
	f.server.backups.Recover(t.Context())
	if got := waitBackup(t, bc, id); got.GetState() != dockwellv1.BackupState_BACKUP_STATE_FAILED || !strings.Contains(got.GetError(), "isn't known to be whole") {
		t.Errorf("a backup left running = %v", got)
	}

	if err := f.stop(); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	api, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.server.Run(stopped, Listeners{API: api, Agents: agents}); err != nil {
		t.Error(err)
	}
	for _, ln := range []net.Listener{api, agents} {
		again, err := lc.Listen(t.Context(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("a stop as it starts kept its address: %v", err)
		}
		_ = again.Close()
	}
}
