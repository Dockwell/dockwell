// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	agentlogs "github.com/dockwell/dockwell/internal/agent/logs"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
)

// touched is a service that runs on a, whose move to b failed and rolled back, so b
// holds what the failed container printed, and has no container and no step of it.
type touched struct {
	f        *fullServer
	c        dockwellv1connect.ChangeServiceClient
	lc       dockwellv1connect.LogServiceClient
	a, b     *dockertest.Engine
	onB      *testServer
	scope    string
	id       string
	bServer  string
	clock    *atomic.Int64
	archiveB *agentlogs.Archive
}

const printedOnB = "password=hunter2 printed on b"

func aFailedMove(t *testing.T) *touched { return aFailedMoveWith(t, logapi.Options{}) }

func aFailedMoveWith(t *testing.T, options logapi.Options) *touched {
	t.Helper()
	clock := &atomic.Int64{}
	options.Now = func() time.Time { return time.Now().Add(time.Duration(clock.Load())) }
	f := startFullWith(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, options)
	browser := signedIn(t, f)
	c := changeClient(browser, browser.origin)
	lc := logClient(browser, browser.origin)
	a := f.addServerNamed(t, "a", nil, nil)
	first := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	if first.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the first deploy ended %v: %s", first.GetState(), first.GetError())
	}
	scope := first.GetPlan().GetResources()[0].GetResource().GetScope()
	id := first.GetPlan().GetResources()[0].GetId()

	var archive *agentlogs.Archive
	b := f.addServerNamed(t, "b", nil, func(_ *dockertest.Engine, on *agentlogs.Archive) { archive = on })
	onB := f.lastDocker
	bID := onB.id.ServerID
	b.Registry["ghcr.io/acme/shop:bad"] = dockertest.Image{Exits: true, Prints: []string{printedOnB}, Config: docker.Config{Cmd: []string{"shop"}}}

	move := propose(t, c, "Move web", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: "web"},
		Servers:  []string{bID},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:bad"}},
		}}},
	})
	w := watch(t, c, move.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: move.GetId(), Fingerprint: move.GetPlan().GetFingerprint()})); err != nil {
		t.Logf("the move: %v", err)
	}
	waitWatch(t, w)
	project := docker.Project(scope)
	eventually(t, "b to keep what the failed container printed", func() bool {
		return len(archive.List(project, "web")) > 0
	})
	if lines := logTexts(t, lc, id); !hasLine(lines, "hunter2") {
		t.Fatalf("before the deletion it read %q, without what b kept", lines)
	}
	return &touched{f: f, c: c, lc: lc, a: a, b: b, onB: onB, scope: scope, id: id, bServer: bID, clock: clock, archiveB: archive}
}

// makeWebAgain makes a new service of the name, on b, and checks it kept its ID.
func (s *touched) makeWebAgain(t *testing.T) {
	t.Helper()
	s.b.Registry["ghcr.io/acme/shop:good"] = dockertest.Image{Prints: []string{"the new one, on b"}, Config: docker.Config{Cmd: []string{"serve"}}}
	again := apply(t, s.c, propose(t, s.c, "Run a new web", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: s.scope, Name: "web"},
		Servers:  []string{s.bServer},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:good"}},
		}}},
	}))
	if again.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the new one ended %v: %s", again.GetState(), again.GetError())
	}
	if got := again.GetPlan().GetResources()[0].GetId(); got != s.id {
		t.Fatalf("the new one is %s, not %s: this test is of the same ID", got, s.id)
	}
}

func hasLine(lines []string, text string) bool {
	return slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, text) })
}

func (s *touched) refused(t *testing.T) {
	t.Helper()
	for name, id := range map[string]string{"its ID": s.id, "its name": "web"} {
		_, err := s.lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: id}))
		if connect.CodeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), "was deleted") {
			t.Errorf("reading by %s said %v", name, err)
		}
		_, err = s.lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: id}))
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("listing by %s said %v", name, err)
		}
		stream, err := s.lc.WatchLogs(t.Context(), connect.NewRequest(&dockwellv1.WatchLogsRequest{Id: id, Once: true}))
		if err == nil {
			for stream.Receive() {
				for _, c := range stream.Msg().GetChunks() {
					if len(c.GetLines()) > 0 {
						t.Errorf("a stream by %s read %v", name, c.GetLines())
					}
				}
			}
			err = stream.Err()
		}
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("a stream by %s said %v", name, err)
		}
	}
}

func (s *touched) keptOnB() int { return len(s.archiveB.List(docker.Project(s.scope), "web")) }

