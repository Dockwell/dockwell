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
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/service"
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

// A restore into a volume that binds the system takes what any plan as good as root
// takes: a person, the plan's fingerprint and a passkey, for that change. It can bring
// back whoever could log in when the backup was taken. Its undo asks the same.
func TestARestoreIntoTheSystemTakesAPasskey(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	token := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)

	made := proposeEdit(t, c, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(volume.Kind), Name: "system"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": "/etc"}}}},
	})
	applyAsRoot(t, b, made)
	taken, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: made.GetPlan().GetResources()[0].GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitBackup(t, bc, taken.Msg.GetBackup().GetId())
	if backup.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
		t.Fatalf("backup = %v", backup)
	}
	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	asks := func(ch *dockwellv1.Change) {
		t.Helper()
		fingerprint := ch.GetPlan().GetFingerprint()
		if err := askedBy(t, token, ch, fingerprint); reasonOf(err) != "needs_session" {
			t.Errorf("a token applying it = %v", err)
		}
		if err := askedBy(t, c, ch, ""); reasonOf(err) != "needs_confirmation" {
			t.Errorf("with no fingerprint = %v", err)
		}
		if err := askedBy(t, c, ch, fingerprint); reasonOf(err) != "needs_step_up" {
			t.Errorf("with no passkey = %v", err)
		}
		if !slices.ContainsFunc(ch.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
			return n.GetReason() == "new_code_as_root" && strings.Contains(n.GetMessage(), "what a backup brings back")
		}) {
			t.Errorf("the plan doesn't say why it asks: %v", ch.GetPlan().GetNotes())
		}
	}
	ch := planned.Msg.GetChange()
	asks(ch)
	applyAsRoot(t, b, ch)

	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	asks(undo.Msg.GetChange())
}

// A backup is tied to a volume by name alone. So a backup of what bound the system,
// restored into an ordinary volume made under its name once the first had gone, can
// be read by anything that mounts it. It takes a passkey, as what it came from did.
func TestABackupOfTheSystemAsksAgainInAnOrdinaryVolume(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	edit := func(spec *resourcev1.Volume) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(volume.Kind), Name: "shared"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: spec}},
		}
	}
	made := proposeEdit(t, c, edit(&resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": "/etc"}}))
	applyAsRoot(t, b, made)
	res := made.GetPlan().GetResources()[0]
	taken, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: res.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitBackup(t, bc, taken.Msg.GetBackup().GetId())

	gone := propose(t, c, "Delete it", &dockwellv1.Edit{Resource: res.GetResource(), Delete: true})
	applyConfirmed(t, c, gone)
	// The server keeps what was deleted for 7 days, and it can't come back as anything
	// else in that time. This is that time passing.
	kept := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: res.GetResource().GetScope(), Name: "shared"}).Name
	if err := e.Client().RemoveVolume(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	again := proposeEdit(t, c, edit(&resourcev1.Volume{}))
	if again.GetPlan().GetHostAccess() {
		t.Fatalf("an ordinary volume needs a person: %v", again.GetPlan())
	}
	if got := apply(t, c, again); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("making it again: %s", got.GetError())
	}

	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	ch := planned.Msg.GetChange()
	if err := askedBy(t, c, ch, ch.GetPlan().GetFingerprint()); reasonOf(err) != "needs_step_up" {
		t.Errorf("with no passkey = %v", err)
	}
	if !slices.ContainsFunc(ch.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "new_code_as_root" && strings.Contains(n.GetMessage(), "This backup was taken when shared reached")
	}) {
		t.Errorf("the plan doesn't say why it asks: %v", ch.GetPlan().GetNotes())
	}
	if !slices.ContainsFunc(ch.GetPlan().GetResources()[0].GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
		return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_BACKUP_OF && a.GetPath() == "/etc" && a.GetSameAsRoot()
	}) {
		t.Errorf("the plan says it reaches %v", ch.GetPlan().GetResources()[0].GetHostAccess())
	}
	applyAsRoot(t, b, ch)
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

// policyEdit is a daily policy on the volume data that keeps keep backups.
// policyEdit is a nightly policy of the volume data that checks no restore, so a test
// of its runs meets no drill (ADR 0068).
func policyEdit(keep uint32) *dockwellv1.Edit {
	e := drillingPolicyEdit(keep)
	e.GetSpec().GetBackupPolicy().Drill = resourcev1.BackupPolicy_DRILL_NEVER
	return e
}

