// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// shellRun is a shell an exec with a terminal runs: /bin/sh on this machine, on a
// pseudo-terminal of its own, standing in for the shell an image has.
type shellRun struct {
	master *os.File
	cmd    *exec.Cmd
}

// startShell runs /bin/sh on a new pseudo-terminal of cols by rows.
func startShell(cols, rows uint16) (*shellRun, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*shellRun, error) {
		_ = master.Close()
		return nil, err
	}
	raw, err := master.SyscallConn()
	if err != nil {
		return fail(err)
	}
	var name string
	var inner error
	if err := raw.Control(func(fd uintptr) {
		if inner = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); inner != nil {
			return
		}
		var n uint32
		if n, inner = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN); inner != nil {
			return
		}
		name = "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
		inner = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	}); err != nil {
		return fail(err)
	}
	if inner != nil {
		return fail(inner)
	}
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	slave := os.NewFile(uintptr(fd), name)
	defer func() { _ = slave.Close() }()
	cmd := exec.Command("/bin/sh") //nolint:noctx // the fake's stand-in for a container's shell, which ends when it's hung up
	cmd.Env = []string{"TERM=xterm-256color", "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/", "PS1=# "}
	cmd.Dir = "/"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	return &shellRun{master: master, cmd: cmd}, nil
}

func (s *shellRun) resize(cols, rows uint16) {
	if raw, err := s.master.SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) {
			_ = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
		})
	}
}

// pump carries the shell's terminal over a hijacked connection until either ends,
// and returns how the shell exited.
func (s *shellRun) pump(in io.Reader, out io.Writer, closeConn func()) int {
	go func() {
		_, _ = io.Copy(s.master, in)
	}()
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, s.master)
		close(copied)
	}()
	err := s.cmd.Wait()
	_ = s.master.Close()
	<-copied
	closeConn()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok { //nolint:errorlint // what Wait returns
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}

// resizeExec sets the size of an exec's terminal.
func (e *Engine) resizeExec(w http.ResponseWriter, r *http.Request, id string) {
	x := e.execs[id]
	if x == nil {
		fail(w, http.StatusNotFound, "No such exec instance: %s", id)
		return
	}
	h, _ := strconv.Atoi(r.URL.Query().Get("h"))
	cols, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if x.shell == nil || h <= 0 || cols <= 0 {
		fail(w, http.StatusBadRequest, "can't resize exec %s", id)
		return
	}
	x.shell.resize(uint16(cols), uint16(h)) //nolint:gosec // a terminal's size
	w.WriteHeader(http.StatusOK)
}

// statPath answers whether a container has a path, as HEAD /containers/{id}/archive
// does: only the image's shell is there.
func (e *Engine) statPath(w http.ResponseWriter, r *http.Request, c *container) {
	if p := r.URL.Query().Get("path"); p == "" || p != c.img.Shell {
		fail(w, http.StatusNotFound, "Could not find the file %s in container %s", p, c.ID)
		return
	}
	w.Header().Set("X-Docker-Container-Path-Stat", "e30=")
	w.WriteHeader(http.StatusOK)
}
