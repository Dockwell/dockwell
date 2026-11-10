// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// flaky is the init system failing a few times to say how a unit is, as systemctl
// does while a systemd upgrade re-executes it.
type flaky struct {
	*units
	mu    sync.Mutex
	fails int
}

func (f *flaky) Active(ctx context.Context, name string) (bool, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return false, errors.New("Failed to connect to bus: Connection refused")
	}
	f.mu.Unlock()
	return f.units.Active(ctx, name)
}

// A foreign architecture's package, which dpkg names without its architecture,
// reads at its version once installed, as does one of no architecture of its own.
func TestAForeignArchitecturesPackageReadsInstalled(t *testing.T) {
	s := installStand(t)
	write(t, s.root, "installed", "libc6\tamd64\t2.39-0ubuntu8.4\tii \nlibc6\ti386\t2.39-0ubuntu8.4\tii \ntzdata\tall\t2024b-0ubuntu0.24.04\tii \n", 0o644)
	planned := []*dockwellv1.PackageUpdate{
		{Name: "libc6", Architecture: "amd64", Available: "2.39-0ubuntu8.4"},
		{Name: "libc6:i386", Architecture: "i386", Available: "2.39-0ubuntu8.4"},
		{Name: "tzdata", Architecture: "all", Available: "2024b-0ubuntu0.24.04"},
	}
	left, err := s.installer(newUnits()).left(t.Context(), planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("left %v, want every package installed", left)
	}
	// A foreign one at the old version is left, though the native one isn't.
	write(t, s.root, "installed", "libc6\tamd64\t2.39-0ubuntu8.4\tii \nlibc6\ti386\t2.39-0ubuntu8.3\tii \n", 0o644)
	if left, _ := s.installer(newUnits()).left(t.Context(), planned[:2]); len(left) != 1 || left[0].GetName() != "libc6:i386" {
		t.Errorf("left %v, want libc6:i386 alone", left)
	}
}

// systemctl failing for a moment while the install runs doesn't fail it.
func TestAnInstallOutlastsSystemctlFailingForAMoment(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	u := &flaky{units: newUnits()}
	in := s.installer(newUnits())
	in.Units = u
	write(t, s.root, "hold", "", 0o644)
	done := make(chan error, 1)
	go func() { done <- in.Install(t.Context(), "chg_1", planned, func(string) {}) }()
	for u.starts() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	u.mu.Lock()
	u.fails = 3
	u.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	_ = os.Remove(filepath.Join(s.root, "hold"))
	if err := <-done; err != nil {
		t.Fatalf("an install while systemctl failed three times = %v", err)
	}
}

// Past half an hour, following says the install still runs; at its call's
// deadline, it says where it goes on and how to see it, and leaves it running,
// its output kept for the next start to follow.
func TestALongInstallSaysItStillRunsAndWhereItGoesOn(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	u := newUnits()
	in := s.installer(u)
	var mu sync.Mutex
	at := time.Now()
	in.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(20 * time.Minute) // each look is 20 minutes later
		return at
	}
	write(t, s.root, "hold", "", 0o644)
	var said []string
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := in.Install(ctx, "chg_1", planned, func(l string) {
		mu.Lock()
		said = append(said, l)
		mu.Unlock()
	})
	if err == nil || !strings.Contains(err.Error(), "goes on in its own unit") {
		t.Fatalf("an install past its call's deadline = %v", err)
	}
	mu.Lock()
	still := slices.ContainsFunc(said, func(l string) bool { return strings.HasPrefix(l, "Still installing after 30 min") })
	mu.Unlock()
	if !still {
		t.Errorf("said %v, want that it still runs", said)
	}
	if active, _ := u.Active(t.Context(), "dockwell-install-chg_1"); !active {
		t.Error("the install stopped with its call")
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "chg_1.log")); err != nil {
		t.Errorf("the install's output went while it runs: %v", err)
	}
	_ = os.Remove(filepath.Join(s.root, "hold"))
}

// The agent installs only what reads as a package's name and version, whatever
// it's sent.
func TestAnInstallRefusesWhatIsntAPackage(t *testing.T) {
	s := installStand(t)
	for _, p := range []*dockwellv1.PackageUpdate{
		{Name: "openssl; reboot", Available: "3.0.13-0ubuntu3.5"},
		{Name: "-o", Available: "3.0.13-0ubuntu3.5"},
		{Name: "openssl", Available: "3.0 $(id)"},
	} {
		err := s.installer(newUnits()).Install(t.Context(), "chg_1", []*dockwellv1.PackageUpdate{p}, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "isn't a package's name and version") {
			t.Errorf("installing %q at %q = %v", p.GetName(), p.GetAvailable(), err)
		}
	}
	if s.called("apt-get") != 0 {
		t.Errorf("apt ran: %v", s.calls())
	}
}

// What an earlier change's install left, as one that failed while its install
// went on to the end, goes at the next install: its output, and its unit.
func TestAnInstallLetsGoWhatAnEndedOneLeft(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	in := s.installer(newUnits())
	write(t, in.Dir, "chg_old.log", "Setting up openssl\n", 0o600)
	if err := in.Install(t.Context(), "chg_1", planned, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "chg_old.log")); !os.IsNotExist(err) {
		t.Errorf("an ended install's output is still there: %v", err)
	}
}

// A server without systemd gets no plan, rather than one that downloads and then
// can't start.
func TestAServerWithoutSystemdGetsNoInstallPlan(t *testing.T) {
	s := installStand(t)
	write(t, s.root, "sim", simulated, 0o644)
	_ = os.RemoveAll(filepath.Join(s.root, "run", "systemd"))
	if _, err := New(Options{Root: s.root}).PlanInstall(t.Context(), false); !errors.Is(err, ErrNoUnits) {
		t.Errorf("a plan without systemd = %v", err)
	}
}

// apt waiting on another's lock past its 120 s gives up before changing anything,
// and says so.
func TestAHeldLockFailsTheDownloadAndChangesNothing(t *testing.T) {
	s := installStand(t)
	planned := planOf(t, s)
	s.tool(aptGet, `case "$*" in
*"--simulate"*) cat "$ROOT/sim" ;;
*"--download-only"*) echo "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 1234 (unattended-upgr)" >&2; exit 100 ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac`)
	u := newUnits()
	err := s.installer(u).Install(t.Context(), "chg_1", planned, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "unattended-upgr") {
		t.Errorf("an install while another holds the lock = %v", err)
	}
	if u.starts() != 0 || s.versions()["openssl"] != "3.0.13-0ubuntu3.4" {
		t.Errorf("started %d installs, installed %v", u.starts(), s.versions())
	}
}

// live-restore keeps containers across Docker's patch releases only: a new minor
// version stops them, and containerd and runc restart Docker too.
func TestDockerKeepsContainersAcrossAPatchReleaseAlone(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{"5:27.4.0-1~ubuntu.24.04~noble", "5:27.4.1-1~ubuntu.24.04~noble"},
	} {
		if !samePatchLine(tc.from, tc.to) {
			t.Errorf("%s to %s reads as more than a patch release", tc.from, tc.to)
		}
	}
	for _, tc := range []struct{ from, to string }{
		{"5:27.3.1-1~ubuntu.24.04~noble", "5:27.4.0-1~ubuntu.24.04~noble"},
		{"5:26.1.4-1~ubuntu.24.04~noble", "5:27.0.0-1~ubuntu.24.04~noble"},
		{"", "5:27.4.0-1~ubuntu.24.04~noble"},
	} {
		if samePatchLine(tc.from, tc.to) {
			t.Errorf("%q to %q reads as a patch release", tc.from, tc.to)
		}
	}
	for _, name := range []string{"containerd.io", "runc", "docker-ce"} {
		if effectOf(dockwellv1.PackageManager_PACKAGE_MANAGER_APT, name) != dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART {
			t.Errorf("%s doesn't read as restarting Docker", name)
		}
	}
}
