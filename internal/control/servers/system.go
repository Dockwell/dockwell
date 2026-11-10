// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
)

// SystemEvery is how often a watch of a server's system services reads them
// again (ADR 0067).
var SystemEvery = 10 * time.Second

// MaxSystemWatchesPerSession is how many servers' system services one session
// may watch at once: a few tabs' worth.
const MaxSystemWatchesPerSession = 8

// systemFeeds reads each watched server's system services once for every watch
// of it, while there's one.
type systemFeeds struct {
	once sync.Once
	// mu guards feeds, one for each server someone watches, and watching, how
	// many watches each session has open.
	mu       sync.Mutex
	feeds    map[string]*systemFeed
	watching map[string]int
}

// systemFeed is one server's list, as last read, for every watch of it.
type systemFeed struct {
	mu       sync.Mutex
	last     *dockwellv1.SystemServiceList
	err      error
	watchers map[chan struct{}]struct{}
	stop     context.CancelFunc
}

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
	sf := s.system()
	session := p.Session.ID
	sf.mu.Lock()
	if sf.watching[session] >= MaxSystemWatchesPerSession {
		sf.mu.Unlock()
		return authapi.ReasonError(connect.CodeResourceExhausted, fmt.Errorf(
			"this browser already watches %d servers' system services, the most one session may. Close a tab that shows them, then look again", MaxSystemWatchesPerSession), "too_many_watches")
	}
	sf.watching[session]++
	f, ok := sf.feeds[srv.GetId()]
	if !ok {
		read, stop := context.WithCancel(context.WithoutCancel(ctx))
		f = &systemFeed{watchers: map[chan struct{}]struct{}{}, stop: stop}
		sf.feeds[srv.GetId()] = f
		go s.readSystem(read, srv, f)
	}
	sf.mu.Unlock()
	told := make(chan struct{}, 1)
	f.mu.Lock()
	f.watchers[told] = struct{}{}
	if f.last != nil || f.err != nil {
		told <- struct{}{}
	}
	f.mu.Unlock()
	defer func() {
		sf.mu.Lock()
		defer sf.mu.Unlock()
		sf.watching[session]--
		if sf.watching[session] == 0 {
			delete(sf.watching, session)
		}
		f.mu.Lock()
		delete(f.watchers, told)
		last := len(f.watchers) == 0
		f.mu.Unlock()
		if last && sf.feeds[srv.GetId()] == f {
			delete(sf.feeds, srv.GetId())
			f.stop()
		}
	}()
	var sent *dockwellv1.SystemServiceList
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-told:
		}
		f.mu.Lock()
		list, err := f.last, f.err
		f.mu.Unlock()
		if err != nil {
			return err
		}
		if list == nil || list == sent {
			continue
		}
		sent = list
		if err := stream.Send(&dockwellv1.WatchSystemServicesResponse{List: list}); err != nil {
			return err
		}
	}
}

func (s *Service) system() *systemFeeds {
	s.systemFeeds.once.Do(func() {
		s.systemFeeds.feeds, s.systemFeeds.watching = map[string]*systemFeed{}, map[string]int{}
	})
	return &s.systemFeeds
}

// readSystem reads srv's system services for f until ctx is done, which it is
// once nobody watches, or a read fails, which every watch then hears.
func (s *Service) readSystem(ctx context.Context, srv *dockwellv1.Server, f *systemFeed) {
	tell := func(list *dockwellv1.SystemServiceList, err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.last, f.err = list, err
		for w := range f.watchers {
			select {
			case w <- struct{}{}:
			default:
			}
		}
	}
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		session, ok := s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
		if !ok {
			tell(nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
				"the agent on %s isn't connected, so its system services can't be read now. Check the server; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable))
			s.dropSystem(srv.GetId(), f)
			return
		}
		list, err := session.SystemServices(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, agentapi.ErrOlderAgent):
			err = authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s's agent is older than the control plane, and can't list its system services. Update the agent on %s, then look again", srv.GetName(), srv.GetName()), "agent_outdated")
		case err != nil:
			err = authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't list its system services now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
		}
		if err != nil {
			tell(nil, err)
			s.dropSystem(srv.GetId(), f)
			return
		}
		tell(list, nil)
		wait = SystemEvery
	}
}

// dropSystem forgets a feed that failed, so the next watch of its server starts
// anew.
func (s *Service) dropSystem(server string, f *systemFeed) {
	sf := s.system()
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.feeds[server] == f {
		delete(sf.feeds, server)
	}
	f.stop()
}
