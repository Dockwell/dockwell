// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// sendingTo is a server with data on it, and a destination called r2 to send it to.
type sendingTo struct {
	f        *fullServer
	b        *browser
	c        dockwellv1connect.ChangeServiceClient
	bc       dockwellv1connect.BackupServiceClient
	events   dockwellv1connect.EventServiceClient
	bucket   *s3test.Server
	volumeID string
}

func startSending(t *testing.T, backend store.Backend) *sendingTo {
	t.Helper()
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	s := &sendingTo{f: f, b: b, c: changeClient(b, b.origin), bc: backupClient(b, b.origin), events: eventClient(b, b.origin), bucket: s3test.New(t)}
	var dir string
	s.volumeID, dir = webWithData(t, e, s.c)
	writeData(t, dir, "orders.db", "the orders")
	f.lastDocker.backups.HTTP = s.bucket.Client()
	apply(t, s.c, propose(t, s.c, "Send backups to R2", destinationEdit(s.bucket, "a-secret")))
	return s
}

// sent takes a backup that goes to r2, and waits for how that ended.
func (s *sendingTo) sent(t *testing.T) *dockwellv1.Backup {
	t.Helper()
	made, err := s.bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: s.volumeID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	return waitCopy(t, s.bc, made.Msg.GetBackup().GetId())
}

// schedule keeps keep backups of the volume, and sends them to r2.
func (s *sendingTo) schedule(t *testing.T, keep uint32) {
	t.Helper()
	edit := policyEdit(keep)
	edit.Spec.GetBackupPolicy().Destination = "r2"
	apply(t, s.c, propose(t, s.c, "Back up data each night", edit))
}

func (s *sendingTo) get(t *testing.T, backupID string) *dockwellv1.Backup {
	t.Helper()
	res, err := s.bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: backupID}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetBackup()
}

// said finds the newest event of an action on the timeline.
func (s *sendingTo) said(t *testing.T, action string) *dockwellv1.Event {
	t.Helper()
	res, err := s.events.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP}}))
	if err != nil {
		t.Fatal(err)
	}
	all := res.Msg.GetEvents()
	if i := slices.IndexFunc(all, func(e *dockwellv1.Event) bool { return e.GetAction() == action }); i >= 0 {
		return all[i]
	}
	return nil
}

// stored is how many backups the bucket holds, without what's kept beside each.
func (s *sendingTo) stored() int {
	n := 0
	for path := range s.bucket.Objects() {
		if strings.HasSuffix(path, ".sealed") {
			n++
		}
	}
	return n
}

// lost is the server's copy going, by itself or by a person's hand.
func (s *sendingTo) lost(t *testing.T, b *dockwellv1.Backup) {
	t.Helper()
	ref := resource.Ref{Kind: volume.Kind, Scope: b.GetEnvironmentId(), Name: b.GetVolume()}
	if _, err := s.f.lastDocker.backups.Delete(t.Context(), ref, b.GetId(), "", nil); err != nil {
		t.Fatal(err)
	}
}

