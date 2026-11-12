// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
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

// PlanReboot plans rebooting servers: an edit of each server's host that makes no
// revision, as good as root, whose step passes once the server's agent connects
// from another boot (ADR 0064).
func (h *Handler) PlanReboot(ctx context.Context, req *connect.Request[dockwellv1.PlanRebootRequest]) (*connect.Response[dockwellv1.PlanRebootResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if len(m.GetServerIds()) == 0 {
		return nil, authapi.Invalid(errors.New("name the servers to reboot, by ID"))
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
				Ref:    resource.Ref{Kind: host.Kind, Scope: id, Name: host.Name},
				Spec:   &resourcev1.ServerHost{},
				Reboot: true,
			})
		}
		return nil
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Reboot " + strings.Join(names, ", ")
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanRebootResponse{Change: c}), nil
}
