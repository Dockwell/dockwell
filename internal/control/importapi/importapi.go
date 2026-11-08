// SPDX-License-Identifier: AGPL-3.0-only

// Package importapi serves ImportService: what another panel runs on a server, and
// what an import would do with each thing (docs/adr/0056-importing-a-1panel-server.md).
package importapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
)

// Handler reads what other panels keep, through each server's agent, and plans
// taking on what runs as it is.
type Handler struct {
	fleet   *fleet.Hub
	agents  *agentapi.Agents
	taker   Taker
	changes *changes.Handler
	logger  *slog.Logger
}

// Taker reads what taking on a Compose project would plan, as AdoptCompose does.
type Taker interface {
	ReadTaking(ctx context.Context, server, name, project, environment string) (*composeapi.Taking, error)
}

var _ dockwellv1connect.ImportServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents, reads what taking a
// project on would be with taker, and plans through ch.
func New(hub *fleet.Hub, agents *agentapi.Agents, taker Taker, ch *changes.Handler, logger *slog.Logger) *Handler {
	return &Handler{fleet: hub, agents: agents, taker: taker, changes: ch, logger: logger}
}

// ImportOnePanel reads what 1Panel keeps on a server, and says what an import would
// do with each thing. For a person, it plans the change that takes on what runs as
// it is. A token gets what was found alone, as a plan that takes a container on shows
// what a token can't read (ADR 0031).
func (h *Handler) ImportOnePanel(ctx context.Context, req *connect.Request[dockwellv1.ImportOnePanelRequest]) (*connect.Response[dockwellv1.ImportOnePanelResponse], error) {
	srv, err := h.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", req.Msg.GetServerId()), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't read what 1Panel keeps there. Try again once it reconnects", srv.GetName()), "server_offline")
	}
	if !session.ReadsOnePanel() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to read 1Panel's records. Update the agent on %s, then try again", srv.GetName(), srv.GetName()), "agent_outdated")
	}
	out, err := session.ReadOnePanel(ctx)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if out.GetFound() == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s doesn't run 1Panel: %s. Run this for a server where 1Panel is installed", srv.GetName(), out.GetNotHere()), "onepanel_not_found")
	}
	res := &dockwellv1.ImportOnePanelResponse{Found: out.GetFound(), Steps: Steps(out.GetFound())}
	if p, ok := authapi.PrincipalFrom(ctx); !ok || p.Token != nil {
		return connect.NewResponse(res), nil
	}
	if res.Change, err = h.plan(ctx, srv, out.GetFound(), res.GetSteps()); err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// plan reads what taking on each app and runtime would be, and plans the ones
// nothing stands in the way of, in one change. One that can't come says why, and
// stays, and so does its database server.
func (h *Handler) plan(ctx context.Context, srv *dockwellv1.Server, found *dockwellv1.OnePanel, steps []*dockwellv1.ImportStep) (*dockwellv1.Change, error) {
	var takings []*composeapi.Taking
	var taken []string
	left := map[string]bool{}
	for _, step := range steps {
		if step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON ||
			step.GetKind() != dockwellv1.ImportKind_IMPORT_KIND_APP && step.GetKind() != dockwellv1.ImportKind_IMPORT_KIND_RUNTIME {
			continue
		}
		project := strings.ToLower(step.GetName())
		t, err := h.taker.ReadTaking(ctx, srv.GetId(), project, "", "")
		switch {
		case reason(err) == "not_found":
			leave(step, fmt.Sprintf("1Panel lists it, but Docker runs no container of its Compose project, %s, so there's nothing to take on.", project))
			left[step.GetName()] = true
			continue
		case connect.CodeOf(err) == connect.CodeUnavailable:
			// What runs can't be read now: nothing is planned, and each says why.
			why := err.Error()
			var cerr *connect.Error
			if errors.As(err, &cerr) {
				why = cerr.Message()
			}
			for _, s := range steps {
				if s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON &&
					(s.GetKind() == dockwellv1.ImportKind_IMPORT_KIND_APP || s.GetKind() == dockwellv1.ImportKind_IMPORT_KIND_RUNTIME) {
					s.Reason = strings.TrimSpace(fmt.Sprintf("Dockwell can't read what runs on %s now, so nothing is planned yet: %s. Try again once it can. %s",
						srv.GetName(), strings.TrimSuffix(why, "."), s.GetReason()))
				}
			}
			return nil, nil
		case err != nil:
			return nil, err
		case len(t.Refusals) > 0:
			leave(step, sentences(t.Refusals)+" It stays as it is, in 1Panel, until that's dealt with.")
			left[step.GetName()] = true
			continue
		}
		step.Becomes = fmt.Sprintf("the project %s, taken on as it runs, with no restart", t.Project)
		takings = append(takings, t)
		taken = append(taken, step.GetName())
	}
	apps := map[string]string{}
	for _, d := range found.GetDatabaseServers() {
		apps[d.GetName()] = d.GetApp()
	}
	for _, step := range steps {
		if step.GetKind() == dockwellv1.ImportKind_IMPORT_KIND_DATABASE_SERVER && left[apps[step.GetName()]] {
			leave(step, fmt.Sprintf("Its app, %s, can't be taken on yet, as it says above.", apps[step.GetName()]))
		}
	}
	if len(takings) == 0 {
		return nil, nil
	}
	also := changes.Also{Adopts: map[resource.Ref]changes.Adoption{}}
	var edits []*dockwellv1.Edit
	for _, t := range takings {
		edits = append(edits, t.Edits...)
		also.Edits = append(also.Edits, t.Made...)
		also.Notes = append(also.Notes, t.Notes...)
		maps.Copy(also.Adopts, t.Adopts)
	}
	also.Notes = append(also.Notes, revision.Note{Reason: "imports_onepanel", Message: fmt.Sprintf(
		"Takes on what 1Panel runs on %s as it runs: %s. Its websites, certificates, databases and cron jobs come as each says, and OpenResty stays 1Panel's until you hand over ports 80 and 443.",
		srv.GetName(), human.List(taken))})
	return h.changes.Propose(ctx, "Take on what 1Panel runs on "+srv.GetName(), edits, nil, also)
}

// reason is what an API error gives as its reason, or nothing.
func reason(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Meta().Get(authapi.ReasonKey)
	}
	return ""
}

// leave makes a step one that stays, for why.
func leave(step *dockwellv1.ImportStep, why string) {
	step.Action, step.Becomes, step.Reason, step.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, "", why, ""
}

// sentences joins what's in the way, each as a sentence.
func sentences(list []string) string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		s = strings.ToUpper(s[:1]) + s[1:]
		if !strings.HasSuffix(s, ".") {
			s += "."
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func (h *Handler) rpcError(ctx context.Context, err error) error {
	var cerr *connect.Error
	var failed *resource.ServerError
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, resource.ErrLost), errors.Is(err, resource.ErrNotConnected):
		return authapi.ReasonError(connect.CodeUnavailable, err, "server_offline")
	case errors.Is(err, context.DeadlineExceeded):
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "agent_timeout")
	case errors.As(err, &failed):
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "agent_failed")
	}
	h.logger.ErrorContext(ctx, "an import's read failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read what 1Panel keeps. Try again, and check its logs if it keeps failing"))
}
