// SPDX-License-Identifier: AGPL-3.0-only

// Package access keeps who reaches the server, as ADR 0052 has it: the firewall
// rules and policy confirmed, and the one change to them that waits for its
// confirmation, on disk and in Dockwell's nftables table. A change loads at
// once, under a timer; the person who applied it confirms they can still reach
// the server, and the agent records it as confirmed on disk before it says it
// kept it. If nobody does in time, or the agent stops first, the agent puts
// back the table as it loaded when it was last confirmed, on its own.
package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

// Timer is how long a change waits for its confirmation before the agent puts
// back what was confirmed.
const Timer = 60 * time.Second

// waitGrace is how much longer than its timer a step waits for its verdict,
// for the verdict to be reached.
const waitGrace = 30 * time.Second

// readWait bounds each read of the server a change makes before it loads: the
// pins, from /proc and systemd, and Docker's networks.
const readWait = 15 * time.Second

// networksEvery is how often the agent checks Docker's networks against those
// the confirmed table leaves to their containers.
const networksEvery = 15 * time.Second

// keptVerdicts is how many changes' verdicts the agent remembers, for a late
// confirmation or a resumed step to hear.
const keptVerdicts = 32

// Table loads and reads Dockwell's nftables table: nftables.Firewall, or a
// stand-in for a test.
type Table interface {
	Load(ctx context.Context, t nftables.Table) (nftables.Loaded, error)
	Delete(ctx context.Context) error
	Probe(ctx context.Context) error
}

// Manager keeps who reaches the server. Its methods are the firewall drivers'
// backend.
type Manager struct {
	// Dir holds its state: confirmed.json, waiting.json, verdicts.json, and
	// confirmed.nft, the table as last confirmed, for loading at boot.
	Dir   string
	Table Table
	// Pins says what keeps the server reachable beyond the table's own: SSH from
	// where people come in now, and the control plane's ports on its server.
	Pins func(ctx context.Context) (Pins, error)
	// PanelOverSSH is a control server whose panel has no domain, so people
	// reach it through SSH (ssh -L): there, deny everything else is refused
	// unless a rule allows SSH's port.
	PanelOverSSH bool
	// Docker reads Docker's networks. Nil is a server without Docker.
	Docker func(ctx context.Context) (DockerNetworks, error)
	Logger *slog.Logger
	// Timer replaces Timer, for a test.
	Timer time.Duration
	now   func() time.Time
	after func(time.Duration, func()) stopper
	// every is how often KeepCurrent checks, for a test; zero is networksEvery.
	every time.Duration
	// readWait replaces readWait, for a test.
	readWait time.Duration

	mu        sync.Mutex
	confirmed confirmedState
	waiting   *pending
	verdicts  []verdict
	// docker is Docker's networks as it last listed them, nil until it has.
	docker   *DockerNetworks
	probed   bool
	unusable error
	// unrestored says why the table as confirmed isn't loaded, after a revert
	// or a start that couldn't load it: the agent tries again at each check.
	unrestored error
}

var (
	_ firewall.Backend       = (*Manager)(nil)
	_ firewall.PolicyBackend = (*Manager)(nil)
	_ firewall.Checker       = (*Manager)(nil)
	_ firewall.Waiter        = (*Manager)(nil)
	_ resource.SharedReadier = (*Manager)(nil)
)

// state is the rules and policy, by name, and the SSH pins they keep.
type state struct {
	Rules  map[string]*resourcev1.FirewallRule
	Policy *resourcev1.FirewallPolicy
	Pinned Pinned
}

// Pinned are the SSH ports and addresses a state keeps reaching SSH: those of
// every session open as a change to it loaded, kept from one confirmed change
// to the next, so a person who logged out since isn't locked out at the next
// load, a restart or a revert. Only a change could drop one, and no kind of
// change does yet.
type Pinned struct {
	SSHPorts []uint16       `json:"ssh_ports,omitempty"`
	SSHFrom  []netip.Prefix `json:"ssh_from,omitempty"`
}

func (s state) clone() state {
	out := state{Rules: make(map[string]*resourcev1.FirewallRule, len(s.Rules))}
	for name, r := range s.Rules {
		out.Rules[name] = proto.CloneOf(r)
	}
	if s.Policy != nil {
		out.Policy = proto.CloneOf(s.Policy)
	}
	out.Pinned = Pinned{SSHPorts: slices.Clone(s.Pinned.SSHPorts), SSHFrom: slices.Clone(s.Pinned.SSHFrom)}
	return out
}

func (s state) empty() bool { return len(s.Rules) == 0 && !s.Policy.GetDenyEverythingElse() }

// confirmedState is what was last confirmed, and the table it loaded as, which
// a revert or a start loads again as it is, needing neither Docker nor the
// pins read again.
type confirmedState struct {
	state
	table nftables.Table
	// wait is the wait that was confirmed, so a start can tell a confirmed
	// change from one that waited.
	wait string
}

// pending is the change that waits for its confirmation.
type pending struct {
	change string
	// wait names this wait, on disk too.
	wait     string
	state    state
	table    nftables.Table
	sshPorts []uint16
	deadline time.Time
	timer    stopper
	// announced is the change's last step here having said it waits: before
	// that, part of the change hasn't loaded, and there's nothing whole to keep.
	announced bool
	// done closes once the change has its verdict.
	done chan struct{}
}

type stopper interface{ Stop() bool }

