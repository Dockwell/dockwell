// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func TestOutputKeepsTheNewest(t *testing.T) {
	o := newOutput(8)
	o.write([]byte("abc"))
	if got := string(o.read(0, 100)); got != "abc" || o.start != 0 || o.end != 3 {
		t.Fatalf("read %q, kept %d to %d", got, o.start, o.end)
	}
	o.write([]byte("defgh"))
	o.write([]byte("ij"))
	if got := string(o.read(0, 100)); got != "cdefghij" || o.start != 2 || o.end != 10 {
		t.Fatalf("after wrapping, read %q, kept %d to %d", got, o.start, o.end)
	}
	if got := string(o.read(5, 3)); got != "fgh" {
		t.Errorf("read 3 from 5 = %q", got)
	}
	o.write([]byte("0123456789AB"))
	if got := string(o.read(0, 100)); got != "456789AB" || o.start != 14 || o.end != 22 {
		t.Fatalf("after more than it keeps, read %q, kept %d to %d", got, o.start, o.end)
	}
	if got := o.read(22, 10); got != nil {
		t.Errorf("read past the end = %q", got)
	}
}

func TestOutputGrowsAsItFills(t *testing.T) {
	small := newOutput(Keep)
	small.write([]byte("x"))
	if len(small.buf) != 4096 {
		t.Errorf("a ring for one byte holds %d", len(small.buf))
	}
	o := newOutput(Keep)
	var want bytes.Buffer
	for i := range 40000 {
		line := fmt.Sprintf("line %d\n", i)
		want.WriteString(line)
		o.write([]byte(line))
	}
	if len(o.buf) != Keep {
		t.Errorf("a full ring holds %d, want %d", len(o.buf), Keep)
	}
	all := want.Bytes()
	if o.end != uint64(len(all)) || o.end-o.start != Keep {
		t.Fatalf("kept %d to %d of %d", o.start, o.end, len(all))
	}
	if got := o.read(0, Keep); !bytes.Equal(got, all[len(all)-Keep:]) {
		t.Errorf("kept %d bytes that aren't the newest", len(got))
	}
}

// fakeConn stands in for the stream to the control plane.
type fakeConn struct {
	ctx   context.Context
	ups   chan *agentv1.TerminalRequest
	downs chan *agentv1.TerminalResponse
}

func newConn(ctx context.Context) *fakeConn {
	return &fakeConn{ctx: ctx, ups: make(chan *agentv1.TerminalRequest, 1024), downs: make(chan *agentv1.TerminalResponse, 16)}
}

func (c *fakeConn) Send(m *agentv1.TerminalRequest) error {
	select {
	case c.ups <- m:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *fakeConn) Receive() (*agentv1.TerminalResponse, error) {
	select {
	case d := <-c.downs:
		return d, nil
	case <-c.ctx.Done():
		return nil, io.EOF
	}
}

// session is one attachment, and what came of it.
type session struct {
	t      *testing.T
	conn   *fakeConn
	cancel context.CancelFunc
	result chan *agentv1.AttachTerminalResult
	errs   chan error
	out    bytes.Buffer
	first  *agentv1.TerminalRequest
	next   uint64 // where what's typed goes next
}

func attach(t *testing.T, m *Manager, offset uint64) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &session{t: t, conn: newConn(ctx), cancel: cancel, result: make(chan *agentv1.AttachTerminalResult, 1), errs: make(chan error, 1)}
	t.Cleanup(cancel)
	go func() {
		r, err := m.Attach(ctx, &agentv1.AttachTerminalCall{Id: "trm_1", Stream: 7, Offset: offset, Columns: 80, Rows: 24}, s.conn)
		s.errs <- err
		s.result <- r
		cancel()
	}()
	select {
	case s.first = <-s.conn.ups:
	case err := <-s.errs:
		t.Fatalf("attaching: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("attaching sent nothing")
	}
	if s.first.GetStream() != 7 {
		t.Fatalf("the first message names stream %d", s.first.GetStream())
	}
	s.next = s.first.GetInputOffset()
	return s
}

// typed types text, and returns how much the terminal says it has taken.
func (s *session) typed(text string) uint64 {
	s.t.Helper()
	s.conn.downs <- &agentv1.TerminalResponse{Offset: s.next, Data: []byte(text)}
	for {
		m := s.recv()
		if m.GetTaken() {
			s.next = m.GetInputOffset()
			return s.next
		}
	}
}

func (s *session) recv() *agentv1.TerminalRequest {
	s.t.Helper()
	select {
	case m := <-s.conn.ups:
		if len(m.GetData()) > 0 {
			s.out.Write(m.GetData())
		}
		return m
	case <-time.After(10 * time.Second):
		s.t.Fatalf("the terminal sent nothing more; it printed %q", s.out.String())
		return nil
	}
}

// waitFor reads what the terminal prints until it holds text.
func (s *session) waitFor(text string) {
	s.t.Helper()
	for !strings.Contains(s.out.String(), text) {
		s.recv()
	}
}

func (s *session) ended() *agentv1.AttachTerminalResult {
	s.t.Helper()
	for {
		select {
		case m := <-s.conn.ups:
			s.out.Write(m.GetData())
		case r := <-s.result:
			for len(s.conn.ups) > 0 {
				s.out.Write((<-s.conn.ups).GetData())
			}
			return r
		case <-time.After(10 * time.Second):
			s.t.Fatalf("the attachment didn't end; it printed %q", s.out.String())
		}
	}
}

func shellManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	opts.Shell = []string{"/bin/sh"}
	opts.Home = t.TempDir()
	m := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return m
}

