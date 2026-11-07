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

// Handler implements ProcessService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	auth   *auth.Service
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time
}

var _ dockwellv1connect.ProcessServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents. people checks the
// passkey ending a process takes.
func New(hub *fleet.Hub, agents *agentapi.Agents, people *auth.Service, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{fleet: hub, agents: agents, auth: people, db: db, logger: logger, now: time.Now}
}

// Detail is what an event of a process keeps, as EventProcess says it.
type Detail struct {
	PID       uint32 `json:"pid"`
	Name      string `json:"name"`
	Command   string `json:"command,omitempty"`
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
	return &dockwellv1.EventProcess{Pid: d.PID, Name: d.Name, Command: d.Command, User: d.User, Container: d.Container, Killed: d.Killed, Error: d.Error}, true
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

// WatchProcesses reads a server's processes every Every for as long as the watch is
// open, and sends each list. The first comes at once, without CPU, as there's
// nothing to measure it against yet, and the second a second later, with it.
func (h *Handler) WatchProcesses(ctx context.Context, req *connect.Request[dockwellv1.WatchProcessesRequest], stream *connect.ServerStream[dockwellv1.WatchProcessesResponse]) error {
	if _, err := person(ctx); err != nil {
		return err
	}
	srv, err := h.server(req.Msg.GetServerId())
	if err != nil {
		return err
	}
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		session, err := h.session(ctx, srv)
		if err != nil {
			return err
		}
		read, cancel := context.WithTimeout(ctx, readWait)
		list, err := session.Processes(read)
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil //nolint:nilerr // the client went away, and nothing failed
		case errors.Is(err, agentapi.ErrOlderAgent):
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s's agent is older than the control plane, and can't list its processes. Update the agent on %s, then look again", srv.GetName(), srv.GetName()), "agent_outdated")
		case err != nil:
			return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't list its processes now: %w", srv.GetName(), plain(err)), authapi.ReasonUnavailable)
		}
		if err := stream.Send(&dockwellv1.WatchProcessesResponse{Processes: list}); err != nil {
			return err
		}
		wait = Every
		if list.GetCpuWindow() == nil {
			wait = time.Second
		}
	}
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
	target := srv.GetId() + ":" + strconv.FormatUint(uint64(m.GetPid()), 10)
	record := h.recorder(ctx, p, srv)
	if m.GetProof() == "" {
		// Not a failure, as every end starts here. What matters is whether a passkey follows.
		h.auth.AskedForStepUp(p, auth.StepUpEndProcess, target, func() {
			if err := record(EventEndUnconfirmed, Detail{PID: m.GetPid()}); err != nil {
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
			h.note(ctx, record, Detail{PID: m.GetPid(), Error: message(err)})
		}
		return nil, err
	}
	if err := record(EventEnd, detailOf(m.GetPid(), out)); err != nil {
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

// detailOf is what the record keeps of a process that ended.
func detailOf(pid uint32, out *agentv1.EndProcessResult) Detail {
	was := out.GetProcess()
	d := Detail{PID: pid, Name: was.GetName(), Command: was.GetCommand(), User: was.GetUser(), Killed: out.GetKilled()}
	if c := was.GetContainer(); c != nil {
		d.Container = c.GetName()
		if d.Container == "" {
			d.Container = c.GetId()[:min(len(c.GetId()), 12)]
		}
	}
	return d
}

// recorder writes a server's audit events for what p does to its processes. It
// writes whether or not the caller stays.
func (h *Handler) recorder(ctx context.Context, p auth.Principal, srv *dockwellv1.Server) func(action string, d Detail) error {
	client, _ := authapi.ClientFrom(ctx)
	ctx = context.WithoutCancel(ctx)
	return func(action string, d Detail) error {
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: action, Subject: strconv.FormatUint(uint64(d.PID), 10),
				Target: srv.GetId(), Surface: client.Surface(p), Detail: string(b),
			})
		})
	}
}

// note puts on record a process someone couldn't end, and why.
func (h *Handler) note(ctx context.Context, record func(string, Detail) error, d Detail) {
	if err := record(EventEndFailed, d); err != nil {
		h.logger.WarnContext(ctx, "can't record a process that wasn't ended", slog.String("error", err.Error()))
	}
}
