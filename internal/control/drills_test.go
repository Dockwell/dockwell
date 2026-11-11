// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// waitDrill waits for a backup's newest drill to end.
func waitDrill(t *testing.T, bc dockwellv1connect.BackupServiceClient, backup string) *dockwellv1.BackupDrill {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: backup}))
		if err != nil {
			t.Fatal(err)
		}
		if d := res.Msg.GetBackup().GetDrill(); d != nil && d.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_RUNNING {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("the drill of %s is still running", backup)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A backup is checked on demand, through the API and the agent (ADR 0068): it
// restores into a volume of the drill's own, which goes, and says so on the backup and
// the timeline. A backup that doesn't restore fails its drill, and the alert follows
// the volume's newest checked backup: an older one passing doesn't resolve it. A
// backup a drill is checking can't be deleted, and one the control plane lost track
// of isn't checked.
func TestABackupIsCheckedInADrill(t *testing.T) { storetest.Each(t, testABackupIsCheckedInADrill) }

func testABackupIsCheckedInADrill(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	volumeID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.csv", "1,2,3\n")
	take := func() *dockwellv1.Backup {
		t.Helper()
		made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
		if err != nil {
			t.Fatal(err)
		}
		got := waitBackup(t, bc, made.Msg.GetBackup().GetId())
		if got.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
			t.Fatalf("backup = %v", got)
		}
		return got
	}
	drill := func(backup string) *dockwellv1.BackupDrill {
		t.Helper()
		res, err := bc.DrillBackup(t.Context(), connect.NewRequest(&dockwellv1.DrillBackupRequest{Id: backup}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Msg.GetDrill().GetResult() != dockwellv1.DrillResult_DRILL_RESULT_RUNNING || res.Msg.GetDrill().GetActor() == nil {
			t.Errorf("DrillBackup answered %v", res.Msg.GetDrill())
		}
		return waitDrill(t, bc, backup)
	}
	older := take()
	passed := drill(older.GetId())
	if passed.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_PASSED || !passed.GetUnpackedOnly() || passed.GetRestoredBytes() == 0 || passed.GetFinishedAt() == nil {
		t.Fatalf("the drill = %v, want passed, unpacked only", passed)
	}
	if left, err := e.Client().ListVolumes(t.Context(), "dev.dockwell.drill"); err != nil || len(left) > 0 {
		t.Errorf("the drill left volumes %v, %v", left, err)
	}

	// A newer backup whose file was damaged on the server doesn't restore.
	newer := take()
	file := filepath.Join(f.lastDocker.backups.Root, newer.GetEnvironmentId(), "data", newer.GetId()+".tar.gz")
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, make([]byte, fi.Size()), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := drill(newer.GetId())
	if failed.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_FAILED || !strings.Contains(failed.GetWhy(), "didn't unpack") {
		t.Fatalf("the drill of a damaged backup = %v, want failed", failed)
	}

	// On the timeline, with how each went.
	res, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
	if err != nil {
		t.Fatal(err)
	}
	var drills []dockwellv1.DrillResult
	for _, ev := range res.Msg.GetEvents() {
		if ev.GetAction() == "backup.drill" {
			drills = append(drills, ev.GetBackup().GetDrill())
		}
	}
	if !slices.Equal(drills, []dockwellv1.DrillResult{dockwellv1.DrillResult_DRILL_RESULT_FAILED, dockwellv1.DrillResult_DRILL_RESULT_PASSED}) {
		t.Errorf("the timeline's drills say %v", drills)
	}

	// The default rule fires on the newest checked backup, and an older one passing
	// again doesn't resolve it.
	at := &clock{now: time.Now()}
	ev := evaluatorOn(f, at)
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	firing := func() []*dockwellv1.Alert {
		t.Helper()
		tickOn(t, ev)
		res, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true}))
		if err != nil {
			t.Fatal(err)
		}
		var out []*dockwellv1.Alert
		for _, a := range res.Msg.GetAlerts() {
			if a.GetRuleName() == "restore-drill-failed" {
				out = append(out, a)
			}
		}
		return out
	}
	if got := firing(); len(got) != 1 || !strings.Contains(got[0].GetTitle(), "doesn't restore") {
		t.Fatalf("restore-drill-failed is firing %v", got)
	}
	if again := drill(older.GetId()); again.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_PASSED {
		t.Fatalf("the older backup's drill = %v", again)
	}
	if got := firing(); len(got) != 1 {
		t.Errorf("an older backup passing resolved the alert: %v", got)
	}

	// A drill under way holds its backup: no delete, and no second drill on its server.
	running := store.Drill{ID: id.New(id.Drill), BackupID: older.GetId(), EnvironmentID: older.GetEnvironmentId(), VolumeID: volumeID, ServerID: older.GetServerId(), Result: store.DrillRunning, Actor: "user:usr_1", StartedAt: time.Now()}
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error { return tx.CreateDrill(t.Context(), running) }); err != nil {
		t.Fatal(err)
	}
	_, err = bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: older.GetId()}))
	if reasonOf(err) != "drill_running" {
		t.Errorf("deleting a backup a drill checks: %v", err)
	}
	// What the control plane lost track of, as it stopped, wasn't checked.
	f.server.backups.Recover(t.Context())
	if got := waitDrill(t, bc, older.GetId()); got.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_NOT_CHECKED || !strings.Contains(got.GetWhy(), "control plane stopped") {
		t.Errorf("a drill the control plane lost = %v", got)
	}
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: older.GetId()})); err != nil {
		t.Errorf("deleting it once the drill ended: %v", err)
	}
	// Deleting the backup that failed, as retention would, keeps the alert: nothing as
	// new has passed. A newer backup that passes resolves it.
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: newer.GetId()})); err != nil {
		t.Fatalf("deleting the backup that failed: %v", err)
	}
	if got := firing(); len(got) != 1 {
		t.Errorf("deleting the backup that failed resolved the alert: %v", got)
	}
	if newest := drill(take().GetId()); newest.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_PASSED {
		t.Fatalf("the newest backup's drill = %v", newest)
	}
	if got := firing(); len(got) != 0 {
		t.Errorf("a newer backup passed, and restore-drill-failed is still firing %v", got)
	}
	_, err = bc.DrillBackup(t.Context(), connect.NewRequest(&dockwellv1.DrillBackupRequest{Id: "bkp_none"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a drill of no backup: %v", err)
	}
}

// A schedule that's new checks a restore weekly (ADR 0068): Dockwell drills the newest
// of its backups that read back whole an hour after its time on Sunday, as its own,
// for the policy. Two weeks without a pass and restore-drill-late says so.
func TestAScheduleChecksARestoreWeekly(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	webWithData(t, e, c)
	if done := apply(t, c, propose(t, c, "Back up data each night", drillingPolicyEdit(7))); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the policy ended %v", done.GetState())
	}
	listed, err := bc.ListBackupPolicies(t.Context(), connect.NewRequest(&dockwellv1.ListBackupPoliciesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	policy := listed.Msg.GetPolicies()[0]
	if policy.GetSpec().GetDrill() != resourcev1.BackupPolicy_DRILL_WEEKLY {
		t.Fatalf("a new policy checks a restore %v, want weekly", policy.GetSpec().GetDrill())
	}

	start := time.Now()
	var now atomic.Int64
	now.Store(start.Add(25 * time.Hour).UnixNano())
	srv, letIn := f.restart(t, func(s *Server) {
		s.backups.Now = func() time.Time { return time.Unix(0, now.Load()) }
	})
	letIn()
	eventually(t, "the server to connect", func() bool {
		_, ok := f.agents.Session(f.lastDocker.id.ServerID)
		return ok
	})
	h := srv.backups
	if err := h.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "its run to finish", func() bool {
		got := backupsOf(t, bc)
		return len(got) == 1 && got[0].GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE
	})
	taken := backupsOf(t, bc)[0]
	if taken.GetDrill() != nil {
		t.Fatalf("a drill ran before it fell due: %v", taken.GetDrill())
	}

	// A week on, past its Sunday, its drill falls due, of the newest that read back.
	now.Store(start.Add(8 * 24 * time.Hour).UnixNano())
	if err := h.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := waitDrill(t, bc, taken.GetId()); got.GetResult() != dockwellv1.DrillResult_DRILL_RESULT_PASSED || got.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
		t.Fatalf("the scheduled drill = %v", got)
	}
	var drills []store.Drill
	if err := f.store.View(t.Context(), func(tx *store.Tx) (err error) {
		drills, err = tx.ListDrills(t.Context(), store.DrillFilter{PolicyID: policy.GetId()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(drills) != 1 || drills[0].BackupID != taken.GetId() {
		t.Fatalf("the policy's drills are %v", drills)
	}
	// Once it ran, the next falls due the Sunday after.
	if err := h.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Late is two weeks without a pass.
	at := &clock{now: start.Add(9 * 24 * time.Hour)}
	ev := evaluatorOn(f, at)
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	late := func() int {
		t.Helper()
		tickOn(t, ev)
		res, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true}))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range res.Msg.GetAlerts() {
			if a.GetRuleName() == "restore-drill-late" {
				n++
			}
		}
		return n
	}
	if n := late(); n != 0 {
		t.Errorf("a schedule that passed a day ago is late %d times", n)
	}
	at.add(15 * 24 * time.Hour)
	if n := late(); n != 1 {
		t.Errorf("two weeks without a pass, restore-drill-late fires %d times", n)
	}
}
