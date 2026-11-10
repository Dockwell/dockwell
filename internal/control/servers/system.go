// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/feeds"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
)

// SystemEvery is how often a watch of a server's system services reads them
// again (ADR 0067). A Service takes it as it's made, so a test sets it first.
var SystemEvery = 10 * time.Second

// MaxSystemWatchesPerSession is how many servers' system services one session
// may watch at once: a few tabs' worth.
const MaxSystemWatchesPerSession = 8

// WatchSystemServices sends a server's system services as its agent reads them,
// every SystemEvery for as long as the watch is open, the first at once. Every
// watch of a server shares one read, and nothing is read while nobody watches.
// Listing them isn't recorded, as reading a server isn't (ADR 0067).
func (s *Service) WatchSystemServices(ctx context.Context, req *connect.Request[dockwellv1.WatchSystemServicesRequest], stream *connect.ServerStream[dockwellv1.WatchSystemServicesResponse]) error {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		// The interceptor refuses tokens already, as the method needs a session.
		return authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	srv, err := s.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetServerId()+". List them with ListServers"))
	}
	if err != nil {
		return err
	}
	if s.agents == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("this control plane reaches no agents"))
	}
	w, ok := s.systemFeeds.Join(ctx, p.Session.ID, srv.GetId(), func(read context.Context, f *feeds.Feed[dockwellv1.SystemServiceList]) {
		s.readSystem(read, srv, f)
	})
	if !ok {
		return authapi.ReasonError(connect.CodeResourceExhausted, fmt.Errorf(
			"this browser already watches %d servers' system services, the most one session may. Close a tab that shows them, then look again", MaxSystemWatchesPerSession), "too_many_watches")
	}
	defer w.Leave()
	for {
		list, err := w.Next(ctx)
		if err != nil {
			return err
		}
		if list == nil {
			return nil
		}
		if err := stream.Send(&dockwellv1.WatchSystemServicesResponse{List: list}); err != nil {
			return err
		}
	}
}

// readSystem reads srv's system services for f until ctx is done, which it is
// once nobody watches, or a read fails, which every watch then hears.
func (s *Service) readSystem(ctx context.Context, srv *dockwellv1.Server, f *feeds.Feed[dockwellv1.SystemServiceList]) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		session, ok := s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
		if !ok {
			f.Tell(nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
				"the agent on %s isn't connected, so its system services can't be read now. Check the server; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable))
			s.systemFeeds.Drop(srv.GetId(), f)
			return
		}
		list, err := session.SystemServices(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, agentapi.ErrOlderAgent):
			err = authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s's agent is older than the control plane, and can't list system services. Update its agent, then look again", srv.GetName()), authapi.ReasonAgentOutdated)
		case err != nil:
			err = authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't list its system services now: %w", srv.GetName(), agentWords(err)), authapi.ReasonUnavailable)
		}
		if err != nil {
			f.Tell(nil, err)
			s.systemFeeds.Drop(srv.GetId(), f)
			return
		}
		f.Tell(list, nil)
		wait = s.systemEvery
	}
}

// agentWords is what a person reads of an agent's error: its plain words, without
// the server's name in front, which the message says already.
func agentWords(err error) error {
	if p, _ := resource.Explain(err); p != "" {
		return errors.New(p)
	}
	var se *resource.ServerError
	if errors.As(err, &se) {
		return se.Err
	}
	return err
}
