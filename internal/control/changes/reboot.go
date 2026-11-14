// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
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
	var edits []revision.Edit
	var set targets
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		if set, err = h.targetsOf(ctx, tx, m.GetServerIds(), m.GetSelector(), m.GetRollout(), "reboot"); err != nil {
			return err
		}
		for i := range set.servers {
			edits = append(edits, revision.Edit{
				Ref:    resource.Ref{Kind: host.Kind, Scope: set.servers[i].ID, Name: host.Name},
				Reboot: true,
			})
		}
		return nil
	})
	if cerr := (*connect.Error)(nil); errors.As(err, &cerr) {
		return nil, cerr
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Reboot " + set.on(m.GetSelector())
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits, Notes: set.notes, Rollout: set.rollout})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanRebootResponse{Change: c}), nil
}
