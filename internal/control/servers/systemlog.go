// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// maxSystemQuery is the longest search of the system log a call asks for.
const maxSystemQuery = 1 << 10

// systemUnit is a system service's name, as the agent checks it again.
var systemUnit = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:[A-Za-z0-9:_.@\\-]+\.(?:service|socket)|[A-Za-z0-9_][A-Za-z0-9_.@-]*)$`)
})

// WatchSystemLog follows a server's system log (ADR 0067): what came after the
// cursor asked for first, then each new line, narrowed to a unit, a level and a
// search, through the agent's one follow of its journal, within its share of the
// server's live rate. Following goes on record as reading it does, again each
// hour it stays open, and once when it ends.
func (s *Service) WatchSystemLog(ctx context.Context, req *connect.Request[dockwellv1.WatchSystemLogRequest], stream *connect.ServerStream[dockwellv1.WatchSystemLogResponse]) error {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		return authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	m := req.Msg
	switch {
	case m.GetUnit() != "" && (len(m.GetUnit()) > 256 || !systemUnit().MatchString(m.GetUnit())):
		return authapi.Invalid(fmt.Errorf("%q isn't a system service's name, such as nginx.service", m.GetUnit()))
	case len(m.GetQuery()) > maxSystemQuery:
		return authapi.Invalid(fmt.Errorf("a search is %d bytes at most. Search for less", maxSystemQuery))
	case len(m.GetAfter()) > 4096:
		return authapi.Invalid(errors.New("that isn't a line of this server's system log"))
	}
	srv, err := s.fleet.Get(m.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("there's no server "+m.GetServerId()+". List them with ListServers"))
	}
	if err != nil {
		return err
	}
	var session *agentapi.Session
	if s.agents != nil {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf(
			"the agent on %s isn't connected, so its system log can't be followed now. This follows again when its agent reconnects", srv.GetName()), authapi.ReasonUnavailable)
	}
	if !session.ActsOnUnits() {
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s's agent is older than the control plane, and can't follow its system log. Update its agent, then look again", srv.GetName()), authapi.ReasonAgentOutdated)
	}
	if err := s.recordLog(ctx, p, srv, "followed the system log on "+srv.GetName()); err != nil {
		return err
	}
	began := time.Now()
	defer s.recordFollowEnd(ctx, p, srv, began)

	// One goroutine sends: what the agent reads, and that the follow is alive.
	out := make(chan *dockwellv1.WatchSystemLogResponse, 4)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		beat := time.NewTicker(HeartbeatEvery)
		defer beat.Stop()
		record := time.NewTicker(10 * time.Minute)
		defer record.Stop()
		for {
			select {
			case <-ctx.Done():
				sent <- nil
				return
			case r := <-out:
				if err := stream.Send(r); err != nil {
					cancel()
					sent <- err
					return
				}
			case <-beat.C:
				if err := stream.Send(&dockwellv1.WatchSystemLogResponse{}); err != nil {
					cancel()
					sent <- err
					return
				}
			case <-record.C:
				// Once an hour while it stays open, as recordLog keeps to.
				if err := s.recordLog(ctx, p, srv, "followed the system log on "+srv.GetName()); err != nil {
					cancel()
					sent <- err
					return
				}
			}
		}
	}()
	call := &agentv1.LogsCall{
		Follow: true, Reader: p.Actor(),
		System: &agentv1.SystemLogFollow{Unit: m.GetUnit(), Level: m.GetLevel(), Query: m.GetQuery(), After: m.GetAfter()},
	}
	err = session.Logs(ctx, call, func(r *agentv1.SendLogsRequest) error {
		if len(r.GetSystemLines()) == 0 && r.GetSystemDropped() == 0 && !r.GetSystemGap() {
			if r.GetStarted() == nil {
				return nil
			}
			// The agent's first message says the follow is open: so does the watch's.
		}
		select {
		case out <- &dockwellv1.WatchSystemLogResponse{Lines: r.GetSystemLines(), Dropped: r.GetSystemDropped(), Gap: r.GetSystemGap()}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	cancel()
	if sendErr := <-sent; sendErr != nil {
		return sendErr
	}
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return nil
	case errors.Is(err, agentapi.ErrOlderAgent):
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s's agent is older than the control plane, and can't follow its system log. Update its agent, then look again", srv.GetName()), authapi.ReasonAgentOutdated)
	default:
		return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s stopped following its system log: %w", srv.GetName(), agentWords(err)), authapi.ReasonUnavailable)
	}
}

// recordFollowEnd puts the end of a follow on record, so the log says when a reader
// stopped seeing what the server wrote. A follow shorter than the time between
// records counts towards the hour, so one that reconnects all day doesn't flood it.
func (s *Service) recordFollowEnd(ctx context.Context, p auth.Principal, srv *dockwellv1.Server, began time.Time) {
	now := time.Now()
	lasted := now.Sub(began)
	key := p.Actor() + " ended " + srv.GetId()
	s.auditMu.Lock()
	last, seen := s.audited[key]
	due := lasted >= LogsAuditEvery || !seen || now.Sub(last) >= LogsAuditEvery
	if due {
		if s.audited == nil || len(s.audited) >= 4096 {
			s.audited = map[string]time.Time{}
		}
		s.audited[key] = now
	}
	s.auditMu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	client, _ := authapi.ClientFrom(ctx)
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: p.Actor(), Action: "logs.read", Subject: srv.GetId(), Target: srv.GetId(),
			Detail: "stopped following the system log on " + srv.GetName() + " after " + human.Duration(lasted.Round(time.Second)), Surface: client.Surface(p),
		})
	})
	if err != nil {
		slog.WarnContext(ctx, "can't put the end of following a system log in the audit log", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
	}
}
