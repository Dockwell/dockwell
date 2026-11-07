// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// deadMount stands in for a mount whose far end is down: a call that touches a
// path under it never comes back, whatever its context says, until the test ends
// or lets it go.
type deadMount struct {
	path  string
	gone  chan struct{}
	once  sync.Once
	stuck atomic.Int32 // calls held right now
	seen  atomic.Int32 // times a call touched it
}

func stopAnswering(t *testing.T, s *Server, path string) *deadMount {
	t.Helper()
	m := &deadMount{path: path, gone: make(chan struct{})}
	s.AnswerWithin = 50 * time.Millisecond
	s.Touching = func(p string) {
		if p == m.path || strings.HasPrefix(p, m.path+"/") {
			m.seen.Add(1)
			m.stuck.Add(1)
			defer m.stuck.Add(-1)
			<-m.gone
		}
	}
	t.Cleanup(m.answer)
	return m
}

// answer brings it back, and what was held returns.
func (m *deadMount) answer() { m.once.Do(func() { close(m.gone) }) }

// within fails a test whose call takes longer than a person should wait.
func within[T any](t *testing.T, limit time.Duration, call func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- call() }()
	select {
	case v := <-done:
		return v
	case <-time.After(limit):
		t.Fatalf("still waiting after %s", limit)
		panic("unreachable")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAPathThatStopsAnsweringSaysSoWithinItsBound(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	write(t, filepath.Join(dead, "f"), "x")
	m := stopAnswering(t, d.server, dead)

	got := within(t, 2*time.Second, func() *agentv1.FileRefusal { return d.read(t, filepath.Join(dead, "f")).GetRefusal() })
	if got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || !strings.Contains(got.GetMessage(), "isn't answering") {
		t.Fatalf("got %v", got)
	}
	got = within(t, 2*time.Second, func() *agentv1.FileRefusal { return d.list(t, dead).GetRefusal() })
	if got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("listing it: %v", got)
	}
	// The first call is held. The second heard from what the first found, and
	// held nothing more: a dead mount costs one wait, not one for each click.
	if n := m.seen.Load(); n != 1 {
		t.Errorf("%d calls went into it, want 1", n)
	}
}

func TestWhatsLeftBehindDoesntStopTheRest(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	write(t, filepath.Join(dead, "f"), "x")
	well := filepath.Join(d.root, "well")
	write(t, well, "fine")
	stopAnswering(t, d.server, dead)
	var calls sync.WaitGroup
	for range slots * 2 { // every slot, and then some
		calls.Go(func() {
			release, err := d.server.Take(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			_, _ = d.server.Read(t.Context(), filepath.Join(dead, "f"))
		})
	}
	within(t, 5*time.Second, func() bool { calls.Wait(); return true })
	got := within(t, 2*time.Second, func() *agentv1.ReadFileResult { return d.read(t, well) })
	if got.GetFile().GetContent() != "fine" {
		t.Fatalf("a healthy path reads as %v", got)
	}
}

func TestAFolderWithALinkIntoADeadMountStillLists(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "site")
	write(t, filepath.Join(dir, "index.html"), "x")
	dead := filepath.Join(d.root, "mnt")
	write(t, filepath.Join(dead, "f"), "x")
	link(t, dead, filepath.Join(dir, "uploads"))
	stopAnswering(t, d.server, filepath.Join(dir, "uploads"))

	first := within(t, 2*time.Second, func() *agentv1.ListFilesResult { return d.list(t, dir) })
	if first.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("got %v", first)
	}
	// Once it's known, the folder lists around it.
	again := within(t, 2*time.Second, func() *agentv1.ListFilesResult { return d.list(t, dir) })
	if got := names(again.GetListing()); len(got) != 2 {
		t.Fatalf("got %v, refusal %v", got, again.GetRefusal())
	}
}

