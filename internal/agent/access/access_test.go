// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

// table is Dockwell's table as the kernel holds it, in memory: a restart of the
// agent keeps it, as the kernel does. Loads and deletes can be told to fail.
type table struct {
	mu      sync.Mutex
	loaded  *nftables.Table
	loads   int
	deleted int
	// failLoads fail the next loads, in turn; ErrUnknown loads first.
	failLoads   []error
	failDeletes int
}

func (t *table) Load(_ context.Context, nt nftables.Table) (nftables.Loaded, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loads++
	if len(t.failLoads) > 0 {
		err := t.failLoads[0]
		t.failLoads = t.failLoads[1:]
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
	if t.failDeletes > 0 {
		t.failDeletes--
		return errors.New("nft: can't remove the table")
	}
	t.loaded, t.deleted = nil, t.deleted+1
	return nil
}

func (*table) Probe(context.Context) error { return nil }

func (t *table) failNext(errs ...error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failLoads = append(t.failLoads, errs...)
}

// rules are the table's rules by name, and its policy, or "(no table)".
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

// pins are the table's pins, with their ports and sources.
func (t *table) pins() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded == nil {
		return "(no table)"
	}
	var out []string
	for _, r := range t.loaded.Pins {
		out = append(out, fmt.Sprintf("%s %v from %v", r.Name, r.Ports, r.Sources))
	}
	return strings.Join(out, "; ")
}

func (t *table) current() nftables.Table {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded == nil {
		return nftables.Table{}
	}
	return *t.loaded
}

// sessions are the SSH sessions open now, as the pins say them.
type sessions struct {
	mu   sync.Mutex
	from []netip.Prefix
	own  []uint16
	err  error
}

func (s *sessions) set(from ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from = nil
	for _, f := range from {
		s.from = append(s.from, netip.MustParsePrefix(f))
	}
}

func (s *sessions) read(context.Context) (Pins, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Pins{SSHPorts: []uint16{22}, SSHFrom: append([]netip.Prefix(nil), s.from...), Own: s.own}, s.err
}

// timers holds the timers a manager starts, for a test to run out by hand.
type timers struct {
	mu  sync.Mutex
	fns []func()
}

type stopped struct{}

func (stopped) Stop() bool { return true }

func (t *timers) after(_ time.Duration, f func()) stopper {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fns = append(t.fns, f)
	return stopped{}
}

// runOut runs out the timer started last, as the one a waiting change has.
func (t *timers) runOut() {
	t.mu.Lock()
	f := t.fns[len(t.fns)-1]
	t.mu.Unlock()
	f()
}

const office = "203.0.113.7/32"

// start starts a manager on dir over tbl, with SSH on 22 and the sessions s says.
func start(t *testing.T, dir string, tbl *table, s *sessions, timer time.Duration, opts ...func(*Manager)) *Manager {
	t.Helper()
	m := &Manager{Dir: dir, Table: tbl, Timer: timer, Pins: s.read}
	for _, o := range opts {
		o(m)
	}
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	return m
}

// manager starts a manager with a session open from the office.
func manager(t *testing.T, dir string, timer time.Duration) (*Manager, *table) {
	t.Helper()
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	return start(t, dir, tbl, s, timer), tbl
}

func step(change string, last bool) context.Context {
	return resource.WithDeploy(context.Background(), resource.Deploy{Change: change, LastAccess: last})
}

// announced marks the waiting change's last step as having said it waits, for
// a test that confirms a change without running that step.
func announced(m *Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting != nil {
		m.waiting.announced = true
	}
}

// confirm confirms a change as the person who applied it would, once its last
// step on the server has said it waits: a test that applies only earlier steps
// says so for it.
func confirm(m *Manager, change string) resource.AccessVerdict {
	announced(m)
	return m.Confirm(context.Background(), change)
}

func rollback(change string) context.Context {
	return resource.WithDeploy(context.Background(), resource.Deploy{Change: change, Rollback: true, LastAccess: true})
}

