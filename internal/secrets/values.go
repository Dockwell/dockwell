// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
)

var _ resource.Values = (*Keeper)(nil)

// ErrNotPlaced means a server asked for, or was about to be sent, the
// values of a resource that isn't placed on it.
var ErrNotPlaced = errors.New("the resource isn't placed on that server")

// Values returns what a resource's latest revision uses, for the server
// it is about to run on: each value by the name the resource sees it as,
// read at the revision the spec pins. It gives values only for a resource
// whose latest revision is placed on server, and only for that revision's
// spec, which desired must be: a server gets the secrets of what runs on
// it, and nothing else.
func (k *Keeper) Values(ctx context.Context, server string, ref resource.Ref, desired proto.Message) (map[string]string, error) {
	t, err := k.kinds.Type(ref.Kind)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	err = k.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return fault.New(ErrNotPlaced, "%s isn't on record, so no values go to %s for it", ref, server)
		}
		if err != nil {
			return err
		}
		placed, err := tx.Placement(ctx, r.ID, r.Revision)
		if err != nil {
			return err
		}
		if !slices.Contains(placed, server) {
			return fault.New(ErrNotPlaced, "%s %s doesn't run on %s, so its variables and secrets don't go there", ref.Kind, ref.Name, server)
		}
		latest, err := k.Decode(ctx, tx, r.ID, ref.Kind, r.Spec)
		if err != nil {
			return err
		}
		if !proto.Equal(latest, desired) {
			return fault.New(ErrNotPlaced, "the spec to apply isn't the latest revision of %s %s, so its values don't go to %s", ref.Kind, ref.Name, server)
		}
		for _, u := range resource.Uses(t, latest) {
			v, err := k.valueAt(ctx, tx, ref.Scope, u)
			if err != nil {
				return err
			}
			out[u.As] = v
		}
		return nil
	})
	if err != nil {
		clear(out)
		return nil, err
	}
	return out, nil
}

// valueAt reads what a use resolves to, at the revision it is pinned to.
func (k *Keeper) valueAt(ctx context.Context, tx *store.Tx, scopeID string, u resource.Use) (string, error) {
	vt, err := k.kinds.Type(u.Kind)
	if err != nil {
		return "", err
	}
	valuer, ok := vt.(resource.Valuer)
	if !ok {
		return "", fmt.Errorf("a %s has no value to use", u.Kind)
	}
	r, err := tx.GetResource(ctx, string(u.Kind), scopeID, u.Name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there's no %s %s for %s", u.Kind, u.Name, u.As)
	}
	if err != nil {
		return "", err
	}
	if u.Revision <= 0 {
		return "", fmt.Errorf("%s uses %s %s at no revision; plan the change again", u.As, u.Kind, u.Name)
	}
	rev, err := tx.GetRevision(ctx, r.ID, u.Revision)
	if err != nil {
		return "", err
	}
	if rev.Spec == nil {
		return "", fmt.Errorf("%s uses %s %s at revision %d, which deleted it", u.As, u.Kind, u.Name, u.Revision)
	}
	spec, err := k.Decode(ctx, tx, r.ID, u.Kind, rev.Spec)
	if err != nil {
		return "", err
	}
	return valuer.Value(spec), nil
}

// Revealed is a secret's value, as RevealSecret returns it.
type Revealed struct {
	Value    string
	Revision int
}

// ErrNotSecret means a reveal named something that isn't a secret.
var ErrNotSecret = errors.New("there's no such secret")

// Reveal returns a secret's latest value, and records in the audit log, in
// the same transaction, that actor revealed it. If the record can't be
// written, nothing is revealed. The caller checks that a person in a
// signed-in session confirmed it with a passkey just now.
func (k *Keeper) Reveal(ctx context.Context, secretID, actor, surface string) (Revealed, error) {
	var out Revealed
	err := k.db.Update(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, secretID)
		if errors.Is(err, store.ErrNotFound) || err == nil && (r.Kind != string(variable.SecretKind) || r.Spec == nil) {
			return fault.New(ErrNotSecret, "there's no secret %s. Check its ID with dockwell variables list", secretID)
		}
		if err != nil {
			return err
		}
		spec, err := k.Decode(ctx, tx, r.ID, variable.SecretKind, r.Spec)
		if err != nil {
			return err
		}
		out = Revealed{Value: variable.SecretType{}.Value(spec), Revision: r.Revision}
		env := r.Scope
		if e, err := tx.GetResourceByID(ctx, r.Scope); err == nil {
			env = e.Name
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: k.now(), Actor: actor, Action: "secret.reveal", Subject: r.ID, Target: r.ID, Surface: surface,
			Detail: fmt.Sprintf("revealed %s, revision %d, in %s", r.Name, r.Revision, env),
		})
	})
	if err != nil {
		out = Revealed{}
		return out, err
	}
	return out, nil
}