func open(t *testing.T, m *Manager, id string) *agentv1.OpenTerminalResult {
	t.Helper()
	r, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: id, Columns: 80, Rows: 24, Record: "evt_" + id})
	if err != nil {
		t.Fatalf("opening %s: %v", id, err)
	}
	return r
}

func takeEnded(t *testing.T, m *Manager, id string) *agentv1.TerminalEnded {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var kept []*agentv1.TerminalEnded
	defer func() { m.Untold(kept) }()
	for {
		for _, e := range m.TakeEnded() {
			if e.GetId() == id {
				return e
			}
			kept = append(kept, e)
		}
		select {
		case <-m.Ended():
		case <-deadline:
			t.Fatalf("%s didn't end", id)
		}
	}
}

func TestAShellRunsTypesAndExits(t *testing.T) {
	m := shellManager(t, Options{})
	r := open(t, m, "trm_1")
	if !strings.Contains(r.GetTitle(), "@") || r.GetOpened() == nil {
		t.Errorf("opened as %v", r)
	}
	s := attach(t, m, 0)
	if n := s.typed("echo $((6*7)) $TERM $GOMEMLIMIT.\n"); n != uint64(len("echo $((6*7)) $TERM $GOMEMLIMIT.\n")) {
		t.Errorf("took %d bytes", n)
	}
	s.waitFor("42 xterm-256color .")
	// The terminal is the shell's controlling terminal, with the size it was given.
	s.typed("stty size; tty\n")
	s.waitFor("24 80")
	s.waitFor("/dev/pts/")
	s.conn.downs <- &agentv1.TerminalResponse{Columns: 132, Rows: 40}
	s.typed("stty size\n")
	s.waitFor("40 132")
	s.typed("exit 3\n")
	res := s.ended()
	if res.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED || res.GetEnd().GetExitCode() != 3 ||
		res.GetEnd().GetTime() == nil || res.GetEnd().GetMessage() != "The shell exited with code 3." {
		t.Errorf("ended with %v", res)
	}
	ended := takeEnded(t, m, "trm_1")
	if ended.GetRecord() != "evt_trm_1" || ended.GetEnd().GetExitCode() != 3 || ended.GetOpened() == nil {
		t.Errorf("told the end as %v", ended)
	}
	if got := m.Known(); len(got) != 0 {
		t.Errorf("knows %v once it's told", got)
	}
	if _, err := m.Close("trm_1", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED); !errors.Is(err, ErrNotFound) {
		t.Errorf("closing an ended terminal = %v", err)
	}
}

// What's typed carries its offset, so a write sent again types nothing twice, and
// one past what the terminal has is refused for the client to send again.
func TestWhatsTypedGoesOnce(t *testing.T) {
	m := shellManager(t, Options{})
	open(t, m, "trm_1")
	s := attach(t, m, 0)
	first := "echo one"
	s.typed(first)
	// The same write again, and one that overlaps it, as after a dropped call.
	s.next = 0
	if n := s.typed(first); n != uint64(len(first)) {
		t.Errorf("a write sent again took the terminal to %d", n)
	}
	s.next = 4
	if n := s.typed(" one; echo two\n"); n != uint64(len(first)+len("; echo two\n")) {
		t.Errorf("an overlapping write took the terminal to %d", n)
	}
	s.waitFor("one\r\ntwo\r\n")
	if strings.Contains(s.out.String(), "one one") {
		t.Errorf("typed twice: %q", s.out.String())
	}
	at := s.next
	s.next = at + 10
	if n := s.typed("lost"); n != at {
		t.Errorf("a write past what it has took the terminal to %d, want %d", n, at)
	}
}

