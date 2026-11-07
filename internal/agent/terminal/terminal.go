// SPDX-License-Identifier: AGPL-3.0-only

// Package terminal runs a server's terminals for a person: root's login shell on
// a pseudo-terminal the agent holds, apart from any stream, so a connection that
// drops leaves it running, and the newest of what it printed kept for the next
// attachment. What's typed and printed is never written anywhere. See
// docs/adr/0037-a-terminal-on-a-server.md and docs/security/terminal.md.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/human"
)

// Limits (ADR 0037).
const (
	// MaxOpen is how many terminals a server has at once.
	MaxOpen = 8
	// Keep is how much of what a terminal printed it keeps for the next
	// attachment, and how much an attachment may fall behind before the shell
	// waits for it.
	Keep = 256 << 10
	// MaxInput is how much of what's typed a terminal holds that its shell hasn't
	// read yet.
	MaxInput = 256 << 10
	// MaxWrite is the most one write carries.
	MaxWrite = 64 << 10
	// DetachedFor ends a terminal nobody has been attached to for this long.
	DetachedFor = 10 * time.Minute
	// IdleFor ends a terminal nothing has been typed into for this long.
	IdleFor = time.Hour
	// MaxColumns bounds a terminal's width.
	MaxColumns = 500
	// MaxRows bounds a terminal's height.
	MaxRows = 200
)

// chunk is the most one message carries of what a terminal printed.
const chunk = 32 << 10

// hangUpWait is how long a shell that was hung up gets to exit before it and what
// it started are killed.
const hangUpWait = 3 * time.Second

// drainWait is how long what a shell wrote last gets to be read once it exits.
const drainWait = 200 * time.Millisecond

// ErrNotFound is a terminal this agent doesn't have.
var ErrNotFound = errors.New("there's no such terminal on this server")

// Options configures a Manager.
type Options struct {
	Logger *slog.Logger
	// Shell runs in place of root's login shell, with its arguments, as for a
	// test. Nil runs root's.
	Shell []string
	// Home is where Shell starts. Empty is root's home.
	Home string
	// Passwd is where root's account is read. Empty is /etc/passwd.
	Passwd string
	// DetachedFor and IdleFor change the package's limits, as for a test.
	DetachedFor, IdleFor time.Duration
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
}

// Manager holds a server's terminals, across the agent's streams.
type Manager struct {
	opts   Options
	logger *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	open  map[string]*Terminal
	ended []*agentv1.TerminalEnded // not yet told
	ready chan struct{}            // signalled when ended has something
}

// New returns a Manager with no terminals. Run it for its timeouts to apply.
func New(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.DetachedFor <= 0 {
		opts.DetachedFor = DetachedFor
	}
	if opts.IdleFor <= 0 {
		opts.IdleFor = IdleFor
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{opts: opts, logger: opts.Logger, now: now, open: map[string]*Terminal{}, ready: make(chan struct{}, 1)}
}

// Run ends terminals that time out, until ctx is done, then hangs up every one: a
// shell is the agent's child, and goes with it. The control plane hears from the
// next agent's hello that they're gone.
func (m *Manager) Run(ctx context.Context) {
	every := min(m.opts.DetachedFor, m.opts.IdleFor) / 10
	every = max(min(every, 15*time.Second), 10*time.Millisecond)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-tick.C:
			m.expire()
		}
	}
}

func (m *Manager) expire() {
	now := m.now()
	for _, t := range m.terminals() {
		t.mu.Lock()
		detached := t.att == nil && now.Sub(t.detachedSince) >= m.opts.DetachedFor
		idle := now.Sub(t.lastInput) >= m.opts.IdleFor
		t.mu.Unlock()
		switch {
		case detached:
			go t.hangUp(dockwellv1.TerminalEndReason_TERMINAL_END_REASON_DETACHED,
				fmt.Sprintf("Nobody was connected to it for %s, so it closed.", human.Duration(m.opts.DetachedFor)))
		case idle:
			go t.hangUp(dockwellv1.TerminalEndReason_TERMINAL_END_REASON_IDLE,
				fmt.Sprintf("Nothing was typed into it for %s, so it closed.", human.Duration(m.opts.IdleFor)))
		}
	}
}

