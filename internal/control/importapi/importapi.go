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
	"sync"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/containers"
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
	// last is the plan each person's last import of a server made, by server and
	// user, so a new one stands in its place rather than leave it planned.
	mu   sync.Mutex
	last map[string]string
}

// Taker reads what taking on a Compose project would plan, as AdoptCompose does.
type Taker interface {
	ReadRunning(ctx context.Context, server string) (*containers.Running, error)
	ReadTakingIn(ctx context.Context, r *containers.Running, name, project, environment string) (*composeapi.Taking, error)
	TakenOn(ctx context.Context, r *containers.Running, name string) (string, error)
	FreeProject(ctx context.Context, want, from string, claimed map[string]bool) (string, error)
	ReadMakingAgain(ctx context.Context, r *containers.Running, list []composeapi.Remake, project string) (*composeapi.Taking, map[string][]string, error)
}

var _ dockwellv1connect.ImportServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents, reads what taking a
// project on would be with taker, and plans through ch.
func New(hub *fleet.Hub, agents *agentapi.Agents, taker Taker, ch *changes.Handler, logger *slog.Logger) *Handler {
	return &Handler{fleet: hub, agents: agents, taker: taker, changes: ch, logger: logger, last: map[string]string{}}
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
			fmt.Errorf("%s doesn't run 1Panel: %s. Run this for a server where 1Panel is installed", srv.GetName(), strings.TrimPrefix(out.GetNotHere(), "1Panel isn't installed here: ")), "onepanel_not_found")
	}
	res := &dockwellv1.ImportOnePanelResponse{Found: out.GetFound(), Steps: Steps(out.GetFound())}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Token != nil {
		return connect.NewResponse(res), nil
	}
	// A read that asks for no plan still says what a plan would hold, app by app,
	// and proposes nothing, so it leaves alone any plan someone is reviewing.
	propose := !req.Msg.GetSkipPlan()
	if res.Change, err = h.plan(ctx, srv, out.GetFound(), res.GetSteps(), propose); err != nil {
		return nil, err
	}
	if propose {
		h.supersede(ctx, srv.GetId()+"/"+p.User.ID, res.GetChange().GetId())
	}
	return connect.NewResponse(res), nil
}

// supersede discards the plan the last import made for key, where it's still
// planned, and keeps id in its place: reading again stands in for what was read.
func (h *Handler) supersede(ctx context.Context, key, id string) {
	h.mu.Lock()
	was := h.last[key]
	if id == "" {
		delete(h.last, key)
	} else {
		h.last[key] = id
	}
	h.mu.Unlock()
	if was == "" || was == id {
		return
	}
	got, err := h.changes.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: was}))
	if err != nil || got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		return
	}
	if _, err := h.changes.DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: was})); err != nil {
		h.logger.WarnContext(ctx, "can't discard an import's plan that another stands in for", slog.String("change", was), slog.String("error", err.Error()))
	}
}

// takesOn reports a step a plan takes on: an app, a runtime or a Compose project.
func takesOn(s *dockwellv1.ImportStep) bool {
	kind := s.GetKind()
	return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON && (kind == dockwellv1.ImportKind_IMPORT_KIND_APP ||
		kind == dockwellv1.ImportKind_IMPORT_KIND_RUNTIME || kind == dockwellv1.ImportKind_IMPORT_KIND_COMPOSE_PROJECT)
}

