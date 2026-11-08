// SPDX-License-Identifier: AGPL-3.0-only

// Package importapi serves ImportService: what another panel runs on a server, and
// what an import would do with each thing (docs/adr/0056-importing-a-1panel-server.md).
package importapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
)

// Handler reads what other panels keep, through each server's agent.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	logger *slog.Logger
}

var _ dockwellv1connect.ImportServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents.
func New(hub *fleet.Hub, agents *agentapi.Agents, logger *slog.Logger) *Handler {
	return &Handler{fleet: hub, agents: agents, logger: logger}
}

// ImportOnePanel reads what 1Panel keeps on a server, and says what an import would
// do with each thing. It plans no change yet: that comes next.
func (h *Handler) ImportOnePanel(ctx context.Context, req *connect.Request[dockwellv1.ImportOnePanelRequest]) (*connect.Response[dockwellv1.ImportOnePanelResponse], error) {
	srv, err := h.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", req.Msg.GetServerId()), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't read what 1Panel keeps there. Try again once it reconnects", srv.GetName()), "server_offline")
	}
	if !session.ReadsOnePanel() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to read 1Panel's records. Update the agent on %s, then try again", srv.GetName(), srv.GetName()), "agent_outdated")
	}
	out, err := session.ReadOnePanel(ctx)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if out.GetFound() == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s doesn't run 1Panel: %s. Run this for a server where 1Panel is installed", srv.GetName(), out.GetNotHere()), "onepanel_not_found")
	}
	return connect.NewResponse(&dockwellv1.ImportOnePanelResponse{Found: out.GetFound(), Steps: Steps(out.GetFound())}), nil
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
	h.logger.ErrorContext(ctx, "an import's read failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read what 1Panel keeps. Try again, and check its logs if it keeps failing"))
}
