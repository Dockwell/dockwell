// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// MoveService plans moving an app, with what only it uses, to another project or
// environment, or says what holds it back (ADR 0084, #572).
func (h *Handler) MoveService(ctx context.Context, req *connect.Request[dockwellv1.MoveServiceRequest]) (*connect.Response[dockwellv1.MoveServiceResponse], error) {
	m := req.Msg
	r, err := h.find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	ask := revision.AppMove{From: r.Scope, Services: append([]string{r.Name}, m.GetAlso()...), NewProject: m.GetNewProject()}
	where := m.GetNewProject()
	if where == "" {
		if m.GetToProject() == "" && m.GetToEnvironment() == "" {
			return nil, authapi.Invalid(errors.New("the move says nowhere to go. Pass to_project, and to_environment where it has more than one, or new_project"))
		}
		p, env, err := h.changes.Environment(ctx, m.GetToProject(), m.GetToEnvironment())
		if err != nil {
			return nil, err
		}
		ask.To = env.ID
		err = h.db.View(ctx, func(tx *store.Tx) error {
			tree, err := scope.Load(ctx, tx)
			where = placeName(tree, p, env)
			return err
		})
		if err != nil {
			return nil, h.internal(ctx, "reading where a service goes", err)
		}
	} else if err := (scope.ProjectType{}).CheckName(where); err != nil {
		return nil, authapi.Invalid(fmt.Errorf("new_project: %w", err))
	}
	slices.Sort(ask.Services[1:])
	ask.Services = slices.Compact(ask.Services)
	edits, report, err := h.engine.PlanAppMove(ctx, ask)
	if err != nil {
		return nil, h.changes.Error(ctx, err)
	}
	res := &dockwellv1.MoveServiceResponse{
		Moves: refs(report.Moves), Copies: refs(report.Copies), Follows: refs(report.Follows), Crosses: refs(report.Crosses),
	}
	for _, b := range report.Blocks {
		res.Blocks = append(res.Blocks, &dockwellv1.MoveBlock{Resource: resourcepb.Ref(b.Ref), Why: b.Why, MoveToo: b.MoveToo})
	}
	if len(res.Blocks) > 0 {
		return connect.NewResponse(res), nil
	}
	summary := cmp.Or(m.GetSummary(), fmt.Sprintf("Move %s to %s", r.Name, where))
	// A value what stays reads too stays, and the app takes a copy: the plan says so.
	var notes []revision.Note
	if len(report.Copies) > 0 {
		names := make([]string, len(report.Copies))
		for i, c := range report.Copies {
			names[i] = c.Name
		}
		notes = append(notes, revision.Note{
			Reason: "move_copies", Ref: resource.Ref{Kind: "service", Scope: r.Scope, Name: r.Name}, Names: names, Named: true,
			Message: copiesWords(names, r.Name),
		})
	}
	// A database it reads that stays, it reaches where it is, which the plan says.
	for _, l := range report.Crosses {
		notes = append(notes, revision.Note{
			Reason: "move_crosses", Ref: l, Names: []string{l.Name}, Named: true,
			Message: fmt.Sprintf("%s reaches its database where it stays, through its link %s, as the user it has there", r.Name, l.Name),
		})
	}
	res.Change, err = h.changes.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		return h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits, Notes: notes})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// copiesWords says which values stay for what else reads them, and that the app
// takes its own copies.
func copiesWords(names []string, app string) string {
	if len(names) == 1 {
		return fmt.Sprintf("%s stays for what else reads it, and %s takes a copy of its own", names[0], app)
	}
	return fmt.Sprintf("%s stay for what else reads them, and %s takes copies of its own", human.List(names), app)
}

func refs(in []resource.Ref) []*dockwellv1.ResourceRef {
	out := make([]*dockwellv1.ResourceRef, len(in))
	for i, r := range in {
		out[i] = resourcepb.Ref(r)
	}
	return out
}
