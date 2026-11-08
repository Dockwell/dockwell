// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/resource"
)

// table is Dockwell's table in memory, which records each load and can be
// told to fail the next.
type table struct {
	mu      sync.Mutex
	loaded  *nftables.Table
	deleted int
	fail    error
}

func (t *table) Load(_ context.Context, nt nftables.Table) (nftables.Loaded, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.fail; err != nil {
		t.fail = nil
		if errors.Is(err, nftables.ErrUnknown) {
			t.loaded = &nt
		}
		return nftables.Loaded{}, err
	}
	t.loaded = &nt
	return nftables.Loaded{Present: true}, nil
}

func (t *table) Delete(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loaded, t.deleted = nil, t.deleted+1
	return nil
}

func (*table) Probe(context.Context) error { return nil }

func (t *table) rules() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded == nil {
		return "(no table)"
	}
	var names []string
	for _, r := range append(append([]nftables.Rule{}, t.loaded.Allows...), t.loaded.Denies...) {
		names = append(names, r.Name)
	}
	if t.loaded.DenyRest {
		names = append(names, "deny the rest")
	}
	return strings.Join(names, ", ")
}

func manager(t *testing.T, dir string, timer time.Duration) (*Manager, *table) {
	t.Helper()
	tbl := &table{}
	m := &Manager{Dir: dir, Table: tbl, Timer: timer, Pins: func(context.Context) (Pins, error) {
		return Pins{SSHPorts: []uint16{22}, SSHFrom: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")}}, nil
	}}
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	return m, tbl
}

func step(change string, last bool) context.Context {
	return resource.WithDeploy(context.Background(), resource.Deploy{Change: change, LastAccess: last})
}

var http = &resourcev1.FirewallRule{Port: 80}

// The last step of a change waits for its confirmation, says until when and on
// which SSH port to test, and ends once it's kept; the agent recorded it on disk
// first, so a restart keeps it.
func TestAChangeWaitsForItsConfirmation(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	var told []resource.Outcome
	ctx := resource.WithOutcome(step("chg_1", true), func(o resource.Outcome) { told = append(told, o) })
	errc := make(chan error, 1)
	go func() { errc <- m.Put(ctx, "http", http) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		waiting := m.waiting != nil
		m.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the change never waited")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("while it waits the table holds %q", got)
	}
	if v := m.Confirm(t.Context(), "chg_1"); v != resource.AccessKept {
		t.Fatalf("confirmed: %v", v)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the step ends %v", err)
	}
	if len(told) != 1 || told[0].Waits == nil || told[0].Waits.Deadline.Before(time.Now().Add(30*time.Second)) || len(told[0].Waits.SSHPorts) != 1 {
		t.Errorf("the step told %+v", told)
	}
	// What was kept is on disk, so a restart holds it, and planning sees it.
	again, tbl2 := manager(t, dir, time.Minute)
	if got := tbl2.rules(); got != "rule http" {
		t.Errorf("after a restart the table holds %q", got)
	}
	if r, _ := again.Rule(context.Background(), "http"); r.GetPort() != 80 {
		t.Errorf("after a restart the rule reads %v", r)
	}
	if v := again.Confirm(t.Context(), "chg_1"); v != resource.AccessKept {
		t.Errorf("a confirmation after a restart hears %v, want kept", v)
	}
}

// Nobody confirms in time: the agent puts back what was confirmed on its own,
// the step fails, and a confirmation after hears it rolled back.
func TestNobodyConfirmsAndItGoesBack(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), 200*time.Millisecond)
	err := m.Put(step("chg_1", true), "http", http)
	if plain, _ := resource.Explain(err); err == nil || !strings.Contains(plain, "Nobody confirmed the change") {
		t.Fatalf("an unconfirmed change's step ends %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it went back the table holds %q", got)
	}
	if v := m.Confirm(t.Context(), "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a late confirmation hears %v", v)
	}
}

