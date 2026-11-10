// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// units is the init system: a unit runs on its own, whatever happens to the call
// that started it or to the agent, as systemd's transient units do.
type units struct {
	mu      sync.Mutex
	running map[string]bool
	codes   map[string]int
	started []string
	envs    map[string][]string
}

func newUnits() *units {
	return &units{running: map[string]bool{}, codes: map[string]int{}, envs: map[string][]string{}}
}

func (u *units) Start(_ context.Context, name string, argv, env []string, log string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.running[name] {
		return errors.New("unit " + name + " already runs")
	}
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	// Not the caller's context: a unit doesn't end with whoever started it.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return err
	}
	u.running[name], u.started, u.envs[name] = true, append(u.started, name), env
	go func() {
		err := cmd.Wait()
		_ = f.Close()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		u.mu.Lock()
		u.running[name], u.codes[name] = false, code
		u.mu.Unlock()
	}()
	return nil
}

func (u *units) Active(context.Context, string) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, r := range u.running {
		if r {
			return true, nil
		}
	}
	return false, nil
}

func (u *units) Ended(_ context.Context, name string) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.codes[name], nil
}

func (u *units) starts() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.started)
}

// installStand is an Ubuntu server whose apt simulates what $ROOT/sim says,
// installs what it's told, slowly while $ROOT/hold is there, and whose dpkg
// knows what $ROOT/installed lists.
func installStand(t *testing.T) *stand {
	t.Helper()
	s := ubuntuStand(t, time.Now().Add(-time.Hour))
	s.tool(aptGet, `case "$*" in
*" update") exit 0 ;;
*"--simulate"*" upgrade") cat "$FIX/apt-simulate-ubuntu.txt" ;;
*"--simulate"*) cat "$ROOT/sim" ;;
*"--download-only"*) echo "Fetched 12 MB" ;;
*"--no-download"*)
  # What it's named, then what that pulls in: the simulation's new packages.
  { for a in "$@"; do case "$a" in *=*) echo "$a" ;; esac; done
    sed -n 's/^Inst \([^ []*\) (\([^ ]*\) .*/\1=\2/p' "$ROOT/sim"; } | while read -r a; do
      n="${a%%=*}"; v="${a#*=}"
      echo "Unpacking $n ($v)"
      while [ -e "$ROOT/hold" ]; do sleep 0.05; done
      grep -v "^$n	" "$ROOT/installed" > "$ROOT/installed.new"; printf '%s\t%s\tii \n' "$n" "$v" >> "$ROOT/installed.new"; mv "$ROOT/installed.new" "$ROOT/installed"
      echo "Setting up $n ($v)"
  done ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac`)
	s.tool(dpkgQuery, `case "$*" in
*"Status-Abbrev"*) cat "$ROOT/installed" ;;
*) cat "$FIX/dpkg-query.txt"; exit 1 ;;
esac`)
	s.tool(dpkg, `case "$*" in
"--audit") cat "$ROOT/audit" 2>/dev/null ;;
*"--configure -a") sed 's/	iU $/	ii /' "$ROOT/installed" > "$ROOT/installed.new"; mv "$ROOT/installed.new" "$ROOT/installed"; rm -f "$ROOT/audit"; echo "Setting up what was left" ;;
esac`)
	write(t, s.root, "installed", "libssl3t64\t3.0.13-0ubuntu3.4\tii \nopenssl\t3.0.13-0ubuntu3.4\tii \n", 0o644)
	return s
}

const simulated = `Inst libssl3t64 [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst openssl [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst libssl-extra (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-security [amd64])
`

func (s *stand) installer(u *units) *Installer {
	return &Installer{Reader: New(Options{Root: s.root}), Units: u, Dir: filepath.Join(s.root, "agent", "install")}
}