// drillingPolicyEdit is a nightly policy that leaves its restore check to the default.
func drillingPolicyEdit(keep uint32) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.Kind), Name: "data-daily"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupPolicy{BackupPolicy: &resourcev1.BackupPolicy{Volume: "data", Keep: keep, At: "03:00"}}},
	}
}

// A schedule is a plan, its runs are Dockwell's, and it keeps what it says it keeps,
// never what someone took by hand, and nothing when a run fails.
func TestAPolicyRunsAndKeepsWhatItSays(t *testing.T) {
	storetest.Each(t, testAPolicyRunsAndKeepsWhatItSays)
}

func testAPolicyRunsAndKeepsWhatItSays(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")

	planned := propose(t, c, "Back up data each night", policyEdit(2))
	if len(planned.GetPlan().GetServers()) != 0 {
		t.Errorf("a policy planned steps on servers: %v", planned.GetPlan().GetServers())
	}
	if done := apply(t, c, planned); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the policy ended %v: %v", done.GetState(), done.GetError())
	}

	byHand, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	waitBackup(t, bc, byHand.Msg.GetBackup().GetId())

	h := f.server.backups
	now := time.Now()
	if err := h.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := backupsOf(t, bc); len(got) != 1 {
		t.Fatalf("a new policy ran at once: %v", got)
	}
	day := func(n int) {
		t.Helper()
		h.Now = func() time.Time { return now.Add(time.Duration(n) * 25 * time.Hour) }
		if err := h.RunDue(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Once a day: the same moment runs nothing twice.
		if err := h.RunDue(t.Context()); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			got := backupsOf(t, bc)
			// A run shows as done a moment before what it pushes out has gone.
			kept := 0
			for _, one := range got {
				if one.GetPolicyId() != "" && one.GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE {
					kept++
				}
			}
			if kept <= 2 && !slices.ContainsFunc(got, func(b *dockwellv1.Backup) bool { return b.GetState() == dockwellv1.BackupState_BACKUP_STATE_RUNNING }) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("still running: %v", got)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	day(1)
	day(2)
	day(3)
	all := backupsOf(t, bc)
	if len(all) != 3 {
		t.Fatalf("after 3 runs, keeping 2, there are %d: %v", len(all), all)
	}
	var fromPolicy int
	for _, one := range all {
		if one.GetPolicyId() != "" {
			fromPolicy++
			if one.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
				t.Errorf("a scheduled backup's actor = %v", one.GetActor())
			}
		}
	}
	if fromPolicy != 2 || !slices.ContainsFunc(all, func(b *dockwellv1.Backup) bool { return b.GetId() == byHand.Msg.GetBackup().GetId() }) {
		t.Errorf("the policy took what someone took by hand, or keeps %d: %v", fromPolicy, all)
	}
	policies, err := bc.ListBackupPolicies(t.Context(), connect.NewRequest(&dockwellv1.ListBackupPoliciesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := policies.Msg.GetPolicies(); len(got) != 1 || got[0].GetKept() != 2 || got[0].GetLast() == nil || got[0].GetNextRun() == nil || got[0].GetSpec().GetVolume() != "data" {
		t.Errorf("policies = %v", got)
	}

	// Failing takes nothing away, and says so where someone looks.
	f.lastDocker.backups.Binary = filepath.Join(t.TempDir(), "gone")
	day(4)
	day(5)
	all = backupsOf(t, bc)
	var good, bad int
	for _, one := range all {
		switch {
		case one.GetPolicyId() == "":
		case one.GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE:
			good++
		case one.GetState() == dockwellv1.BackupState_BACKUP_STATE_FAILED:
			bad++
		}
	}
	if good != 2 || bad != 2 {
		t.Errorf("after 2 failed runs: %d good and %d failed: %v", good, bad, all)
	}
	events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
	if err != nil {
		t.Fatal(err)
	}
	if first := events.Msg.GetEvents()[0]; first.GetAction() != "backup.failed" || first.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL || first.GetSurface() != dockwellv1.Surface_SURFACE_AUTOMATIC {
		t.Errorf("the newest event = %v", first)
	}

	// A server that isn't there fails its run, out loud.
	f.lastDocker.stop()
	waitConnected(t, f, f.lastDocker.id.ServerID, false)
	day(6)
	newest := backupsOf(t, bc)[0]
	if newest.GetState() != dockwellv1.BackupState_BACKUP_STATE_FAILED || !strings.Contains(newest.GetError(), "wasn't connected") {
		t.Errorf("with its server away: %v", newest)
	}

	// A volume a policy backs up can't go until the policy does.
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Remove", Edits: []*dockwellv1.Edit{
		{Resource: &dockwellv1.ResourceRef{Kind: string(volume.Kind), Name: "data"}, Delete: true},
		{Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Name: "web"}, Delete: true},
	}})); err == nil || !strings.Contains(err.Error(), "data-daily") {
		t.Errorf("a volume with a policy on it went: %v", err)
	}
}