// A terminal outlives its attachment, keeps what it printed meanwhile, and the next
// attachment carries on from where the last left off. One that attaches from
// elsewhere takes it, and the first hears that it moved.
func TestATerminalOutlivesItsAttachment(t *testing.T) {
	m := shellManager(t, Options{})
	open(t, m, "trm_1")
	s := attach(t, m, 0)
	s.typed("echo before$((1))\n")
	s.waitFor("before1\r\n")
	had := uint64(s.out.Len())
	s.cancel()
	if r := s.ended(); r.GetMoved() || r.GetEnd() != nil {
		t.Errorf("a dropped attachment ended with %v", r)
	}

	again := attach(t, m, had)
	if again.first.GetOffset() != had || again.first.GetInputOffset() != uint64(len("echo before$((1))\n")) {
		t.Errorf("attached again at %d, typed %d", again.first.GetOffset(), again.first.GetInputOffset())
	}
	again.typed("echo after$((2))\n")
	again.waitFor("after2\r\n")
	if strings.Contains(again.out.String(), "before") {
		t.Errorf("sent again what it had: %q", again.out.String())
	}

	// From the start, it has all it kept.
	all := attach(t, m, 0)
	all.waitFor("after2\r\n")
	if !strings.Contains(all.out.String(), "before1") {
		t.Errorf("from 0 it sent %q", all.out.String())
	}
	if r := again.ended(); !r.GetMoved() {
		t.Errorf("the attachment that was moved ended with %v", r)
	}
	if m.Known()[0] != "trm_1" {
		t.Errorf("knows %v", m.Known())
	}
}

func TestATerminalNobodyWatchesCloses(t *testing.T) {
	m := shellManager(t, Options{DetachedFor: 300 * time.Millisecond})
	open(t, m, "trm_1")
	s := attach(t, m, 0)
	time.Sleep(500 * time.Millisecond)
	s.typed("echo still $((1)) here\n")
	s.waitFor("still 1 here\r\n")
	s.cancel()
	s.ended()
	ended := takeEnded(t, m, "trm_1")
	if ended.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_DETACHED ||
		!strings.Contains(ended.GetEnd().GetMessage(), "Nobody was connected to it") {
		t.Errorf("ended with %v", ended)
	}
}

func TestATerminalNothingIsTypedIntoCloses(t *testing.T) {
	m := shellManager(t, Options{IdleFor: 400 * time.Millisecond})
	open(t, m, "trm_1")
	s := attach(t, m, 0)
	for range 4 {
		time.Sleep(200 * time.Millisecond)
		s.typed(":\n")
	}
	res := s.ended()
	if res.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_IDLE {
		t.Errorf("ended with %v", res)
	}
}

// Closing hangs up the shell, and what it runs in front, which ends whatever it
// was doing.
func TestClosingHangsUp(t *testing.T) {
	m := shellManager(t, Options{})
	open(t, m, "trm_1")
	s := attach(t, m, 0)
	s.typed("sleep 600\n")
	time.Sleep(100 * time.Millisecond)
	end, err := m.Close("trm_1", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED)
	if err != nil || end.GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED || end.GetMessage() != "It was closed." {
		t.Fatalf("closing = %v, %v", end, err)
	}
	if r := s.ended(); r.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED {
		t.Errorf("the attachment ended with %v", r)
	}
	if e := takeEnded(t, m, "trm_1"); e.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED {
		t.Errorf("told the end as %v", e)
	}
}

func TestAServerKeepsEightTerminals(t *testing.T) {
	m := shellManager(t, Options{})
	for i := range MaxOpen {
		open(t, m, fmt.Sprintf("trm_%d", i))
	}
	_, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_9", Columns: 80, Rows: 24})
	if err == nil || !strings.Contains(err.Error(), "8 terminals open") {
		t.Errorf("a ninth = %v", err)
	}
	if _, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_0"}); err == nil {
		t.Error("opened a terminal with an ID that's taken")
	}
	if _, err := m.Close("trm_0", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED); err != nil {
		t.Fatal(err)
	}
	open(t, m, "trm_9")
	if got := len(m.Known()); got != MaxOpen+1 {
		t.Errorf("knows %d, with one end untold", got)
	}
}

