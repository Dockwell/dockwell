// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// served is a spec as it serves a Key: the resource's name in its scope, the item
// that makes it so in this change, if one does, and where it runs.
type served struct {
	name    string
	spec    proto.Message
	by      *item
	servers []string
}

// touched is a Key a change may serve another way, and the item that touches it.
type touched struct {
	key, group string
	by         *item
}

// openings adds a person's approval to each item that lets in someone what served
// one of its Keys kept out, now or within the undo window (resource.Opener). What
// serves a Key is what has the longest Key covering it, so a change opens what it
// serves itself, and what it leaves to a route above: deleting a private route to
// a path under a public domain opens the path, as adding a public route to a path
// under a private domain does. With nothing serving a Key now, what served it within
// the undo window counts, so deleting a private route in one change and making a
// public one in the next opens it as an edit does. With something serving it, only
// that counts: what a person opened stays a token's to edit (ADR 0087).
func (e *Engine) openings(ctx context.Context, tx *store.Tx, kind resource.Kind, past []store.Superseded, items []item, out map[resource.Ref][]resource.Access) error {
	t, err := e.kinds.Type(kind)
	if err != nil {
		return err
	}
	opener, ok := t.(resource.Opener)
	if !ok {
		return nil
	}
	var history []proto.Message
	for _, p := range past {
		spec, err := e.decode(ctx, tx, store.InRevision(p.ResourceID, string(kind), p.Revision), p.Spec)
		if err != nil {
			return err
		}
		history = append(history, spec)
	}
	// What this change deletes or moves in another scope served what it claimed too.
	for i := range items {
		if items[i].ref.Kind == kind && items[i].current != nil {
			history = append(history, items[i].current)
		}
	}
	scopes := map[string][]*item{}
	for i := range items {
		if items[i].ref.Kind == kind {
			scopes[items[i].ref.Scope] = append(scopes[items[i].ref.Scope], &items[i])
		}
	}
	for scope, its := range scopes {
		if err := e.openingsIn(ctx, tx, kind, scope, its, opener, history, out); err != nil {
			return err
		}
	}
	return nil
}

// openingsIn is openings for one scope's items.
func (e *Engine) openingsIn(ctx context.Context, tx *store.Tx, kind resource.Kind, scope string, its []*item, opener resource.Opener, history []proto.Message, out map[resource.Ref][]resource.Access) error {
	stored, err := tx.ListResources(ctx, scope, string(kind))
	if err != nil {
		return err
	}
	var before []served
	for _, r := range stored {
		spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		placed, err := tx.Placement(ctx, r.ID, r.Revision)
		if err != nil {
			return err
		}
		before = append(before, served{name: r.Name, spec: spec, servers: placed})
	}
	after := slices.DeleteFunc(slices.Clone(before), func(s served) bool {
		return slices.ContainsFunc(its, func(it *item) bool { return it.ref.Name == s.name })
	})
	for _, it := range its {
		if it.spec != nil {
			after = append(after, served{name: it.ref.Name, spec: it.spec, by: it, servers: it.servers})
		}
	}
	if err := e.togetherIn(ctx, kind, after, opener); err != nil {
		return err
	}
	keysOf := func(spec proto.Message) []resource.Claim { return e.kinds.Claims(kind, spec) }
	var touches []touched
	for _, it := range its {
		for _, c := range keysOf(it.current) {
			touches = append(touches, touched{c.Key, c.Group, it})
		}
		for _, c := range keysOf(it.spec) {
			touches = append(touches, touched{c.Key, c.Group, it})
			// What it may serve now that nothing under it does any more.
			for _, s := range slices.Concat(specsOf(before), history) {
				for _, o := range keysOf(s) {
					if o.Group == c.Group && under(o.Key, c.Key) {
						touches = append(touches, touched{o.Key, o.Group, it})
					}
				}
			}
		}
	}
	said := map[resource.Ref]map[string]bool{}
	for _, tc := range touches {
		then, ok := servesKey(e, kind, after, tc.key, tc.group)
		if !ok {
			continue // nothing serves it, which keeps everyone out
		}
		var words []string
		if now, ok := servesKey(e, kind, before, tc.key, tc.group); ok {
			words = opener.Opens(now.spec, then.spec)
		} else {
			for _, h := range history {
				if slices.ContainsFunc(keysOf(h), func(c resource.Claim) bool { return c.Group == tc.group && covers(c.Key, tc.key) }) {
					words = appendNew(words, opener.Opens(h, then.spec)...)
				}
			}
		}
		if len(words) == 0 {
			continue
		}
		by := tc.by
		if then.by != nil {
			by = then.by
		}
		if said[by.ref] == nil {
			said[by.ref] = map[string]bool{}
		}
		if said[by.ref][tc.key] {
			continue
		}
		said[by.ref][tc.key] = true
		out[by.ref] = append(out[by.ref], resource.Access{Kind: resource.AccessOpened, Path: tc.key, Detail: strings.Join(words, " ")})
	}
	return nil
}