// With its copies failing, a schedule goes on moving along the server, and the bucket
// holds what it last had: a key that expires doesn't empty it in a week, as it would if
// a backup counted toward what's kept wherever its copy went.
func TestABucketKeepsItsCopiesWhileNewOnesFail(t *testing.T) {
	s := startSending(t, store.SQLite)
	s.schedule(t, 2)
	h, now := s.f.server.backups, time.Now()
	for n := 1; n <= 3; n++ {
		day(t, h, now, n)
	}
	eventually(t, "the first to go", func() bool { return s.stored() == 2 })
	s.bucket.Refuse, s.bucket.RefuseOnly = "AccessDenied", http.MethodPost
	for n := 4; n <= 7; n++ {
		day(t, h, now, n)
	}
	var here, there int
	eventually(t, "the schedule to settle", func() bool {
		here, there = 0, 0
		all := doneOf(backupsOf(t, s.bc))
		for _, one := range all {
			if one.GetOnServer() {
				here++
			}
			if one.GetCopy() == dockwellv1.BackupCopy_BACKUP_COPY_STORED {
				there++
			}
		}
		return here == 2 && there == 2 && len(all) == 4
	})
	if got := s.stored(); got != 2 {
		t.Errorf("the bucket holds %d, with 2 kept and no new copy arriving", got)
	}
	all := doneOf(backupsOf(t, s.bc))
	if all[0].GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED || !all[0].GetOnServer() || all[3].GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED || all[3].GetOnServer() {
		t.Errorf("newest and oldest = %v, %v", all[0], all[3])
	}
	if s.said(t, "backup.server_copy_removed") == nil {
		t.Error("Activity doesn't say the server's copies went")
	}
	res, err := s.bc.ListBackupPolicies(t.Context(), connect.NewRequest(&dockwellv1.ListBackupPoliciesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	info := res.Msg.GetPolicies()[0]
	if info.GetKept() != 2 || info.GetLast().GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED {
		t.Errorf("the schedule shows %d kept, and a last run of %v", info.GetKept(), info.GetLast())
	}

	// New copies arrive again, and the old ones go once that many newer ones are there.
	s.bucket.Refuse = ""
	for n := 8; n <= 9; n++ {
		day(t, h, now, n)
	}
	eventually(t, "the oldest copies to go", func() bool { return s.stored() == 2 })
	eventually(t, "the record of them to go", func() bool { return len(doneOf(backupsOf(t, s.bc))) == 2 })
}

// A backup is done when it's whole. Its volume is free while it's sent, however long
// that takes, and a copy cut short takes nothing from it.
func TestABackupIsDoneBeforeItsCopyLeaves(t *testing.T) {
	s := startSending(t, store.SQLite)
	hold := make(chan struct{})
	t.Cleanup(sync1(hold))
	s.bucket.Hold = hold
	made, err := s.bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: s.volumeID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	b := waitBackup(t, s.bc, made.Msg.GetBackup().GetId())
	if b.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || b.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_PENDING {
		t.Fatalf("with its copy held up, a backup is %v, %v", b.GetState(), b.GetCopy())
	}

	// The volume is free: a restore can plan and another backup can run.
	another, err := s.bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: s.volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitBackup(t, s.bc, another.Msg.GetBackup().GetId()); got.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
		t.Errorf("a second backup, with the first still sending = %v", got)
	}
	plan, err := s.bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: b.GetId()}))
	if err != nil {
		t.Fatalf("a restore, with its backup still sending = %v", err)
	}
	if _, err := s.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: plan.Msg.GetChange().GetId()})); err != nil {
		t.Fatal(err)
	}

	// Deleted now, what lands in the bucket would have no row to find it by.
	if _, err := s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId()})); err == nil || !strings.Contains(err.Error(), "on its way to r2") {
		t.Errorf("deleting what's on its way = %v", err)
	}

	sync1(hold)()
	if got := waitCopy(t, s.bc, b.GetId()); got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Errorf("once the bucket answers = %v", got)
	}
}

// How a send is going shows on the backup, which is done by then.
func TestASendShowsHowItsGoing(t *testing.T) {
	s := startSending(t, store.SQLite)
	hold := make(chan struct{})
	t.Cleanup(sync1(hold))
	s.bucket.Hold, s.bucket.HoldOnly = hold, ".json"
	made, err := s.bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: s.volumeID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the send to say how it's going", func() bool {
		return strings.HasPrefix(s.get(t, made.Msg.GetBackup().GetId()).GetProgress(), "Sending data: ")
	})
}

// A fetch shows from the moment it's asked for, or a page has nothing to wait on.
func TestAFetchShowsFromTheStart(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	s.lost(t, b)
	hold := make(chan struct{})
	t.Cleanup(sync1(hold))
	s.bucket.Hold = hold
	res, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetBackup().GetProgress(); got != "Fetching data" {
		t.Errorf("a fetch just asked for says %q", got)
	}
	sync1(hold)()
	eventually(t, "it to end", func() bool { return s.said(t, "backup.fetched") != nil })
	if got := s.get(t, b.GetId()).GetProgress(); got != "" {
		t.Errorf("a fetch that ended says %q", got)
	}
}

