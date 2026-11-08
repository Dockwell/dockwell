// SPDX-License-Identifier: AGPL-3.0-only

// Package access keeps who reaches the server, as ADR 0052 has it: the firewall
// rules and policy confirmed, and the one change to them that waits for its
// confirmation, on disk and in Dockwell's nftables table. A change loads at
// once, under a timer; the person who applied it confirms they can still reach
// the server, and the agent records it as confirmed on disk before it says it
// kept it. If nobody does in time, or the agent stops first, the agent puts
// back the table as it was last confirmed, on its own.
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
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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
	// Docker lists Docker's networks' interfaces. Nil lists none.
	Docker func(ctx context.Context) ([]string, error)
	Logger *slog.Logger
	// Timer replaces Timer, for a test.
	Timer time.Duration
	now   func() time.Time

	mu        sync.Mutex
	confirmed state
	waiting   *pending
	verdicts  []verdict
	probed    bool
	unusable  error
}

var (
	_ firewall.Backend       = (*Manager)(nil)
	_ firewall.PolicyBackend = (*Manager)(nil)
	_ firewall.Checker       = (*Manager)(nil)
	_ firewall.Waiter        = (*Manager)(nil)
	_ resource.SharedReadier = (*Manager)(nil)
)

// state is the rules and policy, by name, as the table holds them.
type state struct {
	Rules  map[string]*resourcev1.FirewallRule
	Policy *resourcev1.FirewallPolicy
}

func (s state) clone() state {
	out := state{Rules: make(map[string]*resourcev1.FirewallRule, len(s.Rules))}
	for name, r := range s.Rules {
		out.Rules[name] = proto.CloneOf(r)
	}
	if s.Policy != nil {
		out.Policy = proto.CloneOf(s.Policy)
	}
	return out
}

func (s state) empty() bool { return len(s.Rules) == 0 && !s.Policy.GetDenyEverythingElse() }

// pending is the change that waits for its confirmation.
type pending struct {
	change   string
	state    state
	deadline time.Time
	timer    *time.Timer
	// done closes once the change has its verdict.
	done    chan struct{}
	verdict resource.AccessVerdict
}

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

// Start reads what was confirmed, and reverts at once a change that waited for
// its confirmation when the agent stopped, so a restart or a reboot inside its
// timer keeps only what was confirmed. It loads the confirmed table, which a
// flush may have taken, before the agent connects anywhere.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(ctx)
}

