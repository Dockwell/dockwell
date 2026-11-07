// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/human"
)

// How long each kind of run may take, and how long a run that's out of time gets
// between SIGTERM and SIGKILL (ADR 0043).
const (
	RefreshTimeout = 5 * time.Minute
	ReadTimeout    = 2 * time.Minute
	InstallTimeout = 30 * time.Minute
	KillGrace      = 10 * time.Second
)

// What of a run's output is read: at most maxOutput of what it prints, past which
// the run fails, and the last errTail of its errors, for a failure's message.
const (
	maxOutput = 4 << 20
	errTail   = 16 << 10
)

// env is all a tool gets of an environment. Nothing of the agent's own passes
// on, and C makes the output read the same on every server.
var env = []string{
	"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"LC_ALL=C",
	"LANG=C",
	"DEBIAN_FRONTEND=noninteractive",
	"APT_LISTCHANGES_FRONTEND=none",
}

// errTooMuch is a tool that printed more than a read takes.
var errTooMuch = errors.New("printed more than 4 MiB")

// result is what a run printed, and how it ended.
type result struct {
	out []byte
	// errs is the last of what it printed on standard error.
	errs string
	// code is its exit status, or -1 when it didn't exit by itself.
	code int
}

// run starts the tool at path, under the reader's root, with args, waits for it
// within timeout, and returns what it printed. An exit status isn't an error
// here: tools such as dnf say what they found with theirs, so each caller
// decides. A run out of time gets SIGTERM, with everything it started, and
// SIGKILL a grace later, if it hasn't ended by then.
func (r *Reader) run(ctx context.Context, path string, timeout time.Duration, args ...string) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name := filepath.Base(path)
	cmd := exec.CommandContext(ctx, r.path(path), args...) //nolint:gosec // a fixed path under the root, with arguments the tool listed itself
	cmd.Env = env
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &capped{max: maxOutput}
	errs := &tail{max: errTail}
	cmd.Stdout, cmd.Stderr = out, errs
	// What it started goes with it: a maintainer's script can start what outlives
	// the tool. Past the grace, os/exec kills the tool itself, and the group follows.
	var killed atomic.Bool
	cmd.Cancel = func() error {
		killed.Store(true)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = r.grace
	err := cmd.Run()
	if killed.Load() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	res := result{out: out.buf.Bytes(), errs: errs.String(), code: -1}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		res.code = cmd.ProcessState.ExitCode()
	}
	switch {
	case out.over:
		return res, fmt.Errorf("%s %s %w", name, firstWords(args), errTooMuch)
	case killed.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("%s %s didn't finish within %s, so it was stopped%s: %w",
			name, firstWords(args), human.Duration(timeout), said(res.errs), context.DeadlineExceeded)
	case killed.Load():
		return res, fmt.Errorf("%s was stopped, as its caller left: %w", name, context.Cause(ctx))
	case err != nil && res.code < 0:
		return res, fmt.Errorf("can't run %s: %w", r.path(path), err)
	}
	return res, nil
}

// fail is the error of a run that exited with a status its caller doesn't take.
func fail(path string, res result, args ...string) error {
	what := lastLines(res.errs, 3)
	if what == "" {
		what = lastLines(string(res.out), 3)
	}
	if what == "" {
		what = "it said nothing"
	}
	return fmt.Errorf("%s %s ended with status %d: %s", filepath.Base(path), firstWords(args), res.code, what)
}

// said is a run's last error lines, as a clause, for a message.
func said(errs string) string {
	if s := lastLines(errs, 3); s != "" {
		return ", after saying: " + s
	}
	return ""
}

// lastLines is the last n lines of s that hold something, joined with "; ".
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// firstWords is a command's arguments up to its first package name, for a message.
func firstWords(args []string) string {
	var words []string
	for _, a := range args {
		if a == "--" {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// capped keeps what's written up to max, and notes that more came.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, errTooMuch
	}
	return c.buf.Write(p)
}

// tail keeps the last max bytes written.
type tail struct {
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }
