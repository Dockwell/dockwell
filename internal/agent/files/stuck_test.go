// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"errors"
	"os"
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
	s.AnswerWithin = roomy
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

// roomy is how long a path has to answer in a test that holds one step and needs
// every step before it to finish first. With the race detector and coverage on, a
// loaded runner can take tens of milliseconds over steps that are quick anywhere
// else, and then the call is left behind early, at the wrong step.
const roomy = 500 * time.Millisecond

// held is how long a path has to answer in a test that holds a save past the flush
// of its copy. A busy runner's disk can take longer than roomy over that, and a save
// left behind there stops itself before the step the test holds. The step is held
// until the test lets it go, so the deadline runs out there however long it is.
const held = 2 * time.Second

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
	s.AnswerWithin = roomy
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
	if n := s.stuckCount(); n != stopAt {
		t.Errorf("%d calls left behind, want %d", n, stopAt)
	}
	if n := runtime.NumGoroutine() - before; n > stopAt+2 {
		t.Errorf("%d more goroutines, with %d left behind", n, stopAt)
	}
	got := within(t, time.Second, func() *agentv1.ReadFileResult { return d.read(t, well) })
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || !strings.Contains(got.GetRefusal().GetMessage(), "aren't answering") {
		t.Fatalf("a healthy path, past the limit: %v", got)
	}
	// As they come back, everything works again, with nothing to restart.
	back()
	waitFor(t, "what was left behind to return", func() bool { return s.stuckCount() == 0 })
	// A healthy read has only AnswerWithin too, which a loaded machine can outlast,
	// and then it's left behind for a moment itself. So this tries again, as a
	// person would, until it reads.
	var last *agentv1.ReadFileResult
	if !eventually(func() bool { last = d.read(t, well); return last.GetFile().GetContent() == "fine" }) {
		t.Fatalf("after they came back: %v", last)
	}
}

// reach waits for the step a test holds. If the call was left behind before it got
// there, it never comes, so this fails in a few seconds rather than hang the package
// until go test's own timeout.
func reach(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the save never reached the step this test holds, so it was left behind before it")
	}
}

