// SPDX-License-Identifier: AGPL-3.0-only

// Package servers implements dockwell.v1.ServerService from the fleet's
// live state.
package servers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/feeds"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Service answers ServerService calls.
type Service struct {
	fleet  *fleet.Hub
	db     *store.DB
	agents *agentapi.Agents
	// ident makes join tickets, each confirmed with a passkey people checks.
	ident  *identity.Authority
	people *auth.Service
	// systemFeeds read the system services of each server someone watches,
	// every systemEvery, once for every watch of it.
	systemFeeds *feeds.Hub[dockwellv1.SystemServiceList]
	systemEvery time.Duration
}

var _ dockwellv1connect.ServerServiceHandler = (*Service)(nil)

// HeartbeatEvery is how often a watch says it is alive, whatever else it says.
var HeartbeatEvery = 10 * time.Second

// New returns a Service reading hub, which names who sets a server's public
// address from db, and asks servers what only they can say through agents. Nil
// agents asks nothing.
func New(hub *fleet.Hub, db *store.DB, agents *agentapi.Agents) *Service {
	return &Service{
		fleet: hub, db: db, agents: agents,
		systemFeeds: &feeds.Hub[dockwellv1.SystemServiceList]{Max: MaxSystemWatchesPerSession}, systemEvery: SystemEvery,
	}
}

// ListServers returns every server.
func (s *Service) ListServers(context.Context, *connect.Request[dockwellv1.ListServersRequest]) (*connect.Response[dockwellv1.ListServersResponse], error) {
	return connect.NewResponse(&dockwellv1.ListServersResponse{Servers: s.fleet.List()}), nil
}

// GetServer returns one server with history.
func (s *Service) GetServer(_ context.Context, req *connect.Request[dockwellv1.GetServerRequest]) (*connect.Response[dockwellv1.GetServerResponse], error) {
	srv, err := s.fleet.Get(req.Msg.GetId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.GetServerResponse{Server: srv}), nil
}

// GetServerDiskUse asks a server's agent what takes the room on the disk that
// holds Docker's data, only when someone asks (ADR 0040).
func (s *Service) GetServerDiskUse(ctx context.Context, req *connect.Request[dockwellv1.GetServerDiskUseRequest]) (*connect.Response[dockwellv1.GetServerDiskUseResponse], error) {
	srv, err := s.fleet.Get(req.Msg.GetId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	var session *agentapi.Session
	if s.agents != nil && srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so nothing can say what takes its disk now. Check the server; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	use, err := session.DiskUse(ctx)
	if err != nil {
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't say what takes its disk now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return connect.NewResponse(&dockwellv1.GetServerDiskUseResponse{DiskUse: use}), nil
}

// CheckServerPorts asks a server's agent what holds some of its ports, for a form
// that offers a free one, only when someone asks.
func (s *Service) CheckServerPorts(ctx context.Context, req *connect.Request[dockwellv1.CheckServerPortsRequest]) (*connect.Response[dockwellv1.CheckServerPortsResponse], error) {
	asked := req.Msg.GetPorts()
	if len(asked) == 0 {
		return connect.NewResponse(&dockwellv1.CheckServerPortsResponse{}), nil
	}
	if len(asked) > 64 {
		return nil, authapi.Invalid(fmt.Errorf("that asks about %d ports, and a check takes 64 at most. Ask about fewer", len(asked)))
	}
	for _, p := range asked {
		if p.GetPort() == 0 || p.GetPort() > 65535 {
			return nil, authapi.Invalid(fmt.Errorf("%d isn't a port. Use one from 1 to 65535", p.GetPort()))
		}
	}
	srv, err := s.fleet.Get(req.Msg.GetId())
	if errors.Is(err, fleet.ErrNotFound) {
		for _, named := range s.fleet.List() {
			if named.GetName() == req.Msg.GetId() {
				srv, err = named, nil
			}
		}
	}
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	var session *agentapi.Session
	if s.agents != nil && srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so nothing can say what holds its ports now. The plan checks them when it can; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	held, err := session.Ports(ctx, &agentv1.PortsCall{Ports: asked})
	if errors.Is(err, agentapi.ErrOlderAgent) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't say what holds its ports: %w", srv.GetName(), err))
	}
	if err != nil {
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't say what holds its ports now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return connect.NewResponse(&dockwellv1.CheckServerPortsResponse{Ports: held.GetPorts()}), nil
}

// SetServerPublicAddress records a server's public address as a person says it.
func (s *Service) SetServerPublicAddress(ctx context.Context, req *connect.Request[dockwellv1.SetServerPublicAddressRequest]) (*connect.Response[dockwellv1.SetServerPublicAddressResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	id := req.Msg.GetId()
	by := fleet.Setting{Actor: p.Actor(), Surface: client.Surface(p)}
	if err := s.db.View(ctx, func(tx *store.Tx) error {
		by.Who = actors.New().In(ctx, tx).Actor(p.Actor())
		return nil
	}); err != nil {
		return nil, err
	}
	srv, err := s.fleet.SetPublicAddress(ctx, id, strings.TrimSpace(req.Msg.GetAddress()), req.Msg.GetIpv6(), by)
	switch {
	case errors.Is(err, fleet.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+id+". List them with ListServers"))
	case errors.Is(err, fleet.ErrNotAPublicAddress):
		return nil, authapi.Invalid(errors.New(strings.TrimPrefix(err.Error(), fleet.ErrNotAPublicAddress.Error()+": ")))
	case err != nil:
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetServerPublicAddressResponse{Server: srv}), nil
}

// WatchServers streams every server, marks the end of that snapshot, then
// streams each change. A client that reads slowly is sent each server as it
// stands when it can take it, and misses what changed in between.
func (s *Service) WatchServers(ctx context.Context, _ *connect.Request[dockwellv1.WatchServersRequest], stream *connect.ServerStream[dockwellv1.WatchServersResponse]) error {
	snapshot, updates := s.fleet.Watch(ctx)
	for _, srv := range snapshot {
		if err := stream.Send(&dockwellv1.WatchServersResponse{Server: srv}); err != nil {
			return err
		}
	}
	if err := stream.Send(&dockwellv1.WatchServersResponse{SnapshotComplete: true}); err != nil {
		return err
	}
	beat := time.NewTicker(HeartbeatEvery)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-beat.C:
			if err := stream.Send(&dockwellv1.WatchServersResponse{Heartbeat: true}); err != nil {
				return err
			}
		case u, ok := <-updates:
			if !ok {
				return nil // the client went away; nothing failed
			}
			if err := stream.Send(&dockwellv1.WatchServersResponse{Server: u.Server, Removed: u.Removed}); err != nil {
				return err
			}
		}
	}
}
