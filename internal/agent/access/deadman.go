// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/nftables"
)

// A change to the firewall waits for its confirmation under the agent's own
// timer, and an agent that stops inside it puts back what was confirmed as it
// starts again. Neither helps where the agent stops and can't start: a bad
// upgrade, a record it can't read, a full disk, while the table it loaded cuts
// the person off. So before a change's table loads, the agent arms a revert
// that doesn't need the agent: something outside its process that, a little
// after the timer, loads a file with nft. The file is always the table as last
// confirmed, or the script that removes Dockwell's table when nothing was:
// never anything else, so it's safe whenever it fires, late included. The
// agent disarms it once the change is kept, and once its own revert has loaded
// (ADR 0052).

// DeadMan is a revert that fires without the agent.
type DeadMan interface {
	// Arm has nft load file after wait, whatever becomes of the agent,
	// in place of anything armed before.
	Arm(ctx context.Context, file string, wait time.Duration) error
	// Disarm stops what was armed. It's no error when nothing was.
	Disarm(ctx context.Context) error
}

// deadManGrace is how long after the agent's own timer the revert outside it
// fires, so the agent's comes first where the agent is there.
const deadManGrace = 15 * time.Second

// deadManUnit is the transient unit systemd runs the revert as.
const deadManUnit = "dockwell-access-revert"

var (
	systemdRun = hosttool.Tool{Name: "systemd-run", Paths: []string{"/usr/bin/systemd-run", "/bin/systemd-run"}}
	shell      = hosttool.Tool{Name: "sh", Paths: []string{"/bin/sh", "/usr/bin/sh"}}
)

// NewDeadMan is the revert outside the agent this server can run: a transient
// systemd timer where systemd runs the server's services, as a child of the
// agent's dies with its unit there; elsewhere, as on OpenRC, a child of its
// own session that outlives the agent. dir holds the child's token. Nil where
// the tools are off, as in a test, or the server has neither nft nor a shell.
func NewDeadMan(tools *hosttool.Runner, dir string) DeadMan {
	if tools == nil || tools.Off {
		return nil
	}
	nft, ok := tools.Find(nftables.Tool)
	if !ok {
		return nil
	}
	if _, ok := tools.Find(systemdRun); ok && tools.Root == "" {
		if info, err := os.Stat("/run/systemd/system"); err == nil && info.IsDir() {
			return &systemdDeadMan{tools: tools, nft: nft}
		}
	}
	sh, ok := tools.Find(shell)
	if !ok {
		return nil
	}
	return &ChildDeadMan{Shell: sh, Nft: nft, Dir: dir}
}

// systemdDeadMan arms a transient timer, which systemd keeps whatever becomes
// of the agent's own unit. Each arming takes a unit name of its own, and the
// timers armed before are stopped only once the new one is there: under one
// name, a second systemd-run that systemd refused, as while the first unit was
// still unloading, would leave nothing armed.
type systemdDeadMan struct {
	tools *hosttool.Runner
	nft   string
	// now names each unit, for a test; nil is the clock.
	now func() time.Time
}

func (d *systemdDeadMan) Arm(ctx context.Context, file string, wait time.Duration) error {
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	unit := deadManUnit + "-" + strconv.FormatInt(now().UnixNano(), 36)
	secs := strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10)
	args := []string{
		"--quiet", "--collect", "--unit=" + unit, "--on-active=" + secs, "--timer-property=AccuracySec=1s",
		"--description=Put Dockwell's firewall back as it was last confirmed", d.nft, "-f", file,
	}
	res, err := d.tools.Run(ctx, systemdRun, 10*time.Second, nil, args...)
	if err == nil && res.Code != 0 && strings.Contains(res.Errs, "unrecognized option") {
		// systemd before 236 has no --collect: without it, a unit that failed stays
		// listed until it's reset, which changes nothing here, as no name is used twice.
		res, err = d.tools.Run(ctx, systemdRun, 10*time.Second, nil, append(args[:1:1], args[2:]...)...)
	}
	if err == nil && res.Code != 0 {
		err = hosttool.Fail("systemd-run", res)
	}
	if err != nil {
		return fmt.Errorf("can't arm the firewall's revert with systemd-run: %w. Check it works on this server, with systemd-run --on-active=1 true", err)
	}
	// The new one is there: those armed before go. If they can't be stopped, the
	// arming is refused, and the caller loads nothing. The new timer then stays
	// armed with them: each fires at its own time and loads the same file, which
	// is the firewall as last confirmed, so that changes nothing.
	return d.stop(ctx, unit)
}