// What's left behind is held by a call that can't be stopped, so it's bounded. Past
// that a call doesn't start at all, and says why, rather than leave another behind.
func TestWhatsLeftBehindIsBounded(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	well := filepath.Join(d.root, "well")
	write(t, well, "fine")
	s := d.server
	s.AnswerWithin = 20 * time.Millisecond
	gone := make(chan struct{})
	var once sync.Once
	back := func() { once.Do(func() { close(gone) }) }
	t.Cleanup(back)
	s.Touching = func(p string) {
		if strings.HasPrefix(p, dead+"/") {
			<-gone
		}
	}
	for i := range maxStuck * 3 {
		write(t, filepath.Join(dead, itoa(i), "f"), "x")
	}
	before := runtime.NumGoroutine()
	for i := range maxStuck * 3 { // paths apart, so nothing here is known yet
		_, err := s.Read(t.Context(), filepath.Join(dead, itoa(i), "f"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := s.stuckCount(); n != maxStuck {
		t.Errorf("%d calls left behind, want %d", n, maxStuck)
	}
	if n := runtime.NumGoroutine() - before; n > maxStuck+2 {
		t.Errorf("%d more goroutines, with %d left behind", n, maxStuck)
	}
	got := within(t, time.Second, func() *agentv1.ReadFileResult { return d.read(t, well) })
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || !strings.Contains(got.GetRefusal().GetMessage(), "aren't answering") {
		t.Fatalf("a healthy path, past the limit: %v", got)
	}
	// As they come back, everything works again, with nothing to restart.
	back()
	waitFor(t, "what was left behind to return", func() bool { return s.stuckCount() == 0 })
	if got := d.read(t, well); got.GetFile().GetContent() != "fine" {
		t.Fatalf("after they came back: %v", got)
	}
}

// Everything on a mount is down together, so one call left behind keeps the rest
// away from all of it, while a disk's other paths carry on.
func TestOneCallStuckOnARemoteMountKeepsTheRestOffIt(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	write(t, filepath.Join(dead, "a", "f"), "x")
	write(t, filepath.Join(dead, "b", "f"), "x")
	write(t, filepath.Join(d.root, "well"), "fine")
	d.server.mounts = func() string {
		return "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n40 22 0:50 / " + dead + " rw - nfs4 filer:/export rw\n"
	}
	stopAnswering(t, d.server, dead+"/a")
	first := within(t, 2*time.Second, func() *agentv1.FileRefusal { return d.read(t, filepath.Join(dead, "a", "f")).GetRefusal() })
	if first.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || first.GetPath() != dead {
		t.Fatalf("got %v", first)
	}
	// A sibling on the same mount is told at once, and takes no place of its own.
	got := within(t, time.Second, func() *agentv1.FileRefusal { return d.read(t, filepath.Join(dead, "b", "f")).GetRefusal() })
	if got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("a sibling: %v", got)
	}
	if n := d.server.stuckCount(); n != 1 {
		t.Errorf("%d calls left behind, want 1", n)
	}
	if got := d.read(t, filepath.Join(d.root, "well")); got.GetFile().GetContent() != "fine" {
		t.Errorf("a path off the mount reads as %v", got)
	}
}

func TestACallThatComesBackLateSettlesItself(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	write(t, filepath.Join(dead, "f"), "x")
	m := stopAnswering(t, d.server, dead)
	if got := d.read(t, filepath.Join(dead, "f")).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("got %v", got)
	}
	m.answer()
	waitFor(t, "the path to answer again", func() bool { return d.server.stuckCount() == 0 })
	if got := d.read(t, filepath.Join(dead, "f")); got.GetFile().GetContent() != "x" {
		t.Fatalf("it reads as %v", got)
	}
}

func TestACallerThatLeavesIsntKeptWaiting(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	write(t, filepath.Join(dead, "f"), "x")
	m := stopAnswering(t, d.server, dead)
	d.server.AnswerWithin = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := d.server.Read(ctx, filepath.Join(dead, "f"))
		done <- err
	}()
	waitFor(t, "the call to reach the mount", func() bool { return m.stuck.Load() == 1 })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a call whose caller left answered as if nothing happened")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled call is still waiting on a path that won't answer")
	}
}
