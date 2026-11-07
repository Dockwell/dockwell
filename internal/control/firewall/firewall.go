// SPDX-License-Identifier: AGPL-3.0-only

// Package firewall serves dockwell.v1.FirewallService: what filters a server's
// inbound traffic, as its agent reads it, changing nothing. See
// docs/adr/0052-a-servers-firewall-ssh-and-fail2ban.md.
package firewall

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
)

// Handler implements FirewallService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
}

var _ dockwellv1connect.FirewallServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents.
func New(hub *fleet.Hub, agents *agentapi.Agents) *Handler {
	return &Handler{fleet: hub, agents: agents}
}

// GetServerFirewall asks a server's agent what filters its inbound traffic.
func (h *Handler) GetServerFirewall(ctx context.Context, req *connect.Request[dockwellv1.GetServerFirewallRequest]) (*connect.Response[dockwellv1.GetServerFirewallResponse], error) {
	srv, err := h.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", req.Msg.GetServerId()), "not_found")
	}
	if err != nil {
		return nil, err
	}
	var session *agentapi.Session
	if h.agents != nil && srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
		session, _ = h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so nothing can say what filters its traffic now. Check the server; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	read, err := session.Firewall(ctx)
	if err != nil {
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't say what filters its traffic now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return connect.NewResponse(&dockwellv1.GetServerFirewallResponse{Firewall: read}), nil
}