// eventually reports whether ok holds within 5 s, trying it again until then.
func eventually(ok func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
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

// Calls under way as the last place goes can be left behind too, and they count: the
// limit holds with every slot in flight.
func TestTheLimitHoldsWithCallsInFlight(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	s := d.server
	s.AnswerWithin = roomy
	gone := make(chan struct{})
	var once sync.Once
	back := func() { once.Do(func() { close(gone) }) }
	t.Cleanup(back)
	s.Touching = func(p string) {
		if strings.HasPrefix(p, dead+"/") {
			<-gone
		}
	}
	for i := range maxStuck * 2 {
		write(t, filepath.Join(dead, itoa(i), "f"), "x")
	}
	for i := range maxStuck - 1 { // as near the limit as calls will go, one at a time
		if _, err := s.Read(t.Context(), filepath.Join(dead, itoa(i), "f")); err != nil {
			t.Fatal(err)
		}
	}
	var calls sync.WaitGroup
	for i := range slots {
		calls.Go(func() {
			release, err := s.Take(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			_, _ = s.Read(t.Context(), filepath.Join(dead, itoa(maxStuck+i), "f"))
		})
	}
	calls.Wait()
	if n := s.stuckCount(); n > maxStuck {
		t.Errorf("%d calls left behind, with a limit of %d", n, maxStuck)
	}
	back()
	waitFor(t, "them to come back", func() bool { return s.stuckCount() == 0 })
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

func TestASaveThatDoesntAnswerSaysItMayStillLand(t *testing.T) {
	d := newDisk(t)
	dead := filepath.Join(d.root, "mnt")
	p := filepath.Join(dead, "f")
	write(t, p, "before")
	v := d.read(t, p).GetFile().GetVersion()
	stopAnswering(t, d.server, dead)
	got := within(t, 2*time.Second, func() *agentv1.FileRefusal { return d.save(t, p, "after", v).GetRefusal() })
	if got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || !strings.Contains(got.GetMessage(), "Nothing was saved") {
		t.Fatalf("got %v", got)
	}
}

// stallCopy holds each save in the flush of its copy, past the walk, the first
// comparison and everything else it does before it writes, as a struggling disk does.
func stallCopy(t *testing.T, s *Server) (reached chan struct{}, answer func()) {
	t.Helper()
	s.AnswerWithin = roomy
	reached = make(chan struct{}, 8)
	gone := make(chan struct{})
	answer = sync.OnceFunc(func() { close(gone) })
	syncSpare = func(f *os.File) error {
		reached <- struct{}{}
		<-gone
		return f.Sync()
	}
	t.Cleanup(func() { answer(); syncSpare = (*os.File).Sync })
	return reached, answer
}

// A person told a save didn't land has moved on, so one that comes back writes
// nothing, as the file is theirs again. All the checks it passed were before the
// flush that held it.
func TestASaveLeftBehindWritesNothingWhenItComesBack(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "mnt")
	p := filepath.Join(dir, "f")
	write(t, p, "before")
	v := d.read(t, p).GetFile().GetVersion()
	reached, answer := stallCopy(t, d.server)
	got := d.save(t, p, "after", v).GetRefusal()
	if got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || !strings.Contains(got.GetMessage(), "Nothing was saved") {
		t.Fatalf("got %v", got)
	}
	reach(t, reached)
	answer()
	waitFor(t, "the call to come back", func() bool { return d.server.stuckCount() == 0 })
	if content, _ := os.ReadFile(p); string(content) != "before" {
		t.Fatalf("a save the person gave up on landed: %q", content)
	}
	if left := spares(t, dir); len(left) != 0 {
		t.Errorf("it left %v", left)
	}
	select {
	case ended := <-d.server.SavesEnded():
		t.Errorf("a save that wrote nothing has something to say: %v", ended)
	default:
	}
}

// A stall after the copy, with the second reading under way, comes back past
// everything but the last look at whoever asked.
func TestASaveLeftBehindAsItReadsAgainWritesNothing(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "app", "conf")
	write(t, p, "before")
	v := d.read(t, p).GetFile().GetVersion()
	d.server.AnswerWithin = held
	// The flush isn't the step this holds, so it doesn't wait on the disk.
	var flushed atomic.Bool
	syncSpare = func(*os.File) error {
		flushed.Store(true)
		return nil
	}
	t.Cleanup(func() { syncSpare = (*os.File).Sync })
	reached, gone := make(chan struct{}, 1), make(chan struct{})
	d.server.Touching = func(at string) {
		if at == p && flushed.Load() {
			reached <- struct{}{}
			<-gone
		}
	}
	if got := d.save(t, p, "after", v).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("got %v", got)
	}
	reach(t, reached)
	close(gone)
	waitFor(t, "the call to come back", func() bool { return d.server.stuckCount() == 0 })
	if content, _ := os.ReadFile(p); string(content) != "before" {
		t.Fatalf("a save the person gave up on landed: %q", content)
	}
}

// Through a link it's the same, by the path a link led to.
func TestASaveLeftBehindOnALinkWritesNothingWhenItComesBack(t *testing.T) {
	d := newDisk(t)
	target := filepath.Join(d.root, "app", "conf")
	write(t, target, "before")
	via := filepath.Join(d.root, "current")
	link(t, target, via)
	v := d.read(t, via).GetFile().GetVersion()
	reached, answer := stallCopy(t, d.server)
	if got := d.save(t, via, "after", v).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("got %v", got)
	}
	reach(t, reached)
	answer()
	waitFor(t, "the call to come back", func() bool { return d.server.stuckCount() == 0 })
	if content, _ := os.ReadFile(target); string(content) != "before" {
		t.Fatalf("a save the person gave up on landed: %q", content)
	}
}

