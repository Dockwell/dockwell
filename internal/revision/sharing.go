// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// A connection serves one link unless an install admin marks it shared, when each
// app that keeps to a prefix of the bucket may keep its files there beside the
// others' (ADR 0099). Marking it, or unmarking it, is a change with a plan that says
// what it allows.

// sharing says whether a connection's change marks it shared, or unmarks it: what
// it was and is. A new connection was unshared; one deleted is no longer shared,
// which its links' own check refuses first.
func sharing(it *item) (was, is, changes bool) {
	if it.ref.Kind != variable.ConnectionKind {
		return false, false, false
	}
	before, _ := it.current.(*resourcev1.ConnectionSpec)
	after, _ := it.spec.(*resourcev1.ConnectionSpec)
	if after == nil {
		return false, false, false
	}
	return before.GetShared(), after.GetShared(), before.GetShared() != after.GetShared()
}

// sharingAdmins is why marking a connection shared, or unmarking it, is an install
// admin's, said in the refusal of anyone else: it lets apps of every project it
// reaches keep their files beside each other's.
const sharingAdmins = "sharing a connection between apps is an admin's"

// checkUnsharing refuses unmarking a connection that serves more than one app once
// the change applies: the apps would go on keeping their files side by side in a
// bucket that says it's one app's. Disconnecting all but one, first or in the same
// change, lets it go.
func (e *Engine) checkUnsharing(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if was, is, changes := sharing(it); !changes || !was || is {
		return nil
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	// An app is a service: one app's two needs on the connection are one app (the
	// review's L1, the re-check's N2).
	apps, err := e.appsOn(ctx, tx, items, links, it.ref.Name, resource.Ref{})
	if err != nil {
		return err
	}
	n := countApps(apps)
	if n < 2 {
		return nil
	}
	return fault.New(ErrInvalid, "the connection %s serves %d apps, which keep their files side by side in its bucket, so it stays shared while more than one does. Disconnect all but one on its page first, or keep it shared",
		it.ref.Name, n)
}

// ConnectionSharedNote is a plan note's reason where a change marks a connection
// shared, and ConnectionUnsharedNote where it unmarks one (ADR 0099).
const (
	ConnectionSharedNote   = "connection_shared"
	ConnectionUnsharedNote = "connection_unshared"
)

// sharingNotes say what marking each connection shared allows, or unmarking it
// stops, by its label and the projects whose apps keep their files there now. A
// note is about the connection, so a reader who sees it may see the connection,
// which names its links' projects already.
func (e *Engine) sharingNotes(ctx context.Context, items []item) ([]Note, error) {
	var on []*item
	for i := range items {
		if _, _, changes := sharing(&items[i]); changes {
			on = append(on, &items[i])
		}
	}
	if len(on) == 0 {
		return nil, nil
	}
	var out []Note
	err := e.db.View(ctx, func(tx *store.Tx) error {
		links, err := e.allLinks(ctx, tx, items)
		if err != nil {
			return err
		}
		for _, it := range on {
			_, is, _ := sharing(it)
			spec, _ := it.spec.(*resourcev1.ConnectionSpec)
			label := cmp.Or(spec.GetDisplayName(), it.ref.Name)
			if !is {
				out = append(out, Note{
					Reason: ConnectionUnsharedNote, Ref: it.ref,
					Message: "Only one app may keep its files on " + label + " from now on, so no other app is offered it",
				})
				continue
			}
			// An app whose link names no prefix it keeps to may write anywhere in the
			// bucket, which no other app can keep apart from: the note says so, not that
			// others may join (the review's H1). It says only what the link says, as one
			// from before may keep to its prefix without naming it (the re-check's N1).
			var projects, atRoot []string
			for i := range links {
				if links[i].GetConnection() != it.ref.Name {
					continue
				}
				p := projectName(ctx, tx, items, links[i].ref.Scope)
				if p != "" && !slices.Contains(projects, p) {
					projects = append(projects, p)
				}
				if p != "" && link.Kept(links[i].Link) == "" && !slices.Contains(atRoot, p) {
					atRoot = append(atRoot, p)
				}
			}
			owned := func(names []string) string {
				out := make([]string, len(names))
				for i, p := range names {
					out[i] = p + "'s"
				}
				return human.List(out)
			}
			message := "Apps may keep their files on " + label + " side by side, each under its own prefix"
			switch {
			case len(atRoot) > 0:
				says := "link doesn't say which prefix its app keeps to"
				if len(atRoot) > 1 {
					says = "links don't say which prefix their apps keep to"
				}
				message = "No other app can keep its files on " + label + " yet, as " + owned(atRoot) + " " + says
			case len(projects) > 0:
				message = "Other apps may keep their files on " + label + " beside " + owned(projects) + ", each under its own prefix"
			}
			out = append(out, Note{Reason: ConnectionSharedNote, Ref: it.ref, Message: message})
		}
		return nil
	})
	return out, err
}

// projectName is the name of the project an environment's item is in, as the change
// leaves it, or "" where it can't be read.
func projectName(ctx context.Context, tx *store.Tx, items []item, env string) string {
	id := projectOfScope(ctx, tx, items, env)
	for i := range items {
		if items[i].id == id && items[i].ref.Kind == scope.ProjectKind {
			return items[i].ref.Name
		}
	}
	if r, err := tx.GetResourceByID(ctx, id); err == nil {
		return r.Name
	}
	return ""
}

// appsOf is who a link serves, as the change leaves it: each service of its
// environment that reads a name the link sets, as an app is a service (ADR 0084), or,
// where none reads one yet, its environment, as one install's links before its app
// runs are one app (the re-check's N2).
func (e *Engine) appsOf(ctx context.Context, tx *store.Tx, items []item, l *aLink) ([]string, error) {
	names, err := liveIn(ctx, tx, items, l.ref.Scope, "service")
	if err != nil {
		return nil, err
	}
	sets := map[string]bool{}
	for _, s := range l.GetSets() {
		sets[s.GetName()] = true
	}
	var out []string
	for _, name := range names {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: "service", Scope: l.ref.Scope, Name: name})
		if err != nil {
			return nil, err
		}
		svc, ok := spec.(*resourcev1.Service)
		if !ok {
			continue
		}
		reads := false
		for _, from := range svc.GetVariables() {
			reads = reads || sets[from]
		}
		for _, from := range svc.GetSecrets() {
			reads = reads || sets[from]
		}
		if reads {
			out = append(out, l.ref.Scope+"/"+name)
		}
	}
	if len(out) == 0 {
		out = []string{l.ref.Scope}
	}
	return out, nil
}

// appsOn is every app the links to a connection serve once the change applies.
func (e *Engine) appsOn(ctx context.Context, tx *store.Tx, items []item, links []aLink, connection string, skip resource.Ref) (map[string]bool, error) {
	apps := map[string]bool{}
	for i := range links {
		if links[i].GetConnection() != connection || links[i].ref == skip {
			continue
		}
		served, err := e.appsOf(ctx, tx, items, &links[i])
		if err != nil {
			return nil, err
		}
		for _, a := range served {
			apps[a] = true
		}
	}
	return apps, nil
}

// sameApp says two of appsOf's keys may be one app: the same service, or a link's
// environment, where no service reads it yet, and any service of that environment.
func sameApp(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// countApps counts the apps among appsOf's keys: each service, and each environment
// of links no service reads yet where no service there reads one.
func countApps(apps map[string]bool) int {
	n := 0
	for a := range apps {
		if strings.Contains(a, "/") || !slices.ContainsFunc(slices.Collect(maps.Keys(apps)), func(b string) bool { return strings.HasPrefix(b, a+"/") }) {
			n++
		}
	}
	return n
}