// A control plane that stops mid-send leaves a whole backup whole, with or without a
// chance to say so. Each start ends what no call is left to finish.
func TestAStopLeavesABackupWholeWithItsCopyCutShort(t *testing.T) {
	s := startSending(t, store.SQLite)
	hold := make(chan struct{})
	t.Cleanup(sync1(hold))
	s.bucket.Hold = hold
	made, err := s.bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: s.volumeID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	first := waitBackup(t, s.bc, made.Msg.GetBackup().GetId())
	crashed := id.New(id.Backup)
	s.f.restart(t, func(*Server) {
		now := time.Now()
		left := store.Backup{
			ID: crashed, EnvironmentID: first.GetEnvironmentId(), Volume: first.GetVolume(), VolumeID: first.GetVolumeId(), ServerID: first.GetServerId(),
			State: store.BackupRunning, Actor: "system", StartedAt: now, Destination: "r2",
		}
		if err := s.f.config.Store.Update(t.Context(), func(tx *store.Tx) error {
			if err := tx.CreateBackup(t.Context(), left); err != nil {
				return err
			}
			left.State, left.FinishedAt = store.BackupDone, &now
			return tx.FinishBackup(t.Context(), left)
		}); err != nil {
			t.Fatal(err)
		}
	})
	eventually(t, "the control plane to answer", func() bool {
		_, err := s.bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
		return err == nil
	})
	for _, backupID := range []string{first.GetId(), crashed} {
		got := waitCopy(t, s.bc, backupID)
		if got.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED || !strings.Contains(got.GetCopyError(), "safe on its server") {
			t.Errorf("after a stop, %s = %v", backupID, got)
		}
	}
}

// A backup can leave its server and stay on its destination, where a person can get it
// back, and a backup that's only on its server can't be removed that way.
func TestAServersCopyCanGoAndTheDestinationsStays(t *testing.T) {
	storetest.Each(t, testAServersCopyCanGoAndTheDestinationsStays)
}

func testAServersCopyCanGoAndTheDestinationsStays(t *testing.T, backend store.Backend) {
	s := startSending(t, backend)
	b := s.sent(t)
	if _, err := s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId(), ServerOnly: true})); err != nil {
		t.Fatal(err)
	}
	got := s.get(t, b.GetId())
	if got.GetOnServer() || got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED || s.stored() != 1 {
		t.Errorf("on the server %v, copy %v, %d in the bucket", got.GetOnServer(), got.GetCopy(), s.stored())
	}
	if s.said(t, "backup.server_copy_removed") == nil {
		t.Error("Activity doesn't say it left its server")
	}
	_, err := s.bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: b.GetId()}))
	if err == nil || !strings.Contains(err.Error(), "Fetch it from r2") {
		t.Errorf("restoring what left its server = %v", err)
	}
	_, err = s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId(), ServerOnly: true}))
	if err == nil || !strings.Contains(err.Error(), "isn't on its server") {
		t.Errorf("removing it from there twice = %v", err)
	}
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "it to come back", func() bool { return s.get(t, b.GetId()).GetOnServer() })

	s.bucket.Refuse = "AccessDenied"
	only := s.sent(t)
	_, err = s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: only.GetId(), ServerOnly: true}))
	if err == nil || !strings.Contains(err.Error(), "would lose it") {
		t.Errorf("removing the only copy from its server = %v", err)
	}
	if !s.get(t, only.GetId()).GetOnServer() {
		t.Error("the only copy went")
	}
}

// A fetch that fails says so as a fetch, on the row and on Activity: nothing about the
// copy changed. A person trying again and getting it clears it.
func TestAFailedFetchSaysItsAFetch(t *testing.T) {
	storetest.Each(t, testAFailedFetchSaysItsAFetch)
}

func testAFailedFetchSaysItsAFetch(t *testing.T, backend store.Backend) {
	s := startSending(t, backend)
	b := s.sent(t)
	s.lost(t, b)
	s.bucket.Refuse = "AccessDenied"
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the fetch to fail", func() bool { return s.get(t, b.GetId()).GetFetchError() != "" })
	got := s.get(t, b.GetId())
	if !strings.Contains(got.GetFetchError(), "AccessDenied") || got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Errorf("after a failed fetch = %v", got)
	}
	e := s.said(t, "backup.fetch_failed")
	if e == nil || !strings.Contains(e.GetBackup().GetError(), "AccessDenied") {
		t.Errorf("Activity says %v", e)
	}
	if s.said(t, "backup.copy_failed") != nil {
		t.Error("it called a failed fetch a failed copy")
	}
	s.bucket.Refuse = ""
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the fetch to work", func() bool { return s.said(t, "backup.fetched") != nil })
	if got := s.get(t, b.GetId()); got.GetFetchError() != "" {
		t.Errorf("a fetch that worked left %q", got.GetFetchError())
	}
}

