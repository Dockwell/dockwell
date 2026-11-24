// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

type sshFixture struct {
	file            string
	proof           atomic.Bool
	armed, restored int
}

func (s *sshFixture) Plan(context.Context, *resourcev1.SshConfig, *resourcev1.SshConfig) (*SSHChange, error) {
	return &SSHChange{
		Account: "admin", OldPort: 22, Port: 2222, OldFile: s.file, Candidate: "two listeners", Final: "new listener",
		From: []netip.Addr{netip.MustParseAddr("203.0.113.7")},
	}, nil
}

func (s *sshFixture) Arm(context.Context, string, string, time.Duration, *SSHChange) error {
	s.armed++
	return nil
}

func (s *sshFixture) Install(_ context.Context, _ *SSHChange, file string) error {
	if s.armed == 0 {
		return errors.New("installed before arming revert")
	}
	s.file = file
	return nil
}

func (s *sshFixture) Restore(_ context.Context, ch *SSHChange) error {
	s.restored++
	s.file = ch.OldFile
	return nil
}

func (*sshFixture) Started() (uint64, error) { return 1234, nil }

func (s *sshFixture) Proved(_ context.Context, ch *SSHChange) bool {
	return s.proof.Load() && ch.Started == 1234
}

func TestSSHTrialNeedsProofAndKeepsOnlyTheNewPort(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	ssh := &sshFixture{file: "original"}
	m.SSH = ssh
	done := make(chan error, 1)
	reported := make(chan *resource.AccessWait, 1)
	ctx := resource.WithOutcome(step("chg_ssh", true), func(o resource.Outcome) { reported <- o.Waits })
	go func() { done <- m.PutSSH(ctx, &resourcev1.SshConfig{Port: 2222, Account: "admin"}) }()
	wait := sshAnnounced(t, reported, done)
	if wait.SSHAccount != "admin" || !slices.Equal(wait.SSHPorts, []uint32{2222}) {
		t.Fatalf("waiting for %+v", wait)
	}
	if ssh.file != "two listeners" || ssh.armed != 1 {
		t.Fatalf("trial has file %q and %d independent timers", ssh.file, ssh.armed)
	}
	if release, err := m.HoldSSHKeys(); err == nil {
		release()
		t.Fatal("key edits were allowed during the SSH trial")
	}
	if got := m.Confirm(t.Context(), "chg_ssh"); got != resource.AccessNotProved {
		t.Fatalf("unproved trial returned %v", got)
	}
	ssh.proof.Store(true)
	if m.ProvedSSH(t.Context()) != "chg_ssh" {
		t.Fatal("fresh SSH proof was not reported")
	}
	if got := m.Confirm(t.Context(), "chg_ssh"); got != resource.AccessKept {
		t.Fatalf("proved trial returned %v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	release, err := m.HoldSSHKeys()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if ssh.file != "new listener" || m.SSHConfig(t.Context()).GetPort() != 2222 {
		t.Fatal("fresh SSH settings weren't kept")
	}
	for _, pin := range tbl.current().Pins {
		if slices.Contains(pin.Ports, uint16(22)) {
			t.Fatalf("old port still pinned in %v", pin.Ports)
		}
	}
}

func TestSSHTrialRevertsOnTimeoutAndOnRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "restart"}[restart], func(t *testing.T) {
			dir := t.TempDir()
			m, tbl := manager(t, dir, time.Minute)
			ssh := &sshFixture{file: "original"}
			clocks := &timers{}
			m.SSH, m.after = ssh, clocks.after
			ctx, cancel := context.WithCancel(step("chg_ssh", true))
			defer cancel()
			done := make(chan error, 1)
			reported := make(chan *resource.AccessWait, 1)
			go func() {
				done <- m.PutSSH(resource.WithOutcome(ctx, func(o resource.Outcome) { reported <- o.Waits }), &resourcev1.SshConfig{Port: 2222, Account: "admin"})
			}()
			sshAnnounced(t, reported, done)
			if restart {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("interrupted trial returned %v", err)
				}
				after := &Manager{Dir: dir, Table: tbl, SSH: ssh}
				if err := after.Ready(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := after.Confirm(t.Context(), "chg_ssh"); got != resource.AccessRolledBack {
					t.Fatalf("after restart: %v", got)
				}
			} else {
				clocks.runOut()
				if err := <-done; err == nil {
					t.Fatal("unconfirmed SSH returned success")
				}
			}
			if ssh.file != "original" || ssh.restored == 0 {
				t.Fatal("original SSH settings were not restored")
			}
			if tbl.rules() != "(no table)" {
				t.Fatal("trial firewall table was not removed")
			}
		})
	}
}

func sshAnnounced(t *testing.T, reported <-chan *resource.AccessWait, done <-chan error) *resource.AccessWait {
	t.Helper()
	select {
	case wait := <-reported:
		return wait
	case err := <-done:
		t.Fatalf("SSH trial stopped before it announced: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("SSH trial did not announce")
	}
	return nil
}

func TestSSHConnectionsRequireAnAuthenticatedRootMonitor(t *testing.T) {
	p := newProc().
		process("100", "sshd", "1", "1001").
		process("101", "sshd-session", "100", "1002").as("101", 0, "sshd-session: deploy [priv]").
		process("102", "sshd-session", "101", "1002").as("102", 1000, "sshd-session: deploy@pts/3").
		process("201", "sshd-session", "100", "1003").as("201", 0, "sshd-session: deploy [priv]").
		process("202", "sshd-auth", "201", "1003").as("202", 105, "sshd-auth: deploy [net]").
		socket("net/tcp", " 0: 0500000A:0016 077100CB:D431 01 00000000:00000000 00:00000000 00000000 0 0 1002").
		socket("net/tcp", " 1: 0500000A:0016 087100CB:D432 01 00000000:00000000 00:00000000 00000000 0 0 1003")
	for _, pid := range []string{"101", "201"} {
		// Fields through starttime, which follows the first 19 fields after comm.
		p.fs[pid+"/stat"].Data = []byte(pid + " (sshd-session) S 100" + strings.Repeat(" 0", 17) + " 1234")
	}
	read := &PinReader{Proc: p.fs, Passwd: passwd}
	connections, err := read.SSHConnections(t.Context(), []uint16{22})
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 {
		t.Fatalf("wanted one authenticated connection, got %+v", connections)
	}
	got := &connections[0]
	if got.PID != 101 || got.StartTicks != 1234 || got.TTY != "pts/3" || got.User != "deploy" || got.Peer.Addr().String() != "203.0.113.7" {
		t.Fatalf("connection identity differs: %+v", got)
	}
}