// A restart inside the timer reverts the change at once: what loads is what was
// confirmed, and its verdict is rolled back.
func TestARestartRevertsAChangeThatWaited(t *testing.T) {
	dir := t.TempDir()
	m, _ := manager(t, dir, time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "waiting.json")); err != nil {
		t.Fatalf("the waiting change isn't on disk: %v", err)
	}
	m.mu.Lock()
	m.waiting.timer.Stop() // the agent stops here
	m.mu.Unlock()
	again, tbl := manager(t, dir, time.Minute)
	if got := tbl.rules(); got != "(no table)" || tbl.deleted != 1 {
		t.Errorf("after a restart the table holds %q", got)
	}
	if v := again.Confirm(t.Context(), "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation after the restart hears %v", v)
	}
}

// Steps of one change before its last don't wait; a second change while one
// waits is refused; planning sees what was confirmed, and a step of the waiting
// change sees it as it waits.
func TestOneChangeWaitsAndPlanningSeesWhatWasConfirmed(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(step("chg_2", false), "ssh", &resourcev1.FirewallRule{Port: 22}); !errors.Is(err, ErrBusy) {
		t.Errorf("a second change while one waits: %v", err)
	}
	if r, _ := m.Rule(context.Background(), "http"); r != nil {
		t.Errorf("planning sees %v, want what was confirmed", r)
	}
	if r, _ := m.Rule(step("chg_1", false), "http"); r.GetPort() != 80 {
		t.Errorf("the waiting change's step sees %v", r)
	}
	if err := m.PutPolicy(step("chg_1", false), &resourcev1.FirewallPolicy{DenyEverythingElse: true}); err != nil {
		t.Fatal(err)
	}
	if got := tbl.rules(); got != "rule http, deny the rest" {
		t.Errorf("the table holds %q", got)
	}
	m.Confirm(t.Context(), "chg_1")
}

