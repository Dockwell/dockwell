// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// pty is the side of a pseudo-terminal the agent holds. The shell has the other
// side as its controlling terminal.
type pty struct {
	*os.File
}

// start runs cmd on a new pseudo-terminal of cols by rows, leading a session of
// its own with the terminal as its controlling terminal, and returns the
// agent's side. The master stays with Go's poller, so closing it ends a read or
// write under way.
func start(cmd *exec.Cmd, cols, rows uint16) (*pty, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("can't open a pseudo-terminal: %w", err)
	}
	p := &pty{master}
	var name string
	err = p.control(func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
		if err != nil {
			return err
		}
		name = "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
	if err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("can't set up a pseudo-terminal: %w", err)
	}
	// Opened apart from Go's poller, so the shell's side stays blocking, as a
	// shell expects of its terminal.
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("can't open the shell's side of a pseudo-terminal: %w", err)
	}
	slave := os.NewFile(uintptr(fd), name)
	defer func() { _ = slave.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		return nil, err
	}
	return p, nil
}

// control runs fn on the master's descriptor.
func (p *pty) control(fn func(fd int) error) error {
	raw, err := p.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := raw.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return err
	}
	return inner
}

// resize sets the terminal's size, which tells what runs in front that it changed.
func (p *pty) resize(cols, rows uint16) error {
	return p.control(func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
}

// redraw tells what runs in front to draw itself again, as a resize does, though the
// size stays as it is.
func (p *pty) redraw() {
	_ = p.control(func(fd int) error {
		pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if err != nil {
			return err
		}
		if pgrp <= 1 {
			return errors.New("no process group in front")
		}
		return unix.Kill(-pgrp, unix.SIGWINCH)
	})
}

// isHangUp reports the read that ends a terminal: the shell's side has closed.
func isHangUp(err error) bool {
	return errors.Is(err, unix.EIO) || errors.Is(err, os.ErrClosed)
}
