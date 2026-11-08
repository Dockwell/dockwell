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
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/store"
)

// Handler implements FirewallService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	db     *store.DB
}

var _ dockwellv1connect.FirewallServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents.
func New(hub *fleet.Hub, agents *agentapi.Agents, db *store.DB) *Handler {
	return &Handler{fleet: hub, agents: agents, db: db}
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
	if errors.Is(err, agentapi.ErrAgentOutdated) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"the agent on %s is older than the control plane, so it can't read the firewall. Update it by running the install script on %s again", srv.GetName(), srv.GetName()), authapi.ReasonAgentOutdated)
	}
	if err != nil {
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't say what filters its traffic now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	res := &dockwellv1.GetServerFirewallResponse{Firewall: read}
	if err := h.dockwells(ctx, srv.GetId(), res); err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// dockwells reads Dockwell's own rules and policy for a server, as applied.
func (h *Handler) dockwells(ctx context.Context, server string, res *dockwellv1.GetServerFirewallResponse) error {
	if h.db == nil {
		return nil
	}
	return h.db.View(ctx, func(tx *store.Tx) error {
		rules, err := tx.ListResources(ctx, server, string(firewall.Kind))
		if err != nil {
			return err
		}
		for _, r := range rules {
			if r.Spec == nil {
				continue
			}
			rule := &resourcev1.FirewallRule{}
			if err := proto.Unmarshal(r.Spec, rule); err != nil {
				return fmt.Errorf("can't read firewall rule %s: %w", r.Name, err)
			}
			res.Rules = append(res.Rules, &dockwellv1.DockwellFirewallRule{Name: r.Name, Rule: rule})
		}
		policies, err := tx.ListResources(ctx, server, string(firewall.PolicyKind))
		if err != nil {
			return err
		}
		for _, p := range policies {
			if p.Spec == nil {
				continue
			}
			res.Policy = &resourcev1.FirewallPolicy{}
			if err := proto.Unmarshal(p.Spec, res.Policy); err != nil {
				return fmt.Errorf("can't read the firewall's policy: %w", err)
			}
		}
		return nil
	})
}