func (m *Manager) closeAll() {
	var wg sync.WaitGroup
	for _, t := range m.terminals() {
		wg.Go(func() {
			t.hangUp(dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED, "The agent stopped, and its shell with it.")
		})
	}
	wg.Wait()
}

func (m *Manager) terminals() []*Terminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Terminal, 0, len(m.open))
	for _, t := range m.open {
		out = append(out, t)
	}
	return out
}

func (m *Manager) get(id string) (*Terminal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.open[id]; t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("%w: %s may have ended. Open another", ErrNotFound, id)
}

// Known is every terminal the control plane hasn't heard the end of: those open,
// and those whose end hasn't gone yet.
func (m *Manager) Known() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.open)+len(m.ended))
	for id := range m.open {
		out = append(out, id)
	}
	for _, e := range m.ended {
		out = append(out, e.GetId())
	}
	slices.Sort(out)
	return out
}

// Ended signals when a terminal's end is there to tell, with TakeEnded.
func (m *Manager) Ended() <-chan struct{} { return m.ready }

// TakeEnded returns what there is to tell of terminals that ended. Give back what
// couldn't be sent with Untold, so the next stream tells it.
func (m *Manager) TakeEnded() []*agentv1.TerminalEnded {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.ended
	m.ended = nil
	return out
}

// Untold gives back ends that couldn't be told.
func (m *Manager) Untold(ended []*agentv1.TerminalEnded) {
	if len(ended) == 0 {
		return
	}
	m.mu.Lock()
	m.ended = append(ended, m.ended...)
	m.mu.Unlock()
	m.signal()
}

func (m *Manager) signal() {
	select {
	case m.ready <- struct{}{}:
	default:
	}
}

// Open starts root's login shell on a new terminal.
func (m *Manager) Open(call *agentv1.OpenTerminalCall) (*agentv1.OpenTerminalResult, error) {
	id := call.GetId()
	if id == "" {
		return nil, errors.New("a terminal needs an ID; the control plane gives it one")
	}
	m.mu.Lock()
	if _, taken := m.open[id]; taken {
		m.mu.Unlock()
		return nil, fmt.Errorf("there's a terminal %s on this server already", id)
	}
	if len(m.open) >= MaxOpen {
		m.mu.Unlock()
		return nil, fmt.Errorf("this server has %d terminals open, which is as many as it keeps. Close one, then open another", MaxOpen)
	}
	// Held until the shell starts, so two opens can't both take the last place.
	t := &Terminal{m: m, id: id, record: call.GetRecord(), out: newOutput(Keep), changed: make(chan struct{}), exited: make(chan struct{})}
	m.open[id] = t
	m.mu.Unlock()
	if err := t.start(clampColumns(call.GetColumns()), clampRows(call.GetRows())); err != nil {
		m.mu.Lock()
		delete(m.open, id)
		m.mu.Unlock()
		return nil, err
	}
	m.logger.Info("opened a terminal", slog.String("terminal", id))
	return &agentv1.OpenTerminalResult{Title: t.title, Opened: timestamppb.New(t.opened)}, nil
}

// Close hangs up a terminal's shell, and says how it ended.
func (m *Manager) Close(id string, reason dockwellv1.TerminalEndReason) (*dockwellv1.TerminalEnd, error) {
	t, err := m.get(id)
	if err != nil {
		return nil, err
	}
	message := "It was closed."
	switch reason {
	case dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SIGNED_OUT:
		message = "The session that opened it ended, so it closed."
	case dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CONTROL_PLANE_RESTARTED:
		message = "The control plane restarted, so it closed."
	case dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SERVER_REMOVED:
		message = "Its server was removed from Dockwell, so it closed."
	default:
		reason = dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED
	}
	return t.hangUp(reason, message), nil
}

// Conn is one attachment's stream: what the terminal prints goes up it, and
// what's typed comes down.
type Conn interface {
	Send(*agentv1.TerminalRequest) error
	Receive() (*agentv1.TerminalResponse, error)
}

