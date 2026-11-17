// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// ResetCatalogOwnerPassword gives a made owner a new password, by its entry's reset
// steps: it generates one as the entry's password input does, seals it as pending
// beside the current one, and the owners runner sets it in the app, keeping
// whichever one signs in once the outcome is known (ADR 0105).
func (h *Handler) ResetCatalogOwnerPassword(ctx context.Context, req *connect.Request[dockwellv1.ResetCatalogOwnerPasswordRequest]) (*connect.Response[dockwellv1.ResetCatalogOwnerPasswordResponse], error) {
	if h.stepUps == nil || h.keeper == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane sets no new passwords for apps' owners"))
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Session.ID == "" {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("a new password for an app's owner is for a person signed in to the panel, who confirms with a passkey, not for an API token"), "needs_session")
	}
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	var o *installOwner
	if err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		o, err = h.ownerIn(ctx, tx, install.GetEnvironmentId())
		return err
	}); err != nil {
		return nil, h.internal(ctx, err)
	}
	if o == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("Dockwell doesn't make %s's owner, so it keeps no password to change", install.GetProject()), "no_owner")
	}
	if o.state.State != store.OwnerMade {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's owner isn't made yet, so it has no password to change", install.GetProject()), "not_made")
	}
	if o.state.Rotation != "" {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("a new password for %s's owner is being set already. Wait for it, then try again", install.GetProject()), "rotating")
	}
	entry, known := s.entryFor(install.GetPin())
	if !known || entry.GetRelease() != install.GetPin().GetRelease() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("its catalog no longer has release %d of %s, whose steps set a new password. Upgrade it, then try again", install.GetPin().GetRelease(), install.GetPin().GetEntry()), "no_release")
	}
	app := cmp.Or(entry.GetName(), install.GetPin().GetEntry())
	in := passwordInput(entry)
	if len(entry.GetOwner().GetResetSteps()) == 0 || in.GetGenerate() <= 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("Dockwell can't set a new password in %s. Change it in %s, then forget the copy Dockwell keeps", app, app), "no_reset")
	}
	client, _ := authapi.ClientFrom(ctx)
	target := install.GetProjectId()
	giveBack, err := h.stepUps.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpResetCatalogOwnerPassword, target)
	if err != nil && req.Msg.GetProof() == "" {
		if !errors.Is(err, auth.ErrNeedsStepUp) {
			return nil, authapi.StepUpRefused(err)
		}
		h.stepUps.AskedForStepUp(p, auth.StepUpResetCatalogOwnerPassword, target, func() {})
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("a new password changes how %s's owner signs in, so it takes your passkey. Confirm with it, then try again", install.GetProject()),
			authapi.ReasonNeedsStepUp)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	done := false
	defer func() {
		if !done {
			giveBack()
		}
	}()
	password, err := catalog.Generate(in)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	rotation := id.New("rot")
	var owner *dockwellv1.CatalogOwner
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		sealed, err := h.keeper.Seal(ctx, tx, store.AppOwnerPendingAt(o.res.ID, rotation), []byte(password))
		if err != nil {
			return err
		}
		started, err := tx.StartAppOwnerRotation(ctx, o.res.ID, rotation, sealed, time.Now())
		if err != nil {
			return err
		}
		if !started {
			return errRotating
		}
		states, err := tx.AppOwnerStates(ctx, []string{o.res.ID})
		if err != nil {
			return err
		}
		o.state = states[o.res.ID]
		owner = ownerProto(o)
		return nil
	})
	password = ""
	if errors.Is(err, errRotating) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("a new password for %s's owner is being set already. Wait for it, then try again", install.GetProject()), "rotating")
	}
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	done = true
	return connect.NewResponse(&dockwellv1.ResetCatalogOwnerPasswordResponse{Owner: owner}), nil
}

var errRotating = errors.New("a new password is being set already")

// passwordInput is the input an entry's owner's password is, nil where it has none.
func passwordInput(entry *catalogv1.Entry) *catalogv1.Input {
	name := entry.GetOwner().GetPasswordInput()
	if name == "" {
		return nil
	}
	i := slices.IndexFunc(entry.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == name })
	if i < 0 {
		return nil
	}
	return entry.GetInputs()[i]
}

// OwnerResetRun is what the agent runs to set a new password for an environment's
// install's owner: OwnerRun, with the entry's reset steps for its steps, and its
// done to read which password signs in (ADR 0105).
func (h *Handler) OwnerResetRun(ctx context.Context, environment string) (*resourcev1.OwnerRun, map[string]string, []string, error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var pin *resourcev1.CatalogPin
	err = h.db.View(ctx, func(tx *store.Tx) error {
		installs, err := h.installs(ctx, tx, s)
		if err != nil {
			return err
		}
		for _, in := range installs {
			if in.GetEnvironmentId() == environment {
				pin = in.GetPin()
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if pin == nil {
		return nil, nil, nil, fmt.Errorf("%w: this environment has no catalog install", store.ErrNotFound)
	}
	entry, known := s.entryFor(pin)
	if !known || entry.GetRelease() != pin.GetRelease() || len(entry.GetOwner().GetResetSteps()) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: its catalog no longer has release %d of %s, whose steps set a new password. Upgrade it, then try again", ErrNoRelease, pin.GetRelease(), pin.GetEntry())
	}
	run, plain, secretNames, err := h.OwnerRun(ctx, environment)
	if err != nil {
		return nil, nil, nil, err
	}
	run.Steps = cloneSteps(entry.GetOwner().GetResetSteps())
	run.MadeWhenDone, run.SignupsSteps, run.SignupsCheck = false, nil, nil
	if run.GetDone() == nil {
		run.Done = proto.CloneOf(entry.GetOwner().GetDone())
	}
	return run, plain, secretNames, nil
}
