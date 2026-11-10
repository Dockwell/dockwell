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
	srv, session, err := h.session(ctx, req.Msg.GetServerId(), "what filters its traffic")
	if err != nil {
		return nil, err
	}
	read, err := session.Firewall(ctx)
	if err != nil {
		return nil, readError(srv, "the firewall", "what filters its traffic", err)
	}
	return connect.NewResponse(&dockwellv1.GetServerFirewallResponse{Firewall: read}), nil
}

// GetServerSsh asks a server's agent how SSH lets people in.
//
//nolint:revive // the generated FirewallServiceHandler names it
func (h *Handler) GetServerSsh(ctx context.Context, req *connect.Request[dockwellv1.GetServerSshRequest]) (*connect.Response[dockwellv1.GetServerSshResponse], error) {
	srv, session, err := h.session(ctx, req.Msg.GetServerId(), "how SSH lets people in")
	if err != nil {
		return nil, err
	}
	read, err := session.SSH(ctx)
	if err != nil {
		return nil, readError(srv, "SSH", "how SSH lets people in", err)
	}
	return connect.NewResponse(&dockwellv1.GetServerSshResponse{Ssh: read}), nil
}

// GetServerFail2Ban asks a server's agent what fail2ban does.
func (h *Handler) GetServerFail2Ban(ctx context.Context, req *connect.Request[dockwellv1.GetServerFail2BanRequest]) (*connect.Response[dockwellv1.GetServerFail2BanResponse], error) {
	srv, session, err := h.session(ctx, req.Msg.GetServerId(), "what fail2ban does")
	if err != nil {
		return nil, err
	}
	read, err := session.Fail2ban(ctx)
	if err != nil {
		return nil, readError(srv, "fail2ban", "what fail2ban does", err)
	}
	return connect.NewResponse(&dockwellv1.GetServerFail2BanResponse{Fail2Ban: read}), nil
}

// session is a server and its agent's session, which a read goes through, or
// why there's none: what says what the read would have said.
func (h *Handler) session(ctx context.Context, serverID, what string) (*dockwellv1.Server, *agentapi.Session, error) {
	srv, err := h.fleet.Get(serverID)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", serverID), "not_found")
	}
	if err != nil {
		return nil, nil, err
	}
	var session *agentapi.Session
	if h.agents != nil && srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
		session, _ = h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so nothing can say %s now. Check the server; this works again when its agent reconnects", srv.GetName(), what), authapi.ReasonUnavailable)
	}
	return srv, session, nil
}

// readError is what a read through an agent that failed answers.
func readError(srv *dockwellv1.Server, thing, what string, err error) error {
	if errors.Is(err, agentapi.ErrAgentOutdated) {
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"the agent on %s is older than the control plane, so it can't read %s. Update it by running the install script on %s again", srv.GetName(), thing, srv.GetName()), authapi.ReasonAgentOutdated)
	}
	var se *resource.ServerError
	if errors.As(err, &se) {
		err = se.Err
	}
	return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't say %s now: %w", srv.GetName(), what, err), authapi.ReasonUnavailable)
}