type verdict struct {
	Change  string                 `json:"change"`
	Verdict resource.AccessVerdict `json:"verdict"`
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) afterFunc(d time.Duration, f func()) stopper {
	if m.after != nil {
		return m.after(d, f)
	}
	return time.AfterFunc(d, f)
}

func (m *Manager) timer() time.Duration {
	if m.Timer > 0 {
		return m.Timer
	}
	return Timer
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// ReadyKey shares one probe between the rule's and the policy's drivers.
func (m *Manager) ReadyKey() any { return m }

// errStarting is the table as confirmed not loaded yet, as the agent starts.
var errStarting = errors.New("the agent hasn't loaded the firewall as confirmed yet")

// Ready says whether the server can hold Dockwell's table: it has nft, and its
// kernel takes a table. The first time it can, the agent reads what was
// confirmed, puts back a change that waited when it stopped, and loads the
// table as it was confirmed, or removes Dockwell's table with nothing
// confirmed, before it offers the firewall's kinds. If that table won't load,
// it tries again at each check, while it offers none.
func (m *Manager) Ready(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.probed {
		if m.unusable = m.Table.Probe(ctx); m.unusable != nil {
			// No nft, a kernel that takes no table, or a test that runs no tools:
			// the server as it is, not something gone wrong.
			if errors.Is(m.unusable, nftables.ErrNoNft) || errors.Is(m.unusable, nftables.ErrRefused) || errors.Is(m.unusable, hosttool.ErrOff) {
				m.unusable = resource.NotHere(m.unusable)
			}
			return m.unusable
		}
		if err := m.startLocked(ctx); err != nil {
			m.unusable = fmt.Errorf("can't put the firewall back as it was confirmed: %w", err)
			return m.unusable
		}
		m.probed = true
	}
	if m.unrestored == nil || m.waiting != nil {
		return nil
	}
	if err := m.restoreLocked(ctx); err != nil {
		return fmt.Errorf("can't put the firewall back as it was confirmed: %w", err)
	}
	if m.unrestored != nil {
		return m.unrestored
	}
	// Loaded as confirmed: a change that waited is over, on disk too.
	return m.removeWaiting()
}

// startLocked reads what was confirmed, and the change that waited when the
// agent stopped, if one did: its verdict is rolled back, unless it was
// confirmed on disk and the agent stopped before it said so. Ready then loads
// the table as confirmed, so a restart or a reboot inside a change's timer
// keeps only what was confirmed.
func (m *Manager) startLocked(ctx context.Context) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return fmt.Errorf("can't make the folder for the firewall's state: %w", err)
	}
	var c confirmedState
	if err := readJSON(filepath.Join(m.Dir, "confirmed.json"), &c, decodeConfirmed); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(m.Dir, "verdicts.json"), &m.verdicts, json.Unmarshal); err != nil {
		return err
	}
	var w waitingFile
	if err := readJSON(filepath.Join(m.Dir, "waiting.json"), &w, json.Unmarshal); err != nil {
		return err
	}
	m.confirmed = c
	switch {
	case w.Change == "":
	case w.Wait != "" && w.Wait == c.wait:
		m.record(w.Change, resource.AccessKept)
	default:
		m.logger().WarnContext(ctx, "a change to the firewall waited for its confirmation when the agent stopped, so it puts back what was confirmed",
			slog.String("change", w.Change))
		m.record(w.Change, resource.AccessRolledBack)
	}
	m.unrestored = errStarting
	return nil
}

