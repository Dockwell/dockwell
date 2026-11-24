// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/nftables"
)

// SSHChange is a validated and reversible change to the agent's SSH drop-in.
type SSHChange struct {
	Account                   string
	OldPort, Port             uint16
	OldFile, Final, Candidate string
	Unit                      string
	From, Targets             []netip.Addr
	Marker                    string
	RestrictsAccess           bool
	Addresses                 []string
	Started                   uint64
}

// SSHBackend applies the drop-in and tests the new SSH listener.
type SSHBackend interface {
	Plan(context.Context, *resourcev1.SshConfig, *resourcev1.SshConfig) (*SSHChange, error)
	Arm(context.Context, string, string, time.Duration, *SSHChange) error
	Install(context.Context, *SSHChange, string) error
	Restore(context.Context, *SSHChange) error
	Started() (uint64, error)
	Proved(context.Context, *SSHChange) bool
}

// SSHConfig returns the settings last kept by the access manager.
func (m *Manager) SSHConfig(ctx context.Context) *resourcev1.SshConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return proto.CloneOf(m.view(ctx).SSH)
}

// CheckSSH checks that a proposed SSH change can be tested and restored.
func (m *Manager) CheckSSH(ctx context.Context, next *resourcev1.SshConfig) error {
	if m.SSH == nil {
		return errors.New("this agent cannot safely reload and restore SSH settings")
	}
	trial, err := m.SSH.Plan(ctx, m.SSHConfig(ctx), next)
	if err != nil {
		return err
	}
	return m.checkPanelSSH(trial)
}

// PutSSH applies SSH settings under the server's shared access timer.
func (m *Manager) PutSSH(ctx context.Context, next *resourcev1.SshConfig) error {
	return m.editAccess(ctx, func(state *state) { state.SSH = proto.CloneOf(next) }, true)
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

// ProvedSSH identifies a waiting change whose new settings accepted a fresh
// connection. Confirm independently enforces the same test.
func (m *Manager) ProvedSSH(ctx context.Context) string {
	m.mu.Lock()
	w := m.waiting
	if w == nil || w.ssh == nil || !w.announced || !m.clock().Before(w.deadline) {
		m.mu.Unlock()
		return ""
	}
	change, test := w.change, *w.ssh
	m.mu.Unlock()
	if m.SSH.Proved(ctx, &test) {
		return change
	}
	return ""
}

func (m *Manager) startSSHLocked(ctx context.Context, w waitingFile) error {
	if w.SSH == nil || w.Wait == m.confirmed.wait {
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
	return m.SSH.Restore(context.WithoutCancel(ctx), w.SSH)
}

func (m *Manager) recoverSSHBeforeProbe(ctx context.Context) error {
	if m.SSH == nil {
		return nil
	}
	var w waitingFile
	if err := readJSON(filepath.Join(m.Dir, "waiting.json"), &w, json.Unmarshal); err != nil || w.SSH == nil {
		return err
	}
	var c confirmedState
	if err := readJSON(filepath.Join(m.Dir, "confirmed.json"), &c, decodeConfirmed); err != nil {
		return err
	}
	m.confirmed = c
	return m.startSSHLocked(ctx, w)
}

func (m *Manager) restoreSSHLocked(ctx context.Context) error {
	var w waitingFile
	if err := readJSON(filepath.Join(m.Dir, "waiting.json"), &w, json.Unmarshal); err != nil {
		return err
	}
	return m.startSSHLocked(ctx, w)
}

func sshPortPin(port uint16, peers []netip.Addr) nftables.Rule {
	rule := nftables.Rule{Name: "ssh old listener", Protocol: "tcp", Ports: []uint16{port}}
	for _, address := range peers {
		rule.Sources = append(rule.Sources, netip.PrefixFrom(address, address.BitLen()))
	}
	return rule
}

func (m *Manager) armAccessLocked(ctx context.Context, change, wait string, ssh *SSHChange) error {
	if err := m.armDeadManLocked(ctx, change); err != nil {
		return err
	}
	if ssh == nil {
		return nil
	}
	return m.SSH.Arm(context.WithoutCancel(ctx), m.Dir, wait, m.timer()+45*time.Second, ssh)
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