var (
	http   = &resourcev1.FirewallRule{Port: 80}
	https  = &resourcev1.FirewallRule{Port: 443}
	denyDB = &resourcev1.FirewallRule{Port: 5432, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	policy = &resourcev1.FirewallPolicy{DenyEverythingElse: true}
)

// waitFor waits until a change waits on m.
func waitFor(t *testing.T, m *Manager, change string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		waiting := m.waiting != nil && m.waiting.change == change
		m.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited", change)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// keep runs a change's last step and confirms it, and returns how the step ended.
func keep(t *testing.T, m *Manager, change string, put func(ctx context.Context) error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- put(step(change, true)) }()
	waitFor(t, m, change)
	if v := confirm(m, change); v != resource.AccessKept {
		t.Fatalf("confirming %s: %v", change, v)
	}
	return <-errc
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The last step of a change waits for its confirmation, says until when, how
// long that is, and on which SSH port to test, and ends once it's kept; the
// agent recorded it on disk first, so a restart keeps it.
func TestAChangeWaitsForItsConfirmation(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	var told []resource.Outcome
	ctx := resource.WithOutcome(step("chg_1", true), func(o resource.Outcome) { told = append(told, o) })
	errc := make(chan error, 1)
	go func() { errc <- m.Put(ctx, "http", http) }()
	waitFor(t, m, "chg_1")
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("while it waits the table holds %q", got)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessKept {
		t.Fatalf("confirmed: %v", v)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the step ends %v", err)
	}
	if len(told) != 1 || told[0].Waits == nil || told[0].Waits.Deadline.Before(time.Now().Add(30*time.Second)) ||
		told[0].Waits.Left < 30*time.Second || told[0].Waits.Left > time.Minute || fmt.Sprint(told[0].Waits.SSHPorts) != "[22]" {
		t.Errorf("the step told %+v", told[0].Waits)
	}
	// What was kept is on disk, so a restart holds it, and planning sees it.
	again, tbl2 := manager(t, dir, time.Minute)
	if got := tbl2.rules(); got != "rule http" {
		t.Errorf("after a restart the table holds %q", got)
	}
	if r, _ := again.Rule(context.Background(), "http"); r.GetPort() != 80 {
		t.Errorf("after a restart the rule reads %v", r)
	}
	if v := confirm(again, "chg_1"); v != resource.AccessKept {
		t.Errorf("a confirmation after a restart hears %v, want kept", v)
	}
}

// Nobody confirms in time: the agent puts back what was confirmed on its own,
// the step fails, saying how long there was, and a confirmation after hears it
// rolled back.
func TestNobodyConfirmsAndItGoesBack(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), 200*time.Millisecond)
	err := m.Put(step("chg_1", true), "http", http)
	if plain, _ := resource.Explain(err); err == nil || !strings.Contains(plain, "Nobody confirmed the change within 200 ms") {
		t.Fatalf("an unconfirmed change's step ends %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it went back the table holds %q", got)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a late confirmation hears %v", v)
	}
	if got := wording(time.Minute); got != "60 s" {
		t.Errorf("a minute reads %q", got)
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
	if !exists(filepath.Join(dir, "waiting.json")) {
		t.Fatal("the waiting change isn't on disk")
	}
	m.mu.Lock()
	m.waiting.timer.Stop() // the agent stops here
	m.mu.Unlock()
	again, tbl := manager(t, dir, time.Minute)
	if got := tbl.rules(); got != "(no table)" || tbl.deleted != 1 {
		t.Errorf("after a restart the table holds %q", got)
	}
	if v := confirm(again, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation after the restart hears %v", v)
	}
	if exists(filepath.Join(dir, "waiting.json")) {
		t.Error("the change that waited is still on disk once it went back")
	}
}

// The change is on disk before its table loads, so an agent that stops as nft
// runs, on a first change, puts back what was confirmed as it starts, which is
// nothing: it removes the table. A start with nothing confirmed and nothing
// waiting removes any table too, as one only the kernel remembers wasn't
// confirmed.
func TestTheChangeIsOnDiskBeforeItLoads(t *testing.T) {
	dir := t.TempDir()
	s := &sessions{}
	s.set(office)
	disk := &checksDisk{table: &table{}, path: filepath.Join(dir, "waiting.json")}
	m := start(t, dir, disk.table, s, time.Minute, func(m *Manager) { m.Table = disk })
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	if !disk.saw {
		t.Error("the change wasn't on disk as its table loaded")
	}
	confirm(m, "chg_1")

	disk.failNext(errors.New("nft: syntax error"))
	if err := m.Put(step("chg_2", false), "https", https); err == nil {
		t.Fatal("a load nft refused passed")
	}
	if exists(disk.path) {
		t.Error("a change nft refused is on disk as waiting")
	}

	kernel := &table{loaded: &nftables.Table{DenyRest: true}}
	start(t, t.TempDir(), kernel, &sessions{}, time.Minute)
	if got := kernel.rules(); got != "(no table)" {
		t.Errorf("a start with nothing confirmed leaves %q loaded", got)
	}
}

// checksDisk is a table that notes, as a change loads, whether it's on disk.
type checksDisk struct {
	*table
	path string
	saw  bool
}

func (c *checksDisk) Load(ctx context.Context, nt nftables.Table) (nftables.Loaded, error) {
	c.saw = exists(c.path)
	return c.table.Load(ctx, nt)
}

// A start whose revert fails keeps the change on disk, so the next check puts
// it back: the table never stays as the change left it while the verdict says
// rolled back.
func TestARevertThatFailsAtStartIsTriedAgain(t *testing.T) {
	dir := t.TempDir()
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	m := start(t, dir, tbl, s, time.Minute)
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.waiting.timer.Stop() // the agent stops here
	m.mu.Unlock()
	tbl.mu.Lock()
	tbl.failDeletes = 1
	tbl.mu.Unlock()
	again := &Manager{Dir: dir, Table: tbl, Timer: time.Minute, Pins: s.read}
	if err := again.Ready(t.Context()); err == nil {
		t.Error("a start that couldn't put the firewall back says it's ready")
	}
	if !exists(filepath.Join(dir, "waiting.json")) {
		t.Error("the change that waited is gone from disk before it went back")
	}
	if err := again.Ready(t.Context()); err != nil {
		t.Fatalf("the next check: %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after the next check the table holds %q", got)
	}
	if exists(filepath.Join(dir, "waiting.json")) {
		t.Error("the change that waited is still on disk once it went back")
	}
	if v := confirm(again, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("its verdict: %v", v)
	}
}

// A confirmed table that won't load as the agent starts goes, which leaves the
// server's own firewall, and the agent loads it at its next check.
func TestAConfirmedTableThatWontLoadIsTriedAgain(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	if err := keep(t, m, "chg_1", func(ctx context.Context) error { return m.Put(ctx, "http", http) }); err != nil {
		t.Fatal(err)
	}
	tbl.failNext(errors.New("nft: busy"))
	s := &sessions{}
	again := &Manager{Dir: dir, Table: tbl, Timer: time.Minute, Pins: s.read}
	if err := again.Ready(t.Context()); err == nil {
		t.Error("a start that couldn't load the confirmed table says it's ready")
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("the table holds %q once the confirmed one didn't load", got)
	}
	if err := again.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("the next check loads %q", got)
	}
}

