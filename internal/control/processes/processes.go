// SPDX-License-Identifier: AGPL-3.0-only

// Package processes serves dockwell.v1.ProcessService: what runs on a server,
// process by process, read by its agent only while someone watches, and ending
// one, which takes a person and a passkey and goes on the activity log. See
// docs/adr/0041-processes-on-a-server.md.
package processes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// What goes on the activity log.
const (
	// EventEnd is a process someone ended.
	EventEnd = "process.end"
	// EventEndFailed is one they tried to end, and couldn't.
	EventEndFailed = "process.end_failed"
	// EventEndUnconfirmed is one they asked to end, where the passkey it took
	// didn't come within the minute a proof lasts.
	EventEndUnconfirmed = "process.end_unconfirmed"
)

// Every is how often a watch reads the list again.
var Every = 2 * time.Second

// readWait bounds one read of the list.
const readWait = 10 * time.Second

// MaxPID is the highest PID Linux gives, its pid_max at most. A number past it,
// which a signal would read as a negative PID that names every process, is never
// sent to an agent.
const MaxPID = 4194304

// Target is what a passkey's proof for ending a process names: the server, the
// PID, and when the process started, so a proof given back after a process went
// can't end another that has its PID now.
func Target(serverID string, pid uint32, startTicks uint64) string {
	return serverID + ":" + strconv.FormatUint(uint64(pid), 10) + ":" + strconv.FormatUint(startTicks, 10)
}

// MaxWatchesPerSession is how many process lists one session may watch at once:
// a few tabs' worth. Each reads its server's /proc every 2 s.
const MaxWatchesPerSession = 8

// Handler implements ProcessService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	auth   *auth.Service
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time

	// mu guards feeds, one for each server someone watches, which every watch of
	// that server shares, so its agent reads /proc once however many look; and
	// watching, how many watches each session has open.
	mu       sync.Mutex
	feeds    map[string]*feed
	watching map[string]int
}

// feed reads one server's processes for every watch of it, while there's one.
type feed struct {
	// mu guards what's below: the latest list, or why there's none, and the watches,
	// each told when there's something new.
	mu       sync.Mutex
	last     *dockwellv1.ProcessList
	err      error
	watchers map[chan struct{}]struct{}
	stop     context.CancelFunc
}

var _ dockwellv1connect.ProcessServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents. people checks the
// passkey ending a process takes.
func New(hub *fleet.Hub, agents *agentapi.Agents, people *auth.Service, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{fleet: hub, agents: agents, auth: people, db: db, logger: logger, now: time.Now, feeds: map[string]*feed{}, watching: map[string]int{}}
}

