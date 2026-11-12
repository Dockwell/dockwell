// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

// Rebooting a server for a change (ADR 0064): a mark of the change and the boot it
// was asked on, kept in the agent's data folder, then the init system's reboot once
// the answer is on its way. A later call for the same change reads the mark: the
// server rebooted (another boot), is on its way down (this boot), or was never
// asked, as when the call never got here.

// rebootAfter is how long a reboot waits once asked, for the answer to reach the
// control plane before the server goes down.
const rebootAfter = 2 * time.Second

// ErrNoBoot is a server whose boots can't be told apart, which Dockwell doesn't
// reboot, as it couldn't tell when the reboot was done.
var ErrNoBoot = errors.New("can't read this boot's ID")

// rebootMark is what the agent keeps of a reboot it was asked for.
type rebootMark struct {
	Change string    `json:"change"`
	Boot   string    `json:"boot"`
	Asked  time.Time `json:"asked"`
	// Failed is what the reboot command said when it failed, and FailedAt when:
	// the server is still up, on the boot it was asked on.
	Failed   string    `json:"failed,omitempty"`
	FailedAt time.Time `json:"failed_at,omitzero"`
}

// BootID names this boot of the server, as the kernel does, read under the
// reader's root so a test can stand in another boot. Empty where nothing names it.
func (r *Reader) BootID() string {
	b, err := os.ReadFile(r.path("proc/sys/kernel/random/boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (in *Installer) rebootFile() string { return filepath.Join(filepath.Dir(in.Dir), "reboot") }

func (in *Installer) readMark() (rebootMark, bool) {
	b, err := os.ReadFile(in.rebootFile())
	if err != nil {
		return rebootMark{}, false
	}
	var m rebootMark
	if json.Unmarshal(b, &m) != nil || m.Change == "" {
		return rebootMark{}, false
	}
	return m, true
}

// Rebooted says where the change's reboot is, by the mark the agent keeps of it.
func (h Host) Rebooted(_ context.Context, change string) (host.RebootState, error) {
	in := h.Installer
	m, ok := in.readMark()
	if !ok || m.Change != change {
		return host.RebootNone, nil
	}
	boot := in.Reader.BootID()
	if boot == "" {
		return host.RebootNone, resource.Plainly("Dockwell can't tell this server's boots apart, so it doesn't reboot it. Reboot it over SSH.", ErrNoBoot)
	}
	switch {
	case m.Boot != boot:
		return host.RebootDone, nil
	case m.Failed != "":
		return host.RebootFailed, nil
	}
	return host.RebootAsked, nil
}

// Reboot marks the change's reboot with this boot and asks the server to reboot
// once the answer is on its way: systemctl reboot under systemd, or reboot. Asked
// again on the same boot it does nothing, as the server is on its way down, unless
// the command failed, when it fails with what the command said; on another boot it
// lets the mark go, as the reboot is done. Either way the mark goes once it's said.
func (h Host) Reboot(ctx context.Context, change string) error {
	in, r := h.Installer, h.Installer.Reader
	if r.unavailable != "" {
		return resource.Plainly("This is a test, so Dockwell reboots nothing here.", errors.New(r.unavailable))
	}
	boot := r.BootID()
	if boot == "" {
		return resource.Plainly("Dockwell can't tell this server's boots apart, so it doesn't reboot it. Reboot it over SSH.", ErrNoBoot)
	}
	if m, ok := in.readMark(); ok && m.Change == change {
		switch {
		case m.Boot != boot:
			_ = os.Remove(in.rebootFile())
		case m.Failed != "":
			_ = os.Remove(in.rebootFile())
			return fmt.Errorf("it's still up, as the reboot command failed: %s", m.Failed)
		}
		return nil
	}
	b, err := json.Marshal(rebootMark{Change: change, Boot: boot, Asked: in.now()})
	if err != nil {
		return err
	}
	if err := writeSynced(in.rebootFile(), b); err != nil {
		return fmt.Errorf("can't keep a mark of the reboot, so it doesn't reboot: %w", err)
	}
	argv := []string{"/sbin/reboot"}
	if r.exists("run/systemd/system") {
		argv = []string{systemctl, "reboot"}
	}
	after := cmpOr0(in.RebootAfter, rebootAfter)
	// Not the call's context: the reboot goes on once the answer is sent.
	time.AfterFunc(after, func() {
		res, err := r.run(context.WithoutCancel(ctx), "Rebooting the server", argv[0], ReadTimeout, argv[1:]...)
		if err == nil && res.code != 0 {
			err = fail("Rebooting the server", res)
		}
		if err != nil {
			r.logger.Error("couldn't reboot the server; the change fails with this as soon as the control plane looks again", "change", change, "error", err)
			in.markFailed(change, boot, err)
		}
	})
	return nil
}

// markFailed notes in the change's mark that its reboot command failed, and what
// it said, so the next look at the step fails it with that rather than waiting for
// a server that's still up.
func (in *Installer) markFailed(change, boot string, cause error) {
	m, ok := in.readMark()
	if !ok || m.Change != change || m.Boot != boot {
		return
	}
	m.Failed, m.FailedAt = lastLines(cause.Error()), in.now()
	if b, err := json.Marshal(m); err == nil {
		_ = writeSynced(in.rebootFile(), b)
	}
}

// writeSynced writes a file whole, through a file beside it, and syncs it, so a
// reboot right after finds it.
func writeSynced(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // a path under the agent's own data folder
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

func cmpOr0(d, or time.Duration) time.Duration {
	if d <= 0 {
		return or
	}
	return d
}
