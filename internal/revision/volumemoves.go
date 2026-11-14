// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Moving a volume's data into another, as moving an app to another project does
// (ADR 0084, #572). Docker can't rename a volume, so the change makes one of the new
// name with the old one's data (Edit.MoveFrom) and deletes the old one, which stays,
// empty, for 7 days. Nothing may mount either while the data moves, so what mounted
// the old one is deleted before, and undoing the change moves the data back the same
// way.

// movesWhole refuses a move the change doesn't carry through: the data moves into a
// new volume, from one of another environment on the same servers, which the change
// deletes.
func movesWhole(items []item) error {
	for i := range items {
		it := &items[i]
		if it.moveFrom == (resource.Ref{}) {
			continue
		}
		from := slices.IndexFunc(items, func(o item) bool { return o.ref == it.moveFrom })
		// A link moved is the one it was, for the backups taken for it.
		if it.ref.Kind == linkKind && it.moveFrom.Kind == linkKind {
			if it.spec == nil || it.moveFrom.Scope == it.ref.Scope || from < 0 || items[from].spec != nil {
				return fault.New(ErrInvalid, "%s is %s moved only as the change makes it in another environment and deletes %s", it.ref, it.moveFrom, it.moveFrom.Name)
			}
			continue
		}
		switch {
		case it.ref.Kind != volumeKind || it.moveFrom.Kind != volumeKind:
			return fault.New(ErrInvalid, "only a volume's data moves into another, and a link into another environment: %s can't take %s's", it.ref, it.moveFrom)
		case it.spec == nil:
			return fault.New(ErrInvalid, "%s is deleted in this change, so it can't take the data of %s", it.ref, it.moveFrom)
		case it.moveFrom.Scope == it.ref.Scope:
			return fault.New(ErrInvalid, "%s and %s are in one environment, and data moves only into a volume of another", it.ref, it.moveFrom)
		case from < 0 || items[from].spec != nil:
			return fault.New(ErrInvalid, "%s takes the data of %s, so the change deletes %s too", it.ref, it.moveFrom, it.moveFrom.Name)
		case !slices.Equal(items[from].placed, it.servers):
			return fault.New(ErrInvalid, "%s takes the data of %s, which moves only on the servers it's on, so %s goes on the same", it.ref, it.moveFrom, it.ref.Name)
		}
	}
	return nil
}

// moveFromIn is the volume whose data change c moves into ref, as its plan says.
func moveFromIn(c store.Change, ref resource.Ref) resource.Ref {
	for _, m := range movesIn(c) {
		if m.to == ref {
			return m.from
		}
	}
	return resource.Ref{}
}

// deletesSharedIn reports a deletion of a scope that change c's plan says takes what
// other projects' apps use too.
func deletesSharedIn(c store.Change, ref resource.Ref) bool {
	if len(c.Plan) == 0 {
		return false
	}
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(p.Resources, func(r ResourcePlan) bool { return r.Ref == ref && r.DeleteShared })
}

// volumeMove is data a change moves from one volume into another.
type volumeMove struct{ from, to resource.Ref }

// movesIn is each volume's data change c moves, as its plan says.
func movesIn(c store.Change) []volumeMove {
	if len(c.Plan) == 0 {
		return nil
	}
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	var out []volumeMove
	for i := range p.Resources {
		if r := &p.Resources[i]; r.MovesFrom != nil {
			out = append(out, volumeMove{from: *r.MovesFrom, to: r.Ref})
		}
	}
	return out
}

// deletesBeforeMoves puts what the change deletes that mounts a volume whose data
// moves, and what it deletes that uses that in turn, as a route uses its service,
// before the first step that moves data on the server: nothing may mount it as its
// data moves. Those keep their order. Deleting a volume whose data moves destroys
// nothing: it's empty by then.
func (e *Engine) deletesBeforeMoves(steps []resource.Step, its []*item) []resource.Step {
	current := make(map[resource.Ref]proto.Message, len(its))
	sources := map[resource.Ref]bool{}
	for _, it := range its {
		current[it.ref] = it.current
		if it.moveFrom.Kind == volumeKind {
			sources[it.moveFrom] = true
		}
	}
	for i := range steps {
		if steps[i].Action == resource.ActionDelete && sources[steps[i].Ref] {
			steps[i].Destroys = false
		}
	}
	first := slices.IndexFunc(steps, func(s resource.Step) bool { return s.VolumeMove != nil })
	if first < 0 {
		return steps
	}
	early := map[resource.Ref]bool{}
	for grew := true; grew; {
		grew = false
		for i := range steps {
			s := &steps[i]
			if s.Action != resource.ActionDelete || early[s.Ref] {
				continue
			}
			for _, use := range e.kinds.References(s.Ref.Kind, current[s.Ref]) {
				if r := (resource.Ref{Kind: use.Kind, Scope: s.Ref.Scope, Name: use.Name}); sources[r] || early[r] {
					early[s.Ref], grew = true, true
					break
				}
			}
		}
	}
	if len(early) == 0 {
		return steps
	}
	out := make([]resource.Step, 0, len(steps))
	var moved []resource.Step
	for i := range steps {
		s := steps[i]
		if i > first && s.Action == resource.ActionDelete && early[s.Ref] {
			moved = append(moved, s)
			continue
		}
		out = append(out, s)
	}
	return slices.Insert(out, first, moved...)
}

// movesVolumes reports an agent that makes a volume with another's data, as it says
// as it connects.
func movesVolumes(agent resource.Agent) bool {
	a, ok := agent.(interface{ VolumeMoves() bool })
	return ok && a.VolumeMoves()
}
