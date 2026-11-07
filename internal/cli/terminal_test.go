// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && !agent

package cli

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal, and returns the end a program writes to.
func openPTY(t *testing.T) *os.File {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("there's no pseudo-terminal here: %v", err)
	}
	ptmx := os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = ptmx.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("can't unlock a pseudo-terminal here: %v", err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("can't name a pseudo-terminal here: %v", err)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("can't open a pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { _ = pts.Close() })
	return pts
}

// The check dockwell run makes when nothing stands in for it: a claim link
// goes to a terminal, and never to a file or a pipe, which something keeps
// (#110).
func TestOnlyATerminalReadsAsOne(t *testing.T) {
	if !(IO{Out: openPTY(t)}).outIsTerminal() {
		t.Error("a pseudo-terminal doesn't read as a terminal, so dockwell run would never print a claim link")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if (IO{Out: f}).outIsTerminal() {
		t.Error("a file reads as a terminal, so a claim link would go into it")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if (IO{Out: w}).outIsTerminal() {
		t.Error("a pipe reads as a terminal, so a claim link would go to whatever reads it")
	}
	if (IO{Out: &bytes.Buffer{}}).outIsTerminal() {
		t.Error("what isn't a file reads as a terminal")
	}
}