func (m *Manager) startLocked(ctx context.Context) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return fmt.Errorf("can't make the folder for the firewall's state: %w", err)
	}
	if err := readJSON(filepath.Join(m.Dir, "confirmed.json"), &m.confirmed, decodeState); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(m.Dir, "verdicts.json"), &m.verdicts, json.Unmarshal); err != nil {
		return err
	}
	var w struct {
		Change string `json:"change"`
	}
	if err := readJSON(filepath.Join(m.Dir, "waiting.json"), &w, json.Unmarshal); err != nil {
		return err
	}
	if w.Change != "" {
		m.logger().WarnContext(ctx, "a change to the firewall waited for its confirmation when the agent stopped, so it puts back what was confirmed",
			slog.String("change", w.Change))
		m.record(w.Change, resource.AccessRolledBack)
		if err := os.Remove(filepath.Join(m.Dir, "waiting.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return m.loadConfirmed(ctx)
	}
	if m.confirmed.empty() {
		return nil
	}
	return m.loadConfirmed(ctx)
}

// ReadyKey shares one probe between the rule's and the policy's drivers.
func (m *Manager) ReadyKey() any { return m }

// Ready says whether the server can hold Dockwell's table: it has nft, and its
// kernel takes a table. The first time it can, the agent reads what was
// confirmed, puts back a change that waited when it stopped, and loads the
// confirmed table, before it offers the firewall's kinds.
func (m *Manager) Ready(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.probed {
		return nil
	}
	if m.unusable = m.Table.Probe(ctx); m.unusable != nil {
		return m.unusable
	}
	if err := m.startLocked(ctx); err != nil {
		m.unusable = fmt.Errorf("can't put the firewall back as it was confirmed: %w", err)
		return m.unusable
	}
	m.probed = true
	return nil
}

// view is the state a call sees: the waiting change's, for a step of that
// change, and what was confirmed otherwise, as for planning, so that whether
// a step changes anything is judged against what was confirmed.
func (m *Manager) view(ctx context.Context) state {
	if d := resource.DeployOf(ctx); m.waiting != nil && d.Change == m.waiting.change {
		return m.waiting.state
	}
	return m.confirmed
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
// timer, as it needs no confirmation of its own. Any other step loads under the
// change's timer, which starts with its first step; its last step on the
// server waits for the verdict.
func (m *Manager) edit(ctx context.Context, fn func(*state)) error {
	d := resource.DeployOf(ctx)
	if d.Change == "" {
		return errors.New("a change to the firewall comes through a change, which names it")
	}
	m.mu.Lock()
	if d.Rollback {
		defer m.mu.Unlock()
		next := m.confirmed.clone()
		fn(&next)
		if err := m.load(ctx, next); err != nil {
			return err
		}
		m.confirmed = next
		return m.saveConfirmed(next)
	}
	if m.waiting != nil && m.waiting.change != d.Change {
		other := m.waiting.change
		m.mu.Unlock()
		return fmt.Errorf("%w (%s). Wait for it to be confirmed or rolled back, then try again", ErrBusy, other)
	}
	first := m.waiting == nil
	var next state
	if first {
		next = m.confirmed.clone()
	} else {
		next = m.waiting.state.clone()
	}
	fn(&next)
	if err := m.load(ctx, next); err != nil {
		// A load nft refused changed nothing, but one that can't be seen through
		// may be live, and a change that had loaded steps before has them live:
		// either way, what was confirmed goes back.
		if !first || errors.Is(err, nftables.ErrUnknown) {
			m.rollBackLocked(ctx, d.Change)
		}
		m.mu.Unlock()
		return err
	}
	if first {
		m.waiting = &pending{change: d.Change, done: make(chan struct{})}
		m.armLocked(d.Change)
	}
	m.waiting.state = next
	if err := m.saveWaiting(); err != nil {
		m.rollBackLocked(ctx, d.Change)
		m.mu.Unlock()
		return err
	}
	if !d.LastAccess {
		m.mu.Unlock()
		return nil
	}
	// The change's last step here: the person who applied it has the timer's
	// whole length from now to check and confirm.
	m.armLocked(d.Change)
	done, wait := m.waiting.done, m.announceLocked(ctx)
	m.mu.Unlock()
	return m.await(ctx, d.Change, done, wait)
}

// armLocked starts, or starts again, the waiting change's timer.
func (m *Manager) armLocked(change string) {
	m.waiting.deadline = m.clock().Add(m.timer())
	if m.waiting.timer != nil {
		m.waiting.timer.Stop()
	}
	m.waiting.timer = time.AfterFunc(m.timer(), func() { m.expire(change) })
}

// announceLocked tells whoever applies the step what it waits for, and returns
// how long to wait.
func (m *Manager) announceLocked(ctx context.Context) time.Duration {
	w := resource.AccessWait{Deadline: m.waiting.deadline}
	if pins, err := m.pins(ctx); err == nil {
		for _, p := range pins.SSHPorts {
			w.SSHPorts = append(w.SSHPorts, uint32(p))
		}
	}
	resource.Report(ctx, resource.Outcome{Waits: &w})
	return m.waiting.deadline.Sub(m.clock()) + waitGrace
}

// await waits for a change's verdict.
func (m *Manager) await(ctx context.Context, change string, done <-chan struct{}, wait time.Duration) error {
	select {
	case <-done:
	case <-time.After(wait):
		return fmt.Errorf("no verdict on %s came within %s", change, wait.Round(time.Second))
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
		fmt.Sprintf("Nobody confirmed the change within %s, so the server put back its firewall as it was.", m.timer().Round(time.Second)),
		fmt.Errorf("%s wasn't confirmed in time, so the server put back its firewall as it was", change))
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
// kept it.
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
	if err := m.saveConfirmed(w.state); err != nil {
		m.logger().ErrorContext(ctx, "can't record a confirmed change to the firewall, so the agent puts back what was confirmed before",
			slog.String("change", change), slog.String("error", err.Error()))
		m.rollBackLocked(ctx, change)
		return resource.AccessRolledBack
	}
	m.confirmed = w.state
	w.timer.Stop()
	_ = os.Remove(filepath.Join(m.Dir, "waiting.json"))
	m.waiting = nil
	m.record(change, resource.AccessKept)
	w.verdict = resource.AccessKept
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
// change with its verdict.
func (m *Manager) rollBackLocked(ctx context.Context, change string) {
	if err := m.loadConfirmed(context.WithoutCancel(ctx)); err != nil {
		m.logger().ErrorContext(ctx, "can't put back the firewall as it was confirmed, so the agent removes its table, which leaves what the server's own firewall does",
			slog.String("change", change), slog.String("error", err.Error()))
		if err := m.Table.Delete(context.WithoutCancel(ctx)); err != nil {
			m.logger().ErrorContext(ctx, "can't remove the firewall's table either", slog.String("error", err.Error()))
		}
	}
	m.record(change, resource.AccessRolledBack)
	if w := m.waiting; w != nil && w.change == change {
		if w.timer != nil {
			w.timer.Stop()
		}
		w.verdict = resource.AccessRolledBack
		close(w.done)
		m.waiting = nil
	}
	_ = os.Remove(filepath.Join(m.Dir, "waiting.json"))
}

func (m *Manager) record(change string, v resource.AccessVerdict) {
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

// loadConfirmed loads the table as last confirmed, or removes Dockwell's table
// when nothing is confirmed.
func (m *Manager) loadConfirmed(ctx context.Context) error {
	if m.confirmed.empty() {
		return m.Table.Delete(ctx)
	}
	return m.load(ctx, m.confirmed)
}

// load renders a state with what keeps the server reachable, and loads it.
func (m *Manager) load(ctx context.Context, s state) error {
	t, err := m.table(ctx, s)
	if err != nil {
		return err
	}
	loaded, err := m.Table.Load(ctx, t)
	for _, name := range loaded.Skipped {
		m.logger().WarnContext(ctx, "the firewall leaves out one of Docker's interfaces", slog.String("interface", name), slog.String("why", nftables.SkippedWords(name)))
	}
	return err
}

func (m *Manager) table(ctx context.Context, s state) (nftables.Table, error) {
	var t nftables.Table
	pins, err := m.pins(ctx)
	if err != nil {
		return t, fmt.Errorf("can't tell what keeps the server reachable, so the firewall stays as it is: %w", err)
	}
	t.Pins = pins.Rules()
	for _, name := range slices.Sorted(maps.Keys(s.Rules)) {
		r := s.Rules[name]
		rule := nftablesRule(name, r)
		if r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY {
			t.Denies = append(t.Denies, rule)
		} else {
			t.Allows = append(t.Allows, rule)
		}
	}
	t.DenyRest = s.Policy.GetDenyEverythingElse()
	if m.Docker != nil {
		if t.DockerInterfaces, err = m.Docker(ctx); err != nil {
			return t, fmt.Errorf("can't read Docker's networks, which the firewall leaves to their containers, so it stays as it is: %w", err)
		}
	}
	return t, nil
}

func (m *Manager) pins(ctx context.Context) (Pins, error) {
	if m.Pins == nil {
		return Pins{}, nil
	}
	return m.Pins(ctx)
}

// nftablesRule is a rule as the table writes it.
func nftablesRule(name string, r *resourcev1.FirewallRule) nftables.Rule {
	protocol := "tcp"
	if r.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
		protocol = "udp"
	}
	out := nftables.Rule{Name: "rule " + name, Protocol: protocol, Ports: []uint16{uint16(r.GetPort())}} //nolint:gosec // Validate keeps it within 1 to 65535
	for _, s := range r.GetSources() {
		if p, err := netip.ParsePrefix(s); err == nil {
			out.Sources = append(out.Sources, p)
		}
	}
	return out
}

// CheckRule refuses, when it's planned, a rule the server can't take safely: a
// deny of a port the control plane listens on, or one that narrows SSH while no
// SSH session from outside says whose address to keep.
func (m *Manager) CheckRule(r *resourcev1.FirewallRule) error {
	if r.GetAction() != resourcev1.FirewallAction_FIREWALL_ACTION_DENY {
		return nil
	}
	pins, err := m.checkPins()
	if err != nil {
		return err
	}
	port := uint16(r.GetPort()) //nolint:gosec // Validate keeps it within 1 to 65535
	tcp := r.GetProtocol() != resourcev1.Protocol_PROTOCOL_UDP
	if tcp && slices.Contains(pins.Own, port) {
		return fmt.Errorf("this rule would deny port %d, which the control plane on this server listens on, for the panel or its agents. Leave that port out", port)
	}
	if tcp && slices.Contains(pins.SSHPorts, port) && len(pins.SSHFrom) == 0 {
		return errNoSession
	}
	return nil
}

// CheckPolicy refuses turning on deny everything else while no SSH session
// from outside says whose address to keep reaching SSH.
func (m *Manager) CheckPolicy(p *resourcev1.FirewallPolicy) error {
	if !p.GetDenyEverythingElse() {
		return nil
	}
	pins, err := m.checkPins()
	if err != nil {
		return err
	}
	if len(pins.SSHPorts) > 0 && len(pins.SSHFrom) == 0 {
		return errNoSession
	}
	return nil
}

var errNoSession = errors.New("no SSH session is open to this server from outside it now, so Dockwell can't tell whose address to keep reaching SSH. Open an SSH session from where you'll come in, then plan this again")

func (m *Manager) checkPins() (Pins, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pins, err := m.pins(ctx)
	if err != nil {
		return Pins{}, fmt.Errorf("can't tell what keeps the server reachable, so Dockwell won't narrow it: %w", err)
	}
	return pins, nil
}

// saveConfirmed records a state as confirmed, and the table it makes, for the
// unit that loads it at boot.
func (m *Manager) saveConfirmed(s state) error {
	if err := writeJSON(filepath.Join(m.Dir, "confirmed.json"), encodeState(s)); err != nil {
		return fmt.Errorf("can't record the firewall as confirmed: %w", err)
	}
	t, err := m.table(context.Background(), s)
	if err != nil {
		return err
	}
	script, err := t.Script()
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(m.Dir, "confirmed.nft"), []byte(script))
}

func (m *Manager) saveWaiting() error {
	return writeJSON(filepath.Join(m.Dir, "waiting.json"), map[string]string{"change": m.waiting.change})
}

// stored is a state as it's kept on disk: each spec as protojson.
type stored struct {
	Rules  map[string]json.RawMessage `json:"rules,omitempty"`
	Policy json.RawMessage            `json:"policy,omitempty"`
}

func encodeState(s state) stored {
	out := stored{Rules: map[string]json.RawMessage{}}
	for name, r := range s.Rules {
		b, _ := protojson.Marshal(r)
		out.Rules[name] = b
	}
	if s.Policy != nil {
		out.Policy, _ = protojson.Marshal(s.Policy)
	}
	return out
}

func decodeState(b []byte, v any) error {
	var in stored
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	s := v.(*state)
	s.Rules = map[string]*resourcev1.FirewallRule{}
	for name, raw := range in.Rules {
		r := &resourcev1.FirewallRule{}
		if err := protojson.Unmarshal(raw, r); err != nil {
			return fmt.Errorf("rule %s: %w", name, err)
		}
		s.Rules[name] = r
	}
	if len(in.Policy) > 0 {
		s.Policy = &resourcev1.FirewallPolicy{}
		if err := protojson.Unmarshal(in.Policy, s.Policy); err != nil {
			return fmt.Errorf("the policy: %w", err)
		}
	}
	return nil
}

func readJSON(path string, v any, decode func([]byte, any) error) error {
	b, err := os.ReadFile(path) //nolint:gosec // the agent's own folder
	if errors.Is(err, os.ErrNotExist) {
		if s, ok := v.(*state); ok {
			*s = state{Rules: map[string]*resourcev1.FirewallRule{}}
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
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the agent's own folder
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
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