// The flush can take a while, and what writes in place meanwhile isn't undone: a
// save compares the file after it, not only before.
func TestAFileChangedAsTheCopyFlushesIsntSavedOver(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "app")
	p := filepath.Join(dir, "conf")
	write(t, p, "before")
	v := d.read(t, p).GetFile().GetVersion()
	syncSpare = func(f *os.File) error {
		other, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if _, err := other.WriteString("theirs"); err != nil {
			t.Fatal(err)
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncSpare = (*os.File).Sync })
	got := d.save(t, p, "after", v).GetRefusal()
	if got.GetKind() != agentv1.FileRefusal_KIND_CHANGED {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "theirs" {
		t.Fatalf("the file holds %q", content)
	}
	if left := spares(t, dir); len(left) != 0 {
		t.Errorf("it left %v", left)
	}
}

// stallWrite holds each save as it writes the file, until told how that goes.
func stallWrite(t *testing.T, s *Server, failing error) (reached chan struct{}, answer func()) {
	t.Helper()
	s.AnswerWithin = held
	// The flush before it isn't the step this holds, so it doesn't wait on the disk.
	syncSpare = func(*os.File) error { return nil }
	reached = make(chan struct{}, 8)
	gone := make(chan struct{})
	answer = sync.OnceFunc(func() { close(gone) })
	first := true
	putIn = func(f *os.File, text []byte) error {
		if !first {
			return overwrite(f, text)
		}
		first = false
		reached <- struct{}{}
		<-gone
		if failing != nil {
			return failing
		}
		return overwrite(f, text)
	}
	t.Cleanup(func() { answer(); putIn = overwrite; syncSpare = (*os.File).Sync })
	return reached, answer
}

// A save left behind as it wrote may land, so that's what the person hears, and when
// it comes back the control plane hears how it came out.
func TestASaveLeftBehindAsItWritesSaysItMayLandAndHowItEnded(t *testing.T) {
	for name, failing := range map[string]error{"lands": nil, "doesn't land": errors.New("no space left on device")} {
		t.Run(name, func(t *testing.T) {
			d := newDisk(t)
			p := filepath.Join(d.root, "app", "conf")
			write(t, p, "before")
			v := d.read(t, p).GetFile().GetVersion()
			reached, answer := stallWrite(t, d.server, failing)
			got := d.save(t, p, "after", v).GetRefusal()
			if got.GetKind() != agentv1.FileRefusal_KIND_MAY_LAND || !strings.Contains(got.GetMessage(), "may still land") || got.GetPath() != p {
				t.Fatalf("got %v", got)
			}
			reach(t, reached)
			answer()
			select {
			case ended := <-d.server.SavesEnded():
				if ended.GetRecord() != "evt_save" || ended.GetPath() != p || ended.GetLanded() != (failing == nil) {
					t.Errorf("it says %v", ended)
				}
				if failing != nil && !strings.Contains(ended.GetReason(), "no space left") {
					t.Errorf("it says %v", ended)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("nothing says how the save came out")
			}
			want := "after"
			if failing != nil {
				want = "before"
			}
			if content, _ := os.ReadFile(p); string(content) != want {
				t.Fatalf("the file holds %q", content)
			}
		})
	}
}

// One that comes back just as it's being given up on answers, rather than say it
// didn't.
func TestACallThatComesBackAsItsLeftAnswers(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "app", "conf")
	write(t, p, "before")
	gone := make(chan struct{})
	d.server.AnswerWithin = roomy
	d.server.Touching = func(at string) {
		if at == p {
			<-gone
		}
	}
	// Reading what's mounted is the first thing leaving a call does.
	var leaving atomic.Bool
	d.server.mounts = func() string {
		if leaving.CompareAndSwap(false, true) {
			close(gone)
			time.Sleep(200 * time.Millisecond)
		}
		return ""
	}
	got := d.read(t, p)
	if got.GetFile().GetContent() != "before" || got.GetRefusal() != nil {
		t.Fatalf("a call that came back says %v", got)
	}
	if n := d.server.stuckCount(); n != 0 {
		t.Errorf("%d left behind", n)
	}
}
