// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
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
	var others []string
	var policies []Edit
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if vol, err = e.load(ctx, tx, from); err != nil {
			return err
		}
		if to, err = e.freeVolume(ctx, tx, p.Edits, it.ref.Scope, move.Volume+"-"+move.To); err != nil {
			return err
		}
		if others, err = e.otherMounts(ctx, tx, p.Edits, it.ref, move.Volume); err != nil {
			return err
		}
		policies, err = e.policiesOf(ctx, tx, p.Edits, it.ref.Scope, move.Volume)
		return err
	}); err != nil {
		return dumpMove{}, err
	}
	// Another service on the same data would go on with the old volume, which
	// nothing writes to any more, or be stopped and started again on it.
	if len(others) > 0 {
		return dumpMove{}, fault.New(ErrInvalid, "%s, and %s mount%s %s too, which would go on with the old data, so its image stays as it is. "+
			"Stop mounting %s in %s first", moves, human.List(others), map[bool]string{true: "s", false: ""}[len(others) == 1], move.Volume, move.Volume, human.List(others))
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
	// Its backups follow its data: each schedule of the old volume backs up the new
	// one, in the same change, so undo puts them back too.
	var scheduled []string
	for i := range policies {
		ed := &policies[i]
		ed.Spec.(*resourcev1.BackupPolicy).Volume = to
		if k := slices.IndexFunc(p.Edits, func(x Edit) bool { return x.Ref == ed.Ref }); k >= 0 {
			p.Edits[k].Spec = ed.Spec
		} else {
			p.Edits = append(p.Edits, *ed)
		}
		scheduled = append(scheduled, ed.Ref.Name)
	}
	if len(scheduled) > 0 {
		p.Notes = append(p.Notes, Note{
			Reason: "database_move_backups", Ref: it.ref, Names: append([]string{to}, scheduled...), Named: true, IfItChanges: true,
			Message: fmt.Sprintf("The backup schedules of %s, %s, back up %s from now on, as its data moves there. Undoing this puts them back on %s",
				move.Volume, human.List(scheduled), to, move.Volume),
		})
	}
	return dumpMove{
		service: it.ref, volume: from,
		asked: &resource.MoveAsked{
			Server: it.placed[0], Backup: id.New(id.Backup), From: move.Volume, FromID: vol.id, To: to,
			Database: move.Engine.Name, FromSeries: move.From, ToSeries: move.To,
		},
	}, nil
}