// Detail is what an event of a process keeps, as EventProcess says it.
type Detail struct {
	PID       uint32 `json:"pid"`
	Name      string `json:"name"`
	User      string `json:"user,omitempty"`
	Container string `json:"container,omitempty"`
	Killed    bool   `json:"killed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ReadDetail reads what an event of a process kept, for the timeline.
func ReadDetail(detail string) (*dockwellv1.EventProcess, bool) {
	var d Detail
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		return nil, false
	}
	return &dockwellv1.EventProcess{Pid: d.PID, Name: d.Name, User: d.User, Container: d.Container, Killed: d.Killed, Error: d.Error}, true
}

// person is who is asking. The interceptor refuses tokens already, as these
// methods need a session; this holds if it didn't.
func person(ctx context.Context) (auth.Principal, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		return auth.Principal{}, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	return p, nil
}

func (h *Handler) server(serverID string) (*dockwellv1.Server, error) {
	srv, err := h.fleet.Get(serverID)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", serverID), "not_found")
	}
	return srv, err
}

func (h *Handler) session(ctx context.Context, srv *dockwellv1.Server) (*agentapi.Session, error) {
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so what runs there can't be read now. Check the server; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	return session, nil
}

// WatchProcesses sends a server's processes as its agent reads them, every Every
// for as long as the watch is open. The first comes at once, without CPU, as
// there's nothing to measure it against yet, and the second a second later, with
// it. Every watch of a server shares one read.
func (h *Handler) WatchProcesses(ctx context.Context, req *connect.Request[dockwellv1.WatchProcessesRequest], stream *connect.ServerStream[dockwellv1.WatchProcessesResponse]) error {
	p, err := person(ctx)
	if err != nil {
		return err
	}
	srv, err := h.server(req.Msg.GetServerId())
	if err != nil {
		return err
	}
	session := p.Session.ID
	h.mu.Lock()
	if h.watching[session] >= MaxWatchesPerSession {
		h.mu.Unlock()
		return authapi.ReasonError(connect.CodeResourceExhausted, fmt.Errorf(
			"this browser already watches %d process lists, the most one session may. Close a tab that shows processes, then look again", MaxWatchesPerSession), "too_many_watches")
	}
	h.watching[session]++
	f, ok := h.feeds[srv.GetId()]
	if !ok {
		read, stop := context.WithCancel(context.WithoutCancel(ctx))
		f = &feed{watchers: map[chan struct{}]struct{}{}, stop: stop}
		h.feeds[srv.GetId()] = f
		go h.read(read, srv, f)
	}
	h.mu.Unlock()
	told := make(chan struct{}, 1)
	f.mu.Lock()
	f.watchers[told] = struct{}{}
	if f.last != nil || f.err != nil {
		told <- struct{}{}
	}
	f.mu.Unlock()
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.watching[session]--
		if h.watching[session] == 0 {
			delete(h.watching, session)
		}
		f.mu.Lock()
		delete(f.watchers, told)
		last := len(f.watchers) == 0
		f.mu.Unlock()
		if last && h.feeds[srv.GetId()] == f {
			delete(h.feeds, srv.GetId())
			f.stop()
		}
	}()
	var sent *dockwellv1.ProcessList
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-told:
		}
		f.mu.Lock()
		list, err := f.last, f.err
		f.mu.Unlock()
		if err != nil {
			return err
		}
		if list == nil || list == sent {
			continue
		}
		sent = list
		if err := stream.Send(&dockwellv1.WatchProcessesResponse{Processes: list}); err != nil {
			return err
		}
	}
}

// read reads srv's processes for f until ctx is done, which it is once nobody
// watches, or a read fails, which every watch then hears.
func (h *Handler) read(ctx context.Context, srv *dockwellv1.Server, f *feed) {
	tell := func(list *dockwellv1.ProcessList, err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.last, f.err = list, err
		for w := range f.watchers {
			select {
			case w <- struct{}{}:
			default:
			}
		}
	}
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		session, err := h.session(ctx, srv)
		if err != nil {
			tell(nil, err)
			h.drop(srv.GetId(), f)
			return
		}
		read, cancel := context.WithTimeout(ctx, readWait)
		list, err := session.Processes(read)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, agentapi.ErrOlderAgent):
			err = authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s's agent is older than the control plane, and can't list its processes. Update the agent on %s, then look again", srv.GetName(), srv.GetName()), "agent_outdated")
		case err != nil:
			err = authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't list its processes now: %w", srv.GetName(), plain(err)), authapi.ReasonUnavailable)
		}
		if err != nil {
			tell(nil, err)
			h.drop(srv.GetId(), f)
			return
		}
		tell(list, nil)
		wait = Every
		if list.GetCpuWindow() == nil {
			wait = time.Second
		}
	}
}

// drop forgets a feed that failed, so the next watch of its server starts anew.
func (h *Handler) drop(server string, f *feed) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.feeds[server] == f {
		delete(h.feeds, server)
	}
	f.stop()
}

// EndProcess ends a process on a server, once a passkey says so just now.
func (h *Handler) EndProcess(ctx context.Context, req *connect.Request[dockwellv1.EndProcessRequest]) (*connect.Response[dockwellv1.EndProcessResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	srv, err := h.server(m.GetServerId())
	if err != nil {
		return nil, err
	}
	if m.GetPid() == 0 || m.GetStartTicks() == 0 {
		return nil, authapi.Invalid(errors.New("say which process to end, by its PID and when it started, as the process list gives them"))
	}
	if m.GetPid() > MaxPID {
		return nil, authapi.Invalid(fmt.Errorf("there's no PID %d: Linux gives PIDs up to %d. Pick the process from the list", m.GetPid(), MaxPID))
	}
	target := Target(srv.GetId(), m.GetPid(), m.GetStartTicks())
	rec := h.recorder(ctx, p, srv)
	if m.GetProof() == "" {
		// Not a failure, as every end starts here. What matters is whether a passkey follows.
		h.auth.AskedForStepUp(p, auth.StepUpEndProcess, target, func() {
			if err := rec.record(EventEndUnconfirmed, Detail{PID: m.GetPid()}); err != nil {
				h.logger.WarnContext(ctx, "can't record an end that wasn't confirmed", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
			}
		})
		return nil, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsStepUp, authapi.ReasonNeedsStepUp)
	}
	giveBack, err := h.auth.HoldStepUp(p, m.GetProof(), auth.StepUpEndProcess, target)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		giveBack()
		return nil, err
	}
	// The process ends whether or not whoever asked stays to hear it.
	ctx = context.WithoutCancel(ctx)
	out, err := session.EndProcess(ctx, m.GetPid(), m.GetStartTicks())
	if err != nil {
		reason := resource.ReasonOf(err)
		if reason == collect.ReasonProcessGone || reason == collect.ReasonProcessProtected {
			// Nothing happened, so the passkey stands for another try.
			giveBack()
		}
		err = h.endError(srv, m.GetPid(), err)
		if reason != collect.ReasonProcessGone {
			h.note(ctx, rec, Detail{PID: m.GetPid(), Error: message(err)})
		}
		return nil, err
	}
	if err := rec.record(EventEnd, detailOf(m.GetPid(), out)); err != nil {
		// It has ended, and the person hears so.
		h.logger.ErrorContext(ctx, "ended a process, and can't record that it did", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
	}
	return connect.NewResponse(&dockwellv1.EndProcessResponse{Killed: out.GetKilled()}), nil
}

// endError says why a process couldn't be ended, as the API says it.
func (h *Handler) endError(srv *dockwellv1.Server, pid uint32, err error) error {
	switch resource.ReasonOf(err) {
	case collect.ReasonProcessGone:
		return authapi.ReasonError(connect.CodeNotFound, plain(err), "process_gone")
	case collect.ReasonProcessProtected:
		return authapi.ReasonError(connect.CodeFailedPrecondition, plain(err), "process_protected")
	}
	if errors.Is(err, agentapi.ErrOlderAgent) {
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s's agent is older than the control plane, and can't end a process. Update the agent on %s, then try again", srv.GetName(), srv.GetName()), "agent_outdated")
	}
	return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't end process %d now: %w", srv.GetName(), pid, plain(err)), authapi.ReasonUnavailable)
}

// plain is what a person reads of an agent's error: its plain words, without the
// server's name in front.
func plain(err error) error {
	if p, _ := resource.Explain(err); p != "" {
		return errors.New(p)
	}
	var se *resource.ServerError
	if errors.As(err, &se) {
		return se.Err
	}
	return err
}

// message is an error's words, without Connect's code in front.
func message(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}

// detailOf is what the record keeps of a process that ended: what it was called,
// who it ran as and where, but never its command line, which can hold a password
// typed with it, as every reader of the activity log, an API token too, reads it.
func detailOf(pid uint32, out *agentv1.EndProcessResult) Detail {
	was := out.GetProcess()
	d := Detail{PID: pid, Name: was.GetName(), User: was.GetUser(), Killed: out.GetKilled()}
	if c := was.GetContainer(); c != nil {
		d.Container = c.GetName()
		if d.Container == "" {
			d.Container = c.GetId()[:min(len(c.GetId()), 12)]
		}
	}
	return d
}

// recorder writes a server's audit events whether or not the caller stays.
type recorder struct {
	h      *Handler
	ctx    context.Context //nolint:containedctx // outlives the call, as what's recorded must
	p      auth.Principal
	client auth.Client
	srv    *dockwellv1.Server
}

func (h *Handler) recorder(ctx context.Context, p auth.Principal, srv *dockwellv1.Server) *recorder {
	client, _ := authapi.ClientFrom(ctx)
	return &recorder{h: h, ctx: context.WithoutCancel(ctx), p: p, client: client, srv: srv}
}

func (r *recorder) record(action string, d Detail) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return r.h.db.Update(r.ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(r.ctx, store.Event{
			ID: id.New(id.Event), At: r.h.now(), Actor: r.p.Actor(), Action: action, Subject: strconv.FormatUint(uint64(d.PID), 10),
			Target: r.srv.GetId(), Surface: r.client.Surface(r.p), Detail: string(b),
		})
	})
}

// note puts on record a process someone couldn't end, and why.
func (h *Handler) note(ctx context.Context, rec *recorder, d Detail) {
	if err := rec.record(EventEndFailed, d); err != nil {
		h.logger.WarnContext(ctx, "can't record a process that wasn't ended", slog.String("error", err.Error()))
	}
}