// Attach carries a terminal's bytes over conn until ctx ends, conn fails, another
// attachment takes its place, or the terminal ends, which the result says. The
// first message names call's stream. Cancel conn's context once it returns, as a
// receive may still wait on it.
func (m *Manager) Attach(ctx context.Context, call *agentv1.AttachTerminalCall, conn Conn) (*agentv1.AttachTerminalResult, error) {
	t, err := m.get(call.GetId())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a, from, taken := t.attach(call.GetOffset(), conn, cancel)
	defer t.detach(a)
	if cols, rows := call.GetColumns(), call.GetRows(); cols > 0 && rows > 0 {
		t.resize(cols, rows)
	}
	// What runs in front draws itself again, so a full-screen program comes back whole.
	t.pty.redraw()
	if err := a.send(&agentv1.TerminalRequest{Stream: call.GetStream(), Offset: from, InputOffset: taken}); err != nil {
		return nil, err
	}
	received := make(chan error, 1)
	go func() { received <- t.receive(ctx, a, conn) }()
	for {
		data, off, end, moved, wait := t.next(a)
		switch {
		case moved:
			return &agentv1.AttachTerminalResult{Moved: true}, nil
		case data != nil:
			if err := a.send(&agentv1.TerminalRequest{Offset: off, Data: data}); err != nil {
				return nil, err
			}
			t.sentTo(a, off+uint64(len(data)))
			continue
		case end != nil:
			return &agentv1.AttachTerminalResult{End: end}, nil
		}
		select {
		case <-wait:
		case err := <-received:
			if ctx.Err() == nil && err != nil {
				return nil, err
			}
			return t.left(a, ctx.Err())
		case <-ctx.Done():
			return t.left(a, ctx.Err())
		}
	}
}

// Terminal is one shell on its pseudo-terminal.
type Terminal struct {
	m      *Manager
	id     string
	record string
	title  string
	opened time.Time
	pty    *pty
	cmd    *exec.Cmd
	exited chan struct{} // closed once the shell has exited

	mu            sync.Mutex
	out           *output
	changed       chan struct{} // closed and replaced whenever something here changes
	att           *attachment
	detachedSince time.Time
	lastInput     time.Time
	taken         uint64 // of all that's been typed
	input         []byte // typed, not yet written to the shell
	end           *dockwellv1.TerminalEnd
}

// notify wakes whatever waits on the terminal. Call it with mu held.
func (t *Terminal) notify() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *Terminal) start(cols, rows uint16) error {
	opts := t.m.opts
	acct := accountOf(cmpOr(opts.Passwd, "/etc/passwd"), os.Getuid())
	var cmd *exec.Cmd
	if len(opts.Shell) > 0 {
		// Not tied to a context: a shell ends when it's hung up, not when a call does.
		cmd = exec.CommandContext(context.WithoutCancel(context.Background()), opts.Shell[0], opts.Shell[1:]...) //nolint:gosec // a test's own shell
		acct.shell = opts.Shell[0]
	} else {
		if acct.shell == "" {
			return fmt.Errorf("this server has no shell for %s: /etc/passwd names none that runs, and there's no /bin/bash or /bin/sh. "+
				"Connect over SSH to fix %s's shell", acct.name, acct.name)
		}
		cmd = &exec.Cmd{Path: acct.shell, Args: []string{"-" + filepath.Base(acct.shell)}} // a login shell
	}
	if opts.Home != "" {
		acct.home = opts.Home
	}
	cmd.Env = environment(acct)
	cmd.Dir = "/"
	if st, err := os.Stat(acct.home); err == nil && st.IsDir() && unix.Access(acct.home, unix.X_OK) == nil {
		cmd.Dir = acct.home
	}
	p, err := start(cmd, cols, rows)
	if err != nil {
		return fmt.Errorf("can't start a shell: %w", err)
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	t.title = acct.name + "@" + cmpOr(host, "server")
	t.pty, t.cmd = p, cmd
	now := t.m.now()
	t.opened, t.lastInput, t.detachedSince = now, now, now
	read := make(chan struct{})
	go t.readOutput(read)
	go t.writeInput()
	go t.wait(read)
	return nil
}

// readOutput keeps what the shell prints, and waits while an attachment is Keep
// behind, so a slow reader holds up the shell as it would over SSH. With nobody
// attached it reads on, and keeps the newest.
func (t *Terminal) readOutput(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, chunk)
	for {
		t.mu.Lock()
		for t.end == nil && t.att != nil && t.out.end-t.att.sent >= Keep {
			wait := t.changed
			t.mu.Unlock()
			<-wait
			t.mu.Lock()
		}
		room := chunk
		if t.att != nil {
			room = min(room, Keep-int(t.out.end-t.att.sent)) //nolint:gosec // under Keep, as the wait above holds it there
		}
		ended := t.end != nil && t.end.GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED
		t.mu.Unlock()
		if ended {
			return
		}
		n, err := t.pty.Read(buf[:room])
		if n > 0 {
			t.mu.Lock()
			t.out.write(buf[:n])
			t.notify()
			t.mu.Unlock()
		}
		if err != nil {
			if !isHangUp(err) {
				t.m.logger.Warn("can't read a terminal; ending it", slog.String("terminal", t.id), slog.String("error", err.Error()))
			}
			return
		}
	}
}

