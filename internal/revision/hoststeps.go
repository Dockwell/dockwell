// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/store"
)

// A step on a server's host, a reboot, an install, a unit's action or a panel's stop
// or start, keeps the host as it is, labels and all (ADR 0091): such an edit leaves
// its spec out, and planning starts it from what's stored, so no caller can lose
// what it didn't read.

// NewHost is the host a server starts with, before anything made it: the labels it
// joined with, as its join token had them (ADR 0091). Making a server's host, a
// step on it before that's made, and a first change of its labels all start here,
// so none loses what the server joined with.
func NewHost(ctx context.Context, tx *store.Tx, server string) (*resourcev1.ServerHost, error) {
	labels, err := tx.JoinLabels(ctx, server)
	if err != nil {
		return nil, err
	}
	return &resourcev1.ServerHost{Labels: labels}, nil
}

// hostStep reports an edit that takes a step on its server's host.
func hostStep(ed *Edit) bool {
	return ed.Reboot || ed.Install != nil || ed.Unit != nil || ed.Panel != nil || ed.UpgradeAgent != nil
}

// hostsAsIs gives edits with each step on a host given that host's stored spec, or
// a new one's for a server that has none yet, as a copy. An edit that says a spec
// with such a step is refused: it would change what the step was asked to keep.
func (e *Engine) hostsAsIs(ctx context.Context, edits []Edit) ([]Edit, error) {
	var out []Edit
	for i := range edits {
		ed := &edits[i]
		if !hostStep(ed) {
			continue
		}
		if ed.Spec != nil {
			return nil, fault.New(ErrInvalid, "%s takes a step on the server and keeps its spec as it is. Leave the spec out", ed.Ref)
		}
		if out == nil {
			out = append([]Edit(nil), edits...)
		}
		if err := e.db.View(ctx, func(tx *store.Tx) error {
			it, err := e.load(ctx, tx, ed.Ref)
			if err != nil {
				return err
			}
			if it.current != nil {
				out[i].Spec, out[i].Base = it.current, it.base
				return nil
			}
			out[i].Spec, err = NewHost(ctx, tx, ed.Ref.Scope)
			return err
		}); err != nil {
			return nil, err
		}
	}
	if out == nil {
		return edits, nil
	}
	return out, nil
}