// The plan says its logs can't be read, and on every server they are on that holds:
// b, whose failed move left them, has no step of the deletion.
func TestADeletionReachesAServerItsFailedMoveTouched(t *testing.T) {
	s := aFailedMove(t)
	gone := deleteWeb(t, s.c, s.scope)
	said := false
	for _, server := range gone.GetPlan().GetServers() {
		for _, step := range server.GetSteps() {
			said = said || step.GetHidesLogs()
		}
	}
	if !said {
		t.Errorf("the plan doesn't say what happens to its logs: %v", gone.GetPlan().GetServers())
	}
	s.refused(t)

	// They stay until it can't be undone, and then go, by the deletion's 7 days and not
	// a kept log's 30, though b has no step of it.
	s.f.server.logs.TellAll(t.Context())
	if s.keptOnB() == 0 {
		t.Fatal("what b kept went before the 7 days it can be undone for")
	}
	s.clock.Store(int64(6 * 24 * time.Hour))
	s.f.server.logs.TellAll(t.Context())
	if s.keptOnB() == 0 {
		t.Fatal("what b kept went on day 6")
	}
	s.clock.Store(int64(7*24*time.Hour + time.Minute))
	s.f.server.logs.TellAll(t.Context())
	if got := s.keptOnB(); got != 0 {
		t.Errorf("7 days on, b keeps %d of what the service left", got)
	}
}

// A new service of the name starts clean on every server, keeps its old ID, and reads
// what it writes itself. Asked for the old change, it finds nothing.
func TestANewServiceOfTheNameDoesntReadWhatTheOldOneLeftOnAnyServer(t *testing.T) {
	s := aFailedMove(t)
	deleteWeb(t, s.c, s.scope)
	s.makeWebAgain(t)
	for name, id := range map[string]string{"name": "web", "ID": s.id} {
		lines := logTexts(t, s.lc, id)
		if hasLine(lines, "hunter2") {
			t.Errorf("by its %s, the new service reads what the old one left: %q", name, lines)
		}
		if !hasLine(lines, "the new one, on b") {
			t.Errorf("by its %s, the new service doesn't read its own lines: %q", name, lines)
		}
	}
	// A stream leaves it out too, however it reads, and says nothing of where it was.
	for name, req := range map[string]*dockwellv1.WatchLogsRequest{
		"following":  {Id: s.id, Tail: 500},
		"to the end": {Id: s.id, Tail: 500, Once: true},
		"by a time":  {Id: s.id, Last: durationpb.New(time.Hour)},
	} {
		r := follow(t, s.lc, req)
		eventually(t, name+" to read its own lines", func() bool { return hasLine(r.got(), "the new one, on b") })
		r.waitCaughtUp()
		if lines := r.got(); hasLine(lines, "hunter2") {
			t.Errorf("%s, the new service reads what the old one left: %q", name, lines)
		}
		r.mu.Lock()
		for _, source := range r.sources {
			if source.GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT {
				t.Errorf("%s, the new service lists what the old one left: %v", name, source)
			}
		}
		r.mu.Unlock()
		r.cancel()
	}
	list, err := s.lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range list.Msg.GetSources() {
		if source.GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT {
			t.Errorf("the new service lists what the old one left: %v", source)
		}
	}
}

// Undoing the deletion brings back what every server kept.
func TestUndoingADeletionBringsBackWhatEveryServerKept(t *testing.T) {
	s := aFailedMove(t)
	gone := deleteWeb(t, s.c, s.scope)
	s.refused(t)
	s.f.server.logs.TellAll(t.Context())
	undo, err := s.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: gone.GetId()}))
	if err != nil {
		t.Fatalf("undoing: %v", err)
	}
	if back := apply(t, s.c, undo.Msg.GetChange()); back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing ended %v: %s", back.GetState(), back.GetError())
	}
	if lines := logTexts(t, s.lc, s.id); !hasLine(lines, "hunter2") {
		t.Errorf("after undoing it reads %q, without what b kept", lines)
	}
	// Long past when it would have gone, it stays, as nothing puts it out of reach.
	s.clock.Store(int64(8 * 24 * time.Hour))
	s.f.server.logs.TellAll(t.Context())
	if s.keptOnB() == 0 {
		t.Error("what b kept went though the deletion was undone")
	}
}

// A server that stays connected hears without anyone asking, or what it holds would stay
// past the deletion's 7 days for as long as it stays up.
func TestADeletionLeavesAConnectedServerOnceItCantBeUndone(t *testing.T) {
	s := aFailedMoveWith(t, logapi.Options{TellEvery: 20 * time.Millisecond})
	deleteWeb(t, s.c, s.scope)
	s.clock.Store(int64(7*24*time.Hour + time.Minute))
	eventually(t, "what b kept to go by itself", func() bool { return s.keptOnB() == 0 })
}