func backupsOf(t *testing.T, bc dockwellv1connect.BackupServiceClient) []*dockwellv1.Backup {
	t.Helper()
	res, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetBackups()
}

func destinationEdit(bucket *s3test.Server, secret string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
			Endpoint: bucket.URL(), Bucket: "acme-backups", AccessKey: "AKIAEXAMPLE", SecretKey: secret, Passphrase: "a long passphrase of our own",
		}}},
	}
}

// waitCopy waits for a backup, and for what became of its copy.
func waitCopy(t *testing.T, bc dockwellv1connect.BackupServiceClient, id string) *dockwellv1.Backup {
	t.Helper()
	var last *dockwellv1.Backup
	eventually(t, "a copy to land or fail", func() bool {
		res, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		last = res.Msg.GetBackup()
		return last.GetState() != dockwellv1.BackupState_BACKUP_STATE_RUNNING && last.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_PENDING
	})
	return last
}

// What signs in to a destination goes to a server for one call, sealed, and is in no
// plan, no answer and no row. A backup survives its server through it, and a person
// gets it back.
func TestABackupOutlivesItsServerThroughADestination(t *testing.T) {
	storetest.Each(t, testABackupOutlivesItsServerThroughADestination)
}

func testABackupOutlivesItsServerThroughADestination(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	bucket := s3test.New(t)
	f.lastDocker.backups.HTTP = bucket.Client()
	const secret = "wJalrXUtnFEMI-K7MDENG-the-secret"

	planned := propose(t, c, "Send backups to R2", destinationEdit(bucket, secret))
	if text := protojson.Format(planned); strings.Contains(text, secret) || strings.Contains(text, "AKIAEXAMPLE") || strings.Contains(text, "long passphrase") {
		t.Errorf("the plan holds what signs in: %s", text)
	}
	if len(planned.GetPlan().GetServers()) != 0 {
		t.Errorf("a destination planned steps on servers: %v", planned.GetPlan().GetServers())
	}
	apply(t, c, planned)

	// Setting what signs in always plans as a change, so a plan can't confirm a guess.
	same := destinationEdit(bucket, secret)
	same.GetSpec().GetBackupDestination().Passphrase = ""
	again := propose(t, c, "The same again", same)
	if len(again.GetPlan().GetResources()) != 1 || again.GetPlan().GetResources()[0].GetAction() != dockwellv1.PlanAction_PLAN_ACTION_UPDATE {
		t.Errorf("the same secret planned %v, so a plan confirms a guess", again.GetPlan().GetResources())
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: again.GetId()})); err != nil {
		t.Fatal(err)
	}

	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Stop: true, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitCopy(t, bc, made.Msg.GetBackup().GetId())
	if backup.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED || backup.GetDestination() != "r2" || !backup.GetOnServer() {
		t.Fatalf("backup = %v", backup)
	}
	listed, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := listed.Msg.GetDestinations()
	if len(got) != 1 || got[0].GetBackups() != 1 || got[0].GetUsedBytes() == 0 || got[0].GetLastUpload() == nil || got[0].GetBucket() != "acme-backups" {
		t.Errorf("destinations = %v", got)
	}
	if strings.Contains(protojson.Format(listed.Msg), secret) {
		t.Error("the list holds what signs in")
	}
	for _, auth := range bucket.Authorizations {
		if strings.Contains(auth, secret) {
			t.Error("a request carried the secret")
		}
	}

	// The server's copy goes, and it's fetched back, and restored.
	if _, err := f.lastDocker.backups.Delete(t.Context(), resource.Ref{Kind: volume.Kind, Scope: backup.GetEnvironmentId(), Name: "data"}, backup.GetId(), "", nil); err != nil {
		t.Fatal(err)
	}
	writeData(t, dir, "orders.db", "a bad migration ate these")
	if _, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()})); err == nil || !strings.Contains(err.Error(), "no longer has") {
		t.Fatalf("restoring what the server lost = %v", err)
	}
	if _, err := bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: backup.GetId()})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
		if err != nil {
			t.Fatal(err)
		}
		if len(events.Msg.GetEvents()) > 0 && events.Msg.GetEvents()[0].GetAction() == "backup.fetched" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("it wasn't fetched: %v", events.Msg.GetEvents())
		}
		time.Sleep(5 * time.Millisecond)
	}
	restore, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	ch := restore.Msg.GetChange()
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if end := waitWatch(t, w).last(); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the restore ended %v: %v", end.GetState(), end.GetError())
	}
	if got := readData(t, dir, "orders.db"); got != "the orders" {
		t.Errorf("orders.db = %q", got)
	}

	// Deleting it takes it off the bucket too.
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: backup.GetId()})); err != nil {
		t.Fatal(err)
	}
	if objects := bucket.Objects(); len(objects) != 0 {
		t.Errorf("left in the bucket: %d objects", len(objects))
	}
}

