// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// Upgrading the agent for a change (ADR 0094): the change's step carries the
// control plane's own install script, which the agent checks against the SHA-256
// its plan named, writes to a folder of its own that only root reads, and runs
// outside its own service, as the script restarts that: as systemd's transient
// unit, or in a session of its own without systemd. A mark there names the change,
// so the script runs at most once for it. The script checks the release with its
// pinned cosign, installs it and restarts the agent, which connects at the new
// version; a script that ends with the agent at its old version leaves its status,
// which the next look at the step fails with, with what the script said.

// upgradeUnit names the transient unit an upgrade runs in.
const upgradeUnit = "dockwell-upgrade"

// upgradeStale is how long an upgrade's script may run without leaving its status
// before another may start: past the control plane's limit for the step, as a script
// killed before it ended never leaves one.
const upgradeStale = 30 * time.Minute

// releaseVersion is what a release's version looks like, without its "v": the
// script takes it as an argument, so nothing else passes. It's made on first use.
var releaseVersion = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
})

// upgradeWrapper runs the script with its arguments, then removes it and leaves its
// status: $1 is the script, $2 the version, $3 the mirror or "", $4 the status file,
// $5 cosign's mirror or "".
const upgradeWrapper = `sh "$1" --version "$2" ${3:+--mirror "$3"} ${5:+--cosign-mirror "$5"}
code=$?
rm -f "$1"
echo "$code" > "$4.new" && mv "$4.new" "$4"
exit "$code"`

// Upgrader upgrades the agent by the install script a change carries.
type Upgrader struct {
	Reader *Reader
	// Dir is the folder it keeps the script, its output, its status and its mark
	// in while an upgrade runs, under the agent's data folder.
	Dir string
	// Version is the agent's own, as its build says, without a "v".
	Version string
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
	// Unavailable, when set, says why it upgrades nothing here, as under a test:
	// the script it's sent would install a release on the machine.
	Unavailable string
}

