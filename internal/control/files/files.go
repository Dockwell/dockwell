// SPDX-License-Identifier: AGPL-3.0-only

// Package files serves dockwell.v1.FileService: a person browsing and reading the
// files on a server, through its agent, which decides what's refused. Nothing
// here is a revision: a file on a server isn't managed state. See
// docs/adr/0031-files-on-a-server.md and docs/security/files.md.
package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Audit events.
const (
	eventRead   = "file.read"
	eventRefuse = "file.refuse"
)

// Handler implements FileService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time
}

var _ dockwellv1connect.FileServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents.
func New(hub *fleet.Hub, agents *agentapi.Agents, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{fleet: hub, agents: agents, db: db, logger: logger, now: time.Now}
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

// server finds a server by its ID.
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

// session finds a server's agent. One moving to a new stream is waited for, as it
// does that each time its certificate is renewed.
func (h *Handler) session(ctx context.Context, srv *dockwellv1.Server) (*agentapi.Session, error) {
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't reach its files. Try again once it reconnects", srv.GetName()), "server_offline")
	}
	return session, nil
}

// ListFiles returns what a directory on a server holds. A listing that works isn't
// on record: it returns names, and it runs on every click. A refused one is, as
// someone looking for what isn't theirs mostly finds nothing. One that can't reach
// its server isn't, as the panel asks again by itself until it does.
func (h *Handler) ListFiles(ctx context.Context, req *connect.Request[dockwellv1.ListFilesRequest]) (*connect.Response[dockwellv1.ListFilesResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	path := req.Msg.GetPath()
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	record := h.recorder(ctx, p, srv)
	if err := files.CheckPath(path); err != nil {
		h.noteRefusal(ctx, record, "list", path, srv, err)
		return nil, authapi.Invalid(err)
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	out, err := session.ListFiles(ctx, path)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		err := refusal(r)
		h.noteRefusal(ctx, record, "list", path, srv, err)
		return nil, err
	}
	return connect.NewResponse(out.GetListing()), nil
}

// ReadFile returns a file on a server. The read goes on record first, and what
// can't go on record doesn't go out, as with a secret's reveal. A read that doesn't
// come to anything is on record too, however it failed.
func (h *Handler) ReadFile(ctx context.Context, req *connect.Request[dockwellv1.ReadFileRequest]) (*connect.Response[dockwellv1.ReadFileResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	path := req.Msg.GetPath()
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	record := h.recorder(ctx, p, srv)
	failed := func(err error) error {
		h.noteRefusal(ctx, record, "read", path, srv, err)
		return err
	}
	if err := files.CheckPath(path); err != nil {
		return nil, failed(authapi.Invalid(err))
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, failed(err)
	}
	out, err := session.ReadFile(ctx, path)
	if err != nil {
		return nil, failed(h.rpcError(ctx, err))
	}
	if r := out.GetRefusal(); r != nil {
		return nil, failed(refusal(r))
	}
	read := out.GetFile().GetPath()
	if err := record(eventRead, read, fmt.Sprintf("read %s on %s", read, srv.GetName())); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(out.GetFile()), nil
}

// recorder writes a server's audit events for what p does to one of its files, by
// a path that's fit to store, as a name can hold anything but a slash. It writes
// whether or not the caller stays.
func (h *Handler) recorder(ctx context.Context, p auth.Principal, srv *dockwellv1.Server) func(action, path, detail string) error {
	client, _ := authapi.ClientFrom(ctx)
	ctx = context.WithoutCancel(ctx)
	return func(action, path, detail string) error {
		return h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: action, Subject: files.Printable(path), Target: srv.GetId(),
				Surface: client.Surface(p), Detail: files.Printable(detail),
			})
		})
	}
}

// noteRefusal puts on record a read or a listing that came to nothing, and why. A
// refusal stands whether or not this does. A caller that left isn't a refusal.
func (h *Handler) noteRefusal(ctx context.Context, record func(action, path, detail string) error, doing, path string, srv *dockwellv1.Server, why error) {
	if errors.Is(why, context.Canceled) || connect.CodeOf(why) == connect.CodeCanceled {
		return
	}
	detail := fmt.Sprintf("couldn't %s %s on %s: %s", doing, path, srv.GetName(), why.Error())
	if err := record(eventRefuse, path, detail); err != nil {
		h.logger.WarnContext(ctx, "can't record a refused file", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
	}
}

// refusal is what the agent said, as the API says it.
func refusal(r *agentv1.FileRefusal) error {
	err := errors.New(r.GetMessage())
	switch r.GetKind() {
	case agentv1.FileRefusal_KIND_NOT_FOUND:
		return authapi.ReasonError(connect.CodeNotFound, err, "not_found")
	case agentv1.FileRefusal_KIND_REFUSED:
		return authapi.ReasonError(connect.CodePermissionDenied, err, "file_refused")
	case agentv1.FileRefusal_KIND_NOT_A_DIRECTORY:
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "not_a_directory")
	case agentv1.FileRefusal_KIND_NOT_A_FILE:
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "not_a_file")
	case agentv1.FileRefusal_KIND_NOT_ANSWERING:
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "path_not_answering")
	case agentv1.FileRefusal_KIND_DENIED, agentv1.FileRefusal_KIND_UNSPECIFIED:
	}
	return authapi.ReasonError(connect.CodePermissionDenied, err, "file_denied")
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
	h.logger.ErrorContext(ctx, "file call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't reach that file; nothing went out. Try again, and check its logs if it keeps failing"))
}
