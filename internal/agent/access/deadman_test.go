// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/resource"
)

// notes is a revert outside the agent that notes what it's asked, with what
// the file it would load held then, and what the table held as it was armed.
type notes struct {
	mu    sync.Mutex
	said  []string
	armed bool
	tbl   *table
	fail  error
}

func (n *notes) Arm(_ context.Context, file string, wait time.Duration) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail != nil {
		return n.fail
	}
	b, _ := os.ReadFile(file)
	n.said = append(n.said, fmt.Sprintf("arm %s, would load %s, table %s", wait, what(string(b)), n.tbl.rules()))
	n.armed = true
	return nil
}

func (n *notes) Disarm(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.said = append(n.said, "disarm")
	n.armed = false
	return nil
}

func (n *notes) take() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := strings.Join(n.said, "; ")
	n.said = nil
	return out
}

// what says a script in a few words: the rules it holds, or that it removes the table.
func what(script string) string {
	if script == nftables.DeleteScript() {
		return "(remove the table)"
	}
	var out []string
	for line := range strings.SplitSeq(script, "\n") {
		if _, name, ok := strings.Cut(line, `comment "rule `); ok {
			if name = strings.TrimSuffix(name, `"`); !strings.Contains(strings.Join(out, " "), name) {
				out = append(out, name)
			}
		}
		if strings.Contains(line, "policy deny everything else") && !strings.Contains(strings.Join(out, " "), "deny the rest") {
			out = append(out, "deny the rest")
		}
	}
	if len(out) == 0 {
		return "(an empty table)"
	}
	return strings.Join(out, ", ")
}

func withDeadMan(t *testing.T) (*Manager, *table, *notes, *timers) {
	t.Helper()
	s := &sessions{}
	s.set(office)
	tbl := &table{}
	n := &notes{tbl: tbl}
	tm := &timers{}
	m := start(t, t.TempDir(), tbl, s, time.Minute, func(m *Manager) { m.DeadMan, m.after = n, tm.after })
	n.take()
	return m, tbl, n, tm
}