// A resumed step of a change that still waits waits for its verdict.
func TestAResumedStepWaitsForTheVerdict(t *testing.T) {
	m, _ := manager(t, t.TempDir(), time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- m.Wait(step("chg_1", true)) }()
	select {
	case err := <-errc:
		t.Fatalf("a resumed step ended at once: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	m.Confirm(t.Context(), "chg_1")
	if err := <-errc; err != nil {
		t.Errorf("a resumed step ends %v", err)
	}
}

// A load that can't be seen through may be live, so what was confirmed goes
// back; one nft refused changed nothing.
func TestALoadThatMayBeLiveGoesBack(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	tbl.fail = nftables.ErrUnknown
	if err := m.Put(step("chg_1", false), "http", http); !errors.Is(err, nftables.ErrUnknown) {
		t.Fatalf("a load that can't be seen through: %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after a load that may be live the table holds %q", got)
	}
	if v := m.Confirm(t.Context(), "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("its verdict is %v", v)
	}
	// The next change starts afresh.
	if err := m.Put(step("chg_2", false), "http", http); err != nil {
		t.Errorf("a change after one went back: %v", err)
	}
	m.Confirm(t.Context(), "chg_2")
}

// A rollback puts back what was confirmed with no timer, and it's confirmed.
func TestARollbackNeedsNoConfirmation(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	ctx := resource.WithDeploy(context.Background(), resource.Deploy{Change: "chg_r", Rollback: true, LastAccess: true})
	if err := m.Put(ctx, "http", http); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	waiting := m.waiting
	m.mu.Unlock()
	if waiting != nil || tbl.rules() != "rule http" {
		t.Errorf("a rollback waits %v, table %q", waiting, tbl.rules())
	}
	if r, _ := m.Rule(context.Background(), "http"); r.GetPort() != 80 {
		t.Errorf("after a rollback planning sees %v", r)
	}
}

// Narrowing SSH while no session says whose address to keep, or denying the
// control plane's own port, is refused when it's planned.
func TestChecksRefuseWhatWouldCutAccess(t *testing.T) {
	m, _ := manager(t, t.TempDir(), time.Minute)
	m.Pins = func(context.Context) (Pins, error) { return Pins{SSHPorts: []uint16{22}, Own: []uint16{7600}}, nil }
	deny := func(port uint32) *resourcev1.FirewallRule {
		return &resourcev1.FirewallRule{Port: port, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	}
	if err := m.CheckRule(deny(22)); err == nil || !strings.Contains(err.Error(), "no SSH session") {
		t.Errorf("denying SSH with no session: %v", err)
	}
	if err := m.CheckRule(deny(7600)); err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Errorf("denying the control plane's port: %v", err)
	}
	if err := m.CheckRule(deny(5432)); err != nil {
		t.Errorf("denying another port: %v", err)
	}
	if err := m.CheckPolicy(&resourcev1.FirewallPolicy{DenyEverythingElse: true}); err == nil {
		t.Error("deny everything else with no session went through")
	}
	m.Pins = func(context.Context) (Pins, error) {
		return Pins{SSHPorts: []uint16{22}, SSHFrom: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")}}, nil
	}
	if err := m.CheckRule(deny(22)); err != nil {
		t.Errorf("denying SSH with a session to pin: %v", err)
	}
}

// The pins come from the kernel's socket table: the ports sshd's processes
// listen on and hold sessions on, and their sessions' peers from outside, and
// ssh.socket's ports where systemd holds SSH's.
func TestPinsComeFromTheSocketTable(t *testing.T) {
	socket := func(inode string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("socket:[" + inode + "]"), Mode: os.ModeSymlink}
	}
	proc := fstest.MapFS{
		"100/comm":  {Data: []byte("sshd\n")},
		"100/fd/3":  socket("1001"),
		"200/comm":  {Data: []byte("sshd-session\n")},
		"200/fd/4":  socket("1002"),
		"200/fd/5":  socket("1003"),
		"300/comm":  {Data: []byte("nginx\n")},
		"300/fd/6":  socket("1004"),
		"self/comm": {Data: []byte("dockwell\n")},
		// sshd on 22; a session from 203.0.113.7 to 10.0.0.5; one over loopback,
		// as ssh -L's; and nginx on 80.
		"net/tcp": {Data: []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001
   1: 0500000A:0016 077100CB:D431 01 00000000:00000000 00:00000000 00000000     0        0 1002
   2: 0100007F:0016 0100007F:D432 01 00000000:00000000 00:00000000 00000000     0        0 1003
   3: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1004
`)},
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr", "bin", "systemctl"), []byte("#!/bin/sh\necho '0.0.0.0:2222 (Stream)'\necho '[::]:2222 (Stream)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &PinReader{Proc: proc, Tools: &hosttool.Runner{Root: root}, Own: []uint16{7600}}
	pins, err := r.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pins.SSHPorts); got != "[22 2222]" {
		t.Errorf("SSH's ports %v, want 22 and ssh.socket's 2222, not nginx's 80", got)
	}
	if fmt.Sprint(pins.SSHFrom) != "[203.0.113.7/32]" {
		t.Errorf("SSH from %v, want the session from outside alone", pins.SSHFrom)
	}
	rules := pins.Rules()
	if len(rules) != 2 || rules[0].Name != "pin ssh" || rules[1].Name != "pin control plane" {
		t.Errorf("pins as rules %+v", rules)
	}
}

// A confirmation the agent can't record on disk isn't kept: it says so only
// once it's recorded, and otherwise puts back what was confirmed before.
func TestAConfirmationNotOnDiskIsntKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a folder whatever its mode")
	}
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if v := m.Confirm(t.Context(), "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation the disk refused: %v, want rolled back", v)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it the table holds %q", got)
	}
}

// A confirmation that comes once the deadline has passed, before the timer has
// put the firewall back, loses: running out wins, and the firewall goes back.
func TestAConfirmationPastItsDeadlineLoses(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if v := m.Confirm(t.Context(), "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation past its deadline: %v, want rolled back", v)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it the table holds %q", got)
	}
}
