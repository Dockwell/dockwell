// SPDX-License-Identifier: AGPL-3.0-only

package owners

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

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
// apart, while the outcome isn't known. Then done is read with each password every
// Poll, until store.RotateStuck reads in all, and every RotateSlow after that, with
// no end: both are kept sealed until one reads signing in, or a person settles it
// (ADR 0105, the review's H1).
const (
	RotateTries = 3
	RotateAfter = 3 * Every
	RotateSlow  = time.Hour
)

// NoteOwnerPassword is the note of the change that keeps an owner's new password.
const NoteOwnerPassword = "owner_password"

// errRotationGone is a rotation that ended, or another took its place, since the
// look found it.
var errRotationGone = errors.New("this rotation ended")

// rotateWait is how long after its last read a rotation is read again.
func rotateWait(tries int) time.Duration {
	switch {
	case tries < RotateTries:
		return RotateAfter
	case tries < store.RotateStuck:
		return Poll
	default:
		return RotateSlow
	}
}

// rotations finds the new passwords to set, or whose outcome to read, now, and the
// rotations to end, whose owner is gone or isn't made: those can't be read, and
// nothing they'd set is anyone's now (the review's L3).
func (r *Runner) rotations(ctx context.Context, tx *store.Tx) ([]job, []job, error) {
	list, err := tx.AppOwnersRotating(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := r.now()
	var jobs, ends []job
	for i := range list {
		st := &list[i]
		this := job{id: st.ResourceID, environment: st.Environment, rotation: st.Rotation, tries: st.RotateTries, since: st.UpdatedAt}
		res, err := tx.GetResource(ctx, string(appowner.Kind), st.Environment, appowner.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && (res.ID != st.ResourceID || res.Spec == nil) || st.State != store.OwnerMade {
			ends = append(ends, this)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if st.RotateTries > 0 && now.Sub(st.UpdatedAt) < rotateWait(st.RotateTries) {
			continue
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return nil, nil, err
		}
		owner, ok := spec.(*resourcev1.AppOwner)
		if !ok {
			continue
		}
		server, err := liveServer(ctx, tx, st.Environment, owner.GetService())
		if err != nil {
			return nil, nil, err
		}
		if server == "" {
			continue
		}
		this.server = server
		jobs = append(jobs, this)
	}
	return jobs, ends, nil
}

// readyRotation reads what sets a new password, the values its steps name, which
// hold the current password, and the new one, opened for this rotation alone.
// Where the reset steps can't be read, as after an upgrade to a release without
// them, done alone is read, from the run that makes the owner. Where nothing reads,
// or the new one can't be opened now, it waits for the next look: nothing but a
// password read signing in ends a rotation (the review's H1).
func (r *Runner) readyRotation(ctx context.Context, j job, runs Runs) (*job, error) {
	var run *resourcev1.OwnerRun
	var plain map[string]string
	var secretNames []string
	var err error
	if resets, ok := runs.(ResetRuns); ok {
		if run, plain, secretNames, err = resets.OwnerResetRun(ctx, j.environment); err != nil {
			run = nil
		}
	}
	if run == nil {
		j.readsOnly = true
		if run, plain, secretNames, err = runs.OwnerRun(ctx, j.environment); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r.logger().WarnContext(ctx, "can't read which password an app's owner takes now; reading again later", slog.String("environment", j.environment), slog.String("error", err.Error()))
			return nil, nil
		}
	}
	if run.GetPassword() == "" || run.GetDone() == nil {
		r.logger().WarnContext(ctx, "an app's owner has a new password and nothing to read it with; reading again later", slog.String("environment", j.environment))
		return nil, nil
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
		return nil, nil
	}
	if err != nil {
		r.logger().WarnContext(ctx, "can't open an app owner's new password now; trying again later", slog.String("environment", j.environment), slog.String("error", err.Error()))
		return nil, nil
	}
	j.pending = string(pending)
	clear(pending)
	values, err := r.secrets(ctx, j.environment, plain, secretNames)
	if err != nil {
		j.pending = ""
		r.logger().WarnContext(ctx, "can't read the secrets an app owner's new password needs; trying again later", slog.String("environment", j.environment), slog.String("error", err.Error()))
		return nil, nil
	}
	j.owner, j.values = run, values
	return &j, nil
}

// rotate runs the entry's reset steps with the new password, a few times at most,
// then keeps whichever password reads signing in: the steps' answer made, or done
// read with each. Only a password read signing in ends a rotation: the new one is
// kept, or the current one stays and the new one goes. A no, or no answer, keeps
// both sealed and reads again later; past store.RotateStuck reads, the owner says
// why, and a person may show the new one or keep the current one (ADR 0105, the
// review's H1).
func (r *Runner) rotate(ctx context.Context, j job) {
	a, ok := r.agentFor(j.server)
	if !ok {
		return
	}
	withPending := maps.Clone(j.values)
	withPending[j.owner.GetPassword()] = j.pending
	defer clear(withPending)
	// What the app says is scrubbed of both passwords (the review's L2).
	both := maps.Clone(j.values)
	both[BothPasswordsKey] = j.pending
	defer clear(both)
	said := ""
	if !j.readsOnly && j.tries < RotateTries {
		// A try after the first reads whether the new one signs in already.
		got, err := a.MakeAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, withPending, j.tries > 0)
		switch {
		case err == nil && got.GetMade():
			r.keepPending(ctx, j)
			return
		case err == nil && !got.GetWaiting():
			// A step said no, part of the way: which one the app has is read below.
			said = ownerstep.Scrub(fmt.Appendf(nil, "Step %d: %s", got.GetStep(), got.GetSaid()), both, nil, j.owner)
		default:
			// No answer, or the app doesn't answer yet: try again.
			r.countRotation(ctx, j, "")
			return
		}
	}
	newRead, errNew := a.ReadAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, withPending, false)
	if errNew == nil && newRead.GetYes() {
		r.keepPending(ctx, j)
		return
	}
	oldRead, errOld := a.ReadAppOwner(ctx, j.environment, j.owner.GetService(), j.owner, j.values, false)
	if errOld == nil && oldRead.GetYes() {
		// The app kept the password it had, so that one stays.
		r.endRotation(ctx, j, cut(cmp.Or(said, "The app kept the password it had")))
		return
	}
	// Neither reads signing in. Until a person is asked, it says nothing; then why,
	// as no answer, or the app's no.
	why := ""
	if j.tries+1 >= store.RotateStuck {
		read := "Neither password signs in now"
		switch {
		case errNew != nil || errOld != nil:
			read = "Dockwell can't read which password it takes now: its server's agent doesn't answer"
		case newRead.GetSaid() != "":
			read += ": " + ownerstep.Scrub([]byte(newRead.GetSaid()), both, nil, j.owner)
		}
		why = read
		if said != "" {
			why = said + ". " + read
		}
		why = cut(why)
	}
	r.countRotation(ctx, j, why)
}

// BothPasswordsKey names the new password in what a refusal is scrubbed of, beside
// the current one, which the values hold.
const BothPasswordsKey = "DOCKWELL_NEW_PASSWORD" //nolint:gosec // a value's name, never a credential

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
		if err == nil && res.Spec == nil {
			err = fmt.Errorf("%w: its password's secret %s was deleted", store.ErrNotFound, name)
		}
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
