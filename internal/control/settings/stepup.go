// SPDX-License-Identifier: AGPL-3.0-only

package settings

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionStepUpFloor is the event of the least anyone may choose for how often
// they're asked for their passkey again.
const ActionStepUpFloor = "settings.step_up_floor"

// SetStepUpFloor sets the least anyone may choose for how often they're asked
// for their passkey again (ADR 0075). Only the owner sets it, with a passkey
// each time, as it loosens or tightens every member's checks at once.
func (h *Handler) SetStepUpFloor(ctx context.Context, req *connect.Request[dockwellv1.SetStepUpFloorRequest]) (*connect.Response[dockwellv1.SetStepUpFloorResponse], error) {
	p, err := ownerOf(ctx, "only the owner can set how often everyone is asked for their passkey at least, as it applies to every member, so it didn't change. Ask the owner")
	if err != nil {
		return nil, err
	}
	floor := authapi.StepUpPolicyOf(req.Msg.GetFloor())
	if !floor.Valid() {
		return nil, authapi.Invalid(errors.New("say the least anyone may choose: STEP_UP_POLICY_EACH_TIME, STEP_UP_POLICY_HOURLY, or STEP_UP_POLICY_AT_SIGN_IN to allow all three"))
	}
	if h.domain.Auth == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane can't check a passkey, so the floor didn't change"))
	}
	var current auth.StepUpPolicy
	if err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		current, err = auth.ReadStepUpFloor(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	if floor == current {
		s, err := h.current(ctx)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&dockwellv1.SetStepUpFloorResponse{Settings: s}), nil
	}
	client, _ := authapi.ClientFrom(ctx)
	if err := h.domain.Auth.UseStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpSetFloor, p.User.ID); err != nil {
		return nil, authapi.StepUpRefused(err)
	}
	s, err := h.set(ctx, store.SettingStepUpFloor, ActionStepUpFloor, string(floor))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetStepUpFloorResponse{Settings: s}), nil
}
