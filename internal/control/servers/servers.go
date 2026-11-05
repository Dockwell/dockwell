// SPDX-License-Identifier: AGPL-3.0-only

// Package servers implements dockwell.v1.ServerService from the fleet's
// live state.
package servers

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/fleet"
)

// Service answers ServerService calls.
type Service struct {
	fleet *fleet.Hub
}

var _ dockwellv1connect.ServerServiceHandler = (*Service)(nil)

// HeartbeatEvery is how often a watch says it is alive, whatever else it says.
var HeartbeatEvery = 10 * time.Second

// New returns a Service reading hub.
func New(hub *fleet.Hub) *Service { return &Service{fleet: hub} }

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
