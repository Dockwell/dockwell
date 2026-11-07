// SPDX-License-Identifier: AGPL-3.0-only

// Package terminals serves dockwell.v1.TerminalService: a person's shell on a
// server, through its agent, which holds the shell apart from any stream. A
// terminal belongs to the person and the session that opened it, and ends with
// that session. Opening one takes a passkey. Its opening and closing go on record;
// what's typed and printed goes nowhere. Nothing here is a revision: a shell isn't
// managed state. See docs/adr/0037-a-terminal-on-a-server.md and
// docs/security/terminal.md.
package terminals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/terminal"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Audit events, each naming the terminal as its subject and its server as its
// target. eventOpening goes on record before a shell starts, and what can't go on
// record doesn't start; the rest are on Activity.
const (
	eventOpening     = "terminal.opening"
	EventOpen        = "terminal.open"
	EventOpenFailed  = "terminal.open_failed"
	EventClose       = "terminal.close"
	EventUnconfirmed = "terminal.open_unconfirmed"
)

// Actions are the events of terminals that go on Activity and a server's timeline.
var Actions = []string{EventOpen, EventOpenFailed, EventClose, EventUnconfirmed}

// Told is what a terminal's event on Activity says. It holds nothing typed or
// printed, and only this package writes it.
type Told struct {
	ID       string    `json:"id,omitempty"`     // none for one that asked for a passkey and never opened
	Server   string    `json:"server,omitempty"` // by name, for whoever reads the audit log
	Title    string    `json:"title,omitempty"`
	Opened   time.Time `json:"opened,omitzero"`
	Reason   int32     `json:"reason,omitempty"`
	ExitCode int32     `json:"exit_code,omitempty"`
	Ended    time.Time `json:"ended,omitzero"`
	Message  string    `json:"message,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// ReadTold reads what a terminal's event says.
func ReadTold(detail string) (Told, bool) {
	var t Told
	err := json.Unmarshal([]byte(detail), &t)
	return t, err == nil
}

// Proto is t as the timeline says it.
func (t Told) Proto() *dockwellv1.EventTerminal {
	out := &dockwellv1.EventTerminal{Id: t.ID, Title: t.Title, Error: t.Error}
	if !t.Opened.IsZero() {
		out.OpenTime = timestamppb.New(t.Opened)
	}
	if t.Reason != 0 {
		out.End = &dockwellv1.TerminalEnd{Reason: dockwellv1.TerminalEndReason(t.Reason), ExitCode: t.ExitCode, Message: t.Message}
		if !t.Ended.IsZero() {
			out.End.Time = timestamppb.New(t.Ended)
		}
	}
	return out
}

func (t Told) String() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// MaxOpen is how many terminals the control plane holds, across servers.
const MaxOpen = 32

// What one session may hold of those: half a server's, and half the control
// plane's, so a session, a stolen one included, can't take every place there is.
const (
	MaxPerSession      = terminal.MaxOpen / 2
	MaxPerSessionInAll = MaxOpen / 2
)

// HeartbeatEvery is how often an attachment says it's still there.
var HeartbeatEvery = 10 * time.Second

// RemovedEvery is how often a terminal looks for its server, which may have been
// removed from the fleet, as its agent can't say so once it is.
var RemovedEvery = 30 * time.Second

// Handler implements TerminalService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	auth   *auth.Service
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	terms map[string]*entry
}

var _ dockwellv1connect.TerminalServiceHandler = (*Handler)(nil)

// entry is a terminal the control plane knows of: whose it is, and its attachment.
type entry struct {
	id, server     string
	user, session  string // whose it is: no one else reaches it
	actor, surface string
	title          string
	opened         time.Time
	cols, rows     uint32
	stop           func() // lets go of the session it's tied to

	mu   sync.Mutex
	link *agentapi.TerminalLink
}

// New returns a handler that opens terminals through agents. people checks the
// passkey opening one takes, and ties each to the session that opened it.
func New(hub *fleet.Hub, agents *agentapi.Agents, people *auth.Service, db *store.DB, logger *slog.Logger) *Handler {
	h := &Handler{fleet: hub, agents: agents, auth: people, db: db, logger: logger, now: time.Now, terms: map[string]*entry{}}
	agents.OnTerminalEnded(h.ended)
	agents.OnConnect(h.reconcile)
	return h
}

// person is who is asking. The interceptor refuses tokens already, as these
// methods need a session; this holds if it didn't.
func person(ctx context.Context) (auth.Principal, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil || p.Session.ID == "" {
		return auth.Principal{}, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	return p, nil
}

func (h *Handler) server(ctx context.Context, serverID string) (*dockwellv1.Server, error) {
	srv, err := h.fleet.Get(serverID)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", serverID), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return srv, nil
}

func (h *Handler) session(ctx context.Context, srv *dockwellv1.Server) (*agentapi.Session, error) {
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't reach its terminals. Try again once it reconnects", srv.GetName()), "server_offline")
	}
	return session, nil
}

// mine finds one of p's terminals. Anyone else's reads as not there.
func (h *Handler) mine(p auth.Principal, terminalID string) (*entry, error) {
	h.mu.Lock()
	e := h.terms[terminalID]
	h.mu.Unlock()
	if e == nil || e.user != p.User.ID || e.session != p.Session.ID {
		return nil, authapi.ReasonError(connect.CodeNotFound,
			fmt.Errorf("you have no open terminal %s here. It may have ended; open another", terminalID), "not_found")
	}
	return e, nil
}

func (e *entry) proto() *dockwellv1.Terminal {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &dockwellv1.Terminal{Id: e.id, ServerId: e.server, Title: e.title, OpenTime: timestamppb.New(e.opened), Columns: e.cols, Rows: e.rows}
}

// OpenTerminal starts root's login shell on a server, once everything else is
// checked and a passkey confirms it.
func (h *Handler) OpenTerminal(ctx context.Context, req *connect.Request[dockwellv1.OpenTerminalRequest]) (*connect.Response[dockwellv1.OpenTerminalResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	cols, rows := req.Msg.GetColumns(), req.Msg.GetRows()
	if cols < 2 || cols > terminal.MaxColumns || rows < 1 || rows > terminal.MaxRows {
		return nil, authapi.Invalid(fmt.Errorf("a terminal is %d × %d, and it can be from 2 × 1 to %d × %d characters. Make it fit, then open it again",
			cols, rows, terminal.MaxColumns, terminal.MaxRows))
	}
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !session.Terminals() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to open terminals. Update the agent on %s, then try again", srv.GetName(), srv.GetName()), "agent_outdated")
	}
	if err := h.room(srv, p.Session.ID); err != nil {
		return nil, err
	}
	record := h.recorder(ctx, p, srv)
	// A proof opens one terminal, and is held until the shell starts, as opening it
	// can still be refused.
	proof := req.Msg.GetProof()
	if proof == "" {
		h.auth.AskedForStepUp(p, auth.StepUpOpenTerminal, srv.GetId(), func() {
			if _, err := record(EventUnconfirmed, "", Told{Server: srv.GetName(), Error: "asked for a passkey, and none came"}.String()); err != nil {
				h.logger.WarnContext(ctx, "can't record a terminal that wasn't confirmed", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
			}
		})
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("a terminal on %s is a root shell, so opening one takes your passkey. Confirm with it, then open it again", srv.GetName()),
			authapi.ReasonNeedsStepUp)
	}
	giveBack, err := h.auth.HoldStepUp(p, proof, auth.StepUpOpenTerminal, srv.GetId())
	if err != nil {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	ctx = context.WithoutCancel(ctx)
	termID := id.New(id.Terminal)
	client, _ := authapi.ClientFrom(ctx)
	e := &entry{
		id: termID, server: srv.GetId(), user: p.User.ID, session: p.Session.ID, actor: p.Actor(), surface: client.Surface(p),
		opened: h.now(), cols: cols, rows: rows,
	}
	// Its place is taken before the shell starts, so two can't both take the last,
	// and a shell that ends at once finds what it ends.
	if err := h.reserve(srv, e); err != nil {
		giveBack()
		return nil, err
	}
	opening, err := record(eventOpening, termID, "opening a terminal on "+srv.GetName())
	if err != nil {
		h.forget(termID)
		giveBack()
		return nil, h.rpcError(ctx, err)
	}
	opened, err := session.OpenTerminal(ctx, &agentv1.OpenTerminalCall{Id: termID, Columns: cols, Rows: rows, Record: opening})
	if err != nil {
		h.forget(termID)
		giveBack()
		err = h.rpcError(ctx, err)
		why := Told{ID: termID, Server: srv.GetName(), Error: connect.NewError(connect.CodeUnknown, err).Message()}
		if _, rerr := record(EventOpenFailed, termID, why.String()); rerr != nil {
			h.logger.WarnContext(ctx, "can't record a terminal that didn't open", slog.String("server", srv.GetId()), slog.String("error", rerr.Error()))
		}
		return nil, err
	}
	e.mu.Lock()
	e.title = opened.GetTitle()
	e.mu.Unlock()
	if _, err := record(EventOpen, termID, Told{ID: termID, Server: srv.GetName(), Title: opened.GetTitle(), Opened: e.opened}.String()); err != nil {
		// What can't go on record doesn't stay open.
		if _, cerr := session.CloseTerminal(ctx, termID, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED); cerr != nil {
			h.logger.ErrorContext(ctx, "opened a terminal that can't go on record, and can't close it; it closes once nobody is attached",
				slog.String("terminal", termID), slog.String("error", cerr.Error()))
		}
		return nil, h.rpcError(ctx, err)
	}
	// It ends with the session that opened it: signed out, revoked or past its limits.
	// Following it isn't use: a terminal nobody is attached to keeps no session alive,
	// and its address stays the one the session last really came from.
	tied, stop := h.auth.Follow(context.Background(), client, p)
	h.mu.Lock()
	if h.terms[termID] == nil {
		// It ended as it opened, and went on record so.
		h.mu.Unlock()
		stop()
		return connect.NewResponse(&dockwellv1.OpenTerminalResponse{Terminal: e.proto()}), nil
	}
	e.stop = stop
	h.mu.Unlock()
	go h.watch(tied, e) //nolint:gosec // it lasts as long as the terminal, past the request, and ends with the session tied to it
	h.logger.InfoContext(ctx, "opened a terminal", slog.String("terminal", termID), slog.String("server", srv.GetId()), slog.String("user", p.User.ID))
	return connect.NewResponse(&dockwellv1.OpenTerminalResponse{Terminal: e.proto()}), nil
}

// reserve takes e's place, if there's room.
func (h *Handler) reserve(srv *dockwellv1.Server, e *entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.roomLocked(srv, e.session); err != nil {
		return err
	}
	h.terms[e.id] = e
	return nil
}

// room refuses a terminal past what a server, and the control plane, hold, and
// past what one session may hold of those.
func (h *Handler) room(srv *dockwellv1.Server, session string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.roomLocked(srv, session)
}

func (h *Handler) roomLocked(srv *dockwellv1.Server, session string) error {
	here, mine, mineHere := 0, 0, 0
	for _, e := range h.terms {
		if e.server == srv.GetId() {
			here++
		}
		if e.session == session {
			mine++
			if e.server == srv.GetId() {
				mineHere++
			}
		}
	}
	switch {
	case mineHere >= MaxPerSession:
		return authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("you have %d terminals open on %s from this session, which is as many as one session keeps there. Close one, then open another", MaxPerSession, srv.GetName()), "too_many_terminals")
	case mine >= MaxPerSessionInAll:
		return authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("you have %d terminals open from this session, which is as many as one session keeps. Close one, then open another", MaxPerSessionInAll), "too_many_terminals")
	case here >= terminal.MaxOpen:
		return authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("%s has %d terminals open, which is as many as it keeps. Close one, then open another", srv.GetName(), terminal.MaxOpen), "too_many_terminals")
	case len(h.terms) >= MaxOpen:
		return authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("this control plane has %d terminals open across its servers, which is as many as it keeps. Close one, then open another", MaxOpen), "too_many_terminals")
	}
	return nil
}

// ListTerminals returns p's open terminals on a server, from this session.
func (h *Handler) ListTerminals(ctx context.Context, req *connect.Request[dockwellv1.ListTerminalsRequest]) (*connect.Response[dockwellv1.ListTerminalsResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	var list []*entry
	for _, e := range h.terms {
		if e.server == srv.GetId() && e.user == p.User.ID && e.session == p.Session.ID {
			list = append(list, e)
		}
	}
	h.mu.Unlock()
	slices.SortFunc(list, func(a, b *entry) int { return a.opened.Compare(b.opened) })
	out := &dockwellv1.ListTerminalsResponse{}
	for _, e := range list {
		out.Terminals = append(out.Terminals, e.proto())
	}
	return connect.NewResponse(out), nil
}

// AttachTerminal carries what a terminal prints to the client, from where it asks,
// across the agent's moves to new streams, until the terminal ends, it's attached
// from somewhere else, or the client goes.
func (h *Handler) AttachTerminal(ctx context.Context, req *connect.Request[dockwellv1.AttachTerminalRequest], stream *connect.ServerStream[dockwellv1.AttachTerminalResponse]) error {
	p, err := person(ctx)
	if err != nil {
		return err
	}
	e, err := h.mine(p, req.Msg.GetTerminalId())
	if err != nil {
		return err
	}
	srv, err := h.server(ctx, e.server)
	if err != nil {
		return err
	}
	// The heartbeat stops before this returns, as the stream is the handler's alone
	// once it does: a send under way as it closes would race with the closing.
	var beating sync.WaitGroup
	defer beating.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sendMu sync.Mutex
	send := func(m *dockwellv1.AttachTerminalResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}
	beating.Go(func() {
		beat := time.NewTicker(HeartbeatEvery)
		defer beat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-beat.C:
				if send(&dockwellv1.AttachTerminalResponse{Heartbeat: true}) != nil {
					cancel()
					return
				}
			}
		}
	})
	offset, cols, rows := req.Msg.GetOffset(), req.Msg.GetColumns(), req.Msg.GetRows()
	if cols > 0 && rows > 0 {
		cols, rows = min(max(cols, 2), terminal.MaxColumns), min(max(rows, 1), terminal.MaxRows)
	}
	first := true
	for {
		session, err := h.session(ctx, srv)
		if err != nil {
			return err
		}
		var mine *agentapi.TerminalLink
		attached := func(l *agentapi.TerminalLink) {
			mine = l
			e.mu.Lock()
			e.link = l
			e.mu.Unlock()
		}
		res, err := session.AttachTerminal(ctx, &agentv1.AttachTerminalCall{Id: e.id, Offset: offset, Columns: cols, Rows: rows}, attached,
			func(up *agentv1.TerminalRequest) error {
				if up.GetStream() != 0 {
					offset = up.GetOffset()
					if !first {
						return nil // carried on over a new stream, as if nothing happened
					}
					first = false
					return send(&dockwellv1.AttachTerminalResponse{Attached: true, Offset: up.GetOffset(), InputOffset: up.GetInputOffset()})
				}
				if len(up.GetData()) == 0 {
					return nil
				}
				offset = up.GetOffset() + uint64(len(up.GetData()))
				return send(&dockwellv1.AttachTerminalResponse{Offset: up.GetOffset(), Data: up.GetData()})
			})
		e.mu.Lock()
		if e.link == mine {
			e.link = nil
		}
		e.mu.Unlock()
		switch {
		case errors.Is(err, agentapi.ErrMoving):
			cols, rows = 0, 0
			continue // the agent is on its way back, and its terminal with it
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			return h.rpcError(ctx, err)
		case res.GetMoved():
			return send(&dockwellv1.AttachTerminalResponse{Moved: true})
		case res.GetEnd() != nil:
			return send(&dockwellv1.AttachTerminalResponse{End: res.GetEnd()})
		}
		// The agent let it go with no reason, as when its own stream ended.
		return authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent stopped carrying the terminal. Attach again", srv.GetName()), "server_offline")
	}
}

// WriteTerminal types into a terminal, through its attachment.
func (h *Handler) WriteTerminal(ctx context.Context, req *connect.Request[dockwellv1.WriteTerminalRequest]) (*connect.Response[dockwellv1.WriteTerminalResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	data := req.Msg.GetData()
	if len(data) > terminal.MaxWrite {
		return nil, authapi.Invalid(fmt.Errorf("that write is %d bytes, and one carries %d at most. Send it in parts", len(data), terminal.MaxWrite))
	}
	cols, rows := req.Msg.GetColumns(), req.Msg.GetRows()
	if (cols > 0 || rows > 0) && (cols < 2 || cols > terminal.MaxColumns || rows < 1 || rows > terminal.MaxRows) {
		return nil, authapi.Invalid(fmt.Errorf("a terminal can be from 2 × 1 to %d × %d characters, not %d × %d", terminal.MaxColumns, terminal.MaxRows, cols, rows))
	}
	e, err := h.mine(p, req.Msg.GetTerminalId())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	link := e.link
	if cols > 0 {
		e.cols, e.rows = cols, rows
	}
	e.mu.Unlock()
	if link == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			errors.New("this terminal isn't attached here, so nothing was typed. Attach to it, then type again"), "not_attached")
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	taken, err := link.Write(wait, req.Msg.GetOffset(), data, cols, rows)
	switch {
	case errors.Is(err, agentapi.ErrNotAttached):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			errors.New("this terminal's attachment ended as you typed. Attach to it again, then send what it hasn't taken"), "not_attached")
	case err != nil && ctx.Err() == nil && wait.Err() != nil:
		return nil, authapi.ReasonError(connect.CodeDeadlineExceeded,
			errors.New("the terminal didn't say what it took within 10 s. Send it again; nothing is typed twice"), "agent_timeout")
	case err != nil:
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.WriteTerminalResponse{Offset: taken}), nil
}

// CloseTerminal hangs up a terminal's shell.
func (h *Handler) CloseTerminal(ctx context.Context, req *connect.Request[dockwellv1.CloseTerminalRequest]) (*connect.Response[dockwellv1.CloseTerminalResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	e, err := h.mine(p, req.Msg.GetTerminalId())
	if err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, e.server)
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	end, err := session.CloseTerminal(context.WithoutCancel(ctx), e.id, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED)
	if err != nil {
		var failed *resource.ServerError
		if !errors.As(err, &failed) {
			return nil, h.rpcError(ctx, err)
		}
		// It had ended already; how is on its way, and goes on record then.
		end = &dockwellv1.TerminalEnd{Reason: dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED, Time: timestamppb.New(h.now()), Message: "It had ended."}
	}
	return connect.NewResponse(&dockwellv1.CloseTerminalResponse{End: end}), nil
}

// watch ends a terminal with the session that opened it, or with its server, once
// it's removed from the fleet: nobody can reach a removed server's terminal, and its
// agent, which can't reach the control plane either, hangs it up once nobody has
// been attached for a while.
func (h *Handler) watch(tied context.Context, e *entry) {
	tick := time.NewTicker(RemovedEvery)
	defer tick.Stop()
	for {
		select {
		case <-tied.Done():
			if h.forget(e.id) == nil {
				return // it ended first
			}
			h.closeFor(e, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SIGNED_OUT)
			return
		case <-tick.C:
			if _, err := h.fleet.Get(e.server); !errors.Is(err, fleet.ErrNotFound) || h.forget(e.id) == nil {
				continue
			}
			e.mu.Lock()
			title := e.title
			e.mu.Unlock()
			h.recordClose(context.Background(), e.server, e.id, e.actor, e.surface, title, e.opened, &dockwellv1.TerminalEnd{
				Reason: dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SERVER_REMOVED, Time: timestamppb.New(h.now()),
				Message: "Its server was removed from Dockwell.",
			})
			return
		}
	}
}

// forget drops a terminal the control plane knows of, and returns it if it was there.
func (h *Handler) forget(terminalID string) *entry {
	h.mu.Lock()
	e := h.terms[terminalID]
	delete(h.terms, terminalID)
	var stop func()
	if e != nil {
		stop = e.stop
	}
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
	return e
}

// closeFor hangs up a terminal the control plane let go of, for reason. How it
// ended goes on record as the agent says it.
func (h *Handler) closeFor(e *entry, reason dockwellv1.TerminalEndReason) {
	ctx := context.Background()
	session, ok := h.agents.Session(e.server)
	if !ok {
		// Its agent ends it once nobody has been attached for a while, and says so
		// when it's back.
		return
	}
	if _, err := session.CloseTerminal(ctx, e.id, reason); err != nil {
		h.logger.WarnContext(ctx, "can't close a terminal; it closes once nobody has been attached for a while",
			slog.String("terminal", e.id), slog.String("error", err.Error()))
	}
}

// ended puts on record how a terminal ended, as its agent says, for whoever opened
// it. An agent speaks for its own server alone, so it has to name a terminal there.
func (h *Handler) ended(server string, msg *agentv1.TerminalEnded) {
	ctx := context.Background()
	e := h.forget(msg.GetId())
	if e != nil && e.server != server {
		h.logger.WarnContext(ctx, "an agent said a terminal on another server ended", slog.String("server", server), slog.String("terminal", msg.GetId()))
		h.mu.Lock()
		h.terms[e.id] = e
		h.mu.Unlock()
		return
	}
	var actor, surface, title string
	var opened time.Time
	if e != nil {
		e.mu.Lock()
		actor, surface, title, opened = e.actor, e.surface, e.title, e.opened
		e.mu.Unlock()
	} else {
		// One this control plane doesn't know, as it restarted since: its opening
		// says whose it was.
		var opening store.Event
		err := h.db.View(ctx, func(tx *store.Tx) (err error) {
			opening, err = tx.GetEvent(ctx, msg.GetRecord())
			return err
		})
		if err != nil || opening.Action != eventOpening || opening.Target != server || opening.Subject != msg.GetId() {
			h.logger.WarnContext(ctx, "an agent said how a terminal ended, and it can't go on record", slog.String("server", server), slog.String("terminal", msg.GetId()))
			return
		}
		actor, surface, opened = opening.Actor, opening.Surface, msg.GetOpened().AsTime()
	}
	h.recordClose(ctx, server, msg.GetId(), actor, surface, title, opened, msg.GetEnd())
}

func (h *Handler) recordClose(ctx context.Context, server, terminalID, actor, surface, title string, opened time.Time, end *dockwellv1.TerminalEnd) {
	name := server
	if srv, err := h.fleet.Get(server); err == nil {
		name = srv.GetName()
	}
	at := h.now()
	if end.GetTime() != nil {
		at = end.GetTime().AsTime()
	}
	detail := Told{
		ID: terminalID, Server: name, Title: title, Opened: opened, Reason: int32(end.GetReason()), ExitCode: end.GetExitCode(), Ended: at, Message: end.GetMessage(),
	}.String()
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: actor, Action: EventClose, Subject: terminalID, Target: server,
			Surface: surface, Detail: detail,
		})
	}); err != nil {
		h.logger.ErrorContext(ctx, "can't record that a terminal closed", slog.String("terminal", terminalID), slog.String("error", err.Error()))
	}
}

// reconcile squares what a server's agent has with what the control plane knows,
// as the agent connects: a terminal the agent doesn't have ended with it, and one
// the control plane doesn't know, as it restarted, is closed, as nobody can reach it.
func (h *Handler) reconcile(server string) {
	session, ok := h.agents.Session(server)
	if !ok || !session.Terminals() {
		return
	}
	has := session.OpenTerminals()
	ctx := context.Background()
	h.mu.Lock()
	var gone []*entry
	for _, e := range h.terms {
		if e.server == server && e.opened.Before(session.Opened()) && !slices.Contains(has, e.id) {
			gone = append(gone, e)
		}
	}
	var unknown []string
	for _, termID := range has {
		if h.terms[termID] == nil {
			unknown = append(unknown, termID)
		}
	}
	h.mu.Unlock()
	for _, e := range gone {
		if h.forget(e.id) == nil {
			continue
		}
		e.mu.Lock()
		title := e.title
		e.mu.Unlock()
		h.recordClose(ctx, server, e.id, e.actor, e.surface, title, e.opened, &dockwellv1.TerminalEnd{
			Reason: dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED, Time: timestamppb.New(h.now()),
			Message: "Its server's agent restarted, and its shell with it.",
		})
	}
	for _, termID := range unknown {
		if _, err := session.CloseTerminal(ctx, termID, dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CONTROL_PLANE_RESTARTED); err != nil {
			h.logger.WarnContext(ctx, "can't close a terminal the control plane doesn't know; it closes once nobody has been attached for a while",
				slog.String("terminal", termID), slog.String("error", err.Error()))
		}
	}
}

// recorder writes a server's audit events for what p does with its terminals.
func (h *Handler) recorder(ctx context.Context, p auth.Principal, srv *dockwellv1.Server) func(action, subject, detail string) (string, error) {
	client, _ := authapi.ClientFrom(ctx)
	ctx = context.WithoutCancel(ctx)
	return func(action, subject, detail string) (string, error) {
		event := id.New(id.Event)
		return event, h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: event, At: h.now(), Actor: p.Actor(), Action: action, Subject: subject, Target: srv.GetId(),
				Surface: client.Surface(p), Detail: detail,
			})
		})
	}
}

func (h *Handler) rpcError(ctx context.Context, err error) error {
	var cerr *connect.Error
	var failed *resource.ServerError
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, resource.ErrLost), errors.Is(err, resource.ErrNotConnected):
		return authapi.ReasonError(connect.CodeUnavailable, err, "server_offline")
	case errors.Is(err, context.DeadlineExceeded):
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "agent_timeout")
	case errors.As(err, &failed):
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "agent_failed")
	}
	h.logger.ErrorContext(ctx, "terminal call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't reach that terminal. Try again, and check its logs if it keeps failing"))
}
