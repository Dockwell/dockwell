// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// gatedListener takes no connection until it opens, as a server is still on its way
// back when a control plane starts.
type gatedListener struct {
	net.Listener
	open chan struct{}
}

func (g *gatedListener) Accept() (net.Conn, error) {
	<-g.open
	return g.Listener.Accept()
}

// restart stops the control plane and starts another on the same store and addresses,
// as dockwell run does, with servers let in once the returned func says so.
func (f *fullServer) restart(t *testing.T, prepare func(*Server)) (srv *Server, letIn func()) {
	t.Helper()
	if err := f.stop(); err != nil {
		t.Fatal(err)
	}
	config := f.config
	agents := agentapi.NewAgents(f.kinds, config.Logger)
	engine := revision.New(config.Store, f.kinds, agents, f.keeper, config.Logger)
	config.Agents, config.Changes, f.agents, f.engine = agents, engine, agents, engine
	srv, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	prepare(srv)
	listen := func(addr string) net.Listener {
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	gate := &gatedListener{Listener: listen(f.agentAddr), open: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, Listeners{API: listen(f.api), Agents: gate}) }()
	var opened atomic.Bool
	letIn = func() {
		if opened.CompareAndSwap(false, true) {
			close(gate.open)
		}
	}
	t.Cleanup(func() {
		letIn()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v after cancel", err)
		}
	})
	return srv, letIn
}

// A run that fell due while the control plane was down waits for its server to connect,
// as that takes a moment after a start, and happens once it has. Only a server still
// away past the grace fails its run, where someone can see it.
func TestARunMissedWhileDownHappensOnceItsServerIsBack(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	webWithData(t, e, c)
	if done := apply(t, c, propose(t, c, "Back up data each night", policyEdit(7))); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the policy ended %v", done.GetState())
	}

	var clock atomic.Int64
	clock.Store(time.Now().Add(25 * time.Hour).UnixNano())
	srv, letIn := f.restart(t, func(s *Server) {
		s.backups.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	})
	h := srv.backups
	eventually(t, "the control plane to answer", func() bool {
		res, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
		return err == nil && res != nil
	})
	// The look a start takes at once, and every look within the grace, find it due and
	// leave it due.
	for range 3 {
		if err := h.RunDue(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := backupsOf(t, bc); len(got) != 0 {
		t.Fatalf("it ran with its server away: %v", got)
	}
	letIn()
	eventually(t, "the server to connect", func() bool {
		_, ok := f.agents.Session(f.lastDocker.id.ServerID)
		return ok
	})
	if err := h.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the missed run to finish", func() bool {
		got := backupsOf(t, bc)
		return len(got) == 1 && got[0].GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE
	})
}

// Past the grace, a server that's away fails its run where someone looks.
func TestARunFailsOnceItsServerIsPastTheGrace(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	webWithData(t, e, c)
	if done := apply(t, c, propose(t, c, "Back up data each night", policyEdit(7))); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the policy ended %v", done.GetState())
	}
	var clock atomic.Int64
	clock.Store(time.Now().Add(25 * time.Hour).UnixNano())
	srv, _ := f.restart(t, func(s *Server) {
		s.backups.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	})
	eventually(t, "the control plane to answer", func() bool {
		_, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
		return err == nil
	})
	clock.Add(int64(backups.ConnectGrace + time.Second))
	if err := srv.backups.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The look a start takes can come after the clock moved here, so it may be two.
	got := backupsOf(t, bc)
	if len(got) == 0 {
		t.Fatal("a run with its server away past the grace left no word of it")
	}
	for _, one := range got {
		if one.GetState() != dockwellv1.BackupState_BACKUP_STATE_FAILED || !strings.Contains(one.GetError(), "wasn't connected") {
			t.Errorf("a backup with its server away = %v", one)
		}
	}
}

// day moves a handler's clock to n days on, and lets what's due there run.
func day(t *testing.T, h *backups.Handler, bc dockwellv1connect.BackupServiceClient, from time.Time, n int) {
	t.Helper()
	h.Now = func() time.Time { return from.Add(time.Duration(n) * 25 * time.Hour) }
	if err := h.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, bc)
}

// waitIdle waits for what's running to end, and for what a run pushes out to go.
func waitIdle(t *testing.T, bc dockwellv1connect.BackupServiceClient) {
	t.Helper()
	eventually(t, "the run to end", func() bool {
		for _, one := range backupsOf(t, bc) {
			if one.GetState() == dockwellv1.BackupState_BACKUP_STATE_RUNNING {
				return false
			}
		}
		return true
	})
	time.Sleep(50 * time.Millisecond) // retention runs just after a run ends
}

func doneOf(all []*dockwellv1.Backup) (done []*dockwellv1.Backup) {
	for _, one := range all {
		if one.GetState() == dockwellv1.BackupState_BACKUP_STATE_DONE {
			done = append(done, one)
		}
	}
	return done
}

// A schedule cut back holds more than it keeps, and a run that won't fit used to be
// the only thing that could take those away. It comes down first, so "keep fewer" helps.
func TestASmallerScheduleComesDownBeforeARunIsRefusedForRoom(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	_, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	apply(t, c, propose(t, c, "Back up data each night", policyEdit(3)))
	h, now := f.server.backups, time.Now()
	for n := 1; n <= 3; n++ {
		day(t, h, bc, now, n)
	}
	if got := doneOf(backupsOf(t, bc)); len(got) != 3 {
		t.Fatalf("after 3 runs: %v", got)
	}
	apply(t, c, propose(t, c, "Keep fewer", policyEdit(1)))
	f.lastDocker.backups.Disk = backup.NoRoom
	day(t, h, bc, now, 4)
	all := backupsOf(t, bc)
	if got := doneOf(all); len(got) != 1 {
		t.Errorf("a schedule that keeps 1 holds %d: %v", len(got), got)
	}
	if !strings.Contains(all[0].GetError(), "keep fewer") {
		t.Errorf("the run that didn't fit said %q", all[0].GetError())
	}
}

// Retention leaves what a planned or running restore brings back, and takes it once
// that's over.
func TestRetentionLeavesWhatARestoreNames(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	_, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	apply(t, c, propose(t, c, "Back up data each night", policyEdit(1)))
	h, now := f.server.backups, time.Now()
	day(t, h, bc, now, 1)
	oldest := backupsOf(t, bc)[0].GetId()
	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: oldest}))
	if err != nil {
		t.Fatal(err)
	}
	day(t, h, bc, now, 2)
	day(t, h, bc, now, 3)
	has := func() bool {
		for _, one := range backupsOf(t, bc) {
			if one.GetId() == oldest {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("retention took the backup a planned restore brings back")
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: planned.Msg.GetChange().GetId()})); err != nil {
		t.Fatal(err)
	}
	day(t, h, bc, now, 4)
	if has() {
		t.Error("it stayed once nothing named it")
	}
}

// Two schedules due at once on a server take turns: each looks for room once the last
// is done, and neither finds the volume taken.
func TestRunsDueTogetherOnAServerTakeTurns(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	_, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	second := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.Kind), Name: "data-too"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupPolicy{BackupPolicy: &resourcev1.BackupPolicy{Volume: "data", Keep: 7, Stop: true}}},
	}
	apply(t, c, propose(t, c, "Back up data twice", policyEdit(7), second))
	day(t, f.server.backups, bc, time.Now(), 1)
	all := backupsOf(t, bc)
	if len(all) != 2 || len(doneOf(all)) != 2 {
		t.Errorf("backups = %v", all)
	}
}