func (s *stand) versions() map[string]string {
	b, _ := os.ReadFile(filepath.Join(s.root, "installed"))
	out := map[string]string{}
	for line := range strings.Lines(string(b)) {
		parts := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(parts) == 3 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func planOf(t *testing.T, s *stand) []*dockwellv1.PackageUpdate {
	t.Helper()
	write(t, s.root, "sim", simulated, 0o644)
	p, err := New(Options{Root: s.root}).PlanInstall(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	return p.Packages
}

// The plan is the exact command simulated, versions pinned: the security fixes,
// and what they pull in, which is listed too.
func TestAnInstallsPlanIsTheExactCommandSimulated(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	names := make([]string, 0, len(planned))
	for _, p := range planned {
		names = append(names, p.GetName()+"="+p.GetAvailable())
	}
	if want := []string{"libssl3t64=3.0.13-0ubuntu3.5", "openssl=3.0.13-0ubuntu3.5", "libssl-extra=3.0.13-0ubuntu3.5"}; !slices.Equal(names, want) {
		t.Errorf("planned %v, want %v", names, want)
	}
	if p := byName(planned, "libssl-extra"); p.GetInstalled() != "" {
		t.Errorf("a package pulled in reads %v, want nothing installed", p)
	}
	sim := ""
	for _, c := range s.calls() {
		if strings.Contains(c, "--simulate_--only-upgrade_--no-remove_install") {
			sim = c
		}
	}
	if !strings.Contains(sim, "libssl3t64=3.0.13-0ubuntu3.5") || !strings.Contains(sim, "openssl=3.0.13-0ubuntu3.5") || strings.Contains(sim, "systemd=") {
		t.Errorf("simulated %q, want the security fixes pinned and nothing else", sim)
	}
}

// At its first start, an install whose updates moved since the plan is refused,
// and nothing is downloaded or started.
func TestAnInstallWhoseUpdatesMovedIsRefused(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	write(t, s.root, "sim", strings.ReplaceAll(simulated, "3.0.13-0ubuntu3.5", "3.0.13-0ubuntu3.6"), 0o644)
	u := newUnits()
	err := s.installer(u).Install(t.Context(), "chg_1", planned, func(string) {})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("an install whose updates moved = %v", err)
	}
	if s.called("apt-get_-q_-y_--download-only") != 0 || u.starts() != 0 {
		t.Errorf("a refused install downloaded or started: %v", s.calls())
	}
}

// The download is first, and safe to stop; the install runs in a unit of its own,
// with needrestart only listing; its lines are said as they come; once every
// package is at its version, its output is gone.
func TestAnInstallDownloadsThenRunsInItsOwnUnit(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	u := newUnits()
	var said []string
	in := s.installer(u)
	if err := in.Install(t.Context(), "chg_1", planned, func(l string) { said = append(said, l) }); err != nil {
		t.Fatal(err)
	}
	calls := s.calls()
	download := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "apt-get_-q_-y_--download-only") })
	install := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "--no-download") })
	if download < 0 || install < download {
		t.Errorf("calls %v, want the download before the install", calls)
	}
	if !slices.Contains(u.envs["dockwell-install-chg_1"], "NEEDRESTART_MODE=l") {
		t.Errorf("the install's environment %v, want needrestart to list only", u.envs["dockwell-install-chg_1"])
	}
	if v := s.versions(); v["libssl3t64"] != "3.0.13-0ubuntu3.5" || v["libssl-extra"] != "3.0.13-0ubuntu3.5" {
		t.Errorf("installed %v", v)
	}
	if !slices.Contains(said, "Setting up openssl (3.0.13-0ubuntu3.5)") || !slices.Contains(said, "Downloading the packages") {
		t.Errorf("said %v", said)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "chg_1.log")); !os.IsNotExist(err) {
		t.Errorf("the install's output is still there: %v", err)
	}
}

// The stream drops while the install runs: following stops, the install doesn't,
// and the change's next start follows it to its end, starting nothing again.
func TestADroppedStreamLeavesTheInstallRunning(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	write(t, s.root, "hold", "", 0o644)
	u := newUnits()
	ctx, drop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.installer(u).Install(ctx, "chg_1", planned, func(string) {}) }()
	for u.starts() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	drop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a dropped stream = %v", err)
	}
	if active, _ := u.Active(t.Context(), "dockwell-install-chg_1"); !active {
		t.Fatal("the install stopped with the stream")
	}
	_ = os.Remove(filepath.Join(s.root, "hold"))
	if err := s.installer(u).Install(t.Context(), "chg_1", planned, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if u.starts() != 1 || s.called("apt-get_-q_-y_--download-only") != 1 {
		t.Errorf("the resume started %d installs and %d downloads, want 1 and 1", u.starts(), s.called("apt-get_-q_-y_--download-only"))
	}
	if v := s.versions(); v["openssl"] != "3.0.13-0ubuntu3.5" {
		t.Errorf("installed %v", v)
	}
}