// Disarm stops every timer armed, and a revert that has fired and still runs.
func (d *systemdDeadMan) Disarm(ctx context.Context) error { return d.stop(ctx, "") }

// stop stops the timers armed, and the services of those that fired, but for
// keep's: a timer that has fired has started a service of its name, which
// stopping the timer alone would leave to load the file.
func (d *systemdDeadMan) stop(ctx context.Context, keep string) error {
	res, err := d.tools.Run(ctx, systemctl, 10*time.Second, nil, "list-units", "--all", "--plain", "--no-legend", "--type=timer", "--type=service", deadManUnit+"-*")
	if err == nil && res.Code != 0 {
		err = hosttool.Fail("systemctl", res)
	}
	if err != nil {
		return fmt.Errorf("can't list the firewall's reverts armed with systemd: %w. Check systemctl list-units works for the agent on this server", err)
	}
	var units []string
	for line := range strings.SplitSeq(string(res.Out), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], deadManUnit+"-") && (strings.HasSuffix(f[0], ".timer") || strings.HasSuffix(f[0], ".service")) &&
			f[0] != keep+".timer" && f[0] != keep+".service" {
			units = append(units, f[0])
		}
	}
	if len(units) == 0 {
		return nil
	}
	res, err = d.tools.Run(ctx, systemctl, 10*time.Second, nil, append([]string{"stop"}, units...)...)
	if err == nil && res.Code != 0 {
		err = hosttool.Fail("systemctl", res)
	}
	if err != nil {
		return fmt.Errorf("can't stop the firewall's reverts armed before, %s: %w. Check systemctl stop works for the agent on this server", strings.Join(units, ", "), err)
	}
	return nil
}

// ChildDeadMan arms a child in a session of its own, which sleeps, and then
// loads the file if its token is still the one on disk. Disarming removes the
// token, so a child that wakes later does nothing, as does one armed before
// the last.
type ChildDeadMan struct {
	// Shell and Nft are the paths of sh and nft.
	Shell, Nft string
	// Dir holds the token.
	Dir string
}

func (d *ChildDeadMan) token() string { return filepath.Join(d.Dir, "deadman.token") }

// Arm starts the child. It isn't run through the tool runner, which ends
// whatever a tool leaves behind: outliving the agent is the point of it.
func (d *ChildDeadMan) Arm(ctx context.Context, file string, wait time.Duration) error {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	if err := writeFile(d.token(), []byte(token)); err != nil {
		return fmt.Errorf("can't arm the firewall's revert: %w", err)
	}
	secs := strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10)
	// The words are arguments, never part of the script.
	const script = `sleep "$1" || exit 0; [ "$(cat "$2" 2>/dev/null)" = "$3" ] || exit 0; rm -f "$2"; exec "$4" -f "$5"`
	// No context ends it: it has to outlive the call, and the agent.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), d.Shell, "-c", script, "dockwell-access-revert", secs, d.token(), token, d.Nft, file) //nolint:gosec // fixed script, paths as arguments
	cmd.Dir = "/"
	// Its own PATH, not the agent's: sleep, cat and rm are the system's.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(d.token())
		return fmt.Errorf("can't arm the firewall's revert: %w", err)
	}
	// It's on its own from here: the agent only reaps it if it's still there.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Disarm removes the token, so the child does nothing when it wakes.
func (d *ChildDeadMan) Disarm(context.Context) error {
	if err := os.Remove(d.token()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("can't disarm the firewall's revert: %w", err)
	}
	return nil
}