// togetherIn refuses a change that would leave a Group that keeps anyone out served
// from more than one place. What serves a Key is worked out from specs, wherever
// they run, so a private path on one server and its public parent on another would
// leave the path to the parent there: moving the path alone would open it on the
// server it left, and placing it apart from the start leaves it open there already.
// A Group's resources go together, then, once any of them keeps anyone out.
func (e *Engine) togetherIn(ctx context.Context, kind resource.Kind, after []served, opener resource.Opener) error {
	groups := map[string][]served{}
	for _, s := range after {
		for _, c := range e.kinds.Claims(kind, s.spec) {
			groups[c.Group] = append(groups[c.Group], s)
		}
	}
	for _, group := range slices.Sorted(maps.Keys(groups)) {
		members := groups[group]
		if !slices.ContainsFunc(members, func(s served) bool { return opener.KeepsOut(s.spec) }) {
			continue
		}
		var first *served
		for i := range members {
			s := &members[i]
			if len(s.servers) == 0 {
				continue // nowhere yet, as a resource that never ran
			}
			if first == nil {
				first = s
				continue
			}
			if !slices.Equal(slices.Sorted(slices.Values(s.servers)), slices.Sorted(slices.Values(first.servers))) {
				names, err := e.serverNames(ctx, slices.Concat(first.servers, s.servers))
				if err != nil {
					return err
				}
				on := func(ids []string) string {
					list := make([]string, len(ids))
					for i, id := range ids {
						list[i] = names[id]
					}
					return human.List(list)
				}
				return fault.New(ErrInvalid, "%s keeps strangers out of some of its routes, so all its routes run on one server: a path left on a server without its own route falls to the route above it there. This would run %s on %s, and %s on %s. Place them on one server, together",
					group, first.name, on(first.servers), s.name, on(s.servers))
			}
		}
	}
	return nil
}

// servesKey is what serves key among specs: the one whose Key in group covers it
// and is the longest.
func servesKey(e *Engine, kind resource.Kind, specs []served, key, group string) (served, bool) {
	var best served
	longest := -1
	for _, s := range specs {
		for _, c := range e.kinds.Claims(kind, s.spec) {
			if c.Group == group && covers(c.Key, key) && len(c.Key) > longest {
				best, longest = s, len(c.Key)
			}
		}
	}
	return best, longest >= 0
}

// covers reports a Key that is key, or that key is under.
func covers(cover, key string) bool { return key == cover || under(key, cover) }

// under reports a Key under another, after a "/", as a path is under its domain.
func under(key, cover string) bool { return strings.HasPrefix(key, cover+"/") }

func specsOf(s []served) []proto.Message {
	out := make([]proto.Message, len(s))
	for i := range s {
		out[i] = s[i].spec
	}
	return out
}

// appendNew appends what list hasn't got.
func appendNew(list []string, words ...string) []string {
	for _, w := range words {
		if !slices.Contains(list, w) {
			list = append(list, w)
		}
	}
	return list
}