// The revert outside the agent is armed before a change's table loads, for a
// little after the agent's own timer, with what was confirmed to load: the
// script that removes the table when nothing was. The change's last step arms
// it again. Keeping the change makes what it would load the table just kept,
// and then disarms it, so one that fires all the same changes nothing.
func TestTheRevertOutsideTheAgentIsArmedBeforeTheLoadAndDisarmedOnceKept(t *testing.T) {
	m, _, n, _ := withDeadMan(t)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	if got := n.take(); got != "arm 1m15s, would load (remove the table), table (no table)" {
		t.Errorf("as the first change loads: %s", got)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessKept {
		t.Fatalf("keeping it: %v", v)
	}
	if got := n.take(); got != "disarm" {
		t.Errorf("as it's kept: %s", got)
	}
	if got := what(read1(t, m.deadManFile())); got != "http" {
		t.Errorf("once kept, it would load %s", got)
	}

	// A second change: armed before it loads, with the first as what goes back.
	done := make(chan error, 1)
	go func() { done <- m.PutPolicy(step("chg_2", true), policy) }()
	until(t, func() bool { return strings.Count(n.peek(), "arm ") == 2 })
	if got := n.take(); got != "arm 1m15s, would load http, table rule http; arm 1m15s, would load http, table rule http, deny the rest" {
		t.Errorf("as the second change loads, and at its last step: %s", got)
	}
	if v := m.Confirm(t.Context(), "chg_2"); v != resource.AccessKept {
		t.Fatalf("keeping chg_2: %v", v)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := n.take(); got != "disarm" || what(read1(t, m.deadManFile())) != "http, deny the rest" {
		t.Errorf("once kept: %s, and it would load %s", got, what(read1(t, m.deadManFile())))
	}
}

// When the agent's own timer puts the firewall back, nothing is left for the
// revert outside it, and it's disarmed. When the agent's own revert fails, it
// stays armed: it's the second chance.
func TestTheRevertOutsideTheAgentStaysArmedUntilTheAgentsOwnHasLoaded(t *testing.T) {
	m, tbl, n, tm := withDeadMan(t)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	n.take()
	tm.runOut()
	if got := n.take(); got != "disarm" || tbl.rules() != "(no table)" {
		t.Errorf("after the agent's own revert: %s, table %s", got, tbl.rules())
	}

	if err := m.Put(step("chg_2", false), "https", https); err != nil {
		t.Fatal(err)
	}
	n.take()
	tbl.failDeletes = 1
	tm.runOut()
	if got := n.take(); got != "" || !n.armed {
		t.Errorf("after a revert that failed: %q, armed %t", got, n.armed)
	}
	// The agent's next check puts it back, and disarms it then.
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := n.take(); got != "disarm" || tbl.rules() != "(no table)" {
		t.Errorf("after the agent's next check: %s, table %s", got, tbl.rules())
	}
}

// A load nft refused changed nothing, so the revert armed for it is disarmed.
// A change whose revert can't be armed doesn't load at all: with no safety
// net, nothing reaches the kernel, no change waits, and the step says why. At
// a change's last step, where earlier steps are loaded already, what was
// confirmed goes back.
func TestARefusedLoadDisarmsAndAChangeWithNoNetDoesntLoad(t *testing.T) {
	m, tbl, n, _ := withDeadMan(t)
	tbl.failNext(errors.New("nft: syntax error"))
	if err := m.Put(step("chg_1", false), "http", http); err == nil {
		t.Fatal("a load nft refused passed")
	}
	if got := n.take(); !strings.HasPrefix(got, "arm ") || !strings.HasSuffix(got, "; disarm") {
		t.Errorf("around a refused load: %s", got)
	}

	n.fail = errors.New("systemd-run: Failed to connect to bus")
	loads := tbl.loads
	err := m.Put(step("chg_2", false), "http", http)
	if err == nil || !strings.Contains(err.Error(), "can't arm the revert") || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("a change whose revert couldn't be armed: %v", err)
	}
	m.mu.Lock()
	waiting := m.waiting != nil
	m.mu.Unlock()
	if tbl.loads != loads || waiting || exists(filepath.Join(m.Dir, "waiting.json")) || tbl.rules() != "(no table)" {
		t.Errorf("with no net: %d loads, waiting %t, table %s", tbl.loads-loads, waiting, tbl.rules())
	}

	// The first step arms; the last step's arming fails: what loaded goes back.
	n.fail = nil
	if err := m.Put(step("chg_3", false), "http", http); err != nil {
		t.Fatal(err)
	}
	n.fail = errors.New("systemd-run: Unit dockwell-access-revert.timer already exists")
	if err := m.Put(step("chg_3", true), "https", https); err == nil || !strings.Contains(err.Error(), "can't arm the revert") {
		t.Errorf("a last step whose revert couldn't be armed again: %v", err)
	}
	if got := tbl.rules(); got != "(no table)" {
		t.Errorf("after it the table holds %s", got)
	}
	if v := confirm(m, "chg_3"); v != resource.AccessRolledBack {
		t.Errorf("confirming it after: %v", v)
	}
}

func (n *notes) peek() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.said, "; ")
}

