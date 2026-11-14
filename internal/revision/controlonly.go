// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"

	"github.com/dockwell/dockwell/internal/resource"
)

// controlOnlyNotes are a plan's notes, saying of each kind that has words for it
// that its change is only of what the control plane keeps, as a server's labels
// are (resource.ControlOnlySayer): no server takes a step for it, so it applies
// with them online or not. One note for each kind names every server it's on.
func (e *Engine) controlOnlyNotes(ctx context.Context, plan *Plan, items []item, notes []Note) ([]Note, error) {
	stepped := map[resource.Ref]bool{}
	for i := range plan.Servers {
		for j := range plan.Servers[i].Steps {
			stepped[plan.Servers[i].Steps[j].Ref] = true
		}
	}
	type said struct {
		ref     resource.Ref
		servers []string
	}
	var kinds []resource.Kind
	byKind := map[resource.Kind]*said{}
	for i := range items {
		it := &items[i]
		if stepped[it.ref] || it.current == nil || it.spec == nil {
			continue
		}
		if !e.kinds.ControlOnlyChange(it.ref.Kind, it.current, it.spec) {
			continue
		}
		s := byKind[it.ref.Kind]
		if s == nil {
			s = &said{ref: it.ref}
			byKind[it.ref.Kind] = s
			kinds = append(kinds, it.ref.Kind)
		} else {
			s.ref = resource.Ref{} // about more than one
		}
		on := it.servers
		if it.level == resource.LevelServer && len(on) == 0 {
			on = []string{it.ref.Scope}
		}
		for _, server := range on {
			if !slices.Contains(s.servers, server) {
				s.servers = append(s.servers, server)
			}
		}
	}
	if len(kinds) == 0 {
		return notes, nil
	}
	out := slices.Clone(notes)
	for _, kind := range kinds {
		s := byKind[kind]
		names, err := e.serverNames(ctx, s.servers)
		if err != nil {
			return nil, err
		}
		named := make([]string, 0, len(s.servers))
		for _, id := range s.servers {
			named = append(named, names[id])
		}
		slices.Sort(named)
		if reason, message, ok := e.kinds.SayControlOnly(kind, named); ok {
			out = append(out, Note{Reason: reason, Message: message, Ref: s.ref, Names: named, Named: true})
		}
	}
	return out, nil
}