// An undone deletion stops counting against what a server keeps, though no deletion is
// left to name: when the disk is full, what was replaced goes before what failed.
func TestAnUndoneDeletionDoesntGoFirstWhenTheDiskIsFull(t *testing.T) {
	s := aFailedMove(t)
	gone := deleteWeb(t, s.c, s.scope)
	s.f.server.logs.TellAll(t.Context())
	undo, err := s.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: gone.GetId()}))
	if err != nil {
		t.Fatalf("undoing: %v", err)
	}
	if back := apply(t, s.c, undo.Msg.GetChange()); back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing ended %v: %s", back.GetState(), back.GetError())
	}
	s.f.server.logs.TellAll(t.Context())

	s.fillB(t)
	if s.keptOnB() == 0 {
		t.Error("what b kept of web went first, as if its deletion stood")
	}
	if got := s.archiveB.List(docker.Project(s.scope), "older"); len(got) != 0 {
		t.Errorf("what was replaced stayed: %d", len(got))
	}
}

// While a deletion stands, what it left is what goes first when the disk is full, though
// it failed, which would otherwise go last.
func TestWhatADeletionLeftGoesFirstWhenTheDiskIsFull(t *testing.T) {
	s := aFailedMove(t)
	deleteWeb(t, s.c, s.scope)
	s.f.server.logs.TellAll(t.Context())
	s.fillB(t)
	if got := s.keptOnB(); got != 0 {
		t.Errorf("what the deletion left stayed: %d", got)
	}
	if got := s.archiveB.List(docker.Project(s.scope), "older"); len(got) != 1 {
		t.Errorf("what was replaced went before it: %d left", len(got))
	}
}

// fillB has b keep what two other services replaced, then prunes it with a budget that
// makes exactly one thing go.
func (s *touched) fillB(t *testing.T) {
	t.Helper()
	project := docker.Project(s.scope)
	replaced := func(service string) int64 {
		name := docker.ContainerName(project, service)
		ctr := s.b.AddContainer(docker.Container{
			Name: "/" + name,
			Config: docker.Config{Image: "ghcr.io/acme/shop:1.4", Labels: map[string]string{
				docker.LabelProject: project, docker.LabelService: service, docker.LabelNumber: "1",
				container.LabelRevision: "1",
			}},
			Created: time.Now().UTC(),
			State:   docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
		})
		s.b.Print(name, docker.Stdout, "a line of "+service)
		k := s.archiveB.Begin(t.Context(), ctr, false)
		s.b.Stop(name)
		k.Finish(t.Context(), s.b.Container(name))
		return s.archiveB.List(project, service)[0].Disk
	}
	total := replaced("older") + replaced("newer")
	total += s.archiveB.List(project, "web")[0].Disk
	s.archiveB.WaitPruned()
	s.archiveB.Budget = total - 1
	if err := s.archiveB.Prune(); err != nil {
		t.Fatal(err)
	}
}

// A server that is offline can't be told. Its logs can't be read from the moment of the
// deletion, as the control plane reads nothing there, the plan says they stay on its
// disk until it is back, and when it is back they go.
func TestADeletionReachesAServerThatWasOffline(t *testing.T) {
	s := aFailedMove(t)
	s.onB.stop()
	eventually(t, "b to be offline", func() bool {
		_, on := s.f.agents.Session(s.bServer)
		return !on
	})
	gone := deleteWeb(t, s.c, s.scope)
	said := false
	for _, n := range gone.GetPlan().GetNotes() {
		if n.GetReason() == "kept_logs_offline" && len(n.GetNames()) == 1 && n.GetNames()[0] == "b" && strings.Contains(n.GetMessage(), "until it's back") {
			said = true
		}
	}
	if !said {
		t.Errorf("the plan doesn't say b holds on to them: %v", gone.GetPlan().GetNotes())
	}
	s.refused(t)
	if s.keptOnB() == 0 {
		t.Fatal("b is offline, and what it kept went")
	}

	// Back within the 7 days, they stay, for undoing.
	s.onB.connect(t, s.f)
	time.Sleep(200 * time.Millisecond)
	if s.keptOnB() == 0 {
		t.Fatal("back within the 7 days, what b kept went")
	}
	s.refused(t)

	// Away past them, they go as it connects.
	s.onB.stop()
	eventually(t, "b to be offline", func() bool {
		_, on := s.f.agents.Session(s.bServer)
		return !on
	})
	s.clock.Store(int64(7*24*time.Hour + time.Minute))
	s.onB.connect(t, s.f)
	eventually(t, "what b kept to go as it reconnects", func() bool { return s.keptOnB() == 0 })
}

