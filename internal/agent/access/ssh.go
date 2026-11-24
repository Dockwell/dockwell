// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/nftables"
)

// SSHChange is a validated and reversible change to the agent's SSH drop-in.
type SSHChange struct {
	Account                   string
	OldPort, Port             uint16
	OldFile, Final, Candidate string
	Unit                      string
	From, Targets             []netip.Addr
	ProofPeer, ProofHost      netip.Addr
	ProofKeys                 []string
	Marker                    string
	Challenge                 string
	RestrictsAccess           bool
	Addresses                 []string
	Started                   uint64
	StartedAt                 time.Time
	Jail                      *SSHJail
}

// SSHJail holds the port override to restore, trial and keep.
type SSHJail struct {
	Old, Candidate, Final string
	Active                bool
}

// SSHProof identifies the login that carried this trial's challenge.
type SSHProof struct {
	Address, Method, Fingerprint string
}

// SSHBackend applies the drop-in and tests the new SSH listener.
type SSHBackend interface {
	Plan(context.Context, *resourcev1.SshConfig, *resourcev1.SshConfig) (*SSHChange, error)
	Arm(context.Context, string, string, time.Duration, *SSHChange) error
	Rearm(context.Context, string, string, time.Duration, *SSHChange) error
	Install(context.Context, *SSHChange, string) error
	Restore(context.Context, *SSHChange) error
	Started() (uint64, error)
	CandidateLive(*SSHChange) bool
	Proved(context.Context, *SSHChange) (*SSHProof, netip.Addr)
	Disarm(context.Context, string) error
}

// SSHConfig returns the settings last kept by the access manager.
func (m *Manager) SSHConfig(ctx context.Context) *resourcev1.SshConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return proto.CloneOf(m.view(ctx).SSH)
}

// CheckSSH checks that a proposed SSH change can be tested and restored.
func (m *Manager) CheckSSH(ctx context.Context, next *resourcev1.SshConfig) (*dockwellv1.SshPortMove, error) {
	if m.SSH == nil {
		return nil, errors.New("this agent cannot safely reload and restore SSH settings")
	}
	m.mu.Lock()
	base := m.confirmed.clone()
	m.mu.Unlock()
	trial, err := m.SSH.Plan(ctx, base.SSH, next)
	if err != nil {
		return nil, err
	}
	if err := m.checkPanelSSH(trial); err != nil {
		return nil, err
	}
	if trial.Port == trial.OldPort {
		return nil, nil
	}
	return base.planSSHMove(trial)
}

func (s *state) planSSHMove(trial *SSHChange) (*dockwellv1.SshPortMove, error) {
	if trial.Port == trial.OldPort {
		return nil, nil
	}
	names, err := s.moveSSHRules(trial.OldPort, trial.Port)
	if err != nil {
		return nil, err
	}
	return &dockwellv1.SshPortMove{FromPort: uint32(trial.OldPort), ToPort: uint32(trial.Port), FirewallRules: names, Fail2Ban: trial.Jail != nil}, nil
}

// Move the ports in the candidate rule specs. Keep persists these specs with
// the tested table; the control plane records the matching rule revisions.
func (s *state) moveSSHRules(from, to uint16) ([]string, error) {
	var moved []string
	for _, name := range slices.Sorted(maps.Keys(s.Rules)) {
		rule := s.Rules[name]
		if protocolOf(rule) != "tcp" || (portOf(rule) != from && portOf(rule) != to) {
			continue
		}
		if isDeny(rule) || portOf(rule) == to {
			return nil, fmt.Errorf("firewall rule %q uses TCP port %d. Review that rule before moving SSH", name, rule.GetPort())
		}
		moved = append(moved, name)
	}
	for _, name := range moved {
		s.Rules[name].Port = uint32(to)
	}
	return moved, nil
}

// PutSSH applies SSH settings under the server's shared access timer.
func (m *Manager) PutSSH(ctx context.Context, next *resourcev1.SshConfig, move *dockwellv1.SshPortMove) error {
	return m.editAccess(ctx, func(state *state) { state.SSH = proto.CloneOf(next) }, true, move)
}

// HoldSSHKeys serializes key writes with SSH settings so that editing keys
// cannot invalidate a credential during a trial.
func (m *Manager) HoldSSHKeys() (func(), error) {
	m.mu.Lock()
	if (m.waiting != nil && m.waiting.ssh != nil) || m.unrestored != nil {
		m.mu.Unlock()
		return nil, errors.New("an SSH access change is still being tested or restored. Wait for it to finish before editing keys")
	}
	return m.mu.Unlock, nil
}

