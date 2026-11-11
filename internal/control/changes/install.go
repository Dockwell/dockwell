// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// PlanInstallUpdates plans installing the updates waiting on servers: an edit of
// each server's host that makes no revision, its install simulated by the
// server's agent (ADR 0064). A plan with nothing to install is discarded, and
// says so.
func (h *Handler) PlanInstallUpdates(ctx context.Context, req *connect.Request[dockwellv1.PlanInstallUpdatesRequest]) (*connect.Response[dockwellv1.PlanInstallUpdatesResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if len(m.GetServerIds()) == 0 {
		return nil, authapi.Invalid(errors.New("name the servers to install updates on, by ID"))
	}
	var edits []revision.Edit
	var names []string
	err = h.db.View(ctx, func(tx *store.Tx) error {
		for _, id := range m.GetServerIds() {
			s, err := tx.GetServer(ctx, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && s.RevokedAt != nil {
				return fault.New(scope.ErrNotFound, "there's no server %s in the fleet. List them with ListServers", id)
			}
			if err != nil {
				return err
			}
			names = append(names, s.Name)
			edits = append(edits, revision.Edit{
				Ref:     resource.Ref{Kind: host.Kind, Scope: id, Name: host.Name},
				Spec:    &resourcev1.ServerHost{},
				Install: &resource.InstallAsked{All: m.GetAll()},
			})
		}
		return nil
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		on := strings.Join(names, ", ")
		summary = "Install security updates on " + on
		if m.GetAll() {
			summary = "Install updates on " + on
		}
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if !slices.ContainsFunc(plan.Resources, func(r revision.ResourcePlan) bool { return r.Install != nil }) {
		_ = h.engine.Discard(ctx, plan.Change, who)
		what := "security updates"
		if m.GetAll() {
			what = "updates"
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("no "+what+" wait on "+strings.Join(names, ", ")+", so there's nothing to install. Check again on the Updates tab"), "nothing_to_install")
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanInstallUpdatesResponse{Change: c}), nil
}
