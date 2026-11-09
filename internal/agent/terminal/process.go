// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"io"
	"os/exec"
	"syscall"
)

// process is what a terminal runs: a shell on the server, on a pseudo-terminal the
// agent holds, or one in a container, on Docker's.
type process interface {
	io.Reader
	io.Writer
	resize(cols, rows uint16) error
	// redraw tells what runs in front to draw itself again, as a resize does.
	redraw()
	// close ends the terminal's side, which hangs up the process's.
	close() error
	// wait waits for the process to exit and says how it did, once read is closed,
	// which it is when all it printed has been read, if that tells.
	wait(read <-chan struct{}) (code int, err error)
	// signal sends sig to the process, and what it started, where it can.
	signal(sig syscall.Signal)
}

// ptyShell is a shell on a pseudo-terminal the agent holds.
type ptyShell struct {
	pty *pty
	cmd *exec.Cmd
}

func (s *ptyShell) Read(p []byte) (int, error)     { return s.pty.Read(p) }
func (s *ptyShell) Write(p []byte) (int, error)    { return s.pty.Write(p) }
func (s *ptyShell) resize(cols, rows uint16) error { return s.pty.resize(cols, rows) }
func (s *ptyShell) redraw()                        { s.pty.redraw() }
func (s *ptyShell) close() error                   { return s.pty.Close() }

func (s *ptyShell) wait(<-chan struct{}) (int, error) {
	err := s.cmd.Wait()
	code := s.cmd.ProcessState.ExitCode()
	if status, ok := s.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = 128 + int(status.Signal())
	}
	return code, err
}

// signal sends sig to the shell's process group, as it leads a session of its own.
func (s *ptyShell) signal(sig syscall.Signal) {
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, sig)
	}
}
