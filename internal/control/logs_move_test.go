// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// door stands between an agent and the control plane, and holds each new connection
// for as long as it's told, as connecting to a real server takes time. A moving agent
// connects anew, so this is the time between its two streams.
type door struct {
	listener net.Listener
	target   string
	hold     atomic.Int64
	done     chan struct{}
}

func openDoor(t *testing.T, target string) *door {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &door{listener: listener, target: target, done: make(chan struct{})}
	t.Cleanup(func() {
		close(d.done)
		_ = listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go d.pass(conn)
		}
	}()
	return d
}

func (d *door) pass(conn net.Conn) {
	defer conn.Close()
	for at := time.Now(); time.Since(at) < time.Duration(d.hold.Load()); {
		select {
		case <-d.done:
			return
		case <-time.After(time.Millisecond):
		}
	}
	up, err := net.Dial("tcp", d.target)
	if err != nil {
		return
	}
	defer up.Close()
	go func() {
		_, _ = io.Copy(up, conn)
		_ = up.Close()
	}()
	_, _ = io.Copy(conn, up)
}

// moveRig is a service on a server whose agent connects through a door.
type moveRig struct {
	f      *fullServer
	engine *dockertest.Engine
	server *testServer
	door   *door
	logs   dockwellv1connect.LogServiceClient
	name   string
	// signedIn is a browser that signed in, for more clients.
	signedIn *browser
}

func startMoveRig(t *testing.T, wrap func(agent.LogReader) agent.LogReader) *moveRig {
	t.Helper()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	server := f.lastDocker
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	d := openDoor(t, f.agentAddr)
	server.id.ControlURL = "https://" + d.listener.Addr().String()
	if wrap != nil {
		server.logs = wrap(server.logs)
	}
	server.connect(t, f)
	return &moveRig{
		f: f, engine: e, server: server, door: d, logs: logClient(b, b.origin), signedIn: b,
		name: docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web"),
	}
}

func (m *moveRig) print(lines ...string) { m.engine.Print(m.name, docker.Stdout, lines...) }

// move has the agent move to a new stream, as a renewed certificate does.
func (m *moveRig) move() { m.server.id.Replace(m.server.id.Key(), m.server.id.Cert()) }

func (m *moveRig) waitNextStream(t *testing.T, was any) {
	t.Helper()
	eventually(t, "the agent's next stream", func() bool {
		s, ok := m.f.agents.Session(m.server.id.ServerID)
		return ok && any(s) != was
	})
}

// waitBetweenStreams waits for the old stream to end, with the next yet to come.
func (m *moveRig) waitBetweenStreams(t *testing.T) {
	t.Helper()
	eventually(t, "the agent's stream to end", func() bool {
		_, ok := m.f.agents.Session(m.server.id.ServerID)
		return !ok
	})
}

func (m *moveRig) stream(t *testing.T) any {
	t.Helper()
	s, ok := m.f.agents.Session(m.server.id.ServerID)
	if !ok {
		t.Fatal("the agent isn't connected")
	}
	return s
}

func (r *reader) told(state dockwellv1.ServerLogState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.states, state)
}

func offlineIn(servers []*dockwellv1.ServerLogStatus) bool {
	return slices.ContainsFunc(servers, func(s *dockwellv1.ServerLogStatus) bool {
		return s.GetState() == dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE
	})
}

func linesOf(res *dockwellv1.ReadLogsResponse) []string {
	var out []string
	for _, c := range res.GetChunks() {
		for _, l := range c.GetLines() {
			out = append(out, l.GetText())
		}
	}
	return out
}