// A bucket that won't let go of a copy doesn't keep the server's. Activity says what it
// said, and it's tried again: by a person, or by the next run.
func TestWhatABucketWontDeleteGoesFromTheServerAll(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	s.bucket.Refuse, s.bucket.RefuseOnly = "AccessDenied", http.MethodDelete
	_, err := s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId()}))
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "Delete it again") {
		t.Errorf("deleting what the bucket keeps = %v", err)
	}
	if got := s.get(t, b.GetId()); got.GetOnServer() || got.GetCopyError() != "" || got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Errorf("after the bucket said no = %v", got)
	}
	if e := s.said(t, "backup.copy_kept"); e == nil || !strings.Contains(e.GetBackup().GetError(), "AccessDenied") {
		t.Errorf("Activity says %v", e)
	}
	s.bucket.Refuse = ""
	if _, err := s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	if got := s.stored(); got != 0 {
		t.Errorf("left in the bucket: %d", got)
	}

	// Retention carries on the same way.
	s.schedule(t, 1)
	h, now := s.f.server.backups, time.Now()
	day(t, h, now, 1)
	s.bucket.Refuse = "AccessDenied"
	day(t, h, now, 2)
	eventually(t, "the server to give it up", func() bool {
		here := 0
		for _, one := range doneOf(backupsOf(t, s.bc)) {
			if one.GetOnServer() {
				here++
			}
		}
		return here == 1 && s.said(t, "backup.copy_kept") != nil && s.stored() == 2
	})
	s.bucket.Refuse = ""
	day(t, h, now, 3)
	eventually(t, "the next run to try again", func() bool { return s.stored() == 1 })
}

// A schedule outlives the destination it named: its backups go on, on their server, and
// each says what to do.
func TestAScheduleWhoseDestinationWentKeepsBackingUp(t *testing.T) {
	s := startSending(t, store.SQLite)
	s.schedule(t, 3)
	apply(t, s.c, propose(t, s.c, "Delete it", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"}, Delete: true}))
	day(t, s.f.server.backups, time.Now(), 1)
	all := doneOf(backupsOf(t, s.bc))
	if len(all) != 1 || all[0].GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED || !strings.Contains(all[0].GetCopyError(), "Point data-daily at another destination") {
		t.Fatalf("a run with its destination gone = %v", all)
	}
	if s.said(t, "backup.copy_failed") == nil {
		t.Error("Activity doesn't say so")
	}
}

// A destination keeps its passphrase through an update, as what's there was sealed with
// it. Leaving it out keeps it, and the plan says so. Giving one is refused in the same
// words whatever it gives, so a refusal says nothing of a guess.
func TestAPassphraseOutlivesAnUpdate(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	giving := func(passphrase string) string {
		edit := destinationEdit(s.bucket, "a-new-secret")
		edit.Spec.GetBackupDestination().Passphrase = passphrase
		_, err := s.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Change it", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil {
			t.Fatalf("an update that gives a passphrase planned")
		}
		return err.Error()
	}
	right, wrong := giving("a long passphrase of our own"), giving("a different passphrase altogether")
	if right != wrong {
		t.Errorf("a refusal tells a right passphrase from a wrong one: %q, %q", right, wrong)
	}
	for _, want := range []string{"an update can't set one", "remove the destination and add it again", "open only with the old passphrase"} {
		if !strings.Contains(right, want) {
			t.Errorf("the refusal says %q, with no %q", right, want)
		}
	}

	edit := destinationEdit(s.bucket, "a-new-secret")
	edit.Spec.GetBackupDestination().Passphrase = ""
	planned := propose(t, s.c, "Rotate the keys", edit)
	if notes := planned.GetPlan().GetNotes(); !slices.ContainsFunc(notes, func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "kept_passphrase" && strings.Contains(n.GetMessage(), "r2 keeps the passphrase")
	}) {
		t.Errorf("the plan says %v", notes)
	}
	apply(t, s.c, planned)
	s.lost(t, b)
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "it to come back", func() bool { return s.said(t, "backup.fetched") != nil })
	if got := s.get(t, b.GetId()); got.GetFetchError() != "" || got.GetSealedEarlier() {
		t.Fatalf("after keeping its passphrase = %v", got)
	}
}

