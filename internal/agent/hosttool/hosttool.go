// SPDX-License-Identifier: AGPL-3.0-only

// Package hosttool runs the server's own tools, such as nft or sshd, as ADR
// 0043 runs a package manager and ADR 0052 runs a firewall's: from a fixed
// path, with no shell, a clean environment in the C locale, a deadline, bounded
// output, and one at a time.
package hosttool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/human"
)

// Bounds on what a run reads: at most MaxOutput of what it prints, past which
// the run fails, and the last errTail of its errors, for a failure's message.
const (
	MaxOutput = 4 << 20
	errTail   = 16 << 10
)

// KillGrace is how long a run that's out of time has between SIGTERM and
// SIGKILL, with everything it started.
const KillGrace = 10 * time.Second

// env is all a tool gets of an environment. Nothing of the agent's own passes
// on, and C makes the output read the same on every server.
var env = []string{
	"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"LC_ALL=C",
	"LANG=C",
}

var (
	// ErrMissing is a tool that isn't at any of its paths on this server.
	ErrMissing = errors.New("isn't installed")
	// ErrOff is a tool a test runs with none of the server's own tools.
	ErrOff = errors.New("isn't run in a test without stand-ins")
	// ErrTooMuch is a tool that printed more than a run reads.
	ErrTooMuch = errors.New("printed more than 4 MiB")
)

// Tool is one of the server's tools, by the paths it's at: the first that's
// there runs. A tool is never looked for on PATH.
type Tool struct {
	// Name is what it's called, in messages.
	Name string
	// Paths are where it may be, absolute, in the order they're tried.
	Paths []string
	// Env is what the tool needs beyond the clean environment, such as
	// DEBIAN_FRONTEND for apt.
	Env []string
}

// Result is what a run printed, and how it ended.
type Result struct {
	// Out is what it printed on standard output.
	Out []byte
	// Errs is the last of what it printed on standard error.
	Errs string
	// Code is its exit status, or -1 when it didn't exit by itself.
	Code int
}

// Runner runs tools one at a time. The zero value runs the server's own.
type Runner struct {
	// Root, for a test, is a folder of stand-ins: a tool at /usr/sbin/nft runs
	// Root/usr/sbin/nft. Empty runs the server's own.
	Root string
	// Off refuses every tool, as a test without stand-ins does, so no test runs
	// the machine's own nft or sshd.
	Off bool
	// Grace replaces KillGrace, for a test.
	Grace time.Duration

	// turn holds one token while a tool runs, so a caller waits its turn or
	// gives up as its context ends.
	turn     chan struct{}
	turnOnce sync.Once
}

func (r *Runner) wait(ctx context.Context) (func(), error) {
	r.turnOnce.Do(func() { r.turn = make(chan struct{}, 1) })
	select {
	case r.turn <- struct{}{}:
		return func() { <-r.turn }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waited for another of the server's tools to finish: %w", context.Cause(ctx))
	}
}

// path is where a tool's path is on this server, or under Root for a test.
func (r *Runner) path(p string) string {
	if r.Root == "" {
		return p
	}
	return filepath.Join(r.Root, p)
}

// Find returns the path a tool runs from, or false when it isn't at any of them.
func (r *Runner) Find(t Tool) (string, bool) {
	if r.Off {
		return "", false
	}
	for _, p := range t.Paths {
		if info, err := os.Stat(r.path(p)); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// Run runs a tool with args, waits for it within timeout, and returns what it
// printed. stdin is what it reads; nil reads /dev/null, so nothing can wait for
// an answer. An exit status isn't an error here, as tools such as nft and dnf
// say what they found with theirs: each caller decides. A run out of time gets
// SIGTERM, with everything it started, and SIGKILL a grace later.
func (r *Runner) Run(ctx context.Context, t Tool, timeout time.Duration, stdin []byte, args ...string) (Result, error) {
	if r.Off {
		return Result{Code: -1}, fmt.Errorf("%s %w", t.Name, ErrOff)
	}
	path, ok := r.Find(t)
	if !ok {
		return Result{Code: -1}, fmt.Errorf("%s %w: it isn't at %s", t.Name, ErrMissing, strings.Join(t.Paths, " or "))
	}
	done, err := r.wait(ctx)
	if err != nil {
		return Result{Code: -1}, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.path(path), args...) //nolint:gosec // a fixed path, with arguments its caller builds
	cmd.Env = append(append([]string(nil), env...), t.Env...)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out := &capped{max: MaxOutput}
	errs := &tail{max: errTail}
	cmd.Stdout, cmd.Stderr = out, errs
	var killed atomic.Bool
	cmd.Cancel = func() error {
		killed.Store(true)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = KillGrace
	if r.Grace > 0 {
		cmd.WaitDelay = r.Grace
	}
	err = cmd.Run()
	// What the tool started and left holding its output, past the grace, goes
	// too, as it would go on with nothing waiting for it.
	if killed.Load() || errors.Is(err, exec.ErrWaitDelay) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	res := Result{Out: out.buf.Bytes(), Errs: errs.String(), Code: -1}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		res.Code = cmd.ProcessState.ExitCode()
	}
	switch {
	case out.over:
		return res, fmt.Errorf("%s %w", t.Name, ErrTooMuch)
	case killed.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("%s didn't finish within %s, so it was stopped%s: %w",
			t.Name, human.Duration(timeout), Said(res.Errs), context.DeadlineExceeded)
	case killed.Load():
		return res, fmt.Errorf("%s was stopped, as its caller left: %w", t.Name, context.Cause(ctx))
	case err != nil && res.Code < 0 && !errors.Is(err, exec.ErrWaitDelay):
		return res, fmt.Errorf("can't run %s: %w", r.path(path), err)
	}
	return res, nil
}

// Fail is the error of a run that exited with a status its caller doesn't
// take, with the last of what it said.
func Fail(name string, res Result) error {
	said := LastLines(res.Errs, 3)
	if said == "" {
		said = LastLines(string(res.Out), 3)
	}
	if said == "" {
		said = "it said nothing"
	}
	return fmt.Errorf("%s ended with status %d: %s", name, res.Code, said)
}

// Said is a run's last error lines, as a clause, for a message.
func Said(errs string) string {
	if s := LastLines(errs, 3); s != "" {
		return ", after saying: " + s
	}
	return ""
}

// LastLines is the last n lines of s that hold something, joined with "; ".
func LastLines(s string, n int) string {
	var lines []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
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
		return 0, ErrTooMuch
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
