// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// askFor is an upgrade to 0.9.4 for change chg_1, by script, named by its SHA-256.
func askFor(script string) resource.AgentUpgradeAsked {
	sum := sha256.Sum256([]byte(script))
	return resource.AgentUpgradeAsked{Change: "chg_1", Version: "0.9.4", Script: []byte(script), ScriptSHA256: hex.EncodeToString(sum[:])}
}

// newUpgrader is an upgrader on a stand whose /bin/sh is the machine's, with no
// systemd, so the script runs in a session of its own.
func newUpgrader(t *testing.T) (*Upgrader, *stand) {
	t.Helper()
	s := newStand(t, ubuntu)
	if err := os.MkdirAll(filepath.Join(s.root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(s.root, "bin", "sh")); err != nil {
		t.Fatal(err)
	}
	return &Upgrader{Reader: New(Options{Root: s.root}), Dir: filepath.Join(t.TempDir(), "agent", "upgrade"), Version: "v0.9.3"}, s
}

// waitFor waits for path to be there.
func waitFor(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s wasn't written", path)
		}
	}
}

// The script runs once for its change, from a folder only root reads, with the
// version and the mirror; it's removed as it ends; asked again while it runs,
// nothing more starts; and once it has ended with the agent at its old version,
// asking again fails with what it said.
func TestAnUpgradeRunsTheScriptItWasSentOnce(t *testing.T) {
	u, _ := newUpgrader(t)
	release := filepath.Join(t.TempDir(), "release")
	script := `#!/bin/sh
echo "started $*" >> "` + filepath.Join(filepath.Dir(release), "runs") + `"
while [ ! -e "` + release + `" ]; do sleep 0.05; done
echo "Error: the release's signature doesn't check out"
exit 3
`
	ask := askFor(script)
	ask.Mirror = "https://mirror.example.com/dockwell/releases"
	if err := u.Upgrade(t.Context(), ask); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(u.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the upgrade's folder is %v, %v; want 0700", info.Mode(), err)
	}
	runs := filepath.Join(filepath.Dir(release), "runs")
	waitFor(t, runs)
	// Asked again while it runs, nothing more starts.
	if err := u.Upgrade(t.Context(), ask); err != nil {
		t.Fatalf("asking again while it runs = %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, u.file("status"))
	b, _ := os.ReadFile(runs)
	if got := strings.TrimSpace(string(b)); got != "started --version 0.9.4 --mirror https://mirror.example.com/dockwell/releases" {
		t.Errorf("the script ran as %q", got)
	}
	if _, err := os.Stat(u.file("install.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the script is left behind: %v", err)
	}
	err = u.Upgrade(t.Context(), ask)
	if err == nil || !strings.Contains(err.Error(), "status 3") || !strings.Contains(err.Error(), "still runs 0.9.3") || !strings.Contains(err.Error(), "signature doesn't check out") {
		t.Errorf("asking after it failed = %v", err)
	}
	if _, ok := u.readMark(); ok {
		t.Error("the mark of a failed upgrade is kept, so it can't be planned again")
	}
}

// A script that isn't the one the plan named, a version that isn't a release's,
// and a test, run nothing.
func TestAnUpgradeRefusesWhatItCantTrust(t *testing.T) {
	u, _ := newUpgrader(t)
	ask := askFor("#!/bin/sh\ntouch /should-not-run\n")
	ask.ScriptSHA256 = strings.Repeat("0", 64)
	if err := u.Upgrade(t.Context(), ask); err == nil || !strings.Contains(err.Error(), "isn't the one its plan named") {
		t.Errorf("a script that isn't the plan's = %v", err)
	}
	ask = askFor("#!/bin/sh\n")
	ask.Version = "0.9.4; rm -rf /"
	if err := u.Upgrade(t.Context(), ask); err == nil || !strings.Contains(err.Error(), "isn't a release's version") {
		t.Errorf("a version that isn't a release's = %v", err)
	}
	u.Unavailable = "This is a test, so Dockwell upgrades no agent here."
	if err := u.Upgrade(t.Context(), askFor("#!/bin/sh\n")); err == nil || !strings.Contains(plainOf(err), "This is a test") {
		t.Errorf("an upgrade under a test = %v", err)
	}
	if _, err := os.Stat(u.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an upgrade that was refused made its folder: %v", err)
	}
}

// Under systemd, the script runs as a transient unit of its own, outside the
// agent's, with its output appended to its file.
func TestAnUpgradeRunsAsASystemdUnit(t *testing.T) {
	u, s := newUpgrader(t)
	write(t, s.root, "run/systemd/system/.keep", "", 0o644)
	s.tool(systemdRun, `exit 0`)
	s.tool(systemctl, `exit 0`)
	if err := u.Upgrade(t.Context(), askFor("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	start := ""
	for _, c := range s.calls() {
		if strings.HasPrefix(c, "systemd-run") {
			start = c
		}
	}
	for _, want := range []string{"--unit=dockwell-upgrade", "--property=StandardOutput=append:" + u.file("output.log"), "--_/bin/sh_-c"} {
		if !strings.Contains(start, want) {
			t.Errorf("systemd-run was called as %q, without %q", start, want)
		}
	}
	b, err := os.ReadFile(u.file("install.sh"))
	if err != nil || string(b) != "#!/bin/sh\n" {
		t.Errorf("the script kept for the unit reads %q, %v", b, err)
	}
	// Once the agent runs the version, what the upgrade left goes.
	u.Forget("chg_1")
	if _, err := os.Stat(u.file("mark.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the mark is kept after the upgrade: %v", err)
	}
}

// plainOf is what err says for people.
func plainOf(err error) string {
	plain, _ := resource.Explain(err)
	return plain
}