func waitWithin(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An agent moves to a new stream each time its certificate is renewed, and takes as
// long as connecting does. Someone following its logs hears nothing of that: its
// server isn't offline, and each line comes once, those written in the move too.
func TestAMoveIsntAnOfflineServerToAFollower(t *testing.T) {
	for _, reconnect := range []time.Duration{0, 150 * time.Millisecond, 400 * time.Millisecond} {
		t.Run(reconnect.String(), func(t *testing.T) {
			m := startMoveRig(t, nil)
			m.door.hold.Store(int64(reconnect))
			m.print("start")
			r := follow(t, m.logs, &dockwellv1.WatchLogsRequest{Id: "web"})
			r.waitLines("start")
			want := []string{"start"}
			for i := range 8 {
				was := m.stream(t)
				m.move()
				during, after := fmt.Sprintf("during %d", i), fmt.Sprintf("after %d", i)
				m.print(during)
				m.waitNextStream(t, was)
				m.print(after)
				want = append(want, during, after)
				r.waitLines(want...)
			}
			if r.told(dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE) {
				r.mu.Lock()
				defer r.mu.Unlock()
				t.Errorf("a follower was told %v across 8 moves of %s each", r.states, reconnect)
			}
		})
	}
}

// A read begun between an agent's two streams waits for the next one, whichever read.
func TestAReadBegunInAMoveWaitsForTheNextStream(t *testing.T) {
	m := startMoveRig(t, nil)
	m.print("start")
	r := follow(t, m.logs, &dockwellv1.WatchLogsRequest{Id: "web"})
	r.waitLines("start")
	m.door.hold.Store(int64(400 * time.Millisecond))
	m.move()
	m.waitBetweenStreams(t)

	page := make(chan *dockwellv1.ReadLogsResponse, 1)
	sources := make(chan *dockwellv1.ListLogSourcesResponse, 1)
	go func() {
		res, err := m.logs.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"}))
		if err != nil {
			t.Errorf("ReadLogs: %v", err)
			page <- nil
			return
		}
		page <- res.Msg
	}()
	go func() {
		res, err := m.logs.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: "web"}))
		if err != nil {
			t.Errorf("ListLogSources: %v", err)
			sources <- nil
			return
		}
		sources <- res.Msg
	}()
	once := follow(t, m.logs, &dockwellv1.WatchLogsRequest{Id: "web", Once: true, Tail: 100})

	if res := <-page; res != nil && (offlineIn(res.GetServers()) || !slices.Equal(linesOf(res), []string{"start"})) {
		t.Errorf("a page begun in a move = %v, servers %v, want its line and no word of offline", linesOf(res), res.GetServers())
	}
	if res := <-sources; res != nil && (offlineIn(res.GetServers()) || len(res.GetSources()) == 0) {
		t.Errorf("sources asked for in a move = %v, servers %v, want them and no word of offline", res.GetSources(), res.GetServers())
	}
	once.waitLines("start")
	eventually(t, "the read to the end to finish", func() bool { return once.told(dockwellv1.ServerLogState_SERVER_LOG_STATE_DONE) })
	if once.told(dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE) || r.told(dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE) {
		t.Errorf("a read in a move was told its server is offline: one-off %v, follower %v", once.states, r.states)
	}
}

// cutByMove is a log with three lines, whose first read sends one and goes on until
// it's stopped, as a read does with a slow reader or a long log when its agent moves.
type cutByMove struct {
	agent.LogReader
	at    time.Time // when its lines were written, the same for every read
	calls atomic.Int32
	sent  chan struct{}
}

func (l *cutByMove) Stream(ctx context.Context, call *agentv1.LogsCall, send func(*agentv1.SendLogsRequest) error) error {
	lines := make([]*dockwellv1.LogLine, 3)
	for i := range lines {
		lines[i] = &dockwellv1.LogLine{Time: timestamppb.New(l.at.Add(time.Duration(i) * time.Second)), Text: fmt.Sprintf("line %d", i+1)}
	}
	message := func(lines []*dockwellv1.LogLine) *agentv1.SendLogsRequest {
		return &agentv1.SendLogsRequest{Stream: call.GetStream(), Chunks: []*dockwellv1.LogChunk{{SourceId: "a", Lines: lines}}}
	}
	if l.calls.Add(1) == 1 {
		if err := send(message(lines[:1])); err != nil {
			return err
		}
		// Sent is on its way, not there: a moment, so the control plane has it when
		// the agent moves, and has it to drop. Without that a page has nothing to
		// repeat, and passes either way.
		time.Sleep(300 * time.Millisecond)
		close(l.sent)
		<-ctx.Done()
		return ctx.Err()
	}
	// It carries on from what was read, when it's told that, as the real one does.
	if after := call.GetAfter(); len(after) > 0 {
		lines = slices.DeleteFunc(lines, func(l *dockwellv1.LogLine) bool { return !l.GetTime().AsTime().After(after[0].GetTime().AsTime()) })
	}
	return send(message(lines))
}