// upgradeMark is what the agent keeps of an upgrade it was asked for.
type upgradeMark struct {
	Change  string    `json:"change"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Started time.Time `json:"started"`
	// Boot is the boot it started on: no script outlives one.
	Boot string `json:"boot,omitempty"`
}

func (u *Upgrader) file(name string) string { return filepath.Join(u.Dir, name) }

func (u *Upgrader) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// Own is the agent's own version.
func (u *Upgrader) Own() string { return strings.TrimPrefix(u.Version, "v") }

func (u *Upgrader) readMark() (upgradeMark, bool) {
	b, err := os.ReadFile(u.file("mark.json"))
	if err != nil {
		return upgradeMark{}, false
	}
	var m upgradeMark
	if json.Unmarshal(b, &m) != nil || m.Change == "" {
		return upgradeMark{}, false
	}
	return m, true
}

// ended reads the status the script left, and whether it left one.
func (u *Upgrader) ended() (int, bool) {
	b, err := os.ReadFile(u.file("status"))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1, true
	}
	return code, true
}

// gone reports an upgrade's mark whose script can't be running: it started on a boot
// before this one, at a time to come, as a clock set back says, or long ago.
func (u *Upgrader) gone(m upgradeMark) bool {
	now := u.now()
	return m.Boot != "" && m.Boot != u.Reader.BootID() || m.Started.After(now) || now.Sub(m.Started) >= upgradeStale
}

// Forget lets go of what the change's upgrade left, once the agent runs the
// version it asked: its mark, its status and its output.
func (u *Upgrader) Forget(change string) {
	if m, ok := u.readMark(); !ok || m.Change != change {
		return
	}
	for _, f := range []string{"mark.json", "status", "output.log", "install.sh"} {
		_ = os.Remove(u.file(f))
	}
}

// Upgrade checks the script ask carries against its SHA-256, and starts it for
// the change, at most once: asked again while it runs, it does nothing, and once
// it has ended with the agent still at its own version, it fails with what the
// script said and lets the mark go, so the change can be planned again.
func (u *Upgrader) Upgrade(ctx context.Context, ask resource.AgentUpgradeAsked) error {
	r := u.Reader
	if u.Unavailable != "" {
		return resource.Plainly(u.Unavailable, errors.New("upgrades no agent here"))
	}
	if !releaseVersion().MatchString(ask.Version) {
		return fmt.Errorf("%q isn't a release's version, so the agent wasn't upgraded to it", ask.Version)
	}
	sum := sha256.Sum256(ask.Script)
	if len(ask.Script) == 0 || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(ask.ScriptSHA256)) != 1 {
		return errors.New("the install script it was sent isn't the one its plan named by SHA-256, so it didn't run. Plan the upgrade again")
	}
	for _, m := range []string{ask.Mirror, ask.CosignMirror} {
		if strings.ContainsAny(m, " \t\r\n") {
			return fmt.Errorf("%q isn't a mirror's URL, so the agent wasn't upgraded", m)
		}
	}
	if m, ok := u.readMark(); ok && m.Change == ask.Change {
		code, ended := u.ended()
		if !ended {
			return nil
		}
		out, _ := os.ReadFile(u.file("output.log"))
		u.Forget(ask.Change)
		said := lastLines(string(out))
		if said == "" {
			said = "it said nothing"
		}
		return fmt.Errorf("the install script ended with status %d, and the agent still runs %s: %s", code, u.Own(), said)
	}
	// One upgrade at a time: another change's script that hasn't left its status
	// still runs, as nothing else keeps two from running at once without systemd's
	// unit. One that started long ago and never left one is taken as gone.
	if m, ok := u.readMark(); ok && m.Change != ask.Change {
		if _, ended := u.ended(); !ended && !u.gone(m) {
			return fmt.Errorf("the upgrade to %s that %s asked for still runs here, so this one didn't start. Try again once it ends", m.To, m.Change)
		}
	}
	if err := u.prepare(ask.Script); err != nil {
		return fmt.Errorf("can't keep the install script where only root reads it, so it didn't run: %w", err)
	}
	b, err := json.Marshal(upgradeMark{Change: ask.Change, From: u.Own(), To: ask.Version, Started: u.now(), Boot: r.BootID()})
	if err != nil {
		return err
	}
	if err := writeSynced(u.file("mark.json"), b); err != nil {
		return fmt.Errorf("can't keep a mark of the upgrade, so it didn't start: %w", err)
	}
	argv := []string{"/bin/sh", "-c", upgradeWrapper, "sh", u.file("install.sh"), ask.Version, ask.Mirror, u.file("status"), ask.CosignMirror}
	if err := u.start(ctx, argv); err != nil {
		u.Forget(ask.Change)
		return fmt.Errorf("can't start the install script: %w", err)
	}
	r.logger.Info("upgrading the agent", "change", ask.Change, "from", u.Own(), "to", ask.Version)
	return nil
}

// prepare makes the upgrade's folder, root's alone, clears what an earlier upgrade
// left there, and writes the script into it.
func (u *Upgrader) prepare(script []byte) error {
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return err
	}
	// A folder made before, by another hand, is made root's alone again.
	if err := os.Chmod(u.Dir, 0o700); err != nil { //nolint:gosec // a folder, which its owner needs to open
		return err
	}
	for _, f := range []string{"mark.json", "status", "status.new", "output.log", "install.sh"} {
		if err := os.Remove(u.file(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return writeSynced(u.file("install.sh"), script)
}

// start runs argv outside the agent's service, its output to the upgrade's log: as
// systemd's transient unit, collected as it ends, or in a session of its own.
func (u *Upgrader) start(ctx context.Context, argv []string) error {
	r := u.Reader
	if (Systemd{Reader: r}).Has() {
		_, _ = r.run(ctx, "Letting an earlier upgrade's unit go", systemctl, ReadTimeout, "reset-failed", "--", upgradeUnit)
		return Systemd{Reader: r}.StartAs(ctx, upgradeUnit, "Dockwell upgrades its agent", argv, nil, u.file("output.log"))
	}
	log, err := os.OpenFile(u.file("output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(r.path(argv[0]), argv[1:]...) //nolint:gosec,noctx // the upgrade's own wrapper, which outlives the call and the agent
	cmd.Env, cmd.Dir = env, "/"
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Nothing waits for it here: the agent may be gone by the time it ends, and
	// its status says how it went.
	go func() { _ = cmd.Wait() }()
	return nil
}
