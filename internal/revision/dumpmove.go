// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// dumpMove is a database a proposal moves to a newer release by a dump into a new
// volume (ADR 0080): the service, the volume its data is on, and what its step does.
type dumpMove struct {
	service, volume resource.Ref
	asked           *resource.MoveAsked
}

// maxVolumeName is the longest name a volume takes (volume.Driver.CheckName).
const maxVolumeName = 63

// dumpMoves finds the databases a proposal moves up a release series, PostgreSQL's,
// MySQL's and MariaDB's, and rewrites its edits for each (ADR 0080): the service's
// data mount names a new volume, which an edit makes beside the one it had, on the
// same server, where the new release has its data. The old volume stays as it was,
// so undoing the change puts the old release back on it. What can't move is
// refused, with why. Each move's note replaces what the proposal said of backing
// its volume up: the dump is that backup, and undo puts the data back too.
func (e *Engine) dumpMoves(ctx context.Context, p *Proposal, items []item) ([]dumpMove, error) {
	var out []dumpMove
	for i := range items {
		it := &items[i]
		current, ok := it.current.(*resourcev1.Service)
		if !ok {
			continue
		}
		desired, ok := it.spec.(*resourcev1.Service)
		if !ok {
			continue
		}
		move, err := database.MoveOf(it.ref.Name, current, desired)
		if err != nil || move == nil || !move.Dumps || len(it.placed) == 0 {
			// databaseMoves refused what can't move, and what never ran has no data to.
			continue
		}
		d, err := e.dumpMove(ctx, p, it, move)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
		p.Notes = slices.DeleteFunc(p.Notes, func(n Note) bool {
			return n.Ref == it.ref && (n.Reason == "backup_first" || n.Reason == "no_backup")
		})
		p.Notes = append(p.Notes, Note{
			Reason: "database_move", Ref: it.ref, Names: []string{d.asked.To, d.asked.From}, Named: true, IfItChanges: true,
			Message: fmt.Sprintf("%s moves to %s %s by a dump: Dockwell stops what else of its app runs on its server, dumps it, loads the dump into %s %s on a new volume, %s, and checks it starts there before it replaces %s. "+
				"%s stays as it was, so undoing this puts %s %s back on it, without what was written since", it.ref.Name, move.Engine.Name, move.To, move.Engine.Name, move.To, d.asked.To, it.ref.Name,
				d.asked.From, move.Engine.Name, move.From),
		})
	}
	return out, nil
}

// dumpMove rewrites a proposal's edits for one database's move, or says why it
// can't move.
func (e *Engine) dumpMove(ctx context.Context, p *Proposal, it *item, move *database.Move) (dumpMove, error) {
	moves := fmt.Sprintf("%s %s moves the data of %s, which runs %s %s, by a dump into a new volume", move.Engine.Name, move.To, it.ref.Name, move.Engine.Name, move.From)
	switch {
	case len(it.placed) > 1:
		return dumpMove{}, fault.New(ErrInvalid, "%s on one server, and %s runs on %d, so its image stays as it is. Keep it on %s", moves, it.ref.Name, len(it.placed), move.From)
	case !slices.Equal(it.placed, it.servers):
		return dumpMove{}, fault.New(ErrInvalid, "%s on the server it runs on, and this change moves %s to another too, so its image stays as it is. Move it to the other server in a change of its own first", moves, it.ref.Name)
	}
	from := resource.Ref{Kind: volumeKind, Scope: it.ref.Scope, Name: move.Volume}
	var vol item
	to := ""
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if vol, err = e.load(ctx, tx, from); err != nil {
			return err
		}
		to, err = e.freeVolume(ctx, tx, p.Edits, it.ref.Scope, move.Volume+"-"+move.To)
		return err
	}); err != nil {
		return dumpMove{}, err
	}
	spec, _ := vol.current.(*resourcev1.Volume)
	switch {
	case spec == nil:
		return dumpMove{}, fault.New(ErrInvalid, "%s, and %s isn't a volume of its environment, so its image stays as it is. Keep it on %s", moves, move.Volume, move.From)
	case spec.GetDriver() != "" && spec.GetDriver() != "local" || len(spec.GetOptions()) > 0:
		return dumpMove{}, fault.New(ErrInvalid, "%s, and %s mounts what its driver or options say, not a volume of its server, so Dockwell can't make one like it to move into, and its image stays as it is. Keep it on %s",
			moves, move.Volume, move.From)
	case to == "":
		return dumpMove{}, fault.New(ErrInvalid, "%s, and every name Dockwell would give that volume is taken, so its image stays as it is. Rename %s, then try again", moves, move.Volume)
	}
	j := slices.IndexFunc(p.Edits, func(ed Edit) bool { return ed.Ref == it.ref })
	if j < 0 {
		return dumpMove{}, fmt.Errorf("%s has no edit to move its data in", it.ref.Name)
	}
	svc, ok := proto.Clone(p.Edits[j].Spec).(*resourcev1.Service)
	if !ok {
		return dumpMove{}, fmt.Errorf("%s's edit isn't a service", it.ref.Name)
	}
	found := 0
	for _, m := range svc.GetApp().GetMounts() {
		if m.GetVolume() == move.Volume && !m.GetExternal() {
			m.Source, m.Target = &resourcev1.Mount_Volume{Volume: to}, move.Target
			found++
		}
	}
	if found != 1 {
		return dumpMove{}, fault.New(ErrInvalid, "%s, and %s mounts %s %d times, so Dockwell can't tell which to move, and its image stays as it is. Mount it once, then try again", moves, it.ref.Name, move.Volume, found)
	}
	p.Edits[j].Spec = svc
	p.Edits = append(p.Edits, Edit{
		Ref:  resource.Ref{Kind: volumeKind, Scope: it.ref.Scope, Name: to},
		Spec: &resourcev1.Volume{Labels: maps.Clone(spec.GetLabels())}, Servers: slices.Clone(it.placed),
	})
	return dumpMove{
		service: it.ref, volume: from,
		asked: &resource.MoveAsked{Server: it.placed[0], Backup: id.New(id.Backup), From: move.Volume, FromID: vol.id, To: to},
	}, nil
}

// freeVolume is the first of name, name-2, name-3 and so on that no volume of the
// scope has ever had, nor an edit makes, cut to a length a volume takes: a name a
// deleted volume had would bring back what the server keeps of it. Empty when
// there's none.
func (e *Engine) freeVolume(ctx context.Context, tx *store.Tx, edits []Edit, scope, name string) (string, error) {
	for n := 1; n <= 99; n++ {
		suffix := ""
		if n > 1 {
			suffix = "-" + strconv.Itoa(n)
		}
		try := name
		if len(try)+len(suffix) > maxVolumeName {
			try = try[:maxVolumeName-len(suffix)]
		}
		try += suffix
		ref := resource.Ref{Kind: volumeKind, Scope: scope, Name: try}
		if slices.ContainsFunc(edits, func(ed Edit) bool { return ed.Ref == ref }) {
			continue
		}
		had, err := e.load(ctx, tx, ref)
		if err != nil {
			return "", err
		}
		if had.base == 0 {
			return try, nil
		}
	}
	return "", nil
}
