// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/resource"
)

type sshFixture struct {
	file            string
	proof           atomic.Bool
	unexpected      netip.Addr
	finalError      error
	armed, restored int
	disarmed        int
}

func (s *sshFixture) Plan(context.Context, *resourcev1.SshConfig, *resourcev1.SshConfig) (*SSHChange, error) {
	return &SSHChange{
		Account: "admin", OldPort: 22, Port: 2222, OldFile: s.file, Candidate: "two listeners", Final: "new listener",
		From: []netip.Addr{netip.MustParseAddr("203.0.113.7")}, ProofPeer: netip.MustParseAddr("203.0.113.7"),
	}, nil
}

func (s *sshFixture) Rearm(_ context.Context, _, _ string, _ time.Duration, _ *SSHChange) error {
	if s.file != "two listeners" {
		return errors.New("rearmed before installing the candidate")
	}
	s.armed++
	return nil
}

func (s *sshFixture) Arm(context.Context, string, string, time.Duration, *SSHChange) error {
	s.armed++
	return nil
}

func (s *sshFixture) Install(_ context.Context, _ *SSHChange, file string) error {
	if s.armed == 0 {
		return errors.New("installed before arming revert")
	}
	if file == "new listener" && s.finalError != nil {
		return s.finalError
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

func (s *sshFixture) CandidateLive(ch *SSHChange) bool { return s.file == ch.Candidate }

func (s *sshFixture) Proved(_ context.Context, ch *SSHChange) (*SSHProof, netip.Addr) {
	if s.proof.Load() && ch.Started == 1234 {
		return &SSHProof{Address: ch.ProofPeer.String(), Method: "publickey"}, netip.Addr{}
	}
	return nil, s.unexpected
}

func (s *sshFixture) Disarm(context.Context, string) error {
	s.disarmed++
	return nil
}

func TestSSHTrialNeedsProofAndKeepsOnlyTheNewPort(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	ssh := &sshFixture{file: "original", unexpected: netip.MustParseAddr("203.0.113.25")}
	m.SSH = ssh
	done := make(chan error, 1)
	reported := make(chan *resource.AccessWait, 1)
	ctx := resource.WithOutcome(step("chg_ssh", true), func(o resource.Outcome) { reported <- o.Waits })
	m.confirmed.Policy = &resourcev1.FirewallPolicy{DenyEverythingElse: true}
	m.confirmed.Rules = map[string]*resourcev1.FirewallRule{
		"ssh-anywhere": {Port: 22},
		"ssh-subnet":   {Port: 22, Sources: []string{"198.51.100.0/24"}},
		"dns-local":    {Port: 22, Protocol: resourcev1.Protocol_PROTOCOL_UDP},
	}
	spec := &resourcev1.SshConfig{Port: 2222, Account: "admin"}
	move, err := m.CheckSSH(t.Context(), spec)
	if err != nil || !slices.Equal(move.GetFirewallRules(), []string{"ssh-anywhere", "ssh-subnet"}) {
		t.Fatalf("plan %v, error %v", move, err)
	}
	go func() { done <- m.PutSSH(ctx, spec, move) }()
	wait := sshAnnounced(t, reported, done)
	if wait.SSHAccount != "admin" || !slices.Equal(wait.SSHPorts, []uint32{2222}) {
		t.Fatalf("waiting for %+v", wait)
	}
	if ssh.file != "two listeners" || ssh.armed != 2 {
		t.Fatalf("trial has file %q and %d independent timers", ssh.file, ssh.armed)
	}
	if release, err := m.HoldSSHKeys(); err == nil {
		release()
		t.Fatal("key edits were allowed during the SSH trial")
	}
	if got := m.Confirm(t.Context(), "chg_ssh"); got != resource.AccessNotProved {
		t.Fatalf("unproved trial returned %v", got)
	}
	if _, _, proof, peer := m.SSHTestDetails(t.Context()); proof != nil || peer != ssh.unexpected {
		t.Fatalf("unexpected test address was not reported: %v, %v", proof, peer)
	}
	ssh.proof.Store(true)
	if id, _, proof, _ := m.SSHTestDetails(t.Context()); id != "chg_ssh" || proof == nil {
		t.Fatal("fresh SSH proof was not reported")
	}
	ssh.proof.Store(false)
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
	if ssh.file != "new listener" || ssh.disarmed != 1 || m.SSHConfig(t.Context()).GetPort() != 2222 {
		t.Fatal("fresh SSH settings weren't kept")
	}
	for _, pin := range tbl.current().Pins {
		if slices.Contains(pin.Ports, uint16(22)) || pin.Name == "ssh trial listener" {
			t.Fatalf("old port still pinned in %v", pin.Ports)
		}
	}
	final := tbl.current()
	if !final.DenyRest || len(final.Allows) != 3 ||
		!slices.Equal(final.Allows[1].Ports, []uint16{2222}) || len(final.Allows[1].Sources) != 0 ||
		!slices.Equal(final.Allows[2].Ports, []uint16{2222}) ||
		!slices.Equal(final.Allows[2].Sources, []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}) {
		t.Fatalf("kept table didn't preserve the old SSH access: %+v", final.Allows)
	}
	if rules := m.Applied().Rules; rules["ssh-anywhere"].GetPort() != 2222 || rules["dns-local"].GetPort() != 22 {
		t.Fatalf("firewall read didn't show the ports it applies: %v", rules)
	}
	confirmed, err := os.ReadFile(filepath.Join(dir, "confirmed.json"))
	if err != nil || strings.Contains(string(confirmed), "ssh_rule_ports") {
		t.Fatalf("kept SSH port was still stored as a binding: %s, %v", confirmed, err)
	}
	rule, err := m.Rule(t.Context(), "ssh-anywhere")
	if err != nil || rule.GetPort() != 2222 {
		t.Fatalf("stored rule = %v, %v", rule, err)
	}
	rule.Sources = []string{"198.51.100.128/25"}
	go func() {
		edit := resource.WithOutcome(step("chg_source", true), func(o resource.Outcome) { reported <- o.Waits })
		done <- m.Put(edit, "ssh-anywhere", rule)
	}()
	sshAnnounced(t, reported, done)
	if got := m.Confirm(t.Context(), "chg_source"); got != resource.AccessKept {
		t.Fatalf("source edit returned %v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := m.Applied().Rules["ssh-anywhere"]; r.GetPort() != 2222 || !slices.Equal(r.GetSources(), rule.Sources) {
		t.Fatalf("source edit changed the SSH port: %v", r)
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
				move := &dockwellv1.SshPortMove{FromPort: 22, ToPort: 2222}
				done <- m.PutSSH(resource.WithOutcome(ctx, func(o resource.Outcome) { reported <- o.Waits }), &resourcev1.SshConfig{Port: 2222, Account: "admin"}, move)
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
			if ssh.file != "original" || ssh.restored == 0 || ssh.disarmed == 0 {
				t.Fatal("original SSH settings were not restored")
			}
			if tbl.rules() != "(no table)" {
				t.Fatal("trial firewall table was not removed")
			}
		})
	}
}