// SSHTestDetails reports the waiting trial, the accepted login, and any test
// that authenticated with the right command from a different client address.
func (m *Manager) SSHTestDetails(ctx context.Context) (string, SSHChange, *SSHProof, netip.Addr) {
	m.mu.Lock()
	w := m.waiting
	if w == nil || w.ssh == nil || !w.announced || !m.clock().Before(w.deadline) {
		m.mu.Unlock()
		return "", SSHChange{}, nil, netip.Addr{}
	}
	test, proof := *w.ssh, w.sshProof
	m.mu.Unlock()
	var unexpected netip.Addr
	live := m.SSH.CandidateLive(&test)
	if live && proof == nil {
		proof, unexpected = m.SSH.Proved(ctx, &test)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if w != m.waiting || !m.clock().Before(w.deadline) {
		return "", SSHChange{}, nil, netip.Addr{}
	}
	if !live {
		return w.change, test, nil, netip.Addr{}
	}
	if w.sshProof == nil && proof != nil {
		w.sshProof = proof
	}
	if unexpected.IsValid() {
		w.sshUnexpected = unexpected
	}
	return w.change, test, w.sshProof, w.sshUnexpected
}

func (m *Manager) startSSHLocked(ctx context.Context, w waitingFile) error {
	if w.SSH == nil {
		return nil
	}
	if m.SSH == nil {
		return errors.New("SSH rollback needs the agent's SSH backend")
	}
	unlock, err := m.lockSSH()
	if err != nil {
		return err
	}
	defer unlock()
	if w.Wait != m.confirmed.wait {
		if err := m.SSH.Restore(context.WithoutCancel(ctx), w.SSH); err != nil {
			return err
		}
	}
	m.disarmSSH(ctx)
	return nil
}

func (m *Manager) recoverSSHBeforeProbe(ctx context.Context) error {
	if m.SSH == nil {
		return nil
	}
	var w waitingFile
	if err := readJSON(filepath.Join(m.Dir, "waiting.json"), &w, json.Unmarshal); err != nil {
		return err
	}
	if w.SSH == nil {
		m.disarmSSH(ctx)
		return nil
	}
	var c confirmedState
	if err := readJSON(filepath.Join(m.Dir, "confirmed.json"), &c, decodeConfirmed); err != nil {
		return err
	}
	m.confirmed = c
	return m.startSSHLocked(ctx, w)
}

func (m *Manager) disarmSSH(ctx context.Context) {
	if err := m.SSH.Disarm(context.WithoutCancel(ctx), m.Dir); err != nil {
		m.logger().WarnContext(ctx, "can't remove the SSH rollback helper", "error", err)
	}
}

func (m *Manager) restoreSSHLocked(ctx context.Context) error {
	var w waitingFile
	if err := readJSON(filepath.Join(m.Dir, "waiting.json"), &w, json.Unmarshal); err != nil {
		return err
	}
	return m.startSSHLocked(ctx, w)
}

func sshPortPin(name string, port uint16, peers []netip.Prefix) nftables.Rule {
	return nftables.Rule{Name: name, Protocol: "tcp", Ports: []uint16{port}, Sources: slices.Clone(peers)}
}

func sshPeerReachable(table nftables.Table, trial *SSHChange) bool {
	matches := func(rule nftables.Rule) bool {
		return rule.Protocol == "tcp" && slices.Contains(rule.Ports, trial.Port) &&
			(len(rule.Sources) == 0 || slices.ContainsFunc(rule.Sources, func(p netip.Prefix) bool {
				return p.Contains(trial.ProofPeer)
			}))
	}
	if slices.ContainsFunc(table.Pins, matches) || slices.ContainsFunc(table.Allows, matches) {
		return true
	}
	return !table.DenyRest && !slices.ContainsFunc(table.Denies, matches)
}

func (m *Manager) armAccessLocked(ctx context.Context, change, wait string, ssh *SSHChange) error {
	if err := m.armDeadManLocked(ctx, change); err != nil {
		return err
	}
	if ssh == nil {
		return nil
	}
	// Budget the staged validation, jail reload and listener checks before
	// counting the owner’s full confirmation window.
	return m.SSH.Arm(context.WithoutCancel(ctx), m.Dir, wait, m.timer()+2*time.Minute, ssh)
}

func (m *Manager) undoSSHLocked(fn func(*state)) error {
	next := m.confirmed.clone()
	fn(&next)
	if proto.Equal(next.SSH, m.confirmed.SSH) {
		return nil
	}
	return errors.New("SSH still differs from the confirmed settings. Plan a guarded restoration of SSH settings")
}

// lockSSH shares a file lock with the independent SSH rollback service. The
// manager holds it across the live change and its confirmation on disk.
func (m *Manager) lockSSH() (func(), error) {
	file, err := os.OpenFile(filepath.Join(m.Dir, "ssh.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}

func (m *Manager) checkPanelSSH(trial *SSHChange) error {
	if m.PanelOverSSH && trial.RestrictsAccess {
		return errors.New("the panel is reached over SSH. Give the panel a domain before restricting SSH access")
	}
	return nil
}