// addAgain removes r2 and adds it with another passphrase.
func (s *sendingTo) addAgain(t *testing.T) {
	t.Helper()
	apply(t, s.c, propose(t, s.c, "Delete it", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"}, Delete: true}))
	edit := destinationEdit(s.bucket, "a-new-secret")
	edit.Spec.GetBackupDestination().Passphrase = "a different passphrase altogether"
	apply(t, s.c, propose(t, s.c, "Add it again", edit))
}

// A new destination needs a passphrase, including one added again under a name that's
// been used. Only an update can leave it out.
func TestANewDestinationNeedsAPassphrase(t *testing.T) {
	s := startSending(t, store.SQLite)
	apply(t, s.c, propose(t, s.c, "Delete it", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"}, Delete: true}))
	for _, passphrase := range []string{"", "too short"} {
		edit := destinationEdit(s.bucket, "a-secret")
		edit.Spec.GetBackupDestination().Passphrase = passphrase
		_, err := s.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add it", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(err.Error(), "at least 16 characters") {
			t.Errorf("a new destination with the passphrase %q = %v", passphrase, err)
		}
	}
}

// A copy added again with another passphrase is marked, and can't be what's left: it's
// refused by hand, and retention leaves the server's copy too. Fetching it says why not.
func TestACopyOnlyAnEarlierPassphraseOpensIsNoWayBack(t *testing.T) {
	s := startSending(t, store.SQLite)
	s.schedule(t, 1)
	h, now := s.f.server.backups, time.Now()
	day(t, h, now, 1)
	b := doneOf(backupsOf(t, s.bc))[0]
	if b.GetSealedEarlier() || b.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("a copy its destination opens = %v", b)
	}
	s.addAgain(t)
	if got := s.get(t, b.GetId()); !got.GetSealedEarlier() {
		t.Errorf("a copy sealed with an earlier passphrase = %v", got)
	}
	_, err := s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId(), ServerOnly: true}))
	if err == nil || !strings.Contains(err.Error(), "sealed with an earlier passphrase") || !strings.Contains(err.Error(), "Add r2 again with that passphrase") {
		t.Errorf("leaving only a copy nothing opens = %v", err)
	}
	if !s.get(t, b.GetId()).GetOnServer() {
		t.Fatal("the server's copy went")
	}

	// The next copy can't get there, so this is still the newest on the bucket.
	s.bucket.Refuse, s.bucket.RefuseOnly = "AccessDenied", http.MethodPost
	day(t, h, now, 2)
	if got := s.get(t, b.GetId()); !got.GetOnServer() {
		t.Errorf("retention took the server's copy of what only an earlier passphrase opens: %v", got)
	}

	// Removed for good, it's no way back either.
	apply(t, s.c, propose(t, s.c, "Delete it", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"}, Delete: true}))
	_, err = s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b.GetId(), ServerOnly: true}))
	if err == nil || !strings.Contains(err.Error(), "sealed with an earlier passphrase") {
		t.Errorf("with its destination gone = %v", err)
	}
}

// A fetch that needs an earlier passphrase says so.
func TestAFetchSaysItNeedsAnEarlierPassphrase(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	s.lost(t, b)
	s.addAgain(t)
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the fetch to fail", func() bool { return s.get(t, b.GetId()).GetFetchError() != "" })
	if got := s.get(t, b.GetId()).GetFetchError(); !strings.Contains(got, "sealed with the passphrase r2 had before") {
		t.Errorf("a copy that needs another passphrase says %q", got)
	}
}

// place puts web and its volume on these servers, by ID.
func (s *sendingTo) place(t *testing.T, servers ...string) {
	t.Helper()
	web := runImage("ghcr.io/acme/shop:1.4")
	web.GetSpec().GetService().GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/srv/data"}}
	web.Servers = servers
	data := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(volume.Kind), Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{}}},
		Servers:  servers,
	}
	applyConfirmed(t, s.c, propose(t, s.c, "Run web there", web, data))
}

// A server that's removed takes its disk with it. Its backups read as gone from there,
// restoring one says what to do, and once a new server has the volume, the backup comes
// back through its destination.
func TestARemovedServersBackupComesBackThroughItsDestination(t *testing.T) {
	storetest.Each(t, testARemovedServersBackupComesBackThroughItsDestination)
}