func TestSSHKeepFailureSaysHowToRecover(t *testing.T) {
	m, _ := manager(t, t.TempDir(), time.Minute)
	ssh := &sshFixture{file: "original", finalError: errors.New("fixture reload failed")}
	ssh.proof.Store(true)
	m.SSH = ssh
	reported := make(chan *resource.AccessWait, 1)
	done := make(chan error, 1)
	ctx := resource.WithOutcome(step("chg_ssh", true), func(o resource.Outcome) { reported <- o.Waits })
	go func() {
		done <- m.PutSSH(ctx, &resourcev1.SshConfig{Port: 2222}, &dockwellv1.SshPortMove{FromPort: 22, ToPort: 2222})
	}()
	sshAnnounced(t, reported, done)
	got, failure := m.ConfirmDetails(t.Context(), "chg_ssh")
	if got != resource.AccessRolledBack || !strings.Contains(failure, "Check their service logs") {
		t.Fatalf("Keep = %v, %q", got, failure)
	}
	if err := <-done; err == nil || err.Error() != failure || ssh.file != "original" || ssh.disarmed != 1 {
		t.Fatalf("reverted SSH file %q, helper removed %d times, apply error %v", ssh.file, ssh.disarmed, err)
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

func TestSSHPortPlanNamesARuleItCannotMove(t *testing.T) {
	m, _ := manager(t, t.TempDir(), time.Minute)
	m.SSH = &sshFixture{}
	m.confirmed.Rules["blocked"] = &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	if _, err := m.CheckSSH(t.Context(), &resourcev1.SshConfig{Port: 2222, Account: "admin"}); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("plan with a deny rule returned %v", err)
	}
}

func TestSSHTrialPeerReachIncludesAddressRules(t *testing.T) {
	trial := &SSHChange{Port: 2222, ProofPeer: netip.MustParseAddr("203.0.113.7")}
	r := nftables.Rule{Sources: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}
	for _, protocol := range []string{"", "tcp", "udp"} {
		r.Protocol = protocol
		if got := sshPeerReachable(nftables.Table{Denies: []nftables.Rule{r}}, trial); got != (protocol == "udp") {
			t.Errorf("protocol %q, address deny reach = %t", protocol, got)
		}
		if got := sshPeerReachable(nftables.Table{Allows: []nftables.Rule{r}, DenyRest: true}, trial); got != (protocol != "udp") {
			t.Errorf("protocol %q, address allow reach = %t", protocol, got)
		}
	}
}
