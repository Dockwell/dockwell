// SPDX-License-Identifier: AGPL-3.0-only

package owners

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// What goes once an owner is made or opened, which only making it needed: the
// secrets it held, and the variables its entry takes out of its service, as
// Vaultwarden's ADMIN_TOKEN, which turns its admin page off. It's observed state the
// runner works towards each look, after a failure a minute later, then five, then
// fifteen, then hourly, and Activity says so once it's failed for SettleAlertAfter
// (ADR 0105, the review's M1).
const (
	SettleAlertAfter = time.Hour
	// NoteOwnerUnset is the note of the change that takes it out.
	NoteOwnerUnset = "owner_unset"
	// ActionUnsetFailed is the event that says it's failed for an hour.
	ActionUnsetFailed = "app_owner.unset_failed"
)

// settleWait is how long after its tries-th failure settling is tried again.
func settleWait(tries int) time.Duration {
	switch tries {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

// settleAll settles each owner made or opened that's due.
func (r *Runner) settleAll(ctx context.Context) {
	var due []store.AppOwnerState
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		list, err := tx.AppOwnersUnsettled(ctx)
		now := r.now()
		for i := range list {
			if list[i].SettleNext.IsZero() || !now.Before(list[i].SettleNext) {
				due = append(due, list[i])
			}
		}
		return err
	})
	if err != nil {
		r.logger().WarnContext(ctx, "can't read which apps' owners are settled; looking again", slog.String("error", err.Error()))
		return
	}
	for i := range due {
		r.settleOne(ctx, &due[i])
	}
}

// settleOne settles an owner, or records why it couldn't, and when it's tried again.
func (r *Runner) settleOne(ctx context.Context, st *store.AppOwnerState) {
	ctx = context.WithoutCancel(ctx)
	left, err := r.settle(ctx, st.Environment, st.ResourceID)
	now := r.now()
	if err == nil {
		if err := r.DB.Update(ctx, func(tx *store.Tx) error { return tx.SettleAppOwner(ctx, st.ResourceID, now) }); err != nil {
			r.logger().WarnContext(ctx, "can't record an app's owner settled", slog.String("error", err.Error()))
		}
		return
	}
	r.logger().WarnContext(ctx, "can't take out what only making an app's owner needed; trying again", slog.String("environment", st.Environment), slog.String("error", err.Error()))
	why := cut(err.Error())
	since := st.SettleSince
	if since.IsZero() {
		since = now
	}
	err = r.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.CountAppOwnerSettle(ctx, st.ResourceID, why, strings.Join(left, " "), now.Add(settleWait(st.SettleTries+1)), now); err != nil {
			return err
		}
		if now.Sub(since) < SettleAlertAfter || len(left) == 0 {
			return nil
		}
		first, err := tx.AlertAppOwnerSettle(ctx, st.ResourceID)
		if err != nil || !first {
			return err
		}
		told := *st
		told.Error = cut(strings.Join(left, ", ") + " is still set: " + why)
		return r.recordEvent(ctx, tx, ActionUnsetFailed, told)
	})
	if err != nil {
		r.logger().WarnContext(ctx, "can't record what came of settling an app's owner", slog.String("error", err.Error()))
	}
}

// settle takes out what only making an environment's owner needed, as a change of
// Dockwell's applied at once, and says what it would have taken out of its service
// where that fails.
func (r *Runner) settle(ctx context.Context, environment, id string) ([]string, error) {
	edits, names, project, err := r.SettleEdits(ctx, environment, id)
	if err != nil || len(edits) == 0 {
		return names, err
	}
	by := revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}
	notes := []revision.Note{{Reason: "owner_settled", Message: project + "'s owner exists, so it no longer holds the secrets its steps read"}}
	if len(names) > 0 {
		notes = []revision.Note{{Reason: NoteOwnerUnset, Message: project + "'s owner exists, so " + strings.Join(names, ", ") + " goes, as only making the owner needed it", Names: names}}
	}
	plan, err := r.Engine.Propose(ctx, revision.Proposal{Summary: "Settle " + project + ": its owner exists", Actor: by.Actor, Via: by.Via, Edits: edits, Notes: notes})
	if err != nil {
		return names, err
	}
	if err := r.Engine.Apply(ctx, plan.Change); err != nil {
		_ = r.Engine.Discard(ctx, plan.Change, by)
		return names, err
	}
	return names, nil
}

// SettleEdits are the edits that take out what only making an environment's owner
// needed: its held secrets, and the variables its entry names that its service
// still sets, with those names and the project's name. None where nothing's left,
// or the owner isn't the one named by id.
func (r *Runner) SettleEdits(ctx context.Context, environment, id string) ([]revision.Edit, []string, string, error) {
	r.mu.Lock()
	runs := r.runs
	r.mu.Unlock()
	var svcName string
	var names []string
	if u, ok := runs.(Unsets); ok {
		var err error
		if svcName, names, err = u.OwnerUnset(ctx, environment); err != nil {
			return nil, names, "", err
		}
	}
	var edits []revision.Edit
	var left []string
	var project string
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		res, err := tx.GetResource(ctx, string(appowner.Kind), environment, appowner.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && (res.ID != id || res.Spec == nil) {
			return nil
		}
		if err != nil {
			return err
		}
		spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
		if err != nil {
			return err
		}
		if o, ok := spec.(*resourcev1.AppOwner); ok && len(o.GetHeldSecrets()) > 0 {
			o = proto.CloneOf(o)
			o.HeldSecrets = nil
			edits = append(edits, revision.Edit{Ref: resource.Ref{Kind: appowner.Kind, Scope: environment, Name: appowner.Name}, Spec: o, Base: res.Revision, Owner: true})
		}
		if len(names) > 0 {
			edit, err := r.unsetEdit(ctx, tx, environment, svcName, names)
			if err != nil {
				return err
			}
			if edit != nil {
				edits = append(edits, *edit)
				left = slices.Clone(names)
			}
		}
		project, err = projectOf(ctx, tx, environment)
		return err
	})
	return edits, left, project, err
}

// unsetEdit takes names out of a service's environment: its own values, and the
// variables and secrets it uses under those names. Nil where it sets none of them.
func (r *Runner) unsetEdit(ctx context.Context, tx *store.Tx, environment, name string, names []string) (*revision.Edit, error) {
	res, err := tx.GetResource(ctx, string(service.Kind), environment, name)
	if errors.Is(err, store.ErrNotFound) || err == nil && res.Spec == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
	if err != nil {
		return nil, err
	}
	svc, ok := spec.(*resourcev1.Service)
	if !ok {
		return nil, nil
	}
	changed := false
	for _, n := range names {
		for _, m := range []map[string]string{svc.GetApp().GetEnv(), svc.GetVariables(), svc.GetSecrets()} {
			if _, ok := m[n]; ok {
				delete(m, n)
				changed = true
			}
		}
	}
	if !changed {
		return nil, nil
	}
	return &revision.Edit{Ref: resource.Ref{Kind: service.Kind, Scope: environment, Name: name}, Spec: svc, Base: res.Revision}, nil
}
