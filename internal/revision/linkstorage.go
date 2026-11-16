// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// LinkStorage is where a link to a connection keeps an app's files: the connection,
// by name and by what the panel calls it, and the prefix in its bucket (ADR 0099).
type LinkStorage struct {
	Connection, Label, Prefix string
}

// ConnectionRead is what a variable or a secret reads from a connection: the part,
// through which link, from the connection by name and by what the panel calls it.
type ConnectionRead struct {
	Connection, Label, Link string
	Part                    resourcev1.LinkPart
}

// LinkSetWords is one of a link's sets, whole: the name, the part it reads, and a
// URL's template and words for TLS.
type LinkSetWords struct {
	Name                    string
	Part                    resourcev1.LinkPart
	Template, TLSOn, TLSOff string `json:",omitempty"`
}

// NeedStorage is where a catalog install's need comes from: a connection, with the
// prefix in its bucket, or the app's own values.
type NeedStorage struct {
	Need    string
	Storage *LinkStorage `json:",omitempty"`
	Own     bool         `json:",omitempty"`
}

// connectionWords are what a plan says of an item that keeps files on a connection
// or reads from one, before the change and after it, by side: 0 before, 1 after.
type connectionWords struct {
	storage [2]*LinkStorage
	reads   [2]*ConnectionRead
	sets    [2][]LinkSetWords
	needs   [2][]NeedStorage
}

// empty says the words say nothing.
func (w *connectionWords) empty() bool {
	return w.storage == [2]*LinkStorage{} && w.reads == [2]*ConnectionRead{} && w.sets[0] == nil && w.sets[1] == nil &&
		w.needs[0] == nil && w.needs[1] == nil
}

// setWords are a link's sets as words, or nil for none.
func setWords(l *resourcev1.Link) []LinkSetWords {
	var out []LinkSetWords
	for _, s := range l.GetSets() {
		out = append(out, LinkSetWords{Name: s.GetName(), Part: s.GetPart(), Template: s.GetTemplate(), TLSOn: s.GetTlsOn(), TLSOff: s.GetTlsOff()})
	}
	return out
}

// linkStorages are, for each link to a connection the change plans, by its item's
// ID, where it keeps the app's files before the change and after it, and for each
// variable or secret read from a connection, what it reads. The labels are
// the connections' as the plan is made, as after the change for the after side, so
// a plan reads as it did when it's read again. A link's row is narrowed with the
// link, and the connection reaches the link's project, so a reader who sees the row
// may list the connection.
// linkOf is spec as a link, or nil.
func linkOf(spec any) *resourcev1.Link {
	l, _ := spec.(*resourcev1.Link)
	return l
}

func (e *Engine) linkStorages(ctx context.Context, items []item) (map[string]connectionWords, error) {
	out := map[string]connectionWords{}
	if !hasLinks(items) && !readsConnections(items) {
		return out, nil
	}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		label := func(name string, after bool) (string, error) {
			ref := resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: name}
			var spec any
			if after {
				s, err := e.specAfter(ctx, tx, items, ref)
				if err != nil {
					return "", err
				}
				spec = s
			} else {
				it, err := e.load(ctx, tx, ref)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return "", err
				}
				spec = it.current
			}
			c, _ := spec.(*resourcev1.ConnectionSpec)
			return cmp.Or(c.GetDisplayName(), name), nil
		}
		for i := range items {
			it := &items[i]
			var w connectionWords
			for side, spec := range [2]any{it.current, it.spec} {
				if l, ok := spec.(*resourcev1.Link); ok && l.GetConnection() != "" {
					named, err := label(l.GetConnection(), side == 1)
					if err != nil {
						return err
					}
					w.storage[side] = &LinkStorage{Connection: l.GetConnection(), Label: named, Prefix: l.GetPrefix()}
				}
				if m, ok := spec.(proto.Message); ok && (it.ref.Kind == variable.VariableKind || it.ref.Kind == variable.SecretKind) {
					if c := variable.FromOf(m).GetConnection(); c != nil {
						named, err := label(c.GetName(), side == 1)
						if err != nil {
							return err
						}
						w.reads[side] = &ConnectionRead{Connection: c.GetName(), Label: named, Link: c.GetLink(), Part: c.GetPart()}
					}
				}
			}
			// The same connection on both sides reads by one label, the one it has as
			// the plan is made: a renamed label is the connection's change alone, and
			// moves nothing (the slice 3 review, H1).
			if a, b := w.storage[1], w.storage[0]; a != nil && b != nil && a.Connection == b.Connection {
				b.Label = a.Label
			}
			if a, b := w.reads[1], w.reads[0]; a != nil && b != nil && a.Connection == b.Connection {
				b.Label = a.Label
			}
			// A link's sets go whole where they change, so a row can say a set's name and
			// part though only one of them changed (the slice 3 review, M1).
			was, now := linkOf(it.current), linkOf(it.spec)
			if (was != nil || now != nil) && !slices.EqualFunc(was.GetSets(), now.GetSets(), func(a, b *resourcev1.LinkSet) bool { return proto.Equal(a, b) }) {
				w.sets = [2][]LinkSetWords{setWords(was), setWords(now)}
			}
			// A catalog install's needs on connections, or on their own values, for the
			// project's row, where they change (the slice 3 review, L2).
			if it.ref.Kind == scope.ProjectKind {
				var sides [2][]NeedStorage
				for side, spec := range [2]any{it.current, it.spec} {
					p, _ := spec.(*resourcev1.Project)
					needs := p.GetCatalog().GetNeeds()
					for _, name := range slices.Sorted(maps.Keys(needs)) {
						n := needs[name]
						switch {
						case n.GetConnection() != "":
							named, err := label(n.GetConnection(), side == 1)
							if err != nil {
								return err
							}
							sides[side] = append(sides[side], NeedStorage{Need: name, Storage: &LinkStorage{Connection: n.GetConnection(), Label: named, Prefix: n.GetPrefix()}})
						case n.GetOwn():
							sides[side] = append(sides[side], NeedStorage{Need: name, Own: true})
						}
					}
				}
				// The same connection on both sides reads by one label, as for a link.
				for i := range sides[0] {
					for j := range sides[1] {
						a, b := sides[1][j].Storage, sides[0][i].Storage
						if a != nil && b != nil && sides[0][i].Need == sides[1][j].Need && a.Connection == b.Connection {
							b.Label = a.Label
						}
					}
				}
				if !slices.EqualFunc(sides[0], sides[1], sameNeedStorage) {
					w.needs = sides
				}
			}
			if !w.empty() {
				out[it.id] = w
			}
		}
		return nil
	})
	return out, err
}

// readsConnections says the change plans a value read from a connection.
func readsConnections(items []item) bool {
	for i := range items {
		for _, spec := range [2]any{items[i].current, items[i].spec} {
			if m, ok := spec.(proto.Message); ok && variable.FromOf(m).GetConnection() != nil {
				return true
			}
		}
	}
	return false
}

// hasLinks says the change plans a link, which only then reads connections.
func hasLinks(items []item) bool {
	for i := range items {
		if items[i].ref.Kind == linkKind {
			return true
		}
	}
	return false
}

// sameNeedStorage says two needs come from the same place: the same need, on the same
// connection by name and prefix, or both on their own values, whatever the label.
func sameNeedStorage(a, b NeedStorage) bool {
	if a.Need != b.Need || a.Own != b.Own || (a.Storage == nil) != (b.Storage == nil) {
		return false
	}
	return a.Storage == nil || a.Storage.Connection == b.Storage.Connection && a.Storage.Prefix == b.Storage.Prefix
}
