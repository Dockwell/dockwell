// SPDX-License-Identifier: AGPL-3.0-only

package owners

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ownerstep"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// A new password's reset steps run up to RotateTries times, a look of RotateAfter
// apart, while the outcome isn't known; then its done is read every Poll, both
// passwords kept, up to RotateReads times in all before the new one is given up.
const (
	RotateTries = 3
	RotateAfter = 3 * Every
	RotateReads = 12
)

// NoteOwnerPassword is the note of the change that keeps an owner's new password.
const NoteOwnerPassword = "owner_password"

// errRotationGone is a rotation that ended, or another took its place, since the
// look found it.
var errRotationGone = errors.New("this rotation ended")

// rotations finds the new passwords to set, or whose outcome to read, now.
func (r *Runner) rotations(ctx context.Context, tx *store.Tx) ([]job, error) {
	list, err := tx.AppOwnersRotating(ctx)
	if err != nil {
		return nil, err
	}
	now := r.now()
	var jobs []job
	for i := range list {
		st := &list[i]
		if st.State != store.OwnerMade {
			continue
		}
		wait := RotateAfter
		if st.RotateTries >= RotateTries {
			wait = Poll
		}
		if st.RotateTries > 0 && now.Sub(st.UpdatedAt) < wait {
			continue
		}
		res, err := tx.GetResource(ctx, string(appowner.Kind), st.Environment, appowner.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && (res.ID != st.ResourceID || res.Spec == nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return nil, err
		}
		owner, ok := spec.(*resourcev1.AppOwner)
		if !ok {
			continue
		}
		server, err := liveServer(ctx, tx, st.Environment, owner.GetService())
		if err != nil {
			return nil, err
		}
		if server == "" {
			continue
		}
		jobs = append(jobs, job{id: st.ResourceID, environment: st.Environment, server: server, tries: st.RotateTries, since: st.UpdatedAt, rotation: st.Rotation})
	}
	return jobs, nil
}

// readyRotation reads what sets a new password, the values its steps name, which
// hold the current password, and the new one, opened for this rotation alone.
func (r *Runner) readyRotation(ctx context.Context, j job, runs Runs) (*job, *store.AppOwnerState, error) {
	resets, ok := runs.(ResetRuns)
	if !ok {
		return nil, nil, nil
	}
	run, plain, secretNames, err := resets.OwnerResetRun(ctx, j.environment)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		// Nothing reads which one signs in, so the current one stays.
		r.endRotation(ctx, j, cut(err.Error()))
		return nil, nil, nil
	}
	if run.GetPassword() == "" {
		r.endRotation(ctx, j, "its catalog entry names no password input for its owner")
		return nil, nil, nil
	}
	var pending []byte
	err = r.DB.View(ctx, func(tx *store.Tx) error {
		states, err := tx.AppOwnerStates(ctx, []string{j.id})
		if err != nil {
			return err
		}
		st := states[j.id]
		if st.Rotation != j.rotation {
			return errRotationGone
		}
		pending, err = r.Keeper.Open(ctx, tx, store.AppOwnerPendingAt(j.id, j.rotation), st.Pending)
		return err
	})
	if errors.Is(err, errRotationGone) {
		return nil, nil, nil
	}
	if err != nil {
		// It can't be set, so the current one stays, and the rotation ends saying so.
		r.endRotation(ctx, j, "Dockwell can't open the new password it kept")
		return nil, nil, nil //nolint:nilerr // ended, with what it says
	}
	j.pending = string(pending)
	clear(pending)
	values, err := r.secrets(ctx, j.environment, plain, secretNames)
	if err != nil {
		j.pending = ""
		return nil, nil, err
	}
	j.owner, j.values = run, values
	return &j, nil, nil
}