func read1(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func until(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("waited 5 s")
}

// The child sleeps in a session of its own, then has nft load the file, if its
// token is still the one on disk: disarmed, or armed again since, it does
// nothing. The words it's given are arguments, never part of its script.
func TestTheChildLoadsTheFileUnlessItsDisarmedOrArmedAgain(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran with spaces")
	nft := filepath.Join(dir, "nft")
	if err := os.WriteFile(nft, []byte("#!/bin/sh\necho \"$*\" >> '"+ran+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &ChildDeadMan{Shell: "/bin/sh", Nft: nft, Dir: dir}
	runs := func() string {
		b, _ := os.ReadFile(ran)
		return strings.TrimSpace(string(b))
	}
	first := filepath.Join(dir, "first; touch pwned.nft")
	if err := d.Arm(t.Context(), first, time.Second); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return runs() != "" })
	if got := runs(); got != "-f "+first {
		t.Errorf("it ran nft %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned.nft")); err == nil {
		t.Error("a file's name ran as part of the script")
	}
	_ = os.Remove(ran)

	// Disarmed: it wakes and does nothing.
	if err := d.Arm(t.Context(), "second", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Disarm(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Armed again: the older child does nothing, the newer one loads its own.
	if err := d.Arm(t.Context(), "third", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Arm(t.Context(), "fourth", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return runs() != "" })
	time.Sleep(300 * time.Millisecond)
	if got := runs(); got != "-f fourth" {
		t.Errorf("after a disarm and a second arm it ran %q", got)
	}
	if err := d.Disarm(t.Context()); err != nil {
		t.Errorf("disarming with nothing armed: %v", err)
	}
}

// With DOCKWELL_TEST_NFT naming a real nft, as root, in a network namespace of
// its own: an agent that is killed while a change waits, and never starts
// again, doesn't leave the change's table. The revert outside it loads what
// was confirmed, or removes the table when nothing was; and once a change is
// kept, nothing fires. It changes the machine's firewall, so it runs only
// where CI says.
func TestTheRevertOutsideTheAgentWorksOnRealNftablesWithTheAgentKilled(t *testing.T) {
	nft := os.Getenv("DOCKWELL_TEST_NFT")
	if nft == "" {
		t.Skip("DOCKWELL_TEST_NFT names no nft")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nft, filepath.Join(root, "usr", "sbin", "nft")); err != nil {
		t.Fatal(err)
	}
	fw := &nftables.Firewall{Run: &hosttool.Runner{Root: root}}
	t.Cleanup(func() { _ = fw.Delete(context.Background()) })
	listing := func() string {
		out, _ := exec.CommandContext(t.Context(), nft, "list", "table", nftables.Family, nftables.Name).CombinedOutput()
		return string(out)
	}
	// agent runs this test's binary again as the agent: it applies what's asked,
	// says "waiting" once the change's table is loaded, and then only waits.
	agent := func(dir string, steps string) *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run", "TestTheAgentThatIsKilled", "-test.v")
		cmd.Env = append(os.Environ(), "DOCKWELL_DEADMAN_AGENT="+steps, "DOCKWELL_DEADMAN_DIR="+dir, "DOCKWELL_DEADMAN_ROOT="+root)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		buf := make([]byte, 4096)
		var seen string
		for !strings.Contains(seen, "waiting") {
			n, err := out.Read(buf)
			seen += string(buf[:n])
			if err != nil {
				t.Fatalf("the agent ended before it waited: %v\n%s", err, seen)
			}
		}
		return cmd
	}
	kill := func(cmd *exec.Cmd) {
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
	}

	// Nothing confirmed: a first change waits, the agent is killed, and the table goes.
	first := agent(t.TempDir(), "policy")
	if got := listing(); !strings.Contains(got, "policy deny everything else") {
		t.Fatalf("while the first change waits the kernel holds:\n%s", got)
	}
	kill(first)
	until(t, func() bool { return !strings.Contains(listing(), "policy deny everything else") })
	if got := listing(); strings.Contains(got, "chain input") {
		t.Errorf("with nothing confirmed, after the agent was killed, the kernel still holds:\n%s", got)
	}

	// A rule confirmed, then a change that waits: killed, and the confirmed table comes back.
	dir := t.TempDir()
	second := agent(dir, "keep-rule,policy")
	kill(second)
	until(t, func() bool {
		got := listing()
		return strings.Contains(got, `comment "rule kept"`) && !strings.Contains(got, "policy deny everything else")
	})

	// A change kept, then the agent killed: nothing fires, and what was kept stays.
	third := agent(t.TempDir(), "keep-policy")
	kill(third)
	time.Sleep(realTimer + deadManGraceForTest + 2*time.Second)
	if got := listing(); !strings.Contains(got, "policy deny everything else") {
		t.Errorf("a kept change went after the agent was killed:\n%s", got)
	}
}

const (
	realTimer           = 2 * time.Second
	deadManGraceForTest = time.Second
)

// TestTheAgentThatIsKilled is the agent the test above kills: it does nothing
// unless that test started it.
func TestTheAgentThatIsKilled(t *testing.T) {
	steps := os.Getenv("DOCKWELL_DEADMAN_AGENT")
	if steps == "" {
		t.Skip("only the real nftables test runs this")
	}
	dir, root := os.Getenv("DOCKWELL_DEADMAN_DIR"), os.Getenv("DOCKWELL_DEADMAN_ROOT")
	nft := filepath.Join(root, "usr", "sbin", "nft")
	s := &sessions{}
	s.set(office)
	m := &Manager{
		Dir: dir, Table: &nftables.Firewall{Run: &hosttool.Runner{Root: root}}, Timer: realTimer, Pins: s.read,
		DeadMan: &ChildDeadMan{Shell: "/bin/sh", Nft: nft, Dir: dir}, deadManGrace: deadManGraceForTest,
	}
	if err := m.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i, what := range strings.Split(steps, ",") {
		change := fmt.Sprintf("chg_%d", i)
		switch what {
		case "policy":
			// Its last step waits for a confirmation that never comes.
			go func() { _ = m.PutPolicy(step(change, true), policy) }()
			until(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.waiting != nil && m.waiting.announced })
		case "keep-rule":
			if err := m.Put(step(change, false), "kept", &resourcev1.FirewallRule{Port: 8443}); err != nil {
				t.Fatal(err)
			}
			if v := confirm(m, change); v != resource.AccessKept {
				t.Fatalf("keeping the rule: %v", v)
			}
		case "keep-policy":
			if err := m.PutPolicy(step(change, false), policy); err != nil {
				t.Fatal(err)
			}
			if v := confirm(m, change); v != resource.AccessKept {
				t.Fatalf("keeping the policy: %v", v)
			}
		}
	}
	fmt.Println("waiting")
	select {}
}

// Under systemd the revert is a transient timer, which outlives the agent's
// own unit. Each arming takes a unit name of its own, and the timers armed
// before are stopped only once the new one is there, so a systemd-run that is
// refused leaves the older one armed. A systemd too old for --collect is asked
// again without it. Disarming stops them all, and none armed is no error.
func TestTheSystemdRevertIsATransientTimerUnderAFreshName(t *testing.T) {
	root := t.TempDir()
	said := filepath.Join(root, "said")
	state := filepath.Join(root, "units")
	for name, script := range map[string]string{
		"usr/bin/systemd-run": `echo "systemd-run $*" >> ` + said + `
[ -e ` + root + `/refuse ] && { echo "Failed to start transient timer unit: Unit already exists." >&2; exit 1; }
if [ -e ` + root + `/old ]; then case "$*" in *--collect*) echo "systemd-run: unrecognized option '--collect'" >&2; exit 1 ;; esac; fi
for a in "$@"; do case "$a" in --unit=*) echo "${a#--unit=}.timer loaded active waiting x" >> ` + state + ` ;; esac; done
`,
		"usr/bin/systemctl": `echo "systemctl $*" >> ` + said + `
case "$1" in
list-units) cat ` + state + ` 2>/dev/null; exit 0 ;;
stop) shift; for u in "$@"; do grep -v "^$u " ` + state + ` > ` + state + `.new; mv ` + state + `.new ` + state + `; done ;;
esac
`,
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tick := time.Unix(1_800_000_000, 0)
	d := &systemdDeadMan{tools: &hosttool.Runner{Root: root}, nft: "/usr/sbin/nft", now: func() time.Time { tick = tick.Add(time.Second); return tick }}
	armed := func() string {
		b, _ := os.ReadFile(state)
		var names []string
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				names = append(names, f[0])
			}
		}
		return strings.Join(names, " ")
	}
	file := "/var/lib/dockwell/agent/access/deadman.nft"
	if err := d.Arm(t.Context(), file, 75*time.Second); err != nil {
		t.Fatal(err)
	}
	first := armed()
	if !strings.HasPrefix(first, "dockwell-access-revert-") || strings.Contains(first, " ") {
		t.Fatalf("armed %q", first)
	}
	if got := read1(t, said); !strings.Contains(got, "--quiet --collect --unit="+strings.TrimSuffix(first, ".timer")+" --on-active=75 --timer-property=AccuracySec=1s --description=Put Dockwell's firewall back as it was last confirmed /usr/sbin/nft -f "+file) {
		t.Errorf("it ran\n%s", got)
	}
	// Armed again: a new name, and the older one goes only after.
	if err := d.Arm(t.Context(), file, 75*time.Second); err != nil {
		t.Fatal(err)
	}
	second := armed()
	if second == first || strings.Contains(second, " ") {
		t.Errorf("after a second arming, armed %q (first was %q)", second, first)
	}
	// One systemd refuses leaves the older one armed, and says so.
	if err := os.WriteFile(filepath.Join(root, "refuse"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Arm(t.Context(), file, 75*time.Second); err == nil || !strings.Contains(err.Error(), "Unit already exists") {
		t.Errorf("an arming systemd refused: %v", err)
	}
	if got := armed(); got != second {
		t.Errorf("after a refused arming, armed %q, want the older one, %q", got, second)
	}
	_ = os.Remove(filepath.Join(root, "refuse"))
	// A systemd before 236 takes it without --collect.
	if err := os.WriteFile(filepath.Join(root, "old"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Arm(t.Context(), file, 75*time.Second); err != nil {
		t.Errorf("on a systemd without --collect: %v", err)
	}
	if got := armed(); got == second || strings.Contains(got, " ") {
		t.Errorf("armed %q", got)
	}
	// A timer that fired has started a service of its name, which goes too.
	fired, err := os.OpenFile(state, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fired.WriteString("dockwell-access-revert-old.service loaded activating start x\nssh.service loaded active running x\n")
	_ = fired.Close()
	if err := d.Disarm(t.Context()); err != nil || armed() != "ssh.service" {
		t.Errorf("disarming: %v, left %q, want nothing of Dockwell's and ssh.service untouched", err, armed())
	}
	if got := read1(t, said); !strings.Contains(got, "systemctl list-units --all --plain --no-legend --type=timer --type=service dockwell-access-revert-*") || !strings.Contains(got, "dockwell-access-revert-old.service") {
		t.Errorf("it ran\n%s", got)
	}
	if err := os.WriteFile(state, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Disarm(t.Context()); err != nil {
		t.Errorf("disarming with nothing armed: %v", err)
	}
	if NewDeadMan(&hosttool.Runner{Off: true}, root) != nil || NewDeadMan(nil, root) != nil {
		t.Error("a test with no tools arms a revert")
	}
}

// A kept change never has a stale revert behind it: what the revert would load
// once the change is kept is written before the confirmation is, and where it
// can't be, nothing is kept. The change goes on waiting under its timer, and
// the confirmation may come again.
func TestAConfirmationWhoseRevertFileCantBeWrittenKeepsNothing(t *testing.T) {
	m, tbl, n, _ := withDeadMan(t)
	if err := m.Put(step("chg_1", false), "http", http); err != nil {
		t.Fatal(err)
	}
	n.take()
	// Something in the way of the file it writes beside the one armed.
	block := m.deadManFile() + ".kept"
	if err := os.MkdirAll(filepath.Join(block, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessNotKept {
		t.Fatalf("a confirmation whose revert file can't be written: %v, want not kept", v)
	}
	m.mu.Lock()
	waiting, kept := m.waiting != nil && m.waiting.change == "chg_1", len(m.confirmed.Rules)
	m.mu.Unlock()
	if !waiting || kept != 0 || !n.armed || n.take() != "" {
		t.Errorf("after it: still waiting %t, %d rules confirmed, armed %t", waiting, kept, n.armed)
	}
	if got := what(read1(t, m.deadManFile())); got != "(remove the table)" {
		t.Errorf("the revert would load %s", got)
	}
	if exists(filepath.Join(m.Dir, "confirmed.json")) {
		t.Error("the change is on disk as confirmed")
	}
	// With the way clear, the same confirmation keeps it.
	if err := os.RemoveAll(block); err != nil {
		t.Fatal(err)
	}
	if v := confirm(m, "chg_1"); v != resource.AccessKept {
		t.Fatalf("the confirmation again: %v", v)
	}
	if got := n.take(); got != "disarm" || what(read1(t, m.deadManFile())) != "http" || tbl.rules() != "rule http" {
		t.Errorf("once kept: %s, the revert would load %s, table %s", got, what(read1(t, m.deadManFile())), tbl.rules())
	}
	if exists(block) {
		t.Error("the staged file is left behind")
	}
}
