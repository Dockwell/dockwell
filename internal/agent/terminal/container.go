// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dockwell/dockwell/internal/docker"
)

// shells are the shells a container's terminal runs, the first it has.
var shells = []string{"/bin/bash", "/bin/ash", "/bin/sh"}

// ErrNeedsStepUp is a shell in a container that's as good as root on its server,
// which a passkey hasn't confirmed. Nothing ran.
var ErrNeedsStepUp = errors.New("this container reaches as far as root on its server, so a shell in it takes your passkey")

// execShell is a shell in a container, on a terminal Docker holds.
type execShell struct {
	t      *docker.ExecTerminal
	cancel context.CancelFunc

	mu         sync.Mutex
	cols, rows uint16
}

func (s *execShell) Read(p []byte) (int, error)  { return s.t.Read(p) }
func (s *execShell) Write(p []byte) (int, error) { return s.t.Write(p) }

func (s *execShell) resize(cols, rows uint16) error {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.t.Resize(ctx, cols, rows)
}

// redraw resizes the terminal a line short and back, as Docker says nothing to what
// runs in it of a size that stays as it is.
func (s *execShell) redraw() {
	s.mu.Lock()
	cols, rows := s.cols, s.rows
	s.mu.Unlock()
	if rows < 2 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.t.Resize(ctx, cols, rows-1) == nil {
		_ = s.t.Resize(ctx, cols, rows)
	}
}

func (s *execShell) close() error {
	s.cancel()
	return s.t.Close()
}

// wait waits for what the shell printed to end, as it does when the shell exits,
// then asks Docker how it exited.
func (s *execShell) wait(read <-chan struct{}) (int, error) {
	<-read
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		st, err := s.t.State(ctx)
		if err == nil && !st.Running {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, errors.New("docker didn't say how the shell exited")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// signal sends sig to the shell, and what it leads, by its ID on the server, as the
// agent runs as root there. Docker hangs up nothing when its connection goes.
func (s *execShell) signal(sig syscall.Signal) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := s.t.State(ctx)
	if err != nil || !st.Running || st.Pid <= 1 {
		return
	}
	signalExec(st.Pid, sig, func() bool {
		again, err := s.t.State(ctx)
		return err == nil && again.Running && again.Pid == st.Pid
	})
}

// signalExec sends sig to pid and the group it leads, if still says pid is the
// exec's process yet. A PID Docker gave can be another process's by the time it's
// used, so the agent takes a pidfd on it first, which keeps its number from going
// to another process, then asks again, and sends only then. A kernel before 5.3 has
// no pidfds, and there it's asked again just before.
func signalExec(pid int, sig syscall.Signal, still func() bool) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || !still() {
			return
		}
		if syscall.Kill(-pid, sig) != nil {
			_ = syscall.Kill(pid, sig)
		}
		return
	}
	defer func() { _ = unix.Close(fd) }()
	if !still() {
		return
	}
	// The pidfd holds pid's number, and the group's, for the shell alone.
	_ = syscall.Kill(-pid, sig)
	_ = unix.PidfdSendSignal(fd, sig, nil, 0)
}

// found is the container a call named, exactly: its full ID and its name.
type found struct{ id, name string }

// find is the container ref names, by its full ID or its whole name, never a prefix
// of either, as ADR 0021 has it, so what's checked, recorded and run in is the one
// asked for.
func find(ctx context.Context, d *docker.Client, ref string) (*docker.Container, error) {
	listed, err := d.ListContainers(ctx, docker.Exactly(ref))
	if err != nil {
		return nil, err
	}
	switch len(listed) {
	case 0:
		return nil, fmt.Errorf("there's no container %s on this server. It may have been replaced; open a shell in the one that runs now", ref)
	case 1:
	default:
		return nil, fmt.Errorf("%d containers answer to %s, so a shell can't tell which. Name it by its full ID", len(listed), ref)
	}
	c, err := d.InspectContainer(ctx, listed[0].ID)
	if docker.IsNotFound(err) {
		return nil, fmt.Errorf("%s went as the shell opened. Open a shell in the one that runs now", ref)
	}
	return c, err
}

// startInContainer runs the first shell container has, on a terminal of cols by
// rows, once it's checked whether the container is as good as root on its server,
// which takes confirmed. It returns the shell, who it runs as where, and the
// container it found, which is set with ErrNeedsStepUp too.
func (m *Manager) startInContainer(ctx context.Context, container string, confirmed bool, cols, rows uint16) (process, string, found, error) {
	d := m.opts.Docker
	if d == nil {
		return nil, "", found{}, errors.New("this agent can't reach Docker, so it can't open a shell in a container")
	}
	c, err := find(ctx, d, container)
	if err != nil {
		return nil, "", found{}, err
	}
	name := strings.TrimPrefix(c.Name, "/")
	in := found{id: c.ID, name: name}
	if !c.State.Running {
		return nil, "", in, fmt.Errorf("%s isn't running, so there's no shell to open in it. Start it, then try again", name)
	}
	// What the agent checks is the container it runs the shell in, by its ID.
	root := true
	if m.opts.AsGoodAsRoot != nil {
		if root, err = m.opts.AsGoodAsRoot(ctx, c.ID); err != nil {
			return nil, "", in, fmt.Errorf("can't tell how far %s reaches on this server, so no shell opened: %w", name, err)
		}
	}
	if root && !confirmed {
		return nil, "", in, ErrNeedsStepUp
	}
	shell := ""
	for _, s := range shells {
		has, err := d.HasPath(ctx, c.ID, s)
		if err != nil {
			return nil, "", in, err
		}
		if has {
			shell = s
			break
		}
	}
	if shell == "" {
		return nil, "", in, fmt.Errorf("%s's image has none of %s and %s, as an image made to run one program, such as a distroless one, hasn't. "+
			"Its logs are on its app's page", name, strings.Join(shells[:len(shells)-1], ", "), shells[len(shells)-1])
	}
	// The connection outlives the call that opened it, until the terminal ends.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t, err := d.ExecWithTerminal(life, c.ID, []string{shell}, []string{"TERM=xterm-256color", "COLORTERM=truecolor"}, cols, rows)
	if err != nil {
		cancel()
		return nil, "", in, fmt.Errorf("can't start %s in %s: %w", shell, name, err)
	}
	s := &execShell{t: t, cancel: cancel, cols: cols, rows: rows}
	// An engine older than ConsoleSize starts it at its own size.
	_ = s.resize(cols, rows)
	user, _, _ := strings.Cut(c.Config.User, ":")
	return s, cmpOr(user, "root") + "@" + name, in, nil
}
