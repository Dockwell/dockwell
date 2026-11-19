// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
)

// portChecksPerMinute bounds how often one caller may have an agent ask a service's
// ports, as CheckDomain bounds names: a form asks once as it opens, and again on
// Check again, well within it.
const portChecksPerMinute = 60

// mostPortCallers bounds what's remembered of callers, as a flood of new ones
// mustn't grow memory without bound.
const mostPortCallers = 1000

// portCallers counts each caller's port checks in the last minute.
type portCallers struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string][]time.Time
}

// allow reports whether caller may check now, and counts it if so.
func (c *portCallers) allow(caller string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]time.Time{}
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	old := func(at time.Time) bool { return now.Sub(at) >= time.Minute }
	if _, known := c.m[caller]; !known && len(c.m) >= mostPortCallers {
		for k, v := range c.m {
			if len(slices.DeleteFunc(v, old)) == 0 {
				delete(c.m, k)
			}
		}
		for k := range c.m {
			if len(c.m) < mostPortCallers {
				break
			}
			delete(c.m, k)
		}
	}
	recent := slices.DeleteFunc(c.m[caller], old)
	if len(recent) >= portChecksPerMinute {
		c.m[caller] = recent
		return false
	}
	c.m[caller] = append(recent, now)
	return true
}

// CheckServicePorts asks the server a service runs on which TCP ports its
// container exposes, publishes and listens on, and what answers on each at the
// container's own address (#791). Below an admin, the caller is a developer of the
// service's project: the gate checks that, and so does this, as it has an agent
// connect to the app.
func (h *Handler) CheckServicePorts(ctx context.Context, req *connect.Request[dockwellv1.CheckServicePortsRequest]) (*connect.Response[dockwellv1.CheckServicePortsResponse], error) {
	m := req.Msg
	if h.Agents == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane reaches no agents"))
	}
	r, err := h.find(ctx, m.GetId(), "", "")
	if err != nil {
		return nil, h.findFailed(ctx, m.GetId(), err)
	}
	var servers []string
	err = h.db.View(ctx, func(tx *store.Tx) error {
		if caller, ok := place.CallerFrom(ctx); ok {
			places, err := place.NewPlacer(tx).Of(ctx, r.ID)
			if err != nil {
				return err
			}
			for _, p := range places {
				if store.RoleRank(place.Holds(caller.Grants, p)) < store.RoleRank(store.RoleDeveloper) {
					return store.ErrNotFound
				}
			}
		}
		deployments, err := tx.LatestDeployments(ctx, []string{r.ID})
		if err != nil {
			return err
		}
		for _, d := range deployments[r.ID] { //nolint:gocritic // a few rows
			if d.State != store.DeploymentSuperseded && !slices.Contains(servers, d.Server) {
				servers = append(servers, d.Server)
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.findFailed(ctx, m.GetId(), err)
	}
	server := m.GetServerId()
	switch {
	case len(servers) == 0:
		return connect.NewResponse(&dockwellv1.CheckServicePortsResponse{Unread: r.Name + " runs on no server now"}), nil
	case server == "":
		server = servers[0]
	case !slices.Contains(servers, server):
		return nil, authapi.Invalid(fmt.Errorf("%s doesn't run on %s. Ask one of the servers it runs on, or leave the server out", r.Name, server))
	}
	caller := "someone"
	if p, ok := authapi.PrincipalFrom(ctx); ok {
		caller = p.Actor()
	}
	if !h.portCallers.allow(caller) {
		return nil, authapi.ReasonError(connect.CodeResourceExhausted,
			fmt.Errorf("you've checked an app's ports %d times in the last minute. Wait a minute, then check again", portChecksPerMinute), "too_many_requests")
	}
	session, ok := h.Agents.Session(server)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("the server %s runs on isn't connected, so its ports can't be read now. Try again once it reconnects", r.Name), "unavailable")
	}
	got, err := session.AppPorts(ctx, r.Scope, r.Name)
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("the agent %s runs on is too old to read its ports. Update the agent, then check again", r.Name), "agent_outdated")
	case errors.Is(err, context.Canceled):
		return nil, connect.NewError(connect.CodeCanceled, err)
	case err != nil:
		h.logger.WarnContext(ctx, "an app's ports couldn't be read", "service", r.ID, "server", server, "error", err.Error())
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("the server %s runs on couldn't read its ports now. Check again in a moment", r.Name), "unavailable")
	}
	return connect.NewResponse(&dockwellv1.CheckServicePortsResponse{Ports: got.GetPorts(), Unread: got.GetUnread()}), nil
}

// findFailed is what a lookup of a service says: not found, as a service out of
// the caller's reach is, or what went wrong.
func (h *Handler) findFailed(ctx context.Context, named string, err error) error {
	var cerr *connect.Error
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, store.ErrNotFound):
		return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no service %s. List them with dockwell services list", named), "not_found")
	}
	return h.internal(ctx, "read a service's ports", err)
}