// plan reads what taking on each app, runtime and Compose project would be, and
// plans the ones nothing stands in the way of, in one change. One that can't come
// says why, and stays, and so does its database server. One taken on already says
// as what. Only when what the server runs can't be read is nothing planned. Without
// propose, each step says what the plan would do, and nothing is planned.
func (h *Handler) plan(ctx context.Context, srv *dockwellv1.Server, found *dockwellv1.OnePanel, steps []*dockwellv1.ImportStep, propose bool) (*dockwellv1.Change, error) {
	r, err := h.taker.ReadRunning(ctx, srv.GetId())
	if err != nil {
		if connect.CodeOf(err) != connect.CodeUnavailable {
			return nil, err
		}
		// What runs can't be read now: nothing is planned, and each says why.
		why := err.Error()
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			why = cerr.Message()
		}
		for _, s := range steps {
			if takesOn(s) {
				s.Reason = strings.TrimSpace(fmt.Sprintf("Dockwell can't read what runs on %s now, so nothing is planned yet: %s. Try again once it can. %s",
					srv.GetName(), strings.TrimSuffix(why, "."), s.GetReason()))
			}
		}
		return nil, nil
	}
	// The container 1Panel made each app and runtime as, which keeps its name once
	// it's taken on.
	containerOf := map[string]string{}
	for _, a := range found.GetApps() {
		containerOf[a.GetName()] = a.GetContainer()
	}
	for _, rt := range found.GetRuntimes() {
		containerOf[rt.GetName()] = rt.GetContainer()
	}
	var takings []*composeapi.Taking
	var taken []string
	left, done := map[string]bool{}, map[string]string{}
	for _, step := range steps {
		if !takesOn(step) {
			continue
		}
		if c := containerOf[step.GetName()]; c != "" {
			as, err := h.taker.TakenOn(ctx, r, c)
			if err != nil {
				return nil, err
			}
			if as != "" {
				step.Action, step.Becomes, step.Reason, step.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_DONE,
					"the project "+as+", taken on already", "Change it from Dockwell now. 1Panel still lists it: leave it alone there.", 0
				done[step.GetName()] = as
				continue
			}
		}
		name := strings.ToLower(step.GetName())
		project, err := h.taker.FreeProject(ctx, containers.Label(name), "1panel", nil)
		if err != nil {
			return nil, err
		}
		t, err := h.taker.ReadTakingIn(ctx, r, name, project, "")
		switch {
		case reason(err) == "not_found":
			leave(step, fmt.Sprintf("1Panel lists it, but Docker runs no container of its Compose project, %s, so there's nothing to take on.", name))
			left[step.GetName()] = true
			continue
		case err != nil:
			// One that can't be read says why, and the rest go on.
			leave(step, sentences([]string{message(err)})+" It stays as it is, in 1Panel.")
			left[step.GetName()] = true
			continue
		case len(t.Refusals) > 0:
			leave(step, sentences(actionable(t.Refusals))+" It stays as it is, in 1Panel, until that's dealt with.")
			left[step.GetName()] = true
			continue
		}
		step.Becomes = fmt.Sprintf("the project %s, taken on as it runs, with no restart", t.Project)
		if t.Project != containers.Label(name) {
			step.Becomes += fmt.Sprintf(", as Dockwell has a project %s already", containers.Label(name))
		}
		takings = append(takings, t)
		taken = append(taken, step.GetName())
	}
	apps := map[string]string{}
	for _, d := range found.GetDatabaseServers() {
		apps[d.GetName()] = d.GetApp()
	}
	for _, step := range steps {
		if step.GetKind() != dockwellv1.ImportKind_IMPORT_KIND_DATABASE_SERVER {
			continue
		}
		switch app := apps[step.GetName()]; {
		case left[app]:
			leave(step, fmt.Sprintf("Its app, %s, can't be taken on yet, as it says above.", app))
		case done[app] != "":
			step.Action, step.Becomes, step.Reason, step.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_DONE,
				fmt.Sprintf("part of the project %s, taken on already with the app %s", done[app], app), "", 0
		}
	}
	if len(takings) == 0 || !propose {
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

// message is what an API error says, without its code.
func message(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}

// actionable is what's in the way, less what an import gives no way to do: it
// chooses its own project, so "pass another project" is no fix here.
func actionable(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "Pass another project"))
		out = append(out, strings.TrimSuffix(strings.TrimSpace(s), "."))
	}
	return out
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
	step.Action, step.Becomes, step.Reason, step.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, "", why, 0
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