// A copy that fails leaves the backup, and says so where someone looks.
func TestACopyThatFailsShowsOnTheTimeline(t *testing.T) {
	storetest.Each(t, testACopyThatFailsShowsOnTheTimeline)
}

func testACopyThatFailsShowsOnTheTimeline(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, _ := webWithData(t, e, c)
	bucket := s3test.New(t)
	f.lastDocker.backups.HTTP = bucket.Client()
	bucket.Refuse = "AccessDenied"
	apply(t, c, propose(t, c, "Send backups to R2", destinationEdit(bucket, "a-secret-that-is-wrong")))
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitCopy(t, bc, made.Msg.GetBackup().GetId())
	if backup.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || backup.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED || !strings.Contains(backup.GetCopyError(), "AccessDenied") {
		t.Fatalf("backup = %v", backup)
	}
	events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events.Msg.GetEvents(), func(e *dockwellv1.Event) bool {
		return e.GetAction() == "backup.copy_failed" && e.GetBackup().GetDestination() == "r2" && strings.Contains(e.GetBackup().GetError(), "AccessDenied")
	}) {
		t.Errorf("the timeline says %v", events.Msg.GetEvents())
	}
}

// A destination that took a backup once, then stopped, says so: the newest that
// didn't get there, when and why, beside the last that did, so nothing reads it as
// healthy while its uploads fail. One that gets there again is newer than the failure.
func TestADestinationSaysWhenItsUploadsFail(t *testing.T) {
	storetest.Each(t, testADestinationSaysWhenItsUploadsFail)
}

func testADestinationSaysWhenItsUploadsFail(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, _ := webWithData(t, e, c)
	bucket := s3test.New(t)
	f.lastDocker.backups.HTTP = bucket.Client()
	apply(t, c, propose(t, c, "Send backups to R2", destinationEdit(bucket, "wJalrXUtnFEMI-K7MDENG-the-secret")))
	backUp := func() *dockwellv1.Backup {
		t.Helper()
		made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Destination: "r2"}))
		if err != nil {
			t.Fatal(err)
		}
		return waitCopy(t, bc, made.Msg.GetBackup().GetId())
	}
	r2 := func() *dockwellv1.BackupDestinationInfo {
		t.Helper()
		res, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Msg.GetDestinations()) != 1 {
			t.Fatalf("destinations = %v, want r2", res.Msg.GetDestinations())
		}
		return res.Msg.GetDestinations()[0]
	}

	if got := backUp(); got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("the first backup = %v, want it stored", got)
	}
	if d := r2(); d.GetLastUpload() == nil || d.GetLastFailure() != nil || d.GetLastFailureError() != "" {
		t.Errorf("after one that got there, r2 reads %v, want an upload and no failure", d)
	}

	bucket.Refuse = "AccessDenied"
	if got := backUp(); got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED {
		t.Fatalf("the refused backup = %v, want its copy failed", got)
	}
	d := r2()
	if d.GetLastFailure() == nil || !strings.Contains(d.GetLastFailureError(), "AccessDenied") || d.GetLastUpload() == nil ||
		d.GetLastFailure().AsTime().Before(d.GetLastUpload().AsTime()) {
		t.Errorf("after one that didn't get there, r2 reads %v, want a failure, with why, newer than its last upload", d)
	}

	bucket.Refuse = ""
	if got := backUp(); got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("the next backup = %v, want it stored", got)
	}
	if d := r2(); d.GetLastFailure() == nil || d.GetLastUpload().AsTime().Before(d.GetLastFailure().AsTime()) {
		t.Errorf("once one gets there again, r2 reads %v, want its last upload newer than the failure", d)
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