// A shell that prints faster than its reader takes waits for it, and loses nothing
// on the way.
func TestASlowReaderHoldsTheShell(t *testing.T) {
	m := shellManager(t, Options{})
	open(t, m, "trm_1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn := &slowConn{fakeConn: newConn(ctx)}
	done := make(chan *agentv1.AttachTerminalResult, 1)
	go func() {
		r, _ := m.Attach(ctx, &agentv1.AttachTerminalCall{Id: "trm_1", Stream: 1, Columns: 80, Rows: 24}, conn)
		done <- r
	}()
	<-conn.ups
	lines := 200000 // about 1.3 MB, five times what's kept
	conn.downs <- &agentv1.TerminalResponse{Data: []byte(fmt.Sprintf("stty -echo; echo start-$((1)); seq 1 %d; echo done-$((1+1))\n", lines))}
	var out bytes.Buffer
	deadline := time.After(60 * time.Second)
	for !bytes.Contains(out.Bytes(), []byte("done-2")) {
		select {
		case m := <-conn.ups:
			out.Write(m.GetData())
		case <-deadline:
			t.Fatalf("read %d bytes", out.Len())
		}
	}
	// Every line, in order, as seq wrote them.
	text := strings.ReplaceAll(out.String(), "\r\n", "\n")
	_, text, _ = strings.Cut(text, "start-1\n")
	text, _, _ = strings.Cut(text, "done-2")
	got := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(got) != lines {
		t.Fatalf("read %d lines, want %d", len(got), lines)
	}
	for i, line := range got {
		if line != strconv.Itoa(i+1) {
			t.Fatalf("line %d reads %q", i+1, line)
		}
	}
}

// slowConn takes what the terminal prints slowly.
type slowConn struct {
	*fakeConn
}

func (c *slowConn) Send(m *agentv1.TerminalRequest) error {
	if len(m.GetData()) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	return c.fakeConn.Send(m)
}

func TestRootsShell(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	write := func(s string) {
		if err := os.WriteFile(passwd, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("daemon:x:1:1::/usr/sbin:/usr/sbin/nologin\ntoor:x:0:0:root:/var/root:/bin/sh\n")
	if a := accountOf(passwd, 0); a.name != "toor" || a.home != "/var/root" || a.shell != "/bin/sh" {
		t.Errorf("read %+v", a)
	}
	write("root:x:0:0:root:/root:/usr/sbin/nologin\n")
	if a := accountOf(passwd, 0); a.shell == "" || a.shell == "/usr/sbin/nologin" {
		t.Errorf("with a shell that refuses a login, root's is %q", a.shell)
	}
	if a := accountOf(filepath.Join(dir, "none"), 0); a.name != "root" || a.home != "/root" || a.shell == "" {
		t.Errorf("with no passwd, root is %+v", a)
	}
	write("root:x:0:0:root:/root:/bin/sh\nmira:x:1000:1000::/home/mira:/bin/sh\n")
	if a := accountOf(passwd, 1000); a.name != "mira" || a.home != "/home/mira" {
		t.Errorf("an agent run as 1000 runs %+v", a)
	}
	env := strings.Join(environment(account{name: "root", home: "/root", shell: "/bin/bash"}), " ")
	if !strings.Contains(env, "TERM=xterm-256color") || !strings.Contains(env, "HOME=/root") || strings.Contains(env, "GOMEMLIMIT") {
		t.Errorf("starts with %s", env)
	}
}

// What's typed into a shell that reads none of it waits in the terminal, at most
// MaxInput of it, whatever's being written: a paste of a megabyte into a program
// that doesn't read holds the paste, not the megabyte. Past the kernel's own
// buffer, the terminal takes no more until the shell reads.
func TestInputHeldStaysWithinItsLimit(t *testing.T) {
	m := shellManager(t, Options{})
	open(t, m, "trm_1")
	s := attach(t, m, 0)
	s.typed("stty -echo -icanon; sleep 30\n")
	time.Sleep(300 * time.Millisecond)
	start := s.next
	paste := strings.Repeat("x", MaxInput)
	var took uint64
	for stalled := 0; stalled < 5; {
		n := s.typed(paste)
		if n == took {
			stalled++
			time.Sleep(50 * time.Millisecond)
			continue
		}
		took = n
	}
	// What the kernel's terminal buffer holds, which isn't the agent's to count.
	const kernel = 128 << 10
	if held := took - start; held > MaxInput+kernel {
		t.Errorf("took %d KiB of what's typed into a shell that reads none, want at most %d KiB and the kernel's buffer", held>>10, MaxInput>>10)
	}
	term, err := m.get("trm_1")
	if err != nil {
		t.Fatal(err)
	}
	term.mu.Lock()
	queued := len(term.input)
	term.mu.Unlock()
	if queued > MaxInput {
		t.Errorf("holds %d KiB, more than MaxInput", queued>>10)
	}
}

// Closing a container's shell signals the PID Docker gave only while Docker still
// says it's the shell's: one that ended, and whose PID may be another process's
// now, is left alone.
func TestClosingSignalsOnlyTheShell(t *testing.T) {
	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- other.Wait() }()
	t.Cleanup(func() { _ = other.Process.Kill() })
	pid := other.Process.Pid
	signalExec(pid, syscall.SIGTERM, func() bool { return false })
	select {
	case err := <-done:
		t.Fatalf("signalled a process the exec no longer says is its shell: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	signalExec(pid, syscall.SIGTERM, func() bool { return true })
	select {
	case err := <-done:
		if err == nil {
			t.Error("the shell exited by itself, rather than on the signal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shell wasn't signalled")
	}
}