// A server that joins later hears too, as its disk may hold what a deleted service
// left, as when it was removed and joined again.
func TestADeletionReachesAServerThatJoinsLater(t *testing.T) {
	s := aFailedMove(t)
	deleteWeb(t, s.c, s.scope)
	project := docker.Project(s.scope)
	s.clock.Store(int64(7*24*time.Hour + time.Minute))
	var archive *agentlogs.Archive
	s.f.addServerNamed(t, "later", nil, func(e *dockertest.Engine, on *agentlogs.Archive) {
		archive = on
		ctr := e.AddContainer(docker.Container{
			Name: "/" + docker.ContainerName(project, "web"),
			Config: docker.Config{Image: "ghcr.io/acme/shop:1.4", Labels: map[string]string{
				docker.LabelProject: project, docker.LabelService: "web", docker.LabelNumber: "1",
				container.LabelRevision: "1",
			}},
			Created: time.Now().UTC(),
			State:   docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
		})
		e.Print(strings.TrimPrefix(ctr.Name, "/"), docker.Stdout, "password=hunter2 from before it joined")
		k := on.Begin(context.Background(), ctr, true)
		e.Stop(strings.TrimPrefix(ctr.Name, "/"))
		k.Finish(context.Background(), e.Container(strings.TrimPrefix(ctr.Name, "/")))
		if len(on.List(project, "web")) != 1 {
			t.Fatal("nothing was kept before it joined")
		}
	})
	eventually(t, "what it held to go as it joins", func() bool { return len(archive.List(project, "web")) == 0 })
}

// A first deploy that failed and was rolled back isn't a deletion: what it left is
// what its reader came for, and stays for a kept log's own time.
func TestARolledBackFirstDeployIsNotForgotten(t *testing.T) {
	clock := &atomic.Int64{}
	f := startFullWith(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{Now: func() time.Time {
		return time.Now().Add(time.Duration(clock.Load()))
	}})
	browser := signedIn(t, f)
	var archive *agentlogs.Archive
	e := f.addServerNamed(t, "a", nil, func(_ *dockertest.Engine, on *agentlogs.Archive) { archive = on })
	e.Registry["ghcr.io/acme/shop:bad"] = dockertest.Image{Exits: true, Prints: []string{"ERROR can't reach the database"}, Config: docker.Config{Cmd: []string{"shop"}}}
	c := changeClient(browser, browser.origin)
	lc := logClient(browser, browser.origin)
	failed := propose(t, c, "Run web", runImage("ghcr.io/acme/shop:bad"))
	id, scope := failed.GetPlan().GetResources()[0].GetId(), failed.GetPlan().GetResources()[0].GetResource().GetScope()
	w := watch(t, c, failed.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: failed.GetId(), Fingerprint: failed.GetPlan().GetFingerprint()})); err != nil {
		t.Logf("the deploy: %v", err)
	}
	waitWatch(t, w)
	project := docker.Project(scope)
	eventually(t, "the rollback to keep its log", func() bool { return len(archive.List(project, "web")) > 0 })
	if lines := logTexts(t, lc, id); !hasLine(lines, "can't reach the database") {
		t.Errorf("it read %q", lines)
	}
	clock.Store(int64(8 * 24 * time.Hour))
	f.server.logs.TellAll(t.Context())
	if len(archive.List(project, "web")) == 0 {
		t.Error("what a failed first deploy left went by a deletion's 7 days")
	}
}

// A stream open as its service is deleted ends, and says why.
func TestAFollowEndsWhenItsServiceIsDeleted(t *testing.T) {
	s := aFailedMove(t)
	r := follow(t, s.lc, &dockwellv1.WatchLogsRequest{Id: s.id, Tail: -1})
	r.waitCaughtUp()
	deleteWeb(t, s.c, s.scope)
	select {
	case err := <-r.done:
		if connect.CodeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), "was deleted") {
			t.Errorf("it ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("a stream stayed open after its service was deleted")
	}
}

// A deletion and a new service of the name can both come between two of a stream's
// looks, when the name says nothing has gone. The stream ends all the same, as it
// reads as far back as it did, and its reader comes back to what the new one wrote.
func TestAFollowEndsWhenItsServiceIsDeletedAndMadeAgain(t *testing.T) {
	s := aFailedMove(t)
	r := follow(t, s.lc, &dockwellv1.WatchLogsRequest{Id: s.id, Tail: -1})
	r.waitCaughtUp()
	deleteWeb(t, s.c, s.scope)
	s.makeWebAgain(t)
	select {
	case err := <-r.done:
		// A look may fall between the two, which says it's deleted.
		switch code := connect.CodeOf(err); {
		case code == connect.CodeUnavailable && strings.Contains(err.Error(), "made again"):
		case code == connect.CodeNotFound && strings.Contains(err.Error(), "was deleted"):
		default:
			t.Errorf("it ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a stream stayed open after its service was deleted and made again")
	}
	again := follow(t, s.lc, &dockwellv1.WatchLogsRequest{Id: s.id, Tail: 500, ResumeToken: r.token})
	eventually(t, "its reader to read the new one", func() bool { return hasLine(again.got(), "the new one, on b") })
	if lines := again.got(); hasLine(lines, "hunter2") {
		t.Errorf("back again, it reads what the old one left: %q", lines)
	}
}
