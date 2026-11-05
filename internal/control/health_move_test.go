// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/docker"
)

// saysRunning is an agent's monitor that says web runs, with nothing checking it,
// each time its stream opens, and since when it can't read Docker, when it can't.
type saysRunning struct {
	project string
	blind   time.Time
	// never is a monitor that hasn't read Docker yet, as one started with it down.
	never bool
}

func (s saysRunning) Report() []*dockwellv1.InstanceHealth {
	if s.never {
		return nil
	}
	return []*dockwellv1.InstanceHealth{{
		Project: s.project, Service: "web", Number: 1, Container: s.project + "-web-1",
		State: dockwellv1.HealthState_HEALTH_STATE_NO_CHECK, Check: dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_NONE, Since: timestamppb.Now(),
	}}
}

func (s saysRunning) ScannedAt() time.Time {
	if s.never {
		return time.Time{}
	}
	return time.Now()
}
func (s saysRunning) DockerUnreadableSince() time.Time { return s.blind }
func (saysRunning) Changed() <-chan struct{}           { return nil }

// lookout reads, again and again, how web is, whether its server's agent is there
// to say how a domain is, and whether its server is connected, and keeps when each
// last read as gone.
type lookout struct {
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	// gone is what read as gone, and when, first and last.
	first, last map[string]time.Time
	reads       atomic.Int64
}

func lookOut(t *testing.T, m *moveRig) *lookout {
	t.Helper()
	b := m.signedIn
	health := dockwellv1connect.NewHealthServiceClient(b, b.origin)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)
	id := m.server.id.ServerID
	l := &lookout{stop: make(chan struct{}), done: make(chan struct{}), first: map[string]time.Time{}, last: map[string]time.Time{}}
	gone := func(what string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		now := time.Now()
		if _, ok := l.first[what]; !ok {
			l.first[what] = now
		}
		l.last[what] = now
	}
	go func() {
		defer close(l.done)
		for {
			select {
			case <-l.stop:
				return
			default:
			}
			if res, err := health.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{})); err == nil {
				for _, s := range res.Msg.GetServices() {
					if s.GetState() != dockwellv1.HealthState_HEALTH_STATE_NO_CHECK || s.GetServers()[0].GetStale() {
						gone(fmt.Sprintf("health %v", s.GetState()))
					}
				}
			}
			if _, connected := m.f.agents.Domain(id, "shop.example.com"); !connected {
				gone("domains")
			}
			if res, err := servers.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: id})); err == nil &&
				res.Msg.GetServer().GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED {
				gone("the fleet")
			}
			l.reads.Add(1)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	t.Cleanup(l.end)
	return l
}

func (l *lookout) end() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	<-l.done
}

func (l *lookout) seen() (first, last map[string]time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return maps.Clone(l.first), maps.Clone(l.last)
}

// healthRig is a move rig whose agent says how web is.
func healthRig(t *testing.T) *moveRig {
	t.Helper()
	m := startMoveRig(t, nil)
	m.server.stop()
	waitConnected(t, m.f, m.server.id.ServerID, false)
	m.server.health = saysRunning{project: docker.Project(m.scope(t))}
	m.server.connect(t, m.f)
	b := m.signedIn
	health := dockwellv1connect.NewHealthServiceClient(b, b.origin)
	eventually(t, "web to read as running", func() bool {
		res, err := health.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{}))
		return err == nil && res.Msg.GetServices()[0].GetState() == dockwellv1.HealthState_HEALTH_STATE_NO_CHECK
	})
	return m
}

// scope is the environment web is in.
func (m *moveRig) scope(t *testing.T) string {
	t.Helper()
	b := m.signedIn
	res, err := dockwellv1connect.NewHealthServiceClient(b, b.origin).ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{}))
	if err != nil || len(res.Msg.GetServices()) != 1 {
		t.Fatalf("ListHealth = %v, %v", res, err)
	}
	return res.Msg.GetServices()[0].GetResource().GetScope()
}

// An agent moves to a new stream each time its certificate is renewed, and takes as
// long as connecting does. Nobody reading how its services are, how its domains are or
// whether it's connected hears of it: its server isn't gone.
func TestAMoveIsntAServerGoneToWhatReadsIt(t *testing.T) {
	for _, reconnect := range []time.Duration{0, 150 * time.Millisecond, 400 * time.Millisecond} {
		t.Run(reconnect.String(), func(t *testing.T) {
			m := healthRig(t)
			m.door.hold.Store(int64(reconnect))
			l := lookOut(t, m)
			eventually(t, "a few reads", func() bool { return l.reads.Load() > 5 })
			if first, _ := l.seen(); len(first) > 0 {
				t.Fatalf("before any move, these read as gone: %v", first)
			}
			for range 8 {
				was := m.stream(t)
				m.move()
				m.waitNextStream(t, was)
			}
			at := l.reads.Load()
			eventually(t, "reads after the last move", func() bool { return l.reads.Load() > at+5 })
			l.end()
			if first, _ := l.seen(); len(first) > 0 {
				t.Errorf("in %d reads across 8 moves, these read as gone: %v", l.reads.Load(), first)
			}
		})
	}
}

