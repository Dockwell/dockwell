// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/feeds"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
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
	// Why each failed unit failed, by its name and when it did, read once a failure.
	why := map[string][]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-f.Again():
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
		list.Acts = session.ActsOnUnits()
		s.sayWhy(ctx, session, list, why)
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

// MaxWhyReads is how many failed units' journals a list reads why for.
const MaxWhyReads = 10

// sayWhy gives each failed unit the last lines its own process wrote before it
// failed (ADR 0082), read once for each failure, as known keeps them by the unit's
// name and the time it failed, and forgets those that no longer stand.
func (s *Service) sayWhy(ctx context.Context, session *agentapi.Session, list *dockwellv1.SystemServiceList, known map[string][]string) {
	if !session.ActsOnUnits() {
		return
	}
	stands := map[string]bool{}
	read := 0
	for _, u := range list.GetServices() {
		if u.GetState() != dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED {
			continue
		}
		key := u.GetName() + "@" + u.GetSince().AsTime().String()
		stands[key] = true
		lines, ok := known[key]
		if !ok && read < MaxWhyReads {
			read++
			page, err := session.SystemLog(ctx, &agentv1.SystemLogCall{Unit: u.GetName(), Limit: 20})
			if err == nil {
				lines = system.Why(page.GetLines())
			}
			// One that can't be read says its result alone, and isn't asked again.
			known[key], ok = lines, true
		}
		if ok {
			u.Why = lines
		}
	}
	for key := range known {
		if !stands[key] {
			delete(known, key)
		}
	}
}

// ReadSystemAgain has whoever watches a server's system services read them now,
// as a unit an action moved stands somewhere new (ADR 0082).
func (s *Service) ReadSystemAgain(server string) { s.systemFeeds.Again(server) }

// LogsAuditEvery is how often reading one server's system log goes on record for one
// reader: once an hour, as reading an app's logs does (ADR 0067).
var LogsAuditEvery = time.Hour

// ReadSystemLog reads a page of a server's system log, newest first, narrowed to a
// unit and a level, and puts reading it on record at most once an hour for one
// reader and one server (ADR 0067, ADR 0082).
func (s *Service) ReadSystemLog(ctx context.Context, req *connect.Request[dockwellv1.ReadSystemLogRequest]) (*connect.Response[dockwellv1.ReadSystemLogResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	m := req.Msg
	if m.GetLimit() > system.MaxLines {
		return nil, authapi.Invalid(fmt.Errorf("a page holds %d lines at most. Ask for fewer", system.MaxLines))
	}
	if len(m.GetBefore()) > 4096 {
		return nil, authapi.Invalid(errors.New("that isn't a line of this server's system log"))
	}
	limit := m.GetLimit()
	if limit == 0 {
		limit = 50
	}
	srv, err := s.fleet.Get(m.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+m.GetServerId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	var session *agentapi.Session
	if s.agents != nil {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so its system log can't be read now. Check the server; this works again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	if err := s.recordLogRead(ctx, p, srv); err != nil {
		return nil, err
	}
	page, err := session.SystemLog(ctx, &agentv1.SystemLogCall{Unit: m.GetUnit(), Level: m.GetLevel(), Before: m.GetBefore(), Limit: limit})
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s's agent is older than the control plane, and can't read its system log. Update its agent, then look again", srv.GetName()), authapi.ReasonAgentOutdated)
	case err != nil:
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't read its system log now: %w", srv.GetName(), agentWords(err)), authapi.ReasonUnavailable)
	}
	return connect.NewResponse(&dockwellv1.ReadSystemLogResponse{Lines: page.GetLines(), More: page.GetMore(), Source: page.GetSource()}), nil
}

// recordLogRead puts reading a server's system log on record as logs.read, at most
// once an hour for one reader and one server, never what was read for. A reading
// that can't be recorded isn't served. A restart forgets what was recorded, which
// costs one more event each.
func (s *Service) recordLogRead(ctx context.Context, p auth.Principal, srv *dockwellv1.Server) error {
	key := p.Actor() + " " + srv.GetId()
	now := time.Now()
	s.auditMu.Lock()
	last, seen := s.audited[key]
	due := !seen || now.Sub(last) >= LogsAuditEvery
	if due {
		if s.audited == nil || len(s.audited) >= 4096 {
			s.audited = map[string]time.Time{}
		}
		s.audited[key] = now
	}
	s.auditMu.Unlock()
	if !due {
		return nil
	}
	client, _ := authapi.ClientFrom(ctx)
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: p.Actor(), Action: "logs.read", Subject: srv.GetId(), Target: srv.GetId(),
			Detail: "read the system log on " + srv.GetName(), Surface: client.Surface(p),
		})
	})
	if err != nil {
		s.auditMu.Lock()
		delete(s.audited, key)
		s.auditMu.Unlock()
		return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't put reading the system log on record, so it isn't served; try again, and check its logs if it keeps failing"))
	}
	return nil
}