// A move cuts a read short. A read to the end carries on from its place, and a page
// starts again, so each line comes once and neither says the server is offline.
func TestAMoveDoesntCutAReadShort(t *testing.T) {
	start := func(t *testing.T) (*moveRig, *cutByMove) {
		t.Helper()
		reader := &cutByMove{at: time.Now().Add(-time.Hour), sent: make(chan struct{})}
		m := startMoveRig(t, func(real agent.LogReader) agent.LogReader {
			reader.LogReader = real
			return reader
		})
		m.door.hold.Store(int64(150 * time.Millisecond))
		return m, reader
	}
	want := []string{"line 1", "line 2", "line 3"}
	t.Run("a page", func(t *testing.T) {
		m, reader := start(t)
		page := make(chan *connect.Response[dockwellv1.ReadLogsResponse], 1)
		go func() {
			res, err := m.logs.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"}))
			if err != nil {
				t.Errorf("ReadLogs: %v", err)
			}
			page <- res
		}()
		<-reader.sent
		m.move()
		res := <-page
		if res == nil {
			return
		}
		if got := linesOf(res.Msg); !slices.Equal(got, want) || offlineIn(res.Msg.GetServers()) {
			t.Errorf("a page cut by a move = %v, servers %v, want %v and no word of offline", got, res.Msg.GetServers(), want)
		}
		if n := reader.calls.Load(); n != 2 {
			t.Errorf("the agent was asked %d times, want it read again over its next stream", n)
		}
	})
	t.Run("a read to the end", func(t *testing.T) {
		m, reader := start(t)
		once := follow(t, m.logs, &dockwellv1.WatchLogsRequest{Id: "web", Once: true, Tail: 100})
		<-reader.sent
		m.move()
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Equal(once.got(), want) || !once.told(dockwellv1.ServerLogState_SERVER_LOG_STATE_DONE) {
			if time.Now().After(deadline) {
				once.mu.Lock()
				lines, states := slices.Clone(once.lines), slices.Clone(once.states)
				once.mu.Unlock()
				t.Fatalf("a read to the end cut by a move has %q and was told %v, want %q and done; the agent was asked %d times", lines, states, want, reader.calls.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
		if once.told(dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE) {
			t.Errorf("a read to the end cut by a move was told %v", once.states)
		}
	})
}

// An agent that moves and doesn't come back has a server that really is gone: each
// read says so once logs.MoveGrace is up, and any read after that says so at once.
// It carries on when the agent does come back.
func TestAServerThatDoesntReturnFromAMoveReadsOfflineInTime(t *testing.T) {
	const late = 3 * time.Second // what a busy machine adds to the grace
	m := startMoveRig(t, nil)
	m.print("start")
	r := follow(t, m.logs, &dockwellv1.WatchLogsRequest{Id: "web"})
	r.waitLines("start")

	m.door.hold.Store(int64(time.Hour))
	moved := time.Now()
	m.move()
	m.waitBetweenStreams(t)

	type answer struct {
		res  *dockwellv1.ReadLogsResponse
		took time.Duration
	}
	page := make(chan answer, 1)
	go func() {
		res, err := m.logs.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"}))
		if err != nil {
			t.Errorf("ReadLogs: %v", err)
			page <- answer{}
			return
		}
		page <- answer{res.Msg, time.Since(moved)}
	}()
	once := follow(t, m.logs, &dockwellv1.WatchLogsRequest{Id: "web", Once: true, Tail: 100})

	offline := dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE
	check := func(what string, took time.Duration) {
		t.Helper()
		t.Logf("%s said offline %s after the move", what, took)
		if took < logs.MoveGrace-time.Second || took > logs.MoveGrace+late {
			t.Errorf("%s said offline %s after the move, want it about %s", what, took, logs.MoveGrace)
		}
	}
	waitWithin(t, logs.MoveGrace+late, "the follower to say the server is offline", func() bool { return r.told(offline) })
	check("a follower", time.Since(moved))
	waitWithin(t, late, "the read to the end to say the server is offline", func() bool { return once.told(offline) })
	check("a read to the end", time.Since(moved))
	if err := <-once.done; err != nil {
		t.Errorf("a read to the end whose server is gone = %v, want it ended", err)
	}
	got := <-page
	if got.res != nil {
		check("a page", got.took)
		if !offlineIn(got.res.GetServers()) {
			t.Errorf("a page of a server that didn't come back has servers %v, want it offline", got.res.GetServers())
		}
	}

	begun := time.Now()
	res, err := m.logs.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begun); took > late || !offlineIn(res.Msg.GetServers()) {
		t.Errorf("a page begun after the grace took %s, servers %v, want it offline at once", took, res.Msg.GetServers())
	}

	m.door.hold.Store(0)
	m.print("back")
	waitWithin(t, 30*time.Second, "the follower to carry on", func() bool { return slices.Equal(r.got(), []string{"start", "back"}) })
}

// An agent that goes without saying it's moving isn't waited for.
func TestARealDisconnectReadsOfflineAtOnce(t *testing.T) {
	const atOnce = 3 * time.Second // well inside logs.MoveGrace, on a busy machine
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	e.Print(docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web"), docker.Stdout, "start")
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web"})
	r.waitLines("start")

	gone := time.Now()
	f.lastDocker.stop()
	offline := dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE
	waitWithin(t, atOnce, "the follower to say the server is offline", func() bool { return r.told(offline) })
	t.Logf("a follower said offline %s after the agent went", time.Since(gone))

	begun := time.Now()
	res, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begun); took > atOnce || !offlineIn(res.Msg.GetServers()) {
		t.Errorf("a page after a real disconnect took %s, servers %v, want it offline at once", took, res.Msg.GetServers())
	}
	once := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", Once: true, Tail: 100})
	waitWithin(t, atOnce, "a read to the end to say the server is offline", func() bool { return once.told(offline) })
}