func testARemovedServersBackupComesBackThroughItsDestination(t *testing.T, backend store.Backend) {
	s := startSending(t, backend)
	b := s.sent(t)
	first := s.f.lastDocker
	if err := s.f.ident.Revoke(t.Context(), first.id.ServerID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	first.stop()
	waitConnected(t, s.f, first.id.ServerID, false)
	e := s.f.addServerNamed(t, "hz-2", nil, nil)
	second := s.f.lastDocker
	second.backups.HTTP = s.bucket.Client()
	waitConnected(t, s.f, second.id.ServerID, true)

	got := s.get(t, b.GetId())
	if got.GetOnServer() || got.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED || len(got.GetFetchOnto()) != 0 {
		t.Fatalf("with its server removed and no other with the volume = %v", got)
	}
	want := "Fetch it from r2 onto a server that has data, then restore it"
	if _, err := s.bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: b.GetId()})); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("restoring it = %v", err)
	}

	s.place(t, second.id.ServerID)
	if got := s.get(t, b.GetId()).GetFetchOnto(); !slices.Equal(got, []string{"hz-2"}) {
		t.Fatalf("it can be fetched onto %v", got)
	}
	if _, err := s.bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: b.GetId()})); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("restoring it before it's fetched = %v", err)
	}
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId(), Server: "hz-2"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "it to come back", func() bool { return s.get(t, b.GetId()).GetOnServer() })
	if got := s.get(t, b.GetId()); got.GetServerName() != "hz-2" || len(got.GetFetchOnto()) != 0 {
		t.Errorf("once it's fetched = %v", got)
	}
	ref := resource.Ref{Kind: volume.Kind, Scope: b.GetEnvironmentId(), Name: "data"}
	dir := e.VolumeDir(volume.TargetOf(ref).Name)
	writeData(t, dir, "orders.db", "not the orders")
	restore, err := s.bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: b.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	ch := restore.Msg.GetChange()
	w := watch(t, s.c, ch.GetId())
	if _, err := s.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if end := waitWatch(t, w).last(); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the restore ended %v: %v", end.GetState(), end.GetError())
	}
	if got := readData(t, dir, "orders.db"); got != "the orders" {
		t.Errorf("orders.db on hz-2 = %q", got)
	}
}

// A volume that moved leaves its backups on a server that can't restore them, however
// connected that is. What's stored comes back onto where the volume is now.
func TestABackupOfAVolumeThatMovedCanBeFetchedAfterIt(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	s.f.addServerNamed(t, "hz-2", nil, nil)
	second := s.f.lastDocker
	second.backups.HTTP = s.bucket.Client()
	waitConnected(t, s.f, second.id.ServerID, true)
	s.place(t, second.id.ServerID)
	got := s.get(t, b.GetId())
	if got.GetOnServer() || !slices.Equal(got.GetFetchOnto(), []string{"hz-2"}) {
		t.Errorf("with its volume moved = %v", got)
	}
	if _, err := s.bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: b.GetId()})); err == nil || !strings.Contains(err.Error(), "Fetch it from r2") {
		t.Errorf("restoring it = %v", err)
	}
}

// A server that's away, and not removed, reads the same: what it holds can't be restored
// now, and what's stored can come back onto a server that's there.
func TestABackupOfAServerThatsAwayCanBeFetchedOntoAnother(t *testing.T) {
	s := startSending(t, store.SQLite)
	b := s.sent(t)
	first := s.f.lastDocker
	s.f.addServerNamed(t, "hz-2", nil, nil)
	second := s.f.lastDocker
	second.backups.HTTP = s.bucket.Client()
	waitConnected(t, s.f, second.id.ServerID, true)
	s.place(t, first.id.ServerID, second.id.ServerID)

	local, err := s.bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: s.volumeID, Server: "hz-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitBackup(t, s.bc, local.Msg.GetBackup().GetId()); !got.GetOnServer() {
		t.Fatalf("a backup on a connected server = %v", got)
	}
	if got := s.get(t, b.GetId()); !got.GetOnServer() || len(got.GetFetchOnto()) != 0 {
		t.Fatalf("with its server connected = %v", got)
	}
	first.stop()
	waitConnected(t, s.f, first.id.ServerID, false)

	if got := s.get(t, local.Msg.GetBackup().GetId()); got.GetOnServer() || len(got.GetFetchOnto()) != 0 {
		t.Errorf("what only a server that's away holds = %v", got)
	}
	got := s.get(t, b.GetId())
	if got.GetOnServer() || !slices.Equal(got.GetFetchOnto(), []string{"hz-2"}) {
		t.Fatalf("what's stored too = %v", got)
	}
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b.GetId(), Server: "hz-2"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "it to come back", func() bool { return s.get(t, b.GetId()).GetOnServer() })
	if got := s.get(t, b.GetId()); got.GetServerName() != "hz-2" {
		t.Errorf("once it's fetched = %v", got)
	}
}

// sync1 closes a channel the first time it's called.
func sync1(c chan struct{}) func() {
	return func() {
		select {
		case <-c:
		default:
			close(c)
		}
	}
}