// inputPiece is the most of what's typed one write to the shell carries.
const inputPiece = 4 << 10

// writeInput writes what's typed to the shell, in order, a piece at a time. A
// piece leaves the queue only once the shell has it, so what's being written still
// counts, and the queue never holds more than MaxInput, however long a write blocks.
func (t *Terminal) writeInput() {
	piece := make([]byte, 0, inputPiece)
	for {
		t.mu.Lock()
		for len(t.input) == 0 && t.end == nil {
			wait := t.changed
			t.mu.Unlock()
			<-wait
			t.mu.Lock()
		}
		if t.end != nil {
			t.mu.Unlock()
			return
		}
		piece = append(piece[:0], t.input[:min(len(t.input), inputPiece)]...)
		t.mu.Unlock()
		n, err := t.pty.Write(piece)
		t.mu.Lock()
		t.input = t.input[n:]
		if len(t.input) == 0 {
			t.input = nil
		}
		t.notify() // there's room for more
		t.mu.Unlock()
		if err != nil {
			if !isHangUp(err) {
				t.m.logger.Warn("can't write to a terminal", slog.String("terminal", t.id), slog.String("error", err.Error()))
			}
			return
		}
	}
}

// wait ends the terminal when its shell exits, once what it wrote last is read.
func (t *Terminal) wait(read <-chan struct{}) {
	err := t.cmd.Wait()
	close(t.exited)
	code := t.cmd.ProcessState.ExitCode()
	if status, ok := t.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = 128 + int(status.Signal())
	}
	message := fmt.Sprintf("The shell exited with code %d.", code)
	if code < 0 {
		message = fmt.Sprintf("The shell ended: %s.", err)
	}
	t.mu.Lock()
	if t.end == nil {
		t.end = &dockwellv1.TerminalEnd{Reason: dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED, ExitCode: int32(code), Message: message} //nolint:gosec // an exit status, at most 255, or 128 and a signal
	}
	t.mu.Unlock()
	// A program the shell left running can hold the terminal open; it's hung up
	// once this is done, as SSH does.
	select {
	case <-read:
	case <-time.After(drainWait):
	}
	t.finish()
}

// hangUp ends the terminal for reason, hangs up its shell, and kills it and what it
// started if they're still there after hangUpWait. It returns how the terminal
// ended, which is what came first.
func (t *Terminal) hangUp(reason dockwellv1.TerminalEndReason, message string) *dockwellv1.TerminalEnd {
	t.mu.Lock()
	if t.end == nil {
		t.end = &dockwellv1.TerminalEnd{Reason: reason, Message: message}
	}
	end := t.end
	t.notify()
	t.mu.Unlock()
	t.finish()
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGHUP)
		select {
		case <-t.exited:
		case <-time.After(hangUpWait):
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	return end
}

// finish closes the terminal's side of the pseudo-terminal, which hangs up the
// shell's, and tells the control plane how it ended, once.
func (t *Terminal) finish() {
	t.mu.Lock()
	first := t.end.GetTime() == nil
	if first {
		t.end.Time = timestamppb.New(t.m.now())
	}
	end := t.end
	t.notify()
	t.mu.Unlock()
	if !first {
		return
	}
	_ = t.pty.Close()
	m := t.m
	m.mu.Lock()
	delete(m.open, t.id)
	m.ended = append(m.ended, &agentv1.TerminalEnded{Id: t.id, Record: t.record, End: end, Opened: timestamppb.New(t.opened)})
	m.mu.Unlock()
	m.signal()
	m.logger.Info("a terminal ended", slog.String("terminal", t.id), slog.String("reason", end.GetReason().String()))
}

