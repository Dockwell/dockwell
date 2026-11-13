// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// serviceKind is what an app runs as, whose deletion a tied resource follows.
const serviceKind resource.Kind = "service"

// withTiesGoing takes in what a change's deleted services leave standing for nothing
// (resource.Tied): a link whose database goes, or whose app does, once nothing left
// reads the values it sets, with each of those values nothing left uses. It refuses a
// database's deletion while something left reads it through its link, naming what
// does, as that's what has to go first (ADR 0084).
func (e *Engine) withTiesGoing(ctx context.Context, tx *store.Tx, items []item, named map[resource.Ref]bool) ([]item, error) {
	kinds := e.kinds.TiedKinds()
	if len(kinds) == 0 {
		return items, nil
	}
	going := map[resource.Ref]bool{}
	var scopes []string
	for i := range items {
		if it := &items[i]; it.spec == nil && it.current != nil {
			going[it.ref] = true
			if it.ref.Kind == serviceKind && !slices.Contains(scopes, it.ref.Scope) {
				scopes = append(scopes, it.ref.Scope)
			}
		}
	}
	for _, scopeID := range scopes {
		for _, kind := range kinds {
			stored, err := tx.ListResources(ctx, scopeID, string(kind))
			if err != nil {
				return items, err
			}
			for j := range stored {
				ref := resource.Ref{Kind: kind, Scope: scopeID, Name: stored[j].Name}
				if named[ref] {
					continue
				}
				tied, err := e.load(ctx, tx, ref)
				if err != nil {
					return items, err
				}
				more, err := e.tiedGoing(ctx, tx, &tied, going)
				if err != nil {
					return items, err
				}
				for k := range more {
					named[more[k].ref] = true
					items = append(items, more[k])
				}
			}
		}
	}
	return items, nil
}

// tiedGoing is what goes with a tied resource when a change deletes what it names or
// what reads its values: itself and each of its values nothing left uses, or nothing.
func (e *Engine) tiedGoing(ctx context.Context, tx *store.Tx, tied *item, going map[resource.Ref]bool) ([]item, error) {
	scopeID := tied.ref.Scope
	meets := map[resource.Ref]bool{}
	for _, r := range e.kinds.References(tied.ref.Kind, tied.current) {
		meets[resource.Ref{Kind: r.Kind, Scope: scopeID, Name: r.Name}] = true
	}
	values := e.kinds.Ties(tied.ref.Kind, tied.ref.Name, tied.current)
	own := map[resource.Ref]bool{}
	for _, v := range values {
		own[resource.Ref{Kind: v.Kind, Scope: scopeID, Name: v.Name}] = true
	}
	// What reads its values, and what uses each, but its own values, which write one
	// another, as a URL does the password.
	var readers []string // what's left that reads them, by name, but what meets it
	readerGoes := false
	usedBy := map[resource.Ref][]resource.Ref{}
	for v := range own {
		users, err := tx.UsersOf(ctx, scopeID, string(v.Kind), v.Name)
		if err != nil {
			return nil, err
		}
		for k := range users {
			u := &users[k]
			user := resource.Ref{Kind: resource.Kind(u.Kind), Scope: u.Scope, Name: u.Name}
			if own[user] {
				continue
			}
			usedBy[v] = append(usedBy[v], user)
			switch {
			case meets[user]:
				// What meets the need may read a value too, as a database starts with
				// its user's password: it's no reader of the link.
			case going[user]:
				readerGoes = true
			case !slices.Contains(readers, user.Name):
				readers = append(readers, user.Name)
			}
		}
	}
	meetsGoes := false
	for m := range meets {
		meetsGoes = meetsGoes || going[m]
	}
	switch {
	case meetsGoes && len(readers) > 0:
		slices.Sort(readers)
		var name string
		for m := range meets {
			name = string(m.Kind) + " " + m.Name
		}
		verb := "reads"
		if len(readers) > 1 {
			verb = "read"
		}
		return nil, fault.New(ErrInvalid, "%s can't go while %s %s it through the link %s. Delete %s in the same change, or first",
			name, human.List(readers), verb, tied.ref.Name, human.List(readers))
	case !meetsGoes && !readerGoes, len(readers) > 0:
		return nil, nil
	}
	tied.spec = nil
	out := []item{*tied}
	for _, v := range values {
		ref := resource.Ref{Kind: v.Kind, Scope: scopeID, Name: v.Name}
		left := slices.ContainsFunc(usedBy[ref], func(u resource.Ref) bool { return !going[u] })
		if left || going[ref] {
			continue
		}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		if it.current == nil {
			continue
		}
		it.spec = nil
		out = append(out, it)
	}
	return out, nil
}