// The SSH pin outlives the session it came from: a restart once the person has
// logged out, a revert, and a later change with nobody logged in all keep the
// addresses pinned when the change before was confirmed, in IPv4 and IPv6, and
// so does the table written for the server's start.
func TestASessionPinOutlivesTheSession(t *testing.T) {
	dir := t.TempDir()
	s := &sessions{}
	s.set(office, "2001:db8::7/128")
	tbl := &table{}
	m := start(t, dir, tbl, s, time.Minute)
	if err := m.CheckPolicy(policy); err != nil {
		t.Fatalf("the plan's check with a session open: %v", err)
	}
	if err := keep(t, m, "chg_1", func(ctx context.Context) error { return m.PutPolicy(ctx, policy) }); err != nil {
		t.Fatal(err)
	}
	const pinned = "pin ssh [22] from [203.0.113.7/32 2001:db8::7/128]"
	if got := tbl.pins(); got != pinned {
		t.Errorf("confirmed with sessions open, the table pins %q", got)
	}

	s.set() // everyone logs out
	again := start(t, dir, tbl, s, time.Minute)
	if got := tbl.pins(); got != pinned {
		t.Errorf("after a restart with nobody logged in, the table pins %q", got)
	}
	if err := again.CheckRule(nil, https); err != nil {
		t.Fatal(err)
	}
	if err := keep(t, again, "chg_2", func(ctx context.Context) error { return again.Put(ctx, "https", https) }); err != nil {
		t.Fatal(err)
	}
	if got := tbl.pins(); got != pinned {
		t.Errorf("after a change with nobody logged in, the table pins %q", got)
	}
	script, err := os.ReadFile(filepath.Join(dir, "confirmed.nft"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ip saddr 203.0.113.7/32 tcp dport 22 accept comment "pin ssh"`, `ip6 saddr 2001:db8::7/128 tcp dport 22 accept comment "pin ssh"`} {
		if !strings.Contains(string(script), want) {
			t.Errorf("confirmed.nft lacks %q:\n%s", want, script)
		}
	}

	// A change nobody confirms goes back to the table as confirmed, pins and all.
	tm := &timers{}
	again.after = tm.after
	if err := again.Put(step("chg_3", false), "http", http); err != nil {
		t.Fatal(err)
	}
	tm.runOut()
	if got := tbl.pins(); got != pinned {
		t.Errorf("after a revert, the table pins %q", got)
	}
	if got := tbl.rules(); got != "rule https, deny the rest" {
		t.Errorf("after a revert, the table holds %q", got)
	}
}

// A confirmation records the table as it loaded, reading nothing of the server,
// so Docker going away as the person confirms neither fails it nor leaves the
// disk saying otherwise than the agent.
func TestAConfirmationReadsNothingOfTheServer(t *testing.T) {
	dir := t.TempDir()
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	var down bool
	var mu sync.Mutex
	docker := func(context.Context) (DockerNetworks, error) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return DockerNetworks{}, errors.New("Cannot connect to the Docker daemon")
		}
		return DockerNetworks{Interfaces: []string{"docker0"}}, nil
	}
	m := start(t, dir, tbl, s, time.Minute, func(m *Manager) { m.Docker = docker })
	errc := make(chan error, 1)
	go func() { errc <- m.PutPolicy(step("chg_1", true), policy) }()
	waitFor(t, m, "chg_1")
	mu.Lock()
	down = true
	mu.Unlock()
	s.err = errors.New("can't read /proc")
	if v := confirm(m, "chg_1"); v != resource.AccessKept {
		t.Fatalf("a confirmation with Docker down: %v", v)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	again := start(t, dir, tbl, &sessions{}, time.Minute)
	if p, _ := again.Policy(context.Background()); !p.GetDenyEverythingElse() {
		t.Error("after a restart the policy isn't confirmed")
	}
	if v := confirm(again, "chg_1"); v != resource.AccessKept {
		t.Errorf("after a restart the verdict is %v", v)
	}
	if got := fmt.Sprint(tbl.current().DockerInterfaces); got != "[docker0]" {
		t.Errorf("after a restart the table leaves %s to Docker", got)
	}
}

// A confirmation whose table the disk won't take isn't kept, and confirmed.json
// goes back as it was, so a restart loads what was confirmed before.
func TestAConfirmationTheDiskWontFinishIsntKept(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	if err := keep(t, m, "chg_0", func(ctx context.Context) error { return m.Put(ctx, "deny-db", denyDB) }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "confirmed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	// confirmed.nft can't be replaced: a folder stands in its way.
	nft := filepath.Join(dir, "confirmed.nft")
	if err := os.Remove(nft); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nft, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation the disk didn't finish: %v, want rolled back", v)
	}
	after, err := os.ReadFile(filepath.Join(dir, "confirmed.json"))
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("confirmed.json after it:\n%s\nwant as before:\n%s", after, before)
	}
	if got := tbl.rules(); got != "rule deny-db" {
		t.Errorf("after it the table holds %q", got)
	}
	again := start(t, dir, tbl, &sessions{}, time.Minute)
	if got := tbl.rules(); got != "rule deny-db" {
		t.Errorf("after a restart the table holds %q", got)
	}
	if p, _ := again.Policy(context.Background()); p != nil {
		t.Errorf("after a restart the policy reads %v", p)
	}
}

// A confirmation the agent can't record on disk at all isn't kept either.
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
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation the disk refused: %v, want rolled back", v)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it the table holds %q", got)
	}
}

// The agent stops after recording a confirmation and before it says so: as it
// starts, the change is kept, not rolled back.
func TestAConfirmationOnDiskBeforeTheAgentStoppedIsKept(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	w := m.waiting
	if err := m.saveConfirmed(confirmedState{state: w.state, table: w.table, wait: w.wait}); err != nil {
		t.Fatal(err)
	}
	w.timer.Stop() // the agent stops here
	m.mu.Unlock()
	again := start(t, dir, tbl, &sessions{}, time.Minute)
	if v := confirm(again, "chg_1"); v != resource.AccessKept {
		t.Errorf("its verdict: %v, want kept", v)
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("the table holds %q", got)
	}
}

// The timer's revert needs neither Docker nor the pins: a Docker that stops
// answering after the change loads holds up neither the revert nor a
// confirmation.
func TestTheRevertNeedsNeitherDockerNorThePins(t *testing.T) {
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	tm := &timers{}
	hang := make(chan struct{})
	var hung sync.Once
	var hanging bool
	var mu sync.Mutex
	docker := func(ctx context.Context) (DockerNetworks, error) {
		mu.Lock()
		h := hanging
		mu.Unlock()
		if h {
			select {
			case <-hang:
			case <-ctx.Done():
				return DockerNetworks{}, ctx.Err()
			}
		}
		return DockerNetworks{Interfaces: []string{"docker0"}}, nil
	}
	t.Cleanup(func() { hung.Do(func() { close(hang) }) })
	m := start(t, t.TempDir(), tbl, s, time.Minute, func(m *Manager) { m.Docker, m.after = docker, tm.after })
	if err := keep(t, m, "chg_0", func(ctx context.Context) error { return m.Put(ctx, "http", http) }); err != nil {
		t.Fatal(err)
	}
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	hanging = true
	mu.Unlock()
	s.mu.Lock()
	s.err = errors.New("can't read /proc")
	s.mu.Unlock()
	reverted := make(chan struct{})
	go func() { tm.runOut(); close(reverted) }()
	select {
	case <-reverted:
	case <-time.After(5 * time.Second):
		t.Fatal("the revert waits on Docker")
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("after the revert the table holds %q", got)
	}
	confirmed := make(chan resource.AccessVerdict, 1)
	go func() { confirmed <- m.Confirm(context.Background(), "chg_1") }()
	select {
	case v := <-confirmed:
		if v != resource.AccessRolledBack {
			t.Errorf("a late confirmation: %v", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a confirmation waits on Docker")
	}
}

// Docker's networks for a table: none on a server without Docker, which
// leaves what it forwards alone; as Docker last listed them when it doesn't
// answer, or as the confirmed table has them after a restart; and, with
// neither, the change doesn't load.
func TestDockersNetworksForATable(t *testing.T) {
	s := &sessions{}
	s.set(office)

	none, tbl := manager(t, t.TempDir(), time.Minute)
	if err := keep(t, none, "chg_1", func(ctx context.Context) error { return none.Put(ctx, "http", http) }); err != nil {
		t.Fatal(err)
	}
	if got := tbl.current(); !got.NoDocker || len(got.DockerInterfaces) != 0 {
		t.Errorf("with no Docker the table has %v, %v", got.NoDocker, got.DockerInterfaces)
	}

	var err error
	var nets DockerNetworks
	var mu sync.Mutex
	docker := func(context.Context) (DockerNetworks, error) {
		mu.Lock()
		defer mu.Unlock()
		return nets, err
	}
	set := func(n DockerNetworks, e error) {
		mu.Lock()
		defer mu.Unlock()
		nets, err = n, e
	}
	dir := t.TempDir()
	tbl = &table{}
	set(DockerNetworks{}, errors.New("Cannot connect to the Docker daemon"))
	m := start(t, dir, tbl, s, time.Minute, func(m *Manager) { m.Docker = docker })
	if err := m.Put(step("chg_1", false), "http", http); err == nil || !strings.Contains(err.Error(), "can't read Docker's networks") {
		t.Errorf("a first change with Docker down: %v", err)
	}
	set(DockerNetworks{}, ErrNoDocker)
	if err := keep(t, m, "chg_2", func(ctx context.Context) error { return m.Put(ctx, "http", http) }); err != nil {
		t.Fatal(err)
	}
	if got := tbl.current(); !got.NoDocker {
		t.Error("with no Docker socket the table filters what the server forwards")
	}
	set(DockerNetworks{Interfaces: []string{"docker0"}}, nil)
	if err := keep(t, m, "chg_3", func(ctx context.Context) error { return m.Put(ctx, "https", https) }); err != nil {
		t.Fatal(err)
	}
	// Docker lists a new network as a plan reads it, then stops answering.
	set(DockerNetworks{Interfaces: []string{"br-1a2b3c4d5e6f", "docker0"}}, nil)
	if err := m.CheckRule(nil, denyDB); err != nil {
		t.Fatal(err)
	}
	set(DockerNetworks{}, errors.New("Cannot connect to the Docker daemon"))
	if err := keep(t, m, "chg_4", func(ctx context.Context) error { return m.Put(ctx, "deny-db", denyDB) }); err != nil {
		t.Fatal(err)
	}
	if got := tbl.current(); got.NoDocker || fmt.Sprint(got.DockerInterfaces) != "[br-1a2b3c4d5e6f docker0]" {
		t.Errorf("with Docker down the table leaves %v to Docker, want as it last listed them", got.DockerInterfaces)
	}
	again := start(t, dir, tbl, s, time.Minute, func(m *Manager) { m.Docker = docker })
	if err := keep(t, again, "chg_5", func(ctx context.Context) error { return again.Delete(ctx, "deny-db") }); err != nil {
		t.Fatal(err)
	}
	if got := tbl.current(); fmt.Sprint(got.DockerInterfaces) != "[br-1a2b3c4d5e6f docker0]" {
		t.Errorf("after a restart with Docker down the table leaves %v to Docker, want as confirmed", got.DockerInterfaces)
	}

	// A Docker that takes the connection and doesn't answer holds a change up
	// no longer than its bound, and the change takes the networks as last listed.
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	slow := start(t, dir, tbl, s, time.Minute, func(m *Manager) {
		m.readWait = 50 * time.Millisecond
		m.Docker = func(ctx context.Context) (DockerNetworks, error) {
			select {
			case <-hung:
			case <-ctx.Done():
			}
			return DockerNetworks{}, ctx.Err()
		}
	})
	done := make(chan error, 1)
	go func() { done <- slow.Put(step("chg_6", false), "http2", http) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a change with Docker hung: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a change waits on a hung Docker")
	}
	slow.Confirm(t.Context(), "chg_6")
}

// A container's SSH session to the server isn't a person's: a peer on one of
// Docker's networks is neither pinned nor taken as a session for the checks.
func TestAContainersSessionIsntAPersons(t *testing.T) {
	s := &sessions{}
	s.set("172.18.0.5/32")
	docker := func(context.Context) (DockerNetworks, error) {
		return DockerNetworks{Interfaces: []string{"br-1a2b3c4d5e6f"}, Subnets: []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}}, nil
	}
	tbl := &table{}
	m := start(t, t.TempDir(), tbl, s, time.Minute, func(m *Manager) { m.Docker = docker })
	if err := m.CheckPolicy(policy); !errors.Is(err, errNoSession) {
		t.Errorf("deny everything else with only a container's session: %v", err)
	}
	s.set("172.18.0.5/32", office)
	if err := keep(t, m, "chg_1", func(ctx context.Context) error { return m.PutPolicy(ctx, policy) }); err != nil {
		t.Fatal(err)
	}
	if got := tbl.pins(); got != "pin ssh [22] from [203.0.113.7/32]" {
		t.Errorf("the table pins %q", got)
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
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	if got := tbl.rules(); got != "rule http, deny the rest" {
		t.Errorf("the table holds %q", got)
	}
	confirm(m, "chg_1")
}

// The last step on the server starts the timer again, so the person has its
// whole length from the last step, and says so.
func TestTheLastStepStartsTheTimerAgain(t *testing.T) {
	m, _ := manager(t, t.TempDir(), time.Minute)
	clock := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	m.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clock = clock.Add(50 * time.Second)
	mu.Unlock()
	var told *resource.AccessWait
	ctx := resource.WithOutcome(step("chg_1", true), func(o resource.Outcome) { told = o.Waits })
	errc := make(chan error, 1)
	go func() { errc <- m.Put(ctx, "https", https) }()
	waitForAnnounce := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		got := told
		m.mu.Unlock()
		if got != nil || time.Now().After(waitForAnnounce) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.mu.Lock()
	got := told
	m.mu.Unlock()
	if got == nil || !got.Deadline.Equal(clock.Add(time.Minute)) || got.Left != time.Minute {
		t.Errorf("the last step waits for %+v, want a minute from it", got)
	}
	confirm(m, "chg_1")
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// A step of a change the server already put back fails, so the change rolls
// back, rather than start a wait of its own from what was confirmed.
func TestAStepAfterTheServerPutItBackFails(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	tm := &timers{}
	m.after = tm.after
	if err := m.Put(step("chg_1", false), "deny-db", denyDB); err != nil {
		t.Fatal(err)
	}
	tm.runOut()
	err := m.Put(step("chg_1", true), "http", http)
	if plain, _ := resource.Explain(err); err == nil || !strings.Contains(plain, "put back its firewall as it was before this step") {
		t.Errorf("the next step: %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("the table holds %q", got)
	}
	if r, _ := m.Rule(context.Background(), "deny-db"); r != nil {
		t.Errorf("the first step's rule is confirmed: %v", r)
	}
}

// A resumed step of a change that still waits waits for its verdict; one of a
// change the server put back fails.
func TestAResumedStepWaitsForTheVerdict(t *testing.T) {
	m, _ := manager(t, t.TempDir(), time.Minute)
	tm := &timers{}
	m.after = tm.after
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
	confirm(m, "chg_1")
	if err := <-errc; err != nil {
		t.Errorf("a resumed step ends %v", err)
	}

	if err := m.Put(step("chg_2", false), "https", https); err != nil {
		t.Fatal(err)
	}
	tm.runOut()
	if err := m.Wait(step("chg_2", true)); err == nil {
		t.Error("a resumed step of a change put back passes")
	}
	if err := m.Wait(step("chg_3", true)); err != nil {
		t.Errorf("a resumed step of a change the server knows nothing of: %v", err)
	}
}

// A load that can't be seen through may be live, so what was confirmed goes
// back; one nft refused changed nothing.
func TestALoadThatMayBeLiveGoesBack(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	tbl.failNext(nftables.ErrUnknown)
	if err := m.Put(step("chg_1", false), "http", http); !errors.Is(err, nftables.ErrUnknown) {
		t.Fatalf("a load that can't be seen through: %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after a load that may be live the table holds %q", got)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("its verdict is %v", v)
	}
	// The next change starts afresh.
	if err := m.Put(step("chg_2", false), "http", http); err != nil {
		t.Errorf("a change after one went back: %v", err)
	}
	confirm(m, "chg_2")
}

// A later step whose load nft refused leaves the earlier steps live, so what
// was confirmed goes back, and the change's verdict is rolled back.
func TestALaterStepThatWontLoadGoesBack(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	tbl.failNext(errors.New("nft: syntax error"))
	if err := m.Put(step("chg_1", true), "https", https); err == nil {
		t.Fatal("a later step nft refused passed")
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("the table holds %q", got)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("its verdict is %v", v)
	}
}

// The revert loads the table as confirmed; if that won't load, it removes the
// table, which leaves the server's own firewall, rather than the change's.
func TestARevertThatWontLoadRemovesTheTable(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	if err := keep(t, m, "chg_0", func(ctx context.Context) error { return m.Put(ctx, "http", http) }); err != nil {
		t.Fatal(err)
	}
	tm := &timers{}
	m.after = tm.after
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	tbl.failNext(errors.New("nft: busy"))
	tm.runOut()
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after a revert that wouldn't load the table holds %q", got)
	}
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("the next check loads %q", got)
	}
}

// A revert that can neither load the confirmed table nor remove the table
// leaves the change on disk, so the next check puts back what was confirmed.
func TestARevertThatCantLoadOrRemoveIsTriedAgain(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	if err := keep(t, m, "chg_0", func(ctx context.Context) error { return m.Put(ctx, "http", http) }); err != nil {
		t.Fatal(err)
	}
	tm := &timers{}
	m.after = tm.after
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	tbl.failNext(errors.New("nft: busy"))
	tbl.mu.Lock()
	tbl.failDeletes = 1
	tbl.mu.Unlock()
	tm.runOut()
	if !exists(filepath.Join(dir, "waiting.json")) {
		t.Error("the change is gone from disk while its table may be live")
	}
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("the next check loads %q", got)
	}
	if exists(filepath.Join(dir, "waiting.json")) {
		t.Error("the change is still on disk once it went back")
	}
}

// A rollback puts back what was confirmed with no timer, and it's confirmed; a
// change waiting then ends rolled back, as the rollback overrides it.
func TestARollbackNeedsNoConfirmation(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	if err := m.Put(step("chg_1", false), "deny-db", denyDB); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(rollback("chg_r"), "http", http); err != nil {
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
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("the change that waited: %v", v)
	}
}

// A rollback step that won't load isn't confirmed: neither the agent nor the
// disk takes it.
func TestARollbackThatWontLoadIsntConfirmed(t *testing.T) {
	dir := t.TempDir()
	m, tbl := manager(t, dir, time.Minute)
	tbl.failNext(errors.New("nft: syntax error"))
	if err := m.Put(rollback("chg_r"), "http", http); err == nil {
		t.Fatal("a rollback nft refused passed")
	}
	if r, _ := m.Rule(context.Background(), "http"); r != nil {
		t.Errorf("planning sees %v", r)
	}
	if exists(filepath.Join(dir, "confirmed.json")) {
		t.Error("the rollback is on disk as confirmed")
	}
}

// Narrowing SSH while nobody's address is pinned, or denying the control
// plane's own port, is refused when it's planned; a UDP deny of SSH's port
// narrows nothing.
func TestChecksRefuseWhatWouldCutAccess(t *testing.T) {
	s := &sessions{own: []uint16{7600}}
	m := start(t, t.TempDir(), &table{}, s, time.Minute)
	deny := func(port uint32) *resourcev1.FirewallRule {
		return &resourcev1.FirewallRule{Port: port, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	}
	if err := m.CheckRule(nil, deny(22)); !errors.Is(err, errNoSession) {
		t.Errorf("denying SSH with no session: %v", err)
	}
	if err := m.CheckRule(nil, deny(7600)); err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Errorf("denying the control plane's port: %v", err)
	}
	if err := m.CheckRule(nil, deny(5432)); err != nil {
		t.Errorf("denying another port: %v", err)
	}
	udp := deny(22)
	udp.Protocol = resourcev1.Protocol_PROTOCOL_UDP
	if err := m.CheckRule(nil, udp); err != nil {
		t.Errorf("denying UDP on SSH's port: %v", err)
	}
	if err := m.CheckPolicy(policy); !errors.Is(err, errNoSession) {
		t.Errorf("deny everything else with no session: %v", err)
	}
	s.set(office)
	if err := m.CheckRule(nil, deny(22)); err != nil {
		t.Errorf("denying SSH with a session to pin: %v", err)
	}
}

// With deny everything else on and nobody's address pinned, deleting the allow
// that keeps SSH open, or narrowing it, is refused when it's planned, and the
// driver flags both as able to lock you out; with SSH open from anywhere,
// the policy goes on.
func TestChecksSeeTheStateTheEditLeaves(t *testing.T) {
	s := &sessions{}
	m := start(t, t.TempDir(), &table{}, s, time.Minute)
	ssh := &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Protocol: resourcev1.Protocol_PROTOCOL_TCP}
	m.mu.Lock()
	m.confirmed.Rules["ssh"] = ssh
	m.mu.Unlock()
	if err := m.CheckPolicy(policy); err != nil {
		t.Fatalf("deny everything else with SSH allowed from anywhere: %v", err)
	}
	m.mu.Lock()
	m.confirmed.Policy = policy
	m.mu.Unlock()
	d := &firewall.Driver{Backend: m}
	narrow := &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Protocol: resourcev1.Protocol_PROTOCOL_TCP, Sources: []string{"198.51.100.0/24"}}
	for name, desired := range map[string]*resourcev1.FirewallRule{"deleting": nil, "narrowing": narrow} {
		dec, err := d.Plan(ssh, desired, nil)
		if !errors.Is(err, errNoSession) {
			t.Errorf("%s the allow of SSH under the policy: %v", name, err)
		}
		if len(dec.Warnings) == 0 {
			t.Errorf("%s the allow of SSH isn't flagged", name)
		}
	}
	if _, err := d.Plan(ssh, &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Protocol: resourcev1.Protocol_PROTOCOL_TCP}, nil); err != nil {
		t.Errorf("keeping the allow as it is: %v", err)
	}
}

// The checks run again as each step applies, over the change so far, as the
// server is then: a session that ended since the plan leaves nobody to pin.
func TestTheChecksRunAgainAsAStepApplies(t *testing.T) {
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	m := start(t, t.TempDir(), tbl, s, time.Minute)
	if err := m.CheckPolicy(policy); err != nil {
		t.Fatal(err)
	}
	s.set()
	if err := m.PutPolicy(step("chg_1", false), policy); !errors.Is(err, errNoSession) {
		t.Errorf("applying with nobody's session open now: %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("the table holds %q", got)
	}
}

// Where Dockwell finds no SSH at all, port 22 stands for it, and narrowing it
// is refused with that said.
func TestNoSSHFoundRefusesNarrowing(t *testing.T) {
	m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: func(context.Context) (Pins, error) { return Pins{}, nil }}
	if err := m.CheckPolicy(policy); !errors.Is(err, errNoSSH) {
		t.Errorf("deny everything else with no SSH found: %v", err)
	}
	if err := m.CheckRule(nil, &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}); !errors.Is(err, errNoSSH) {
		t.Errorf("denying 22 with no SSH found: %v", err)
	}
	if err := m.CheckRule(nil, http); err != nil {
		t.Errorf("allowing 80 with no SSH found: %v", err)
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
	if v := confirm(m, "chg_1"); v != resource.AccessRolledBack {
		t.Errorf("a confirmation past its deadline: %v, want rolled back", v)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it the table holds %q", got)
	}
}

// A Docker network made after the last change to the firewall is left to its
// containers once the agent sees it, under the policy, with the rules, the
// policy and the pins as confirmed; the table that does so is confirmed on
// disk, so a restart keeps it. Nothing loads while the networks stay as they
// are, or while a change waits.
func TestANewDockerNetworkIsRenderedIntoTheTable(t *testing.T) {
	dir := t.TempDir()
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	var mu sync.Mutex
	nets := DockerNetworks{Interfaces: []string{"docker0"}}
	docker := func(context.Context) (DockerNetworks, error) {
		mu.Lock()
		defer mu.Unlock()
		return nets, nil
	}
	set := func(n DockerNetworks) {
		mu.Lock()
		defer mu.Unlock()
		nets = n
	}
	m := start(t, dir, tbl, s, time.Minute, func(m *Manager) { m.Docker = docker })
	if err := keep(t, m, "chg_1", func(ctx context.Context) error { return m.PutPolicy(ctx, policy) }); err != nil {
		t.Fatal(err)
	}
	loads := tbl.loads
	if err := m.refresh(t.Context()); err != nil || tbl.loads != loads {
		t.Errorf("with Docker's networks as they were, a refresh loaded %d tables: %v", tbl.loads-loads, err)
	}

	set(DockerNetworks{Interfaces: []string{"docker0", "br-1a2b3c4d5e6f"}})
	if err := m.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := tbl.current()
	if fmt.Sprint(got.DockerInterfaces) != "[br-1a2b3c4d5e6f docker0]" || !got.DenyRest || tbl.pins() != "pin ssh [22] from [203.0.113.7/32]" {
		t.Errorf("after the network was made the table leaves %v to Docker, denies the rest %t, pins %q", got.DockerInterfaces, got.DenyRest, tbl.pins())
	}
	script, err := os.ReadFile(filepath.Join(dir, "confirmed.nft"))
	if err != nil || !strings.Contains(string(script), `"br-1a2b3c4d5e6f"`) {
		t.Errorf("confirmed.nft doesn't leave the new network to its containers: %v\n%s", err, script)
	}
	again := &table{}
	start(t, dir, again, &sessions{}, time.Minute)
	if fmt.Sprint(again.current().DockerInterfaces) != "[br-1a2b3c4d5e6f docker0]" {
		t.Errorf("after a restart the table leaves %v to Docker", again.current().DockerInterfaces)
	}

	// A change waits: its own confirmation or revert renders the table.
	if err := m.Put(step("chg_2", false), "http", http); err != nil {
		t.Fatal(err)
	}
	set(DockerNetworks{Interfaces: []string{"docker0"}})
	loads = tbl.loads
	if err := m.refresh(t.Context()); err != nil || tbl.loads != loads {
		t.Errorf("while a change waits, a refresh loaded %d tables: %v", tbl.loads-loads, err)
	}
	if v := confirm(m, "chg_2"); v != resource.AccessKept {
		t.Fatalf("confirming chg_2: %v", v)
	}

	// Docker goes away from the server: the table leaves forwarding alone.
	set(DockerNetworks{})
	m.Docker = func(context.Context) (DockerNetworks, error) { return DockerNetworks{}, ErrNoDocker }
	if err := m.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := tbl.current(); !got.NoDocker || len(got.DockerInterfaces) != 0 {
		t.Errorf("with no Docker the table has %t, %v", got.NoDocker, got.DockerInterfaces)
	}
}

// A rollback that can't read /proc keeps the pin of the control plane's own
// ports, which never needed it.
func TestARollbackThatCantReadProcKeepsTheControlPlanesPin(t *testing.T) {
	s := &sessions{own: []uint16{7600}}
	s.set(office)
	tbl := &table{}
	m := start(t, t.TempDir(), tbl, s, time.Minute)
	s.mu.Lock()
	s.err = errors.New("can't read /proc")
	s.mu.Unlock()
	if err := m.Put(rollback("chg_r"), "http", http); err != nil {
		t.Fatal(err)
	}
	if got := tbl.pins(); !strings.Contains(got, "pin control plane [7600]") {
		t.Errorf("after a rollback with /proc unreadable the table pins %q", got)
	}
}

// A confirmation that comes before the change's last step on the server has
// said it waits keeps nothing: part of the change hasn't loaded, and keeping
// now would keep a part. The change goes on waiting, and its timer goes on.
func TestAConfirmationBeforeTheLastStepKeepsNothing(t *testing.T) {
	m, tbl := manager(t, t.TempDir(), time.Minute)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	if v := m.Confirm(t.Context(), "chg_1"); v != resource.AccessNotWaiting {
		t.Errorf("a confirmation before the last step: %v, want not waiting", v)
	}
	m.mu.Lock()
	waiting, kept := m.waiting != nil && m.waiting.change == "chg_1", len(m.confirmed.Rules)
	m.mu.Unlock()
	if !waiting || kept != 0 {
		t.Errorf("after it, still waiting %t, %d rules confirmed", waiting, kept)
	}
	if got := tbl.rules(); got != "rule http" {
		t.Errorf("the table holds %q", got)
	}
}

// An address kept from an earlier change leaves once nothing narrows SSH: with
// deny everything else on it's kept past the session; a later change that
// allows SSH's port from anywhere, or turns the policy off, forgets it, so the
// next table that narrows SSH starts from who is signed in then.
func TestAPinLeavesOnceNothingNarrowsSSH(t *testing.T) {
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	m := start(t, t.TempDir(), tbl, s, time.Minute)
	keep := func(change string, fn func(context.Context) error) {
		t.Helper()
		if err := fn(step(change, false)); err != nil {
			t.Fatal(err)
		}
		if v := confirm(m, change); v != resource.AccessKept {
			t.Fatalf("%s: %v", change, v)
		}
	}
	keep("chg_1", func(ctx context.Context) error { return m.PutPolicy(ctx, policy) })
	s.set() // they sign out
	keep("chg_2", func(ctx context.Context) error { return m.Put(ctx, "http", http) })
	if got := tbl.pins(); got != "pin ssh [22] from [203.0.113.7/32]" {
		t.Fatalf("under the policy, after they signed out, the table pins %q", got)
	}
	keep("chg_3", func(ctx context.Context) error {
		return m.Put(ctx, "ssh", &resourcev1.FirewallRule{Port: 22})
	})
	keep("chg_4", func(ctx context.Context) error { return m.Put(ctx, "https", https) })
	if got := tbl.pins(); got != "" {
		t.Errorf("once a rule allows SSH from anywhere, the table still pins %q", got)
	}
	if err := m.Delete(step("chg_5", false), "ssh"); err == nil {
		t.Error("taking the allow of SSH away with nobody signed in and nobody kept passed")
	}
}

// On a control server whose panel has no domain, people reach the panel through
// SSH: deny everything else is refused there unless a rule allows SSH's port,
// whoever is signed in, as a pin holds only while their address does.
func TestThePanelOverSSHNeedsARuleForSSH(t *testing.T) {
	s := &sessions{}
	s.set(office)
	m := start(t, t.TempDir(), &table{}, s, time.Minute)
	m.PanelOverSSH = true
	err := m.CheckPolicy(policy)
	if err == nil || !strings.Contains(err.Error(), "has no domain") || !strings.Contains(err.Error(), "Add a rule that allows port 22") {
		t.Errorf("deny everything else with the panel over SSH: %v", err)
	}
	if err := m.PutPolicy(step("chg_1", true), policy); err == nil {
		t.Error("applying it passed")
	}
	if err := m.Put(step("chg_2", false), "ssh", &resourcev1.FirewallRule{Port: 22, Sources: []string{"198.51.100.0/24"}}); err != nil {
		t.Fatal(err)
	}
	confirm(m, "chg_2")
	if err := m.CheckPolicy(policy); err != nil {
		t.Errorf("with a rule that allows SSH: %v", err)
	}
	m.PanelOverSSH = false
	other := start(t, t.TempDir(), &table{}, s, time.Minute)
	if err := other.CheckPolicy(policy); err != nil {
		t.Errorf("on a server with a session and a domain: %v", err)
	}
}

// A revert that failed is tried again on the agent's own clock, whether or not
// anything asks: the stream to the control plane, which used to ask, may be
// what the table cut.
func TestAFailedRevertIsRetriedOnTheAgentsOwnClock(t *testing.T) {
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	tm := &timers{}
	m := start(t, t.TempDir(), tbl, s, time.Minute, func(m *Manager) { m.after, m.every = tm.after, 5*time.Millisecond })
	if err := m.PutPolicy(step("chg_1", false), policy); err != nil {
		t.Fatal(err)
	}
	// Nobody confirms, and neither loading what was confirmed nor removing the table works.
	tbl.failDeletes = 1
	tm.runOut()
	if got := tbl.rules(); got != "deny the rest" {
		t.Fatalf("after the failed revert the table holds %q", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go m.KeepCurrent(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for tbl.rules() != "(no table)" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("the agent's own clock left %q", got)
	}
}

// On a control server whose panel has no domain, whatever narrows SSH is
// refused while a port of SSH's has no rule that allows it: a deny of SSH's
// port as much as deny everything else, as both leave the same state.
func TestThePanelOverSSHRefusesADenyOfSSHToo(t *testing.T) {
	s := &sessions{}
	s.set(office)
	m := start(t, t.TempDir(), &table{}, s, time.Minute)
	m.PanelOverSSH = true
	deny := &resourcev1.FirewallRule{Port: 22, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	if err := m.CheckRule(nil, deny); err == nil || !strings.Contains(err.Error(), "has no domain") {
		t.Errorf("denying 22 with the panel over SSH: %v", err)
	}
	if err := m.Put(step("chg_1", true), "close", deny); err == nil {
		t.Error("applying it passed")
	}
	if err := m.CheckRule(nil, http); err != nil {
		t.Errorf("allowing 80: %v", err)
	}
	if err := m.Put(step("chg_2", false), "ssh", &resourcev1.FirewallRule{Port: 22, Sources: []string{"198.51.100.0/24"}}); err != nil {
		t.Fatal(err)
	}
	confirm(m, "chg_2")
	if err := m.CheckRule(nil, deny); err != nil {
		t.Errorf("with a rule that allows SSH, a deny of the rest: %v", err)
	}
}
