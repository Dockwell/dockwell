// SPDX-License-Identifier: AGPL-3.0-only

// Package updates serves dockwell.v1.UpdateService: which package updates wait on
// a server, read through its agent with the server's own package manager, only
// when someone asks, and kept in memory for a while. See
// docs/adr/0043-package-updates-on-a-server.md.
package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	agentupdates "github.com/dockwell/dockwell/internal/agent/updates"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
)

// How long a read stands before the next look asks again, and how long one that
// failed does, so a fix shows soon.
const (
	Fresh       = 10 * time.Minute
	FailedFresh = time.Minute
)

// Wait is how long a look waits for a read it started before it answers with
// checking set, so a quick read answers in one go.
const Wait = 3 * time.Second

// Handler implements UpdateService.
type Handler struct {
	fleet    *fleet.Hub
	agents   *agentapi.Agents
	releases *Releases
	build    buildinfo.Info
	logger   *slog.Logger
	// Now is the clock, and Wait how long a look waits, for tests.
	Now  func() time.Time
	Wait time.Duration

	mu      sync.Mutex
	servers map[string]*server
}

var _ dockwellv1connect.UpdateServiceHandler = (*Handler)(nil)

// server is what's known of one server's updates.
type server struct {
	// last is the last read that worked, and read when the last read ended,
	// whether it worked or not, with err why it didn't.
	last *dockwellv1.ServerUpdates
	read time.Time
	err  string
	// running is set while a read is under way, done closed as it ends, and
	// refreshing while it refreshes the lists first.
	running    bool
	refreshing bool
	done       chan struct{}
	// forced is when a person last asked for the lists to be refreshed, and
	// again is set when one asked while a read that doesn't refresh them ran.
	forced time.Time
	again  bool
	// installsEnded is how many installs of updates had ended on the server as
	// the last read started: once another ends, what was read is out of date.
	installsEnded uint64
}

// New returns a handler that reads servers' updates through their agents, and
// Dockwell's newest release through releases, which may be nil.
func New(hub *fleet.Hub, agents *agentapi.Agents, releases *Releases, build buildinfo.Info, logger *slog.Logger) *Handler {
	return &Handler{
		fleet: hub, agents: agents, releases: releases, build: build, logger: logger,
		Now: time.Now, Wait: Wait, servers: map[string]*server{},
	}
}

