// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource/host"
)

// rebootStand is a server under systemd on boot-1, whose systemctl notes each call.
func rebootStand(t *testing.T) (*stand, Host) {
	t.Helper()
	s := newStand(t, ubuntu)
	s.tool(systemctl, "exit 0")
	write(t, s.root, "run/systemd/system/.keep", "", 0o644)
	write(t, s.root, "proc/sys/kernel/random/boot_id", "boot-1\n", 0o644)
	in := &Installer{Reader: New(Options{Root: s.root}), Dir: filepath.Join(s.root, "agent", "install"), RebootAfter: 10 * time.Millisecond}
	return s, Host{Installer: in}
}

func (s *stand) waitCalled(prefix string, n int) {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); s.called(prefix) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("%q was called %d times, want %d: %v", prefix, s.called(prefix), n, s.calls())
		}
	}
}

// A reboot is asked once for its change: marked with this boot, then systemctl
// reboot once the answer is on its way. Asked again on the same boot it does
// nothing; on another boot the change's reboot is done, and the mark goes.
func TestARebootIsAskedOnceForItsChange(t *testing.T) {
	s, h := rebootStand(t)
	ctx := t.Context()
	if got, err := h.Rebooted(ctx, "chg_1"); err != nil || got != host.RebootNone {
		t.Fatalf("before any reboot: %v, %v", got, err)
	}
	if err := h.Reboot(ctx, "chg_1"); err != nil {
		t.Fatal(err)
	}
	s.waitCalled("systemctl_reboot", 1)
	if got, _ := h.Rebooted(ctx, "chg_1"); got != host.RebootAsked {
		t.Errorf("on the same boot the reboot reads %v, want asked", got)
	}
	if got, _ := h.Rebooted(ctx, "chg_2"); got != host.RebootNone {
		t.Errorf("another change's reboot reads %v, want none", got)
	}
	if err := h.Reboot(ctx, "chg_1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := s.called("systemctl_reboot"); n != 1 {
		t.Errorf("asked again on the same boot, it rebooted %d times, want once", n)
	}
	write(t, s.root, "proc/sys/kernel/random/boot_id", "boot-2\n", 0o644)
	if got, _ := h.Rebooted(ctx, "chg_1"); got != host.RebootDone {
		t.Errorf("on another boot the reboot reads %v, want done", got)
	}
	if err := h.Reboot(ctx, "chg_1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := s.called("systemctl_reboot"); n != 1 {
		t.Errorf("finished on another boot, it rebooted %d times, want once", n)
	}
	if got, _ := h.Rebooted(ctx, "chg_1"); got != host.RebootNone {
		t.Errorf("once done the mark reads %v, want gone", got)
	}
}

// Without systemd it asks reboot itself.
func TestARebootWithoutSystemdAsksReboot(t *testing.T) {
	s := newStand(t, ubuntu)
	s.tool("sbin/reboot", "exit 0")
	write(t, s.root, "proc/sys/kernel/random/boot_id", "boot-1\n", 0o644)
	h := Host{Installer: &Installer{Reader: New(Options{Root: s.root}), Dir: filepath.Join(s.root, "agent", "install"), RebootAfter: time.Millisecond}}
	if err := h.Reboot(t.Context(), "chg_1"); err != nil {
		t.Fatal(err)
	}
	s.waitCalled("reboot", 1)
}

// A test with no stand-ins reboots nothing, and a server whose boots can't be told
// apart isn't rebooted, as nobody could tell when it was done.
func TestNothingRebootsWithoutStandInsOrABootID(t *testing.T) {
	s := newStand(t, ubuntu)
	s.tool(systemctl, "exit 0")
	write(t, s.root, "run/systemd/system/.keep", "", 0o644)
	none := Host{Installer: &Installer{Reader: New(Options{Root: s.root, Unavailable: "a test"}), Dir: filepath.Join(s.root, "agent", "install"), RebootAfter: time.Millisecond}}
	if err := none.Reboot(t.Context(), "chg_1"); err == nil {
		t.Error("a test with no stand-ins rebooted")
	}
	noBoot := Host{Installer: &Installer{Reader: New(Options{Root: s.root}), Dir: filepath.Join(s.root, "agent", "install"), RebootAfter: time.Millisecond}}
	if err := noBoot.Reboot(t.Context(), "chg_1"); !errors.Is(err, ErrNoBoot) {
		t.Errorf("with no boot ID, a reboot = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := s.called("systemctl"); n != 0 {
		t.Errorf("systemctl was called %d times", n)
	}
}

// A reboot command that fails is noted in the mark, with what it said: the change's
// reboot reads failed, and applying it again says that, and lets the mark go.
func TestARebootCommandThatFailsIsSaidOnTheNextCall(t *testing.T) {
	s, h := rebootStand(t)
	s.tool(systemctl, `echo "Failed to reboot: Access denied" >&2; exit 1`)
	ctx := t.Context()
	if err := h.Reboot(ctx, "chg_1"); err != nil {
		t.Fatal(err)
	}
	s.waitCalled("systemctl_reboot", 1)
	var got host.RebootState
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if got, _ = h.Rebooted(ctx, "chg_1"); got == host.RebootFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a failed command the reboot reads %v, want failed", got)
		}
	}
	err := h.Reboot(ctx, "chg_1")
	if err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("applying a reboot whose command failed = %v, want what it said", err)
	}
	if n := s.called("systemctl_reboot"); n != 1 {
		t.Errorf("the failed reboot was tried %d times, want once", n)
	}
	if got, _ := h.Rebooted(ctx, "chg_1"); got != host.RebootNone {
		t.Errorf("once said, the mark reads %v, want gone", got)
	}
}