// KeepCurrent renders the confirmed table again whenever Docker's networks
// change, and once as it starts, until ctx is done, so a Compose network made
// since the last change to the firewall is left to its containers, and the
// ports it publishes are filtered (ADR 0052). Only Docker's networks change:
// the rules, the policy and the pins stay as confirmed, and the new table is
// confirmed on disk as it loads. Nothing changes while a change waits; its
// own confirmation or revert renders it.
func (m *Manager) KeepCurrent(ctx context.Context) {
	every := networksEvery
	if m.every > 0 {
		every = m.every
	}
	for {
		// A revert that failed, or a start that couldn't load what was confirmed,
		// is tried again here, on the agent's own clock: the stream to the
		// control plane may be the very thing the table cut.
		if err := m.Ready(ctx); err != nil && ctx.Err() == nil && !resource.IsNotHere(err) {
			m.logger().WarnContext(ctx, "the firewall isn't as it was confirmed yet; the agent tries again", slog.String("error", err.Error()))
		}
		if err := m.refresh(ctx); err != nil && ctx.Err() == nil {
			m.logger().WarnContext(ctx, "can't render the firewall again for Docker's networks; the agent tries again", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// refresh renders the confirmed table again if Docker's networks have changed.
func (m *Manager) refresh(ctx context.Context) error {
	if m.Docker == nil {
		return nil
	}
	// Docker alone: the pins don't change here, so /proc isn't read.
	bound := readWait
	if m.readWait > 0 {
		bound = m.readWait
	}
	dctx, cancel := context.WithTimeout(ctx, bound)
	nets, err := m.Docker(dctx)
	cancel()
	if errors.Is(err, ErrNoDocker) {
		nets, err = DockerNetworks{None: true}, nil
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docker = &nets
	if !m.probed || m.waiting != nil || m.unrestored != nil || m.confirmed.empty() {
		return nil
	}
	was := m.confirmed.table
	interfaces := slices.Sorted(slices.Values(nets.Interfaces))
	if nets.None {
		interfaces = nil
	}
	if was.NoDocker == nets.None && slices.Equal(was.DockerInterfaces, interfaces) {
		return nil
	}
	next := was
	next.DockerInterfaces, next.NoDocker = interfaces, nets.None
	loaded, err := m.Table.Load(ctx, next)
	m.skipped(ctx, loaded)
	if err != nil {
		if errors.Is(err, nftables.ErrUnknown) {
			_ = m.restoreLocked(ctx)
		}
		return err
	}
	c := confirmedState{state: m.confirmed.state, table: next, wait: m.confirmed.wait}
	if err := m.saveConfirmed(c); err != nil {
		// What loaded isn't on record, so what is goes back.
		_ = m.restoreLocked(ctx)
		return err
	}
	m.confirmed = c
	m.logger().InfoContext(ctx, "Docker's networks changed, so the firewall leaves them to their containers as they are now",
		slog.Any("interfaces", interfaces))
	return nil
}

// view is the state a call sees: the waiting change's, for a step of that
// change, and what was confirmed otherwise, as for planning, so that whether
// a step changes anything is judged against what was confirmed.
func (m *Manager) view(ctx context.Context) state {
	if d := resource.DeployOf(ctx); m.waiting != nil && d.Change == m.waiting.change {
		return m.waiting.state
	}
	return m.confirmed.state
}

// Rule returns the named rule, or nil.
func (m *Manager) Rule(ctx context.Context, name string) (*resourcev1.FirewallRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.view(ctx).Rules[name]; r != nil {
		return proto.CloneOf(r), nil
	}
	return nil, nil
}

// Put adds or replaces the named rule.
func (m *Manager) Put(ctx context.Context, name string, rule *resourcev1.FirewallRule) error {
	rule = proto.CloneOf(rule)
	return m.edit(ctx, func(s *state) { s.Rules[name] = rule })
}

// Delete removes the named rule.
func (m *Manager) Delete(ctx context.Context, name string) error {
	return m.edit(ctx, func(s *state) { delete(s.Rules, name) })
}

// Policy returns the server's policy, or nil.
func (m *Manager) Policy(ctx context.Context) (*resourcev1.FirewallPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.view(ctx).Policy; p != nil {
		return proto.CloneOf(p), nil
	}
	return nil, nil
}

// PutPolicy sets the policy.
func (m *Manager) PutPolicy(ctx context.Context, policy *resourcev1.FirewallPolicy) error {
	policy = proto.CloneOf(policy)
	return m.edit(ctx, func(s *state) { s.Policy = policy })
}

// DeletePolicy removes the policy.
func (m *Manager) DeletePolicy(ctx context.Context) error {
	return m.edit(ctx, func(s *state) { s.Policy = nil })
}

// ErrBusy is another change to the firewall that waits for its confirmation.
var ErrBusy = errors.New("another change to this server's firewall waits for its confirmation")

// edit carries out one step. A rollback restores what was confirmed with no
// timer, as it needs no confirmation of its own. Any other step is checked
// again, as the server is now, and loads under the change's timer, which
// starts with its first step; its last step on the server starts the timer
// again and waits for the verdict. A step of a change the server already put
// back fails, so the change rolls back rather than end with part of it here.
func (m *Manager) edit(ctx context.Context, fn func(*state)) error {
	d := resource.DeployOf(ctx)
	if d.Change == "" {
		return errors.New("a change to the firewall comes through a change, which names it")
	}
	// Read before the lock, so neither a slow Docker nor a slow /proc holds up
	// a confirmation or the timer.
	host := m.readHost(ctx)
	m.mu.Lock()
	if d.Rollback {
		defer m.mu.Unlock()
		return m.rollBackStepLocked(ctx, d.Change, fn, host)
	}
	if m.waiting != nil && m.waiting.change != d.Change {
		other := m.waiting.change
		m.mu.Unlock()
		return fmt.Errorf("%w (%s). Wait for it to be confirmed or rolled back, then try again", ErrBusy, other)
	}
	first := m.waiting == nil
	if first && m.verdictOf(d.Change) == resource.AccessRolledBack {
		m.mu.Unlock()
		return resource.Plainly(
			"This server put back its firewall as it was before this step, as nobody confirmed the change in time, so the change rolls back.",
			fmt.Errorf("%s was rolled back on this server before this step, so the step doesn't run", d.Change))
	}
	base := m.confirmed.state
	if !first {
		base = m.waiting.state
	}
	next := base.clone()
	fn(&next)
	next, tbl, pins, err := m.prepareLocked(ctx, next, host, false)
	if err == nil {
		err = check(next, pins, m.PanelOverSSH)
	}
	if err != nil {
		// Nothing loaded, but a change whose earlier steps did has them live.
		if !first {
			m.rollBackLocked(ctx, d.Change)
		}
		m.mu.Unlock()
		return err
	}
	wait := ""
	if first {
		// On disk before the table loads, so an agent that stops from here on
		// puts back what was confirmed as it starts again.
		wait = d.Change + "@" + strconv.FormatInt(m.clock().UnixNano(), 10)
		if err := m.saveWaiting(d.Change, wait); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	loaded, err := m.Table.Load(ctx, tbl)
	m.skipped(ctx, loaded)
	if err != nil {
		// A load nft refused changed nothing, but one that can't be seen through
		// may be live, and a change that had loaded steps before has them live:
		// either way, what was confirmed goes back.
		if !first || errors.Is(err, nftables.ErrUnknown) {
			m.rollBackLocked(ctx, d.Change)
		} else if rerr := m.removeWaiting(); rerr != nil {
			m.logger().WarnContext(ctx, "can't remove the record of a change to the firewall that didn't load; the agent puts back what was confirmed when it starts again",
				slog.String("change", d.Change), slog.String("error", rerr.Error()))
		}
		m.mu.Unlock()
		return err
	}
	if first {
		m.waiting = &pending{change: d.Change, wait: wait, done: make(chan struct{})}
		m.armLocked(d.Change)
	}
	m.waiting.state, m.waiting.table, m.waiting.sshPorts = next, tbl, pins.SSHPorts
	if !d.LastAccess {
		m.mu.Unlock()
		return nil
	}
	// The change's last step here: the person who applied it has the timer's
	// whole length from now to check and confirm.
	m.armLocked(d.Change)
	done, limit := m.waiting.done, m.announceLocked(ctx)
	m.mu.Unlock()
	return m.await(ctx, d.Change, done, limit)
}

// rollBackStepLocked carries out a step of a rollback: on what was confirmed,
// with no timer, ending first any change that waits, which a rollback
// overrides. What it loads is confirmed only once it's loaded and on disk.
func (m *Manager) rollBackStepLocked(ctx context.Context, change string, fn func(*state), host hostNow) error {
	if w := m.waiting; w != nil {
		m.rollBackLocked(ctx, w.change)
	}
	next := m.confirmed.clone()
	fn(&next)
	next, tbl, _, err := m.prepareLocked(ctx, next, host, true)
	if err != nil {
		return err
	}
	loaded, err := m.Table.Load(ctx, tbl)
	m.skipped(ctx, loaded)
	if err != nil {
		if errors.Is(err, nftables.ErrUnknown) {
			_ = m.restoreLocked(ctx)
		}
		return err
	}
	c := confirmedState{state: next, table: tbl, wait: change + "@rollback"}
	if err := m.saveConfirmed(c); err != nil {
		// What loaded isn't on record, so what is goes back.
		_ = m.restoreLocked(ctx)
		return err
	}
	m.confirmed, m.unrestored = c, nil
	return m.removeWaiting()
}

// armLocked starts, or starts again, the waiting change's timer.
func (m *Manager) armLocked(change string) {
	m.waiting.deadline = m.clock().Add(m.timer())
	if m.waiting.timer != nil {
		m.waiting.timer.Stop()
	}
	m.waiting.timer = m.afterFunc(m.timer(), func() { m.expire(change) })
}

// announceLocked tells whoever applies the step what it waits for, and returns
// how long to wait. The time left goes too, so a control plane whose clock
// isn't the agent's counts down the same.
func (m *Manager) announceLocked(ctx context.Context) time.Duration {
	m.waiting.announced = true
	left := m.waiting.deadline.Sub(m.clock())
	w := resource.AccessWait{Deadline: m.waiting.deadline, Left: left}
	for _, p := range m.waiting.sshPorts {
		w.SSHPorts = append(w.SSHPorts, uint32(p))
	}
	resource.Report(ctx, resource.Outcome{Waits: &w})
	return left + waitGrace
}

// await waits for a change's verdict.
func (m *Manager) await(ctx context.Context, change string, done <-chan struct{}, wait time.Duration) error {
	select {
	case <-done:
	case <-time.After(wait):
		return fmt.Errorf("no verdict on %s came within %s", change, wording(wait))
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	m.mu.Lock()
	v := m.verdictOf(change)
	m.mu.Unlock()
	if v == resource.AccessKept {
		return nil
	}
	return resource.Plainly(
		fmt.Sprintf("Nobody confirmed the change within %s, so the server put back its firewall as it was.", wording(m.timer())),
		fmt.Errorf("%s wasn't confirmed in time, so the server put back its firewall as it was", change))
}

// wording says a duration as the panel does: "60 s", or "500 ms" under a second.
func wording(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%d s", int64(d.Round(time.Second)/time.Second))
}

// Wait is a step's check: a resumed step of a change that still waits, waits
// for its verdict; one whose change was rolled back fails.
func (m *Manager) Wait(ctx context.Context) error {
	d := resource.DeployOf(ctx)
	if !d.LastAccess || d.Rollback {
		return nil
	}
	m.mu.Lock()
	if m.waiting != nil && m.waiting.change == d.Change {
		done, wait := m.waiting.done, m.announceLocked(ctx)
		m.mu.Unlock()
		return m.await(ctx, d.Change, done, wait)
	}
	v := m.verdictOf(d.Change)
	m.mu.Unlock()
	if v == resource.AccessRolledBack {
		return m.await(ctx, d.Change, closed, 0)
	}
	return nil
}

var closed = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// Confirm keeps a change that waits for its confirmation, if its time hasn't
// run out: it records the change as confirmed on disk, and only then says it
// kept it. What it records is the table as it loaded, so a confirmation reads
// nothing of the server and can't fail on it.
func (m *Manager) Confirm(ctx context.Context, change string) resource.AccessVerdict {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.waiting
	if w == nil || w.change != change {
		if v := m.verdictOf(change); v != 0 {
			return v
		}
		return resource.AccessNotWaiting
	}
	if !m.clock().Before(w.deadline) {
		// The timer is about to put it back, if it hasn't yet: running out wins.
		m.rollBackLocked(ctx, change)
		return resource.AccessRolledBack
	}
	if !w.announced {
		// Its last step here hasn't loaded yet: keeping it now would keep part of it.
		return resource.AccessNotWaiting
	}
	c := confirmedState{state: w.state, table: w.table, wait: w.wait}
	if err := m.saveConfirmed(c); err != nil {
		m.logger().ErrorContext(ctx, "can't record a confirmed change to the firewall, so the agent puts back what was confirmed before",
			slog.String("change", change), slog.String("error", err.Error()))
		m.rollBackLocked(ctx, change)
		return resource.AccessRolledBack
	}
	m.confirmed = c
	w.timer.Stop()
	m.waiting = nil
	m.record(change, resource.AccessKept)
	if err := m.removeWaiting(); err != nil {
		// confirmed.json names this wait, so a start reads it as kept.
		m.logger().WarnContext(ctx, "can't remove the record of a change to the firewall that waited", slog.String("error", err.Error()))
	}
	close(w.done)
	return resource.AccessKept
}

// expire is a change's timer running out.
func (m *Manager) expire(change string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting == nil || m.waiting.change != change {
		return
	}
	m.logger().Warn("nobody confirmed a change to the firewall in time, so the agent puts back what was confirmed", slog.String("change", change))
	m.rollBackLocked(context.Background(), change)
}

// rollBackLocked puts back the table as last confirmed, and ends the waiting
// change with its verdict. Its record on disk goes only once that's done: if
// neither the confirmed table nor no table at all would load, the change's own
// may still be live, and the agent puts back what was confirmed at its next
// check, and as it starts again.
func (m *Manager) rollBackLocked(ctx context.Context, change string) {
	err := m.restoreLocked(ctx)
	m.record(change, resource.AccessRolledBack)
	if w := m.waiting; w != nil && w.change == change {
		if w.timer != nil {
			w.timer.Stop()
		}
		close(w.done)
		m.waiting = nil
	}
	if err != nil {
		m.logger().ErrorContext(ctx, "can't put back the firewall as it was confirmed, or remove its table, so the change's table may still be live. The agent tries again at its next check; check nft list ruleset",
			slog.String("change", change), slog.String("error", err.Error()))
		return
	}
	if err := m.removeWaiting(); err != nil {
		m.logger().WarnContext(ctx, "can't remove the record of a change to the firewall that waited", slog.String("error", err.Error()))
	}
}

// restoreLocked loads the table as it was confirmed, as it loaded then, which
// needs neither Docker nor the pins read again; with nothing confirmed, it
// removes Dockwell's table. A confirmed table that won't load goes too, which
// leaves what the server's own firewall does, and the agent tries again to
// load it at its next check. It fails only when neither works.
func (m *Manager) restoreLocked(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	if m.confirmed.empty() {
		if err := m.Table.Delete(ctx); err != nil {
			m.unrestored = err
			return err
		}
		m.unrestored = nil
		return nil
	}
	loaded, err := m.Table.Load(ctx, m.confirmed.table)
	m.skipped(ctx, loaded)
	if err == nil {
		m.unrestored = nil
		return nil
	}
	m.logger().ErrorContext(ctx, "can't load the firewall as it was confirmed, so the agent removes its table, which leaves what the server's own firewall does, and tries again at its next check",
		slog.String("error", err.Error()))
	m.unrestored = fmt.Errorf("can't load the firewall as it was confirmed: %w", err)
	if derr := m.Table.Delete(ctx); derr != nil {
		return fmt.Errorf("%w, and can't remove its table either: %w", m.unrestored, derr)
	}
	return nil
}

func (m *Manager) skipped(ctx context.Context, loaded nftables.Loaded) {
	for _, name := range loaded.Skipped {
		m.logger().WarnContext(ctx, "the firewall leaves out one of Docker's interfaces", slog.String("interface", name), slog.String("why", nftables.SkippedWords(name)))
	}
}

// record keeps a change's verdict, once.
func (m *Manager) record(change string, v resource.AccessVerdict) {
	if m.verdictOf(change) == v {
		return
	}
	m.verdicts = append(m.verdicts, verdict{Change: change, Verdict: v})
	if len(m.verdicts) > keptVerdicts {
		m.verdicts = m.verdicts[len(m.verdicts)-keptVerdicts:]
	}
	if err := writeJSON(filepath.Join(m.Dir, "verdicts.json"), m.verdicts); err != nil {
		m.logger().Error("can't record what came of a change to the firewall", slog.String("change", change), slog.String("error", err.Error()))
	}
}

func (m *Manager) verdictOf(change string) resource.AccessVerdict {
	for i := len(m.verdicts) - 1; i >= 0; i-- {
		if m.verdicts[i].Change == change {
			return m.verdicts[i].Verdict
		}
	}
	return 0
}

// hostNow is what the server has now, as a change reads it before it loads.
type hostNow struct {
	pins      Pins
	pinsErr   error
	docker    DockerNetworks
	dockerErr error
}

// readHost reads the pins and Docker's networks, each within readWait.
func (m *Manager) readHost(ctx context.Context) hostNow {
	var h hostNow
	bound := readWait
	if m.readWait > 0 {
		bound = m.readWait
	}
	if m.Pins != nil {
		pctx, cancel := context.WithTimeout(ctx, bound)
		h.pins, h.pinsErr = m.Pins(pctx)
		cancel()
	}
	if m.Docker != nil {
		dctx, cancel := context.WithTimeout(ctx, bound)
		h.docker, h.dockerErr = m.Docker(dctx)
		cancel()
	}
	return h
}

// networksLocked are Docker's networks for the next table: as Docker lists them
// now, or, when it doesn't answer, as it last did, or as the confirmed table
// has them; a server without Docker has none.
func (m *Manager) networksLocked(ctx context.Context, h hostNow) (DockerNetworks, error) {
	switch {
	case m.Docker == nil:
		return DockerNetworks{None: true}, nil
	case h.dockerErr == nil:
		nets := h.docker
		m.docker = &nets
		return nets, nil
	case m.docker != nil:
		m.logger().WarnContext(ctx, "Docker didn't answer, so the firewall takes its networks as Docker last listed them", slog.String("error", h.dockerErr.Error()))
		return *m.docker, nil
	case !m.confirmed.empty():
		m.logger().WarnContext(ctx, "Docker didn't answer, so the firewall takes its networks as they were last confirmed", slog.String("error", h.dockerErr.Error()))
		return DockerNetworks{None: m.confirmed.table.NoDocker, Interfaces: slices.Clone(m.confirmed.table.DockerInterfaces)}, nil
	case errors.Is(h.dockerErr, ErrNoDocker):
		return DockerNetworks{None: true}, nil
	}
	return DockerNetworks{}, fmt.Errorf("can't read Docker's networks, which the firewall leaves to their containers, so it stays as it is: %w. Check Docker answers, with docker network ls, then try again", h.dockerErr)
}

// prepareLocked works out the table a state loads as: its rules, the pins, as
// the sessions open now add to those it keeps, less sessions from Docker's
// networks, and Docker's interfaces. It returns the state with what it now
// keeps pinned. A rollback that can't read the pins keeps those it has.
func (m *Manager) prepareLocked(ctx context.Context, s state, h hostNow, rollback bool) (state, nftables.Table, Pins, error) {
	nets, err := m.networksLocked(ctx, h)
	if err != nil {
		return s, nftables.Table{}, Pins{}, err
	}
	found := h.pins
	if h.pinsErr != nil {
		if !rollback {
			return s, nftables.Table{}, Pins{}, fmt.Errorf("can't tell what keeps the server reachable, so the firewall stays as it is: %w", h.pinsErr)
		}
		// The sessions open now are unknown; the control plane's own ports never
		// needed /proc, so their pin stays.
		found = Pins{Own: h.pins.Own}
	}
	pins := withPinned(found, s.Pinned, nets.Subnets)
	s.Pinned = Pinned{SSHPorts: slices.Clone(pins.SSHPorts), SSHFrom: slices.Clone(pins.SSHFrom)}
	if !narrowsSSH(s, pins.SSHPorts) {
		// Nothing here closes SSH to anyone, so no address needs keeping: the
		// sessions open now are pinned in this table, and none is kept for the
		// next. That's how a pinned address leaves: allow SSH's port, or turn
		// deny everything else off, and keep that change.
		s.Pinned = Pinned{}
	}
	return s, render(s, pins, nets), pins, nil
}

// withPinned is the pins found now, less sessions from Docker's networks, with
// what a state keeps pinned.
func withPinned(found Pins, kept Pinned, docker []netip.Prefix) Pins {
	out := Pins{Own: slices.Clone(found.Own)}
	for _, p := range found.SSHPorts {
		out.SSHPorts = appendNew(out.SSHPorts, p)
	}
	for _, p := range kept.SSHPorts {
		out.SSHPorts = appendNew(out.SSHPorts, p)
	}
	for _, from := range found.SSHFrom {
		if !slices.ContainsFunc(docker, func(n netip.Prefix) bool { return n.Contains(from.Addr()) }) {
			out.SSHFrom = appendNew(out.SSHFrom, from)
		}
	}
	for _, from := range kept.SSHFrom {
		out.SSHFrom = appendNew(out.SSHFrom, from)
	}
	slices.Sort(out.SSHPorts)
	slices.SortFunc(out.SSHFrom, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return out
}

// render is the table a state loads as.
func render(s state, pins Pins, nets DockerNetworks) nftables.Table {
	t := nftables.Table{Pins: pins.Rules(), DenyRest: s.Policy.GetDenyEverythingElse(), NoDocker: nets.None}
	if !nets.None {
		t.DockerInterfaces = slices.Clone(nets.Interfaces)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Rules)) {
		r := s.Rules[name]
		rule := nftablesRule(name, r)
		if r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY {
			t.Denies = append(t.Denies, rule)
		} else {
			t.Allows = append(t.Allows, rule)
		}
	}
	return t
}

// nftablesRule is a rule as the table writes it.
func nftablesRule(name string, r *resourcev1.FirewallRule) nftables.Rule {
	out := nftables.Rule{Name: "rule " + name, Protocol: protocolOf(r), Ports: []uint16{portOf(r)}}
	for _, s := range r.GetSources() {
		if p, err := netip.ParsePrefix(s); err == nil {
			out.Sources = append(out.Sources, p)
		}
	}
	return out
}

func protocolOf(r *resourcev1.FirewallRule) string {
	if r.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
		return "udp"
	}
	return "tcp"
}

func portOf(r *resourcev1.FirewallRule) uint16 {
	return uint16(r.GetPort()) //nolint:gosec // Validate keeps it within 1 to 65535
}

func isDeny(r *resourcev1.FirewallRule) bool {
	return r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY
}

// CheckRule refuses, when it's planned, a change to a rule that leaves the
// server unsafe to reach, as check judges the state it leaves: actual is the
// rule now, nil for a new one, and desired what it becomes, nil to delete it.
func (m *Manager) CheckRule(actual, desired *resourcev1.FirewallRule) error {
	return m.checkPlan(func(s *state) {
		if actual != nil {
			for _, name := range slices.Sorted(maps.Keys(s.Rules)) {
				if proto.Equal(s.Rules[name], actual) {
					delete(s.Rules, name)
					break
				}
			}
		}
		if desired != nil {
			s.Rules["\x00planned"] = desired
		}
	})
}

// CheckPolicy refuses, when it's planned, a policy that leaves the server
// unsafe to reach, as check judges the state it leaves; nil removes it.
func (m *Manager) CheckPolicy(desired *resourcev1.FirewallPolicy) error {
	return m.checkPlan(func(s *state) { s.Policy = desired })
}

// checkPlan judges what was confirmed, as one planned edit leaves it, as the
// server is now. The step checks again as it applies, over all the change's
// edits so far, as the server is then.
func (m *Manager) checkPlan(fn func(*state)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*readWait)
	defer cancel()
	h := m.readHost(ctx)
	if h.pinsErr != nil {
		return fmt.Errorf("can't tell what keeps the server reachable, so Dockwell won't narrow it: %w", h.pinsErr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.confirmed.clone()
	fn(&next)
	// A Docker that doesn't answer only leaves its containers' sessions in.
	nets, _ := m.networksLocked(ctx, h)
	return check(next, withPinned(h.pins, next.Pinned, nets.Subnets), m.PanelOverSSH)
}

// sshPortsOr22 are SSH's ports, or 22 where Dockwell found none.
func sshPortsOr22(ports []uint16) []uint16 {
	if len(ports) == 0 {
		return []uint16{22}
	}
	return ports
}

// allowsFromAnywhere reports a rule that lets a TCP port in from anywhere.
func allowsFromAnywhere(s state, port uint16) bool {
	for _, r := range s.Rules {
		if !isDeny(r) && protocolOf(r) == "tcp" && portOf(r) == port && len(r.GetSources()) == 0 {
			return true
		}
	}
	return false
}

// allows reports a rule that lets a TCP port in, from anywhere or from some.
func allows(s state, port uint16) bool {
	for _, r := range s.Rules {
		if !isDeny(r) && protocolOf(r) == "tcp" && portOf(r) == port {
			return true
		}
	}
	return false
}

// narrowsSSH reports a state that closes SSH to someone: a deny of one of its
// ports, or deny everything else with a port of SSH's that no rule lets in
// from anywhere. Only then does who is pinned decide who gets in.
func narrowsSSH(s state, ports []uint16) bool {
	ports = sshPortsOr22(ports)
	for _, r := range s.Rules {
		if isDeny(r) && protocolOf(r) == "tcp" && slices.Contains(ports, portOf(r)) {
			return true
		}
	}
	if !s.Policy.GetDenyEverythingElse() {
		return false
	}
	for _, port := range ports {
		if !allowsFromAnywhere(s, port) {
			return true
		}
	}
	return false
}

// check refuses a state the server can't take safely, as the pins say: a deny
// of a port the control plane listens on, which its pin would let in anyway;
// or, with nobody's address pinned to SSH, anything that narrows SSH: a deny of
// one of its ports, or deny everything else with no allow of each of its ports
// from anywhere. Where Dockwell finds no SSH at all, port 22 stands for it.
func check(s state, p Pins, panelOverSSH bool) error {
	names := slices.Sorted(maps.Keys(s.Rules))
	if panelOverSSH && s.Policy.GetDenyEverythingElse() {
		// The panel here has no domain, so it's reached through SSH: a pin holds
		// only while the address it names does, and losing SSH loses the panel too.
		for _, port := range sshPortsOr22(p.SSHPorts) {
			if !allows(s, port) {
				return fmt.Errorf("the panel on this server has no domain, so you reach it through SSH, and deny everything else would leave SSH open only from the addresses signed in now. If yours changes, you lose SSH and the panel at once. Add a rule that allows port %d first, from anywhere or from the ranges you'll come from, then plan this again", port)
			}
		}
	}
	for _, name := range names {
		r := s.Rules[name]
		if isDeny(r) && protocolOf(r) == "tcp" && slices.Contains(p.Own, portOf(r)) {
			return fmt.Errorf("this would deny port %d, which the control plane on this server listens on, for the panel or its agents. Leave that port out", portOf(r))
		}
	}
	if len(p.SSHFrom) > 0 {
		return nil
	}
	ports, refuse := p.SSHPorts, errNoSession
	if len(ports) == 0 {
		ports, refuse = []uint16{22}, errNoSSH
	}
	for _, name := range names {
		r := s.Rules[name]
		if isDeny(r) && protocolOf(r) == "tcp" && slices.Contains(ports, portOf(r)) {
			return refuse
		}
	}
	if s.Policy.GetDenyEverythingElse() {
		for _, port := range ports {
			open := slices.ContainsFunc(names, func(name string) bool {
				r := s.Rules[name]
				return !isDeny(r) && protocolOf(r) == "tcp" && portOf(r) == port && len(r.GetSources()) == 0
			})
			if !open {
				return refuse
			}
		}
	}
	return nil
}

var (
	errNoSession = errors.New("nobody is signed in to this server over SSH from outside it now, and nobody was when its firewall last changed, so Dockwell can't tell whose address to keep reaching SSH. Sign in over SSH from where you'll come in, or add a rule that allows SSH's port, then plan this again")
	errNoSSH     = errors.New("this server has no SSH server listening that Dockwell knows of, so Dockwell can't tell which port to keep reachable. If SSH runs here under another name, add a rule that allows its port first, then plan this again")
)

// saveConfirmed records a state as confirmed, and the table it loaded as, for
// the unit that loads it at boot. Both are written in full first, and
// confirmed.json is renamed into place last but one: if confirmed.nft won't
// follow, the old record goes back, so the two never disagree.
func (m *Manager) saveConfirmed(c confirmedState) error {
	script, err := c.table.Script()
	if err != nil {
		return fmt.Errorf("can't write the confirmed table for the server's start: %w", err)
	}
	data, err := json.MarshalIndent(encodeConfirmed(c), "", "  ")
	if err != nil {
		return err
	}
	jsonPath, nftPath := filepath.Join(m.Dir, "confirmed.json"), filepath.Join(m.Dir, "confirmed.nft")
	old, err := os.ReadFile(jsonPath) //nolint:gosec // the agent's own folder
	had := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("can't read what was confirmed before: %w", err)
	}
	if err := stage(nftPath+".new", []byte(script)); err != nil {
		return fmt.Errorf("can't record the firewall as confirmed: %w", err)
	}
	if err := stage(jsonPath+".new", data); err != nil {
		_ = os.Remove(nftPath + ".new")
		return fmt.Errorf("can't record the firewall as confirmed: %w", err)
	}
	if err := os.Rename(jsonPath+".new", jsonPath); err != nil {
		_ = os.Remove(jsonPath + ".new")
		_ = os.Remove(nftPath + ".new")
		return fmt.Errorf("can't record the firewall as confirmed: %w", err)
	}
	if err := os.Rename(nftPath+".new", nftPath); err != nil {
		_ = os.Remove(nftPath + ".new")
		var back error
		if had {
			back = writeFile(jsonPath, old)
		} else {
			back = os.Remove(jsonPath)
		}
		if back != nil {
			m.logger().Error("can't put back what was confirmed before on disk, so the agent loads the change at its next start", slog.String("error", back.Error()))
		}
		return fmt.Errorf("can't record the firewall's table as confirmed: %w", err)
	}
	return nil
}

// waitingFile is waiting.json: the change that waits, and which wait of it.
type waitingFile struct {
	Change string `json:"change"`
	Wait   string `json:"wait,omitempty"`
}

func (m *Manager) saveWaiting(change, wait string) error {
	if err := writeJSON(filepath.Join(m.Dir, "waiting.json"), waitingFile{Change: change, Wait: wait}); err != nil {
		return fmt.Errorf("can't record the change to the firewall before it loads, so it doesn't load: %w", err)
	}
	return nil
}

func (m *Manager) removeWaiting() error {
	if err := os.Remove(filepath.Join(m.Dir, "waiting.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// stored is what was confirmed as it's kept on disk: each spec as protojson,
// the pins it keeps, and the table it loaded as.
type stored struct {
	Wait   string                     `json:"wait,omitempty"`
	Rules  map[string]json.RawMessage `json:"rules,omitempty"`
	Policy json.RawMessage            `json:"policy,omitempty"`
	Pinned Pinned                     `json:"pinned,omitzero"`
	Table  *nftables.Table            `json:"table,omitempty"`
}

func encodeConfirmed(c confirmedState) stored {
	out := stored{Wait: c.wait, Rules: map[string]json.RawMessage{}, Pinned: c.Pinned, Table: &c.table}
	for name, r := range c.Rules {
		b, _ := protojson.Marshal(r)
		out.Rules[name] = b
	}
	if c.Policy != nil {
		out.Policy, _ = protojson.Marshal(c.Policy)
	}
	return out
}

func decodeConfirmed(b []byte, v any) error {
	var in stored
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	c := v.(*confirmedState)
	c.wait, c.Pinned = in.Wait, in.Pinned
	c.Rules = map[string]*resourcev1.FirewallRule{}
	for name, raw := range in.Rules {
		r := &resourcev1.FirewallRule{}
		if err := protojson.Unmarshal(raw, r); err != nil {
			return fmt.Errorf("rule %s: %w", name, err)
		}
		c.Rules[name] = r
	}
	if len(in.Policy) > 0 {
		c.Policy = &resourcev1.FirewallPolicy{}
		if err := protojson.Unmarshal(in.Policy, c.Policy); err != nil {
			return fmt.Errorf("the policy: %w", err)
		}
	}
	if in.Table != nil {
		c.table = *in.Table
	} else if !c.empty() {
		return errors.New("it has rules, and not the table they loaded as")
	}
	return nil
}

func readJSON(path string, v any, decode func([]byte, any) error) error {
	b, err := os.ReadFile(path) //nolint:gosec // the agent's own folder
	if errors.Is(err, os.ErrNotExist) {
		if c, ok := v.(*confirmedState); ok {
			*c = confirmedState{state: state{Rules: map[string]*resourcev1.FirewallRule{}}}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := decode(b, v); err != nil {
		return fmt.Errorf("can't read %s: %w. Move it aside, and the agent starts from nothing confirmed", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

// writeFile replaces a file whole, so a crash leaves the old one or the new.
func writeFile(path string, b []byte) error {
	if err := stage(path+".new", b); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

// stage writes a file in full, to disk.
func stage(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the agent's own folder
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