// A server whose agent doesn't come back from a move reads as gone once the grace is
// out, everywhere at once, and one that went without saying it was moving reads as
// gone at once.
func TestAServerThatDoesntReturnFromAMoveReadsGoneInTime(t *testing.T) {
	const late = 3 * time.Second // what a busy machine adds
	m := healthRig(t)
	l := lookOut(t, m)
	m.door.hold.Store(int64(time.Hour))
	moved := time.Now()
	m.move()
	m.waitBetweenStreams(t)
	for _, what := range []string{"health HEALTH_STATE_UNKNOWN", "domains", "the fleet"} {
		waitWithin(t, agentapi.MoveGrace+late, what+" to read as gone", func() bool {
			first, _ := l.seen()
			_, ok := first[what]
			return ok
		})
	}
	first, _ := l.seen()
	for what, at := range first {
		took := at.Sub(moved)
		t.Logf("%s read as gone %s after the move", what, took)
		if took < agentapi.MoveGrace-time.Second || took > agentapi.MoveGrace+late {
			t.Errorf("%s read as gone %s after the move, want about %s", what, took, agentapi.MoveGrace)
		}
	}
	l.end()

	// Back, then gone without a word.
	m.door.hold.Store(0)
	waitWithin(t, 30*time.Second, "the agent to come back", func() bool {
		_, ok := m.f.agents.Session(m.server.id.ServerID)
		return ok
	})
	again := lookOut(t, m)
	waitWithin(t, late, "health to read as there again", func() bool { return again.reads.Load() > 5 })
	eventually(t, "nothing to read as gone", func() bool {
		_, last := again.seen()
		for _, at := range last {
			if time.Since(at) < 100*time.Millisecond {
				return false
			}
		}
		return true
	})
	gone := time.Now()
	m.server.stop()
	for _, what := range []string{"health HEALTH_STATE_UNKNOWN", "domains", "the fleet"} {
		waitWithin(t, late, what+" to read as gone at once", func() bool {
			_, last := again.seen()
			return last[what].After(gone)
		})
	}
	t.Logf("all read as gone %s after the agent went", time.Since(gone))
}

// An agent that can't read Docker says so with what it says of health, and its
// server says it too, so the panel's top bar isn't green for it; a new stream that
// can says so as well.
func TestAServerSaysItsAgentCantReadDocker(t *testing.T) {
	m := startMoveRig(t, nil)
	scope := m.scope(t)
	m.server.stop()
	waitConnected(t, m.f, m.server.id.ServerID, false)
	blind := time.Now().Add(-time.Minute).Truncate(time.Second)
	m.server.health = saysRunning{project: docker.Project(scope), blind: blind}
	m.server.connect(t, m.f)
	servers := dockwellv1connect.NewServerServiceClient(m.signedIn, m.signedIn.origin)
	since := func() *timestamppb.Timestamp {
		res, err := servers.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: m.server.id.ServerID}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetServer().GetDockerUnreadableSince()
	}
	eventually(t, "the server to say its agent can't read Docker", func() bool { return since() != nil })
	if got := since().AsTime(); !got.Equal(blind) {
		t.Errorf("the server says its agent can't read Docker since %s, want %s", got, blind)
	}
	m.server.stop()
	waitConnected(t, m.f, m.server.id.ServerID, false)
	m.server.health = saysRunning{project: docker.Project(scope)}
	m.server.connect(t, m.f)
	eventually(t, "the server to say its agent reads Docker again", func() bool { return since() == nil })

	// Started with Docker down, as after a reboot where it didn't come up: the agent
	// has read nothing, and says so from the start, so neither its server nor what
	// runs there reads as fine.
	m.server.stop()
	waitConnected(t, m.f, m.server.id.ServerID, false)
	m.server.health = saysRunning{project: docker.Project(scope), blind: blind, never: true}
	m.server.connect(t, m.f)
	eventually(t, "the server to say its agent can't read Docker, having read nothing", func() bool { return since() != nil })
	health := dockwellv1connect.NewHealthServiceClient(m.signedIn, m.signedIn.origin)
	res, err := health.ListHealth(t.Context(), connect.NewRequest(&dockwellv1.ListHealthRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	s := res.Msg.GetServices()[0].GetServers()[0]
	if s.GetState() != dockwellv1.HealthState_HEALTH_STATE_UNKNOWN || s.GetStale() || !s.GetDockerUnreadableSince().AsTime().Equal(blind) {
		t.Errorf("with its agent having read nothing, web reads %v, stale=%t, Docker unreadable since %v; want unknown, not stale, since %s",
			s.GetState(), s.GetStale(), s.GetDockerUnreadableSince(), blind)
	}
}
