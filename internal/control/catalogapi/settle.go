// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// rotatingOwner finds an install's owner whose new password is being set, for a
// person settling it with a passkey for action: their principal, the install's
// project and the owner. It refuses a token, an install without one, and an owner
// with no rotation (not_rotating).
func (h *Handler) rotatingOwner(ctx context.Context, id, proof string, action auth.StepUpAction, why string) (*installOwner, string, func(), error) {
	if h.stepUps == nil || h.keeper == nil {
		return nil, "", nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane sets no new passwords for apps' owners"))
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Session.ID == "" {
		return nil, "", nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("an app owner's new password is for a person signed in to the panel, who confirms with a passkey, not for an API token"), "needs_session")
	}
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	found, err := h.find(ctx, s, id)
	if err != nil {
		return nil, "", nil, err
	}
	install := found.install
	var o *installOwner
	if err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		o, err = h.ownerIn(ctx, tx, install.GetEnvironmentId())
		return err
	}); err != nil {
		return nil, "", nil, h.internal(ctx, err)
	}
	if o == nil || o.state.Rotation == "" {
		return nil, "", nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("no new password for %s's owner is being set", install.GetProject()), "not_rotating")
	}
	client, _ := authapi.ClientFrom(ctx)
	target := install.GetProjectId()
	giveBack, err := h.stepUps.HoldStepUp(ctx, client, p, proof, action, target)
	if err != nil && proof == "" {
		if !errors.Is(err, auth.ErrNeedsStepUp) {
			return nil, "", nil, authapi.StepUpRefused(err)
		}
		h.stepUps.AskedForStepUp(p, action, target, func() {})
		return nil, "", nil, authapi.ReasonError(connect.CodePermissionDenied, fmt.Errorf(why+" Confirm with it, then try again", install.GetProject()), authapi.ReasonNeedsStepUp)
	}
	if err != nil {
		return nil, "", nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	return o, install.GetProject(), giveBack, nil
}

// RevealCatalogOwnerPendingPassword says a rotation's new password, to the person
// asking with a passkey alone, so they can try it where neither reads signing in
// (ADR 0105, the review's H1). It goes to no log, event or cache.
func (h *Handler) RevealCatalogOwnerPendingPassword(ctx context.Context, req *connect.Request[dockwellv1.RevealCatalogOwnerPendingPasswordRequest]) (*connect.Response[dockwellv1.RevealCatalogOwnerPendingPasswordResponse], error) {
	o, _, giveBack, err := h.rotatingOwner(ctx, req.Msg.GetId(), req.Msg.GetProof(), auth.StepUpRevealCatalogOwnerPendingPassword,
		"%s's new password signs in as its owner, so showing it takes your passkey.")
	if err != nil {
		return nil, err
	}
	var plain []byte
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		plain, err = h.keeper.Open(ctx, tx, store.AppOwnerPendingAt(o.res.ID, o.state.Rotation), o.state.Pending)
		return err
	})
	if err != nil {
		giveBack()
		return nil, h.internal(ctx, err)
	}
	res := connect.NewResponse(&dockwellv1.RevealCatalogOwnerPendingPasswordResponse{Password: string(plain)})
	clear(plain)
	res.Header().Set("Cache-Control", "no-store")
	return res, nil
}

// KeepCatalogOwnerPassword ends a rotation a person settles, keeping the current
// password and forgetting the new one (ADR 0105, the review's H1).
func (h *Handler) KeepCatalogOwnerPassword(ctx context.Context, req *connect.Request[dockwellv1.KeepCatalogOwnerPasswordRequest]) (*connect.Response[dockwellv1.KeepCatalogOwnerPasswordResponse], error) {
	o, project, giveBack, err := h.rotatingOwner(ctx, req.Msg.GetId(), req.Msg.GetProof(), auth.StepUpKeepCatalogOwnerPassword,
		"keeping %s's current password forgets the new one for good, so it takes your passkey.")
	if err != nil {
		return nil, err
	}
	var owner *dockwellv1.CatalogOwner
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		ended, err := tx.EndAppOwnerRotation(ctx, o.res.ID, o.state.Rotation, "", time.Now())
		if err != nil {
			return err
		}
		if !ended {
			return errNotRotating
		}
		states, err := tx.AppOwnerStates(ctx, []string{o.res.ID})
		o.state = states[o.res.ID]
		owner = ownerProto(o)
		return err
	})
	if errors.Is(err, errNotRotating) {
		giveBack()
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("no new password for %s's owner is being set now", project), "not_rotating")
	}
	if err != nil {
		giveBack()
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.KeepCatalogOwnerPasswordResponse{Owner: owner}), nil
}

var errNotRotating = errors.New("no new password is being set")

// SettleCatalogOwner plans taking out of a made owner's service what its entry says
// only making the owner needed, as Vaultwarden's ADMIN_TOKEN, where Dockwell's own
// change failed (ADR 0105, the review's M1). It changes nothing until it's applied.
func (h *Handler) SettleCatalogOwner(ctx context.Context, req *connect.Request[dockwellv1.SettleCatalogOwnerRequest]) (*connect.Response[dockwellv1.SettleCatalogOwnerResponse], error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	env := install.GetEnvironmentId()
	name, names, err := h.OwnerUnset(ctx, env)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	var edit *dockwellv1.Edit
	var left []string
	var final bool
	err = h.db.View(ctx, func(tx *store.Tx) error {
		o, err := h.ownerIn(ctx, tx, env)
		if err != nil || o == nil {
			return err
		}
		final = store.OwnerFinal(o.state.State)
		r, err := tx.GetResource(ctx, string(service.Kind), env, name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil || len(names) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		svc, ok := spec.(*resourcev1.Service)
		if !ok {
			return nil
		}
		svc = proto.CloneOf(svc)
		for _, n := range names {
			set := false
			for _, m := range []map[string]string{svc.GetApp().GetEnv(), svc.GetVariables(), svc.GetSecrets()} {
				if _, ok := m[n]; ok {
					delete(m, n)
					set = true
				}
			}
			if set {
				left = append(left, n)
			}
		}
		if len(left) > 0 {
			edit = &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: env, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if edit == nil || !final {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's owner has nothing left to take out", install.GetProject()), "settled")
	}
	notes := []revision.Note{{Reason: "owner_unset", Message: install.GetProject() + "'s owner exists, so " + strings.Join(left, ", ") + " goes, as only making the owner needed it", Names: left}}
	change, err := h.changes.Propose(ctx, "Take "+strings.Join(left, ", ")+" out of "+install.GetProject(), []*dockwellv1.Edit{edit}, nil, changes.Also{Notes: notes})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SettleCatalogOwnerResponse{Change: change}), nil
}
