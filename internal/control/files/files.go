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

// reach finds a server and its agent's stream, for a request's path. A server
// whose agent is moving to a new stream is waited for, as it does that each time
// its certificate is renewed.
func (h *Handler) reach(ctx context.Context, serverID, path string) (*agentapi.Session, *dockwellv1.Server, error) {
	if err := files.CheckPath(path); err != nil {
		return nil, nil, authapi.Invalid(err)
	}
	srv, err := h.fleet.Get(serverID)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", serverID), "not_found")
	}
	if err != nil {
		return nil, nil, h.rpcError(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, serverID, agentapi.MoveGrace)
	if !ok {
		return nil, nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't reach its files. Try again once it reconnects", srv.GetName()), "server_offline")
	}
	return session, srv, nil
}

// ListFiles returns what a directory on a server holds. It isn't on record: it
// returns names, and it runs on every click.
func (h *Handler) ListFiles(ctx context.Context, req *connect.Request[dockwellv1.ListFilesRequest]) (*connect.Response[dockwellv1.ListFilesResponse], error) {
	if _, err := person(ctx); err != nil {
		return nil, err
	}
	session, _, err := h.reach(ctx, req.Msg.GetServerId(), req.Msg.GetPath())
	if err != nil {
		return nil, err
	}
	out, err := session.ListFiles(ctx, req.Msg.GetPath())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(out.GetListing()), nil
}

// ReadFile returns a file on a server. The read goes on record first, and what
// can't go on record doesn't go out, as with a secret's reveal.
func (h *Handler) ReadFile(ctx context.Context, req *connect.Request[dockwellv1.ReadFileRequest]) (*connect.Response[dockwellv1.ReadFileResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	session, srv, err := h.reach(ctx, req.Msg.GetServerId(), req.Msg.GetPath())
	if err != nil {
		return nil, err
	}
	client, _ := authapi.ClientFrom(ctx)
	record := func(action, detail string) error {
		return h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: action, Subject: srv.GetId(), Target: srv.GetId(),
				Surface: client.Surface(p), Detail: detail,
			})
		})
	}
	out, err := session.ReadFile(ctx, req.Msg.GetPath())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		if r.GetKind() == agentv1.FileRefusal_KIND_REFUSED {
			// A refusal stands whether or not this does.
			detail := fmt.Sprintf("refused to read %s on %s, as it reaches %s", req.Msg.GetPath(), srv.GetName(), r.GetPath())
			if err := record(eventRefuse, detail); err != nil {
				h.logger.WarnContext(ctx, "can't record a refused read of a file", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
			}
		}
		return nil, refusal(r)
	}
	if err := record(eventRead, fmt.Sprintf("read %s on %s", out.GetFile().GetPath(), srv.GetName())); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(out.GetFile()), nil
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