// GetServerUpdates answers with what's known, asking the agent first when that's
// older than Fresh, and waiting for it a little.
func (h *Handler) GetServerUpdates(ctx context.Context, req *connect.Request[dockwellv1.GetServerUpdatesRequest]) (*connect.Response[dockwellv1.GetServerUpdatesResponse], error) {
	srv, err := h.server(req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	var newest <-chan struct{}
	if h.releases != nil {
		newest = h.releases.Look()
	}
	out, done := h.look(srv, false)
	if done != nil {
		wait := time.NewTimer(h.Wait)
		defer wait.Stop()
		select {
		case <-done:
			out, _ = h.look(srv, false)
		case <-wait.C:
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}
	if newest != nil {
		select {
		case <-newest:
		case <-time.After(h.Wait):
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}
	return connect.NewResponse(&dockwellv1.GetServerUpdatesResponse{Updates: out, Dockwell: h.release(srv)}), nil
}

// CheckServerUpdates has the agent refresh the server's lists and read again, and
// answers as that starts.
func (h *Handler) CheckServerUpdates(_ context.Context, req *connect.Request[dockwellv1.CheckServerUpdatesRequest]) (*connect.Response[dockwellv1.CheckServerUpdatesResponse], error) {
	srv, err := h.server(req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	out, _ := h.look(srv, true)
	return connect.NewResponse(&dockwellv1.CheckServerUpdatesResponse{Updates: out}), nil
}

// look is what's known of a server's updates, and starts a read when that's
// stale, or force asks for one, returning a channel closed as it ends. A server
// that can't be read says why, with what it last said.
func (h *Handler) look(srv *dockwellv1.Server, force bool) (*dockwellv1.ServerUpdates, <-chan struct{}) {
	id := srv.GetId()
	session, connected := h.agents.Session(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.servers[id]
	if s == nil {
		s = &server{}
		h.servers[id] = s
	}
	now := h.Now()
	installing, ended := h.agents.Installing(id)
	switch {
	case !connected:
		out := h.snapshot(id, s)
		out.Error, out.Reason = srv.GetName()+"'s agent isn't connected, so Dockwell can't read its updates. What's here is from when it last could", agentupdates.ReasonServerOffline
		return out, nil
	case !session.ReadsUpdates():
		return &dockwellv1.ServerUpdates{ServerId: id, Reason: agentupdates.ReasonAgentOutdated, Unavailable: srv.GetName() +
			"'s agent is older than the control plane, and doesn't read package updates. Run the install script on it again, with the control plane's version"}, nil
	case s.running:
		if force && !s.again && now.Sub(s.forced) >= agentupdates.ForceEvery {
			s.again = true
		}
		out := h.snapshot(id, s)
		out.InstallChangeId = installing
		return out, s.done
	case installing != "":
		// What an install moves is read once it ends, not halfway: a read now would
		// race dpkg, and a refresh would wait on its lock.
		out := h.snapshot(id, s)
		out.InstallChangeId = installing
		return out, nil
	}
	stale := s.read.IsZero() || now.Sub(s.read) >= Fresh || s.err != "" && now.Sub(s.read) >= FailedFresh || ended != s.installsEnded
	switch {
	case force && now.Sub(s.forced) >= agentupdates.ForceEvery:
		s.forced = now
	case !stale:
		return h.snapshot(id, s), nil
	default:
		force = false
	}
	h.start(srv, s, session, force)
	return h.snapshot(id, s), s.done
}

// start reads a server's updates on a goroutine of its own, which outlives the
// request that asked, as a refresh can take minutes. h.mu is held.
func (h *Handler) start(srv *dockwellv1.Server, s *server, session *agentapi.Session, force bool) {
	s.running, s.refreshing, s.done = true, false, make(chan struct{})
	_, s.installsEnded = h.agents.Installing(srv.GetId())
	done := s.done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentapi.UpdatesTimeout)
		defer cancel()
		ctx = resource.WithProgress(ctx, func(message string) {
			if message == agentupdates.Refreshing {
				h.mu.Lock()
				s.refreshing = true
				h.mu.Unlock()
			}
		})
		found, err := session.ListUpdates(ctx, true, force)
		h.mu.Lock()
		defer h.mu.Unlock()
		s.running, s.refreshing, s.read = false, false, h.Now()
		if err != nil {
			h.logger.Warn("couldn't read a server's updates", slog.String("server", srv.GetName()), slog.Any("error", err))
			s.err = readError(srv.GetName(), err)
		} else {
			s.last, s.err = found, ""
		}
		close(done)
		if s.again {
			s.again, s.forced = false, h.Now()
			if again, ok := h.agents.Session(srv.GetId()); ok && again.ReadsUpdates() {
				h.start(srv, s, again, true)
			}
		}
	}()
}

// readError says why a read failed, for people.
func readError(name string, err error) string {
	switch {
	case errors.Is(err, resource.ErrLost), errors.Is(err, resource.ErrNotConnected):
		return name + "'s agent went away while it read its updates. They're read again when someone next looks"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("%s's package manager didn't answer in time: %v", name, err)
	}
	return err.Error()
}

// snapshot is what's known, as the API says it. h.mu is held.
func (h *Handler) snapshot(id string, s *server) *dockwellv1.ServerUpdates {
	out := &dockwellv1.ServerUpdates{}
	if s.last != nil {
		out = proto.CloneOf(s.last)
	}
	out.ServerId, out.Checking, out.Refreshing, out.Error = id, s.running, s.refreshing, s.err
	if s.err != "" {
		out.Reason = agentupdates.ReasonReadFailed
	}
	return out
}

// release is Dockwell's version on the server and here, and the newest.
func (h *Handler) release(srv *dockwellv1.Server) *dockwellv1.DockwellRelease {
	out := &dockwellv1.DockwellRelease{Agent: srv.GetAgentBuild().GetVersion(), ControlPlane: h.build.Version}
	if h.releases != nil {
		out.Releases = h.releases.URL
		if v, at := h.releases.Newest(); v != "" {
			out.Newest, out.NewestTime = v, timestamppb.New(at)
		}
	}
	return out
}

// server finds a server by its ID.
func (h *Handler) server(id string) (*dockwellv1.Server, error) {
	srv, err := h.fleet.Get(id)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", id), "not_found")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return srv, nil
}