// attachment is one stream a terminal's bytes go over.
type attachment struct {
	cancel context.CancelFunc
	sendMu sync.Mutex
	conn   Conn
	sent   uint64 // what's gone, by the terminal's mu
	moved  bool   // by the terminal's mu
}

// attach makes conn the terminal's attachment, from off, moving whatever had it,
// and returns where it starts and how much of what's typed the terminal has taken.
func (t *Terminal) attach(off uint64, conn Conn, cancel context.CancelFunc) (a *attachment, from, taken uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev := t.att; prev != nil {
		prev.moved = true
		prev.cancel()
	}
	a = &attachment{cancel: cancel, conn: conn, sent: min(max(off, t.out.start), t.out.end)}
	t.att = a
	t.notify()
	return a, a.sent, t.taken
}

// left is how an attachment that stopped before the terminal ended went.
func (t *Terminal) left(a *attachment, err error) (*agentv1.AttachTerminalResult, error) {
	t.mu.Lock()
	moved := a.moved
	t.mu.Unlock()
	if moved {
		return &agentv1.AttachTerminalResult{Moved: true}, nil
	}
	return &agentv1.AttachTerminalResult{}, err
}

// detach lets the terminal go on without a, if it still has it.
func (t *Terminal) detach(a *attachment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.att == a {
		t.att = nil
		t.detachedSince = t.m.now()
		t.notify()
	}
}

func (a *attachment) send(m *agentv1.TerminalRequest) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return a.conn.Send(m)
}

// next returns what a has yet to send, or how the terminal ended once a has sent
// all it printed, or that another attachment took a's place, or what to wait on.
func (t *Terminal) next(a *attachment) (data []byte, off uint64, end *dockwellv1.TerminalEnd, moved bool, wait <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a.moved {
		return nil, 0, nil, true, nil
	}
	if a.sent < t.out.start {
		a.sent = t.out.start // what came between is lost; the offset says so
	}
	if a.sent < t.out.end {
		return t.out.read(a.sent, chunk), a.sent, nil, false, nil
	}
	// Once the shell exits, what it wrote last is read before it counts as ended.
	if t.end != nil && t.end.GetTime() != nil {
		return nil, 0, t.end, false, nil
	}
	return nil, 0, nil, false, t.changed
}

func (t *Terminal) sentTo(a *attachment, off uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a.sent = max(a.sent, off)
	t.notify()
}

// receive takes what's typed, and the terminal's size, from conn, and answers each
// write with how much the terminal has taken.
func (t *Terminal) receive(ctx context.Context, a *attachment, conn Conn) error {
	for {
		down, err := conn.Receive()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cols, rows := down.GetColumns(), down.GetRows(); cols > 0 && rows > 0 {
			t.resize(cols, rows)
		}
		if len(down.GetData()) == 0 {
			continue // a resize alone has no answer
		}
		taken, ok := t.accept(a, down.GetOffset(), down.GetData())
		if !ok {
			return nil // moved
		}
		if err := a.send(&agentv1.TerminalRequest{InputOffset: taken, Taken: true}); err != nil {
			return err
		}
	}
}

// accept takes what's typed from off, skipping what the terminal has taken already,
// as far as there's room, and reports how much it has taken in all.
func (t *Terminal) accept(a *attachment, off uint64, data []byte) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.att != a {
		return t.taken, false
	}
	if off > t.taken || len(data) == 0 {
		return t.taken, true // a gap: the client sends again from what it's told
	}
	skip := t.taken - off
	if skip >= uint64(len(data)) {
		return t.taken, true
	}
	data = data[skip:]
	n := min(len(data), MaxInput-len(t.input))
	if n <= 0 {
		return t.taken, true
	}
	t.input = append(t.input, data[:n]...)
	t.taken += uint64(n)
	t.lastInput = t.m.now()
	t.notify()
	return t.taken, true
}

func (t *Terminal) resize(cols, rows uint32) {
	if err := t.pty.resize(clampColumns(cols), clampRows(rows)); err != nil && !isHangUp(err) {
		t.m.logger.Warn("can't resize a terminal", slog.String("terminal", t.id), slog.String("error", err.Error()))
	}
}

func clampColumns(n uint32) uint16 { return uint16(min(max(n, 2), MaxColumns)) }
func clampRows(n uint32) uint16    { return uint16(min(max(n, 1), MaxRows)) }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
