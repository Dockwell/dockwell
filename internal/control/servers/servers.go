// SPDX-License-Identifier: AGPL-3.0-only

// Package servers implements dockwell.v1.ServerService from the fleet's
// live state.
package servers

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/store"
)

// Service answers ServerService calls.
type Service struct {
	fleet *fleet.Hub
	db    *store.DB
}

var _ dockwellv1connect.ServerServiceHandler = (*Service)(nil)

// HeartbeatEvery is how often a watch says it is alive, whatever else it says.
var HeartbeatEvery = 10 * time.Second

// New returns a Service reading hub, which names who sets a server's public
// address from db.
func New(hub *fleet.Hub, db *store.DB) *Service { return &Service{fleet: hub, db: db} }

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

// errBehind ends a watch whose client couldn't keep up; it should reconnect
// and start from a fresh snapshot.
var errBehind = connect.NewError(connect.CodeUnavailable, errors.New("the watch fell behind; reconnect to start from a fresh snapshot"))

// WatchServers streams every server, marks the end of that snapshot, then
// streams each change.
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
				if ctx.Err() != nil {
					return nil // the client went away; nothing failed
				}
				return errBehind
			}
			if err := stream.Send(&dockwellv1.WatchServersResponse{Server: u.Server, Removed: u.Removed}); err != nil {
				return err
			}
		}
	}
}