// otherMounts are the services of a scope but one that mount a volume, as the
// proposal leaves them.
func (e *Engine) otherMounts(ctx context.Context, tx *store.Tx, edits []Edit, self resource.Ref, volume string) ([]string, error) {
	list, err := tx.ListResources(ctx, self.Scope, string(self.Kind))
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list {
		r := &list[i]
		if r.Name == self.Name {
			continue
		}
		ref := resource.Ref{Kind: self.Kind, Scope: self.Scope, Name: r.Name}
		var spec proto.Message
		if k := slices.IndexFunc(edits, func(ed Edit) bool { return ed.Ref == ref }); k >= 0 {
			spec = edits[k].Spec
		} else if r.Spec != nil {
			if spec, err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
				return nil, err
			}
		}
		svc, _ := spec.(*resourcev1.Service)
		for _, m := range svc.GetApp().GetMounts() {
			if m.GetVolume() == volume && !m.GetExternal() {
				out = append(out, r.Name)
				break
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// policiesOf are edits of each backup schedule of a scope that backs up a volume, as
// the proposal leaves it, to repoint.
func (e *Engine) policiesOf(ctx context.Context, tx *store.Tx, edits []Edit, scope, volume string) ([]Edit, error) {
	list, err := tx.ListResources(ctx, scope, string(policyKind))
	if err != nil {
		return nil, err
	}
	var out []Edit
	for i := range list {
		r := &list[i]
		ref := resource.Ref{Kind: policyKind, Scope: scope, Name: r.Name}
		var spec proto.Message
		if k := slices.IndexFunc(edits, func(ed Edit) bool { return ed.Ref == ref }); k >= 0 {
			spec = edits[k].Spec
		} else if r.Spec != nil {
			if spec, err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
				return nil, err
			}
		}
		policy, ok := spec.(*resourcev1.BackupPolicy)
		if !ok || policy.GetVolume() != volume {
			continue
		}
		out = append(out, Edit{Ref: ref, Spec: proto.Clone(policy)})
	}
	return out, nil
}

// policyKind is the backup schedules' kind (backuppolicy.Kind, which this package
// doesn't import).
const policyKind resource.Kind = "backup.policy"

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

// MoveRecord is a database move an applied change made, as its plan has it, and
// when it was applied (ADR 0080).
type MoveRecord struct {
	resource.MoveAsked
	At time.Time
}

// moveLookBack is how many of a service's revisions MovesOf reads back.
const moveLookBack = 100

// MovesOf are the moves applied changes made of a service, newest first, from its
// revisions' changes' plans, for what reads a file that names the volume a move
// left, as Compose import does.
func MovesOf(ctx context.Context, tx *store.Tx, serviceID string) ([]MoveRecord, error) {
	revs, err := tx.PageRevisions(ctx, serviceID, 0, moveLookBack)
	if err != nil {
		return nil, err
	}
	var out []MoveRecord
	seen := map[string]bool{}
	for i := range revs {
		changeID, err := tx.RevisionChange(ctx, serviceID, revs[i].Revision)
		if errors.Is(err, store.ErrNotFound) || changeID == "" || seen[changeID] {
			continue
		}
		if err != nil {
			return nil, err
		}
		seen[changeID] = true
		c, err := tx.GetChange(ctx, changeID)
		if err != nil {
			return nil, err
		}
		if c.State != store.ChangeApplied || len(c.Plan) == 0 {
			continue
		}
		var p struct {
			Resources []struct {
				ID   string
				Move *resource.MoveAsked
			}
		}
		if json.Unmarshal(c.Plan, &p) != nil {
			continue
		}
		for _, r := range p.Resources {
			if r.ID == serviceID && r.Move != nil && !r.Move.Finish {
				out = append(out, MoveRecord{MoveAsked: *r.Move, At: c.AppliedAt})
			}
		}
	}
	return out, nil
}

// goingBack is what a proposal does to a database whose data mount goes back to a
// volume an earlier move moved it away from (ADR 0080): that volume holds the data
// from before the move, which a later release would take as it is, as MySQL and
// MariaDB would upgrade the old files in place and PostgreSQL won't start on them,
// so that's refused; the release it held, or an older one, goes back, as undo
// does, which a note says, with what stays behind.
func (e *Engine) goingBack(ctx context.Context, items []item) ([]Note, error) {
	var notes []Note
	for i := range items {
		it := &items[i]
		current, ok := it.current.(*resourcev1.Service)
		if !ok || it.id == "" {
			continue
		}
		desired, ok := it.spec.(*resourcev1.Service)
		if !ok {
			continue
		}
		was, ok := database.FromSpec(current)
		if !ok {
			continue
		}
		now, ok := database.FromSpec(desired)
		if !ok || now.Engine != was.Engine || now.Volume == "" || now.External || now.Volume == was.Volume {
			continue
		}
		var moves []MoveRecord
		if err := e.db.View(ctx, func(tx *store.Tx) error {
			var err error
			moves, err = MovesOf(ctx, tx, it.id)
			return err
		}); err != nil {
			return nil, err
		}
		k := slices.IndexFunc(moves, func(m MoveRecord) bool { return m.From == now.Volume })
		if k < 0 {
			continue
		}
		m := moves[k]
		when := m.At.UTC().Format("2006-01-02 15:04 UTC")
		to := database.Series(now.Engine, desired.GetApp().GetImage())
		if to != "" && m.FromSeries != "" && database.Newer(to, m.FromSeries) {
			return nil, fault.New(ErrInvalid, "%s's data moved from %s to %s when it moved to %s %s on %s, so %s holds %s %s's data from before then, which %s %s would take as it is, and its mount stays as it is. "+
				"Keep %s mounted, or undo that move to go back to %s %s", it.ref.Name, m.From, m.To, now.Engine.Name, m.ToSeries, when, m.From, now.Engine.Name, m.FromSeries,
				now.Engine.Name, to, was.Volume, now.Engine.Name, m.FromSeries)
		}
		stays := "stays there"
		if slices.ContainsFunc(items, func(o item) bool {
			return o.ref.Kind == volumeKind && o.ref.Scope == it.ref.Scope && o.ref.Name == was.Volume && o.spec == nil && o.current != nil
		}) {
			stays = "which this deletes, and its server keeps for 7 days"
		}
		notes = append(notes, Note{
			Reason: "database_back", Ref: it.ref, Names: []string{m.From, was.Volume}, Named: true,
			Message: fmt.Sprintf("%s goes back to %s, which holds its data from before it moved to %s %s on %s. What was written since is in %s, %s",
				it.ref.Name, m.From, now.Engine.Name, m.ToSeries, when, was.Volume, stays),
		})
	}
	return notes, nil
}

// afterMove is what a rollback of a failed change means for a database the change
// had already moved, as its step there ended: it goes back to the old volume, and
// what was written since stays in the new one.
func afterMove(c store.Change, progress *Progress) string {
	if len(c.Plan) == 0 || progress == nil {
		return ""
	}
	var p struct {
		Resources []struct {
			ID   string
			Ref  resource.Ref
			Move *resource.MoveAsked
		}
	}
	if json.Unmarshal(c.Plan, &p) != nil {
		return ""
	}
	var out []string
	for _, r := range p.Resources {
		if r.Move == nil || r.Move.Finish || !progress.ended(r.Move.Server, r.ID) {
			continue
		}
		if i := progress.index(r.Move.Server, r.ID); i < 0 || progress.Steps[i].State != StepDone {
			continue
		}
		out = append(out, fmt.Sprintf("%s had moved to %s %s on %s, so the rollback puts %s %s back on %s, and what was written since stays in %s, which it deletes, and its server keeps for 7 days",
			r.Ref.Name, r.Move.Database, r.Move.ToSeries, r.Move.To, r.Move.Database, r.Move.FromSeries, r.Move.From, r.Move.To))
	}
	return strings.Join(out, ". ")
}