// The agent restarts while the install runs: the new one knows nothing but the
// change, finds the unit by its name, and follows it.
func TestAnAgentRestartFollowsTheInstallItStarted(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	write(t, s.root, "hold", "", 0o644)
	u := newUnits()
	ctx, stop := context.WithCancel(t.Context())
	go func() { _ = s.installer(u).Install(ctx, "chg_1", planned, func(string) {}) }()
	for u.starts() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	stop() // the agent goes, and its following with it
	_ = os.Remove(filepath.Join(s.root, "hold"))
	var said []string
	if err := s.installer(u).Install(t.Context(), "chg_1", planned, func(l string) { said = append(said, l) }); err != nil {
		t.Fatal(err)
	}
	if u.starts() != 1 || !slices.Contains(said, "Following the install that's running") && !slices.ContainsFunc(said, func(l string) bool { return strings.HasPrefix(l, "Setting up") }) {
		t.Errorf("the restarted agent started %d installs, and said %v", u.starts(), said)
	}
}

// A run that stopped partway, as at a reboot, left one package done and one
// unpacked: the resume counts the first as done, has dpkg finish the second, and
// installs only what's left, though a newer version of the rest now waits too.
func TestAResumeCountsWhatsDoneAndFinishesWhatDpkgLeft(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	in := s.installer(newUnits())
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, in.Dir, "chg_1.log", "Unpacking libssl3t64\n", 0o600)
	write(t, s.root, "installed", "libssl3t64\t3.0.13-0ubuntu3.5\tii \nopenssl\t3.0.13-0ubuntu3.5\tiU \n", 0o644)
	write(t, s.root, "audit", "openssl is unpacked, but not configured\n", 0o644)
	// libssl-extra is left, at its planned version still.
	write(t, s.root, "sim", "Inst libssl-extra (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-security [amd64])\nInst systemd [255.4-1] (255.4-2 Ubuntu:24.04/noble-updates [amd64])\n", 0o644)
	u := in.Units.(*units)
	if err := in.Install(t.Context(), "chg_1", planned, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if s.called("dpkg_--force-confold_--configure_-a") != 1 {
		t.Errorf("calls %v, want dpkg to finish what was left", s.calls())
	}
	if u.starts() != 1 || s.versions()["libssl-extra"] != "3.0.13-0ubuntu3.5" {
		t.Errorf("started %d installs, installed %v", u.starts(), s.versions())
	}
}

// An install runs as a transient systemd unit named after its change, its output
// appended to its file, with its own environment; its state and status come from
// systemctl. Without systemd, it isn't started at all.
func TestAnInstallRunsAsASystemdUnit(t *testing.T) {
	s := newStand(t, ubuntu)
	s.tool(systemdRun, `exit 0`)
	s.tool(systemctl, `case "$*" in
*"ActiveState"*) echo active ;;
*"ExecMainStatus"*) echo 100 ;;
esac`)
	units := Systemd{Reader: New(Options{Root: s.root})}
	if err := units.Start(t.Context(), "dockwell-install-chg_1", []string{"/usr/bin/apt-get", "install"}, []string{"NEEDRESTART_MODE=l"}, "/x.log"); !errors.Is(err, ErrNoUnits) {
		t.Fatalf("a start without systemd = %v", err)
	}
	write(t, s.root, "run/systemd/system/.keep", "", 0o644)
	if err := units.Start(t.Context(), "dockwell-install-chg_1", []string{"/usr/bin/apt-get", "install"}, []string{"NEEDRESTART_MODE=l"}, "/x.log"); err != nil {
		t.Fatal(err)
	}
	start := ""
	for _, c := range s.calls() {
		if strings.HasPrefix(c, "systemd-run") {
			start = c
		}
	}
	for _, want := range []string{"--unit=dockwell-install-chg_1", "--property=StandardOutput=append:/x.log", "--setenv=NEEDRESTART_MODE=l", "--_/usr/bin/apt-get_install"} {
		if !strings.Contains(start, want) {
			t.Errorf("systemd-run was called as %q, without %q", start, want)
		}
	}
	if active, err := units.Active(t.Context(), "dockwell-install-chg_1"); err != nil || !active {
		t.Errorf("an active unit reads %v, %v", active, err)
	}
	if code, err := units.Ended(t.Context(), "dockwell-install-chg_1"); err != nil || code != 100 || s.called("systemctl_reset-failed") != 1 {
		t.Errorf("an ended unit reads %d, %v; calls %v", code, err, s.calls())
	}
}