// rotate runs the entry's reset steps with the new password, then keeps whichever
// password signs in, by the steps' answer or, where that's lost or a step said no,
// by done: both are kept until one signs in (ADR 0105).
func (r *Runner) rotate(ctx context.Context, j job) {
	a, ok := r.agentFor(j.server)
	if !ok {
		return
	}
	withPending := maps.Clone(j.values)
	withPending[j.owner.GetPassword()] = j.pending
	defer clear(withPending)
	said := ""
	if j.tries < RotateTries {
		// A try after the first reads whether the new one signs in already.
		got, err := a.MakeAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, withPending, j.tries > 0)
		switch {
		case err == nil && got.GetMade():
			r.keepPending(ctx, j)
			return
		case err == nil && !got.GetWaiting():
			said = ownerstep.Scrub(fmt.Appendf(nil, "Step %d: %s", got.GetStep(), got.GetSaid()), withPending, nil, j.owner)
			// Its steps ran part of the way, so the outcome is read below.
		default:
			// The answer was lost, or the app doesn't answer yet: try again.
			r.countRotation(ctx, j, "")
			return
		}
	}
	newSigns, errNew := a.ReadAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, withPending, false)
	if errNew == nil && newSigns.GetYes() {
		r.keepPending(ctx, j)
		return
	}
	oldSigns, errOld := a.ReadAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, j.values, false)
	if errOld == nil && oldSigns.GetYes() {
		// The app kept the password it had, so that one stays.
		r.endRotation(ctx, j, cut(cmp.Or(said, "The app kept the password it had")))
		return
	}
	if j.tries+1 >= RotateReads {
		r.endRotation(ctx, j, cut(cmp.Or(said, "Neither password signed in, so Dockwell keeps the one it had")))
		return
	}
	r.countRotation(ctx, j, cut(said))
}

// keepPending puts the new password in place of the current one, as a change of
// Dockwell's applied at once, then ends the rotation. Where the change can't apply,
// the rotation stays, and the next look reads the new one signs in and tries again.
func (r *Runner) keepPending(ctx context.Context, j job) {
	ctx = context.WithoutCancel(ctx)
	if err := r.commit(ctx, j); err != nil {
		r.logger().WarnContext(ctx, "can't keep an app owner's new password now; trying again", slog.String("environment", j.environment), slog.String("error", err.Error()))
		r.countRotation(ctx, j, "")
		return
	}
	r.endRotation(ctx, j, "")
}

// commit writes the new password on the owner's password secret.
func (r *Runner) commit(ctx context.Context, j job) error {
	name := j.owner.GetPassword()
	var edit *revision.Edit
	var project string
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		res, err := tx.GetResource(ctx, string(variable.SecretKind), j.environment, name)
		if err != nil {
			return fmt.Errorf("can't read the secret %s: %w", name, err)
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return err
		}
		s, ok := spec.(*resourcev1.Secret)
		if !ok {
			return fmt.Errorf("%s isn't a secret", name)
		}
		if s.GetValue() == j.pending {
			return nil
		}
		next := proto.CloneOf(s)
		next.Value = j.pending
		edit = &revision.Edit{Ref: resource.Ref{Kind: variable.SecretKind, Scope: j.environment, Name: name}, Spec: next, Base: res.Revision}
		project, err = projectOf(ctx, tx, j.environment)
		return err
	})
	if err != nil || edit == nil {
		return err
	}
	by := revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}
	plan, err := r.Engine.Propose(ctx, revision.Proposal{
		Summary: "Keep " + project + "'s new owner password", Actor: by.Actor, Via: by.Via, Edits: []revision.Edit{*edit},
		Notes: []revision.Note{{Reason: NoteOwnerPassword, Message: "The app took a new password for its owner, so Dockwell keeps it in place of the one before"}},
	})
	if err != nil {
		return err
	}
	if err := r.Engine.Apply(ctx, plan.Change); err != nil {
		_ = r.Engine.Discard(ctx, plan.Change, by)
		return err
	}
	return nil
}

// projectOf is the name of an environment's project.
func projectOf(ctx context.Context, tx *store.Tx, environment string) (string, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return "", err
	}
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			if e.ID == environment {
				return p.Name, nil
			}
		}
	}
	return "", nil
}

// countRotation records that a rotation's outcome was read once more.
func (r *Runner) countRotation(ctx context.Context, j job, said string) {
	err := r.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		return tx.CountAppOwnerRotation(ctx, j.id, j.rotation, said, r.now())
	})
	if err != nil {
		r.logger().WarnContext(ctx, "can't record an app owner's new password", slog.String("error", err.Error()))
	}
}

// endRotation forgets a rotation's new password, with what kept it from being set.
func (r *Runner) endRotation(ctx context.Context, j job, said string) {
	err := r.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		_, err := tx.EndAppOwnerRotation(ctx, j.id, j.rotation, said, r.now())
		return err
	})
	if err != nil {
		r.logger().WarnContext(ctx, "can't end an app owner's new password", slog.String("error", err.Error()))
	}
}
