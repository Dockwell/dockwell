// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// withMovedValues turns each edit's EnvToSecrets into what it means: a resource of
// the edit's scope for each value, made with the value the resource has now, and
// the edit's spec using it in place of its own (resource.ValueMover), before the
// resource's own edit, so the change makes what it then uses. The value goes from
// one sealed spec to another here, in the control plane, and nobody reads it. A
// value of that name in the scope already is refused, as making it again would
// change it for everything that uses it.
func (e *Engine) withMovedValues(ctx context.Context, edits []Edit) ([]Edit, error) {
	if !slices.ContainsFunc(edits, func(ed Edit) bool { return len(ed.EnvToSecrets) > 0 }) {
		return edits, nil
	}
	out := make([]Edit, 0, len(edits)+1)
	err := e.db.View(ctx, func(tx *store.Tx) error {
		for i := range edits {
			ed := edits[i]
			if len(ed.EnvToSecrets) == 0 {
				out = append(out, ed)
				continue
			}
			t, err := e.kinds.Type(ed.Ref.Kind)
			if err != nil {
				return fault.New(ErrInvalid, "%w", err)
			}
			mover, ok := t.(resource.ValueMover)
			switch {
			case !ok:
				return fault.New(ErrInvalid, "%s makes secrets of its values, which only a service can. Leave env_to_secrets out", ed.Ref)
			case ed.Spec == nil:
				return fault.New(ErrInvalid, "%s is deleted, and makes secrets of its values too. Leave env_to_secrets out", ed.Ref)
			case ed.Base == 0:
				return fault.New(ErrInvalid, "%s makes secrets of its values, but says no base_revision, so a change since the spec was read would be undone. "+
					"Pass the revision ListServices gave as base_revision", ed.Ref)
			}
			it, err := e.load(ctx, tx, ed.Ref)
			if err != nil {
				return err
			}
			switch {
			case it.current == nil:
				return fault.New(ErrInvalid, "%s makes secrets of its values, but there's no %s yet to take them from", ed.Ref, ed.Ref.Kind)
			case it.base != ed.Base:
				return fault.New(ErrStale, "%s changed while this was being planned, and planning from what was read would undo that. Try again, to plan it from the latest", ed.Ref)
			}
			for _, name := range ed.EnvToSecrets {
				if slices.Contains(ed.KeepEnv, name) {
					return fault.New(ErrInvalid, "%s both keeps %s and makes it a secret. Do one", ed.Ref, name)
				}
			}
			spec := proto.Clone(ed.Spec)
			kind, values, err := mover.MoveValues(it.current, spec, ed.EnvToSecrets)
			if err != nil {
				return fault.New(ErrInvalid, "%s: %w", ed.Ref, err)
			}
			vt, err := e.kinds.Type(kind)
			if err != nil {
				return fault.New(ErrInvalid, "%w", err)
			}
			maker, ok := vt.(resource.ValueMaker)
			if !ok {
				return fault.New(ErrInvalid, "%s holds no value of its own to move one into", kind)
			}
			for _, name := range ed.EnvToSecrets {
				ref := resource.Ref{Kind: kind, Scope: ed.Ref.Scope, Name: name}
				if err := e.noneYet(ctx, tx, ref, edits); err != nil {
					return err
				}
				out = append(out, Edit{Ref: ref, Spec: maker.MakeValue(values[name])})
			}
			ed.Spec, ed.EnvToSecrets = spec, nil
			out = append(out, ed)
		}
		return nil
	})
	return out, err
}

// noneYet refuses moving a value into a resource the scope has already, or that the
// change makes anyway, and into any other kind that holds a value by the same name,
// as an environment's variables and secrets are one list of names (ADR 0022).
func (e *Engine) noneYet(ctx context.Context, tx *store.Tx, ref resource.Ref, edits []Edit) error {
	if slices.ContainsFunc(edits, func(ed Edit) bool { return ed.Ref.Scope == ref.Scope && ed.Ref.Name == ref.Name && ed.Spec != nil }) {
		return fault.New(ErrInvalid, "the change makes %s already, so it can't make a secret of the same name from %s's value. Rename one", ref.Name, ref.Name)
	}
	for _, k := range e.kinds.All() {
		if t, err := e.kinds.Type(k); err != nil || !isValuer(t) {
			continue
		}
		r, err := tx.GetResource(ctx, string(k), ref.Scope, ref.Name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return err
		}
		if r.Spec != nil {
			return fault.New(ErrInvalid, "the environment has a %s %s already, so a secret of that name would change it for everything that uses it. Rename one, then try again", k, ref.Name)
		}
	}
	return nil
}

func isValuer(t resource.Type) bool {
	_, ok := t.(resource.Valuer)
	return ok
}
