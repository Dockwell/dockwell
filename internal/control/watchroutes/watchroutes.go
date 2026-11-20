// SPDX-License-Identifier: AGPL-3.0-only

// Package watchroutes watches each routed domain unless someone says not to (#835):
// a change that adds a route serving a whole domain gains an uptime check of it,
// marked as that route's, and removing the route, or saying not to watch it, removes
// that check in the same change. A check a person made or changed is never touched.
// See docs/adr/0066-checks-of-a-url.md, "Checks Dockwell makes for routes".
package watchroutes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// NoteCap is the reason of the note a plan has when a route's domain isn't watched
// as the install keeps the most checks it may.
const NoteCap = "route_check_cap"

// NoteWatches is the reason of the note a plan has for each check it makes for a route.
const NoteWatches = "route_check"

// NoteUnreadable is the reason of the note a plan that writes a route has while a
// check can't be read: Dockwell leaves that check as it is, and counts it. The note
// names no check, as whoever plans a route may not read checks; the server log
// names each, with why (the re-check's N6).
const NoteUnreadable = "route_check_unreadable"

// Decoder reads a stored spec, opening a sealed one.
type Decoder interface {
	Decode(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) (proto.Message, error)
}

// Watcher makes and removes the checks of routes.
type Watcher struct {
	Keeper Decoder
	// Logger says which checks can't be read, and why. Nil logs nothing.
	Logger *slog.Logger
}

// check is a check as the change will leave it.
type check struct {
	name string
	// spec is the check as the change leaves it, nil where it can't be read or once
	// the change deletes it.
	spec *resourcev1.AlertCheck
	// current is the spec stored now, nil for one the change makes or one that
	// can't be read.
	current *resourcev1.AlertCheck
	// rev is the revision stored now, which Dockwell's own edit of it is made from
	// (revision.Edit.Base), 0 for one the change makes.
	rev int
	// unreadable is why the stored spec can't be read. Such a check is never edited
	// here, but it holds its name and counts toward the most an install keeps.
	unreadable string
	// gone says the change deletes it.
	gone bool
}

// view is what the hook reads once: the install's checks as stored, and as the
// change's edits leave them.
type view struct {
	org    string
	checks map[string]*check
}

func (v *view) live() int {
	n := 0
	for _, c := range v.checks {
		if !c.gone && (c.spec != nil || c.unreadable != "") {
			n++
		}
	}
	return n
}

// watching is a live check of host, whoever made it, if there's one.
func (v *view) watching(host string) *check {
	for _, c := range v.checks {
		if c.spec != nil && hostOf(c.spec.GetAddress()) == host {
			return c
		}
	}
	return nil
}

// madeFor are the live checks made for a route, but for the one named except.
func (v *view) madeFor(routeID, except string) []*check {
	var out []*check
	for _, c := range v.checks {
		if c.spec != nil && routeID != "" && c.spec.GetForRoute() == routeID && c.name != except {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Hook is an edit hook for the engine (revision.Engine.SetEditHook). A check made
// for a route is taken in by the route's change (revision's markMadeForRoute), and
// an undo or a rollback doesn't reverse what was taken in: it takes it in again. So
// the route's rule runs on an undo's and a rollback's routes too, and the rules
// about a person's own write of a check don't.
//
// What it adds is made from what it read, so a check or a route that changes before
// the change is planned makes the plan stale rather than taken over: a check it
// makes must not be there yet (revision.Edit.MustCreate), and one it changes or
// removes, and a route it says not to watch, must be at the revision it read
// (revision.Edit.Base).
func (w *Watcher) Hook(ctx context.Context, tx *store.Tx, edits []revision.Edit) ([]revision.Edit, []revision.Note, error) {
	asks := false
	for i := range edits {
		ed := &edits[i]
		asks = asks || ed.Ref.Kind == route.Kind || ed.Ref.Kind == alert.CheckKind || goesWithContents(ed)
	}
	if !asks {
		return edits, nil, nil
	}
	v, err := w.view(ctx, tx, edits)
	if err != nil {
		return nil, nil, err
	}
	out := make([]revision.Edit, len(edits))
	copy(out, edits)
	var notes []revision.Note

	// A person's write of a check: a change to one keeps no route's mark, so it's
	// theirs; one they make keeps a mark only as that route's one check, of the
	// address Dockwell would make it with.
	for i := range out {
		ed := &out[i]
		c, ok := ed.Spec.(*resourcev1.AlertCheck)
		if ed.Ref.Kind != alert.CheckKind || ed.Reverses || ed.Restores || !ok || c.GetForRoute() == "" {
			continue
		}
		held := v.checks[ed.Ref.Name]
		keep := false
		var turnOn *store.Resource
		switch {
		case held.unreadable != "":
			// What it was can't be read, so nothing says it was the route's.
		case held.current != nil:
			keep = held.current.GetForRoute() == c.GetForRoute() && sameBesidesMark(held.current, c)
		default:
			keep, turnOn, err = w.accepts(ctx, tx, out, v, ed.Ref.Name, c)
			if err != nil {
				return nil, nil, err
			}
		}
		if !keep {
			c = proto.CloneOf(c)
			c.ForRoute = ""
			ed.Spec = c
			held.spec = c
			continue
		}
		// Watching a route that said not to, on purpose, says so in the same plan.
		if turnOn != nil {
			if out, err = w.setUnwatched(ctx, tx, out, *turnOn, false); err != nil {
				return nil, nil, err
			}
		}
	}

	// A person deleting a check Dockwell made for a route says not to watch that
	// route, so the next change of it doesn't make the check again.
	for i := range edits {
		ed := &edits[i]
		if ed.Ref.Kind != alert.CheckKind || ed.Spec != nil || ed.Reverses || ed.Restores {
			continue
		}
		cur := v.checks[ed.Ref.Name].current
		if cur.GetForRoute() == "" {
			continue
		}
		r, err := tx.GetResourceByID(ctx, cur.GetForRoute())
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if r.Kind != string(route.Kind) || r.Spec == nil {
			continue
		}
		if out, err = w.setUnwatched(ctx, tx, out, r, true); err != nil {
			return nil, nil, err
		}
	}

	// A project or an environment deleted with what's in it takes its routes, which
	// the engine adds after this, so their checks go in the same change.
	for i := range edits {
		ed := &edits[i]
		if !goesWithContents(ed) {
			continue
		}
		routes, err := w.routesIn(ctx, tx, ed.Ref)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range routes {
			if slices.ContainsFunc(out, func(o revision.Edit) bool {
				return o.Ref.Kind == route.Kind && o.Ref.Scope == r.Scope && o.Ref.Name == r.Name
			}) {
				continue // its own edit says what it becomes, below
			}
			for _, c := range v.madeFor(r.ID, "") {
				out = appendDelete(out, v, c)
			}
		}
	}

	// Each route the change writes: a check for one that now serves a whole domain
	// and didn't, the check moved to the address it asks now, and none for one that
	// doesn't any more.
	routesWritten := false
	for i := range out {
		ed := &out[i]
		if ed.Ref.Kind != route.Kind || !strings.HasPrefix(ed.Ref.Scope, id.Environment+"_") {
			continue
		}
		routesWritten = true
		stored, current, err := w.route(ctx, tx, ed.Ref)
		if err != nil {
			return nil, nil, err
		}
		routeID := stored.ID
		spec, _ := ed.Spec.(*resourcev1.Route)
		if routeID == "" {
			if spec == nil {
				continue
			}
			// A new route is called what its check names, chosen up front.
			if ed.ID == "" {
				ed.ID = id.New(id.Route)
			}
			routeID = ed.ID
		}
		want, wanted := route.WatchAddress(spec)
		for _, c := range v.madeFor(routeID, "") {
			switch {
			case !wanted:
				out = appendDelete(out, v, c)
			case c.spec.GetAddress() == want:
			case hostOf(c.spec.GetAddress()) == hostOf(want):
				// The same domain, asked another way, as turning TLS off does.
				out = appendMove(out, v, c, want)
			default:
				out = appendDelete(out, v, c)
			}
		}
		if !wanted || len(v.madeFor(routeID, "")) > 0 {
			continue
		}
		// Made only as a route starts serving a whole domain: a new one, another
		// domain, or one that said not to be watched or wasn't a site. A route that
		// served its domain before this change keeps what it had, so routes that
		// existed before checks were made for them stay as they are, offered instead.
		if _, was := route.WatchAddress(current); was && current.GetDomain() == spec.GetDomain() {
			continue
		}
		if v.watching(spec.GetDomain()) != nil {
			continue
		}
		if v.live() >= alert.MaxChecks {
			notes = append(notes, revision.Note{
				Reason:  NoteCap,
				Message: fmt.Sprintf("%s isn't watched: the install keeps %d uptime checks, the most it may. Remove one to watch it", spec.GetDomain(), alert.MaxChecks),
				Ref:     ed.Ref,
				Names:   []string{spec.GetDomain()},
			})
			continue
		}
		name := alert.NameFor(want, func(n string) bool { _, taken := v.checks[n]; return taken })
		c := &resourcev1.AlertCheck{Address: want, ForRoute: routeID}
		v.checks[name] = &check{name: name, spec: c}
		ref := resource.Ref{Kind: alert.CheckKind, Scope: v.org, Name: name}
		out = append(out, revision.Edit{Ref: ref, Spec: c, MustCreate: true, MadeForRoute: true})
		notes = append(notes, revision.Note{
			Reason:  NoteWatches,
			Message: fmt.Sprintf("Watches %s every minute, for its route", spec.GetDomain()),
			Ref:     ref,
			Names:   []string{spec.GetDomain()},
		})
	}

	// A check that can't be read is left as it is, and said, where it might have been
	// a route's: in the plan without its name, and by name in the server log.
	if routesWritten {
		var unreadable []string
		for _, c := range v.checks {
			if c.unreadable != "" && !c.gone {
				unreadable = append(unreadable, c.name)
			}
		}
		sort.Strings(unreadable)
		for _, name := range unreadable {
			if w.Logger != nil {
				w.Logger.WarnContext(ctx, "an uptime check can't be read, so a route's change left it as it is; delete it, or set it again, to have it read",
					slog.String("check", name), slog.String("error", v.checks[name].unreadable))
			}
		}
		if len(unreadable) > 0 {
			notes = append(notes, revision.Note{
				Reason:  NoteUnreadable,
				Message: "A check Dockwell can't read was left as it is. The server log names it, for an admin to delete or set again",
			})
		}
	}
	return out, notes, nil
}

// goesWithContents reports a deletion of a project or an environment with what's in
// it (revision.Edit.WithContents).
func goesWithContents(ed *revision.Edit) bool {
	return ed.Spec == nil && ed.WithContents && (ed.Ref.Kind == scope.ProjectKind || ed.Ref.Kind == scope.EnvironmentKind)
}

// view reads the install's checks, and applies the change's edits of them. One that
// can't be read is kept by name, as unreadable, rather than refusing the change.
func (w *Watcher) view(ctx context.Context, tx *store.Tx, edits []revision.Edit) (*view, error) {
	org, err := tx.Organization(ctx)
	if err != nil {
		return nil, err
	}
	v := &view{org: org.ID, checks: map[string]*check{}}
	list, err := tx.ListKind(ctx, string(alert.CheckKind))
	if err != nil {
		return nil, err
	}
	for i := range list {
		r := &list[i]
		held := &check{name: r.Name, rev: r.Revision}
		v.checks[r.Name] = held
		spec, err := w.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		c, _ := spec.(*resourcev1.AlertCheck)
		if err != nil || c == nil {
			held.unreadable = "it isn't an uptime check as stored"
			if err != nil {
				held.unreadable = err.Error()
			}
			continue
		}
		held.spec, held.current = c, c
	}
	for i := range edits {
		ed := &edits[i]
		if ed.Ref.Kind != alert.CheckKind {
			continue
		}
		c := v.checks[ed.Ref.Name]
		if c == nil {
			c = &check{name: ed.Ref.Name}
			v.checks[ed.Ref.Name] = c
		}
		c.spec, _ = ed.Spec.(*resourcev1.AlertCheck)
		c.gone = ed.Spec == nil
	}
	return v, nil
}

// route reads a route as stored, if it is: its resource, and its spec.
func (w *Watcher) route(ctx context.Context, tx *store.Tx, ref resource.Ref) (store.Resource, *resourcev1.Route, error) {
	r, err := tx.GetResource(ctx, string(route.Kind), ref.Scope, ref.Name)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return store.Resource{ID: r.ID}, nil, nil
	}
	if err != nil {
		return store.Resource{}, nil, err
	}
	spec, err := w.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return store.Resource{}, nil, fmt.Errorf("read the route %s: %w", ref.Name, err)
	}
	rt, _ := spec.(*resourcev1.Route)
	return r, rt, nil
}

// routesIn are the live routes of the project or the environment ref names.
func (w *Watcher) routesIn(ctx context.Context, tx *store.Tx, ref resource.Ref) ([]store.Resource, error) {
	held, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	environments := []string{held.ID}
	if ref.Kind == scope.ProjectKind {
		envs, err := tx.ListResources(ctx, held.ID, string(scope.EnvironmentKind))
		if err != nil {
			return nil, err
		}
		environments = environments[:0]
		for i := range envs {
			environments = append(environments, envs[i].ID)
		}
	}
	var out []store.Resource
	for _, env := range environments {
		routes, err := tx.ListResources(ctx, env, string(route.Kind))
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	return out, nil
}

// accepts reports a check a person makes as the route's own: the one check of a
// route, stored or one the change makes, of exactly the address Dockwell would make
// it with, on the defaults. The route is returned where it says not to be watched,
// as taking the check on purpose turns watching back on.
func (w *Watcher) accepts(ctx context.Context, tx *store.Tx, edits []revision.Edit, v *view, name string, c *resourcev1.AlertCheck) (bool, *store.Resource, error) {
	if _, _, ok := (alert.Check{}).MadeFor(c); !ok || len(v.madeFor(c.GetForRoute(), name)) > 0 {
		return false, nil, nil
	}
	asks := func(rt *resourcev1.Route) bool {
		want, ok := route.Watchable(rt)
		return ok && want == c.GetAddress()
	}
	for i := range edits {
		ed := &edits[i]
		if ed.Ref.Kind == route.Kind && ed.ID == c.GetForRoute() {
			rt, _ := ed.Spec.(*resourcev1.Route)
			return rt != nil && !rt.GetUnwatched() && asks(rt), nil, nil
		}
	}
	r, err := tx.GetResourceByID(ctx, c.GetForRoute())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if r.Kind != string(route.Kind) || r.Spec == nil {
		return false, nil, nil
	}
	for i := range edits {
		ed := &edits[i]
		if ed.Ref.Kind == route.Kind && ed.Ref.Scope == r.Scope && ed.Ref.Name == r.Name {
			rt, _ := ed.Spec.(*resourcev1.Route)
			return rt != nil && !rt.GetUnwatched() && asks(rt), nil, nil
		}
	}
	spec, err := w.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return false, nil, err
	}
	rt, _ := spec.(*resourcev1.Route)
	if !asks(rt) {
		return false, nil, nil
	}
	if rt.GetUnwatched() {
		return true, &r, nil
	}
	return true, nil, nil
}

// setUnwatched says whether to watch the route r is: in its edit, where the change
// writes it, or in an edit of its own, made from the revision read (Edit.Base).
func (w *Watcher) setUnwatched(ctx context.Context, tx *store.Tx, edits []revision.Edit, r store.Resource, unwatched bool) ([]revision.Edit, error) {
	for i := range edits {
		ed := &edits[i]
		if ed.Ref.Kind != route.Kind || ed.Ref.Scope != r.Scope || ed.Ref.Name != r.Name {
			continue
		}
		if rt, ok := ed.Spec.(*resourcev1.Route); ok && rt.GetUnwatched() != unwatched {
			rt = proto.CloneOf(rt)
			rt.Unwatched = unwatched
			ed.Spec = rt
		}
		return edits, nil
	}
	spec, err := w.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	rt, _ := spec.(*resourcev1.Route)
	if rt == nil || rt.GetUnwatched() == unwatched {
		return edits, nil
	}
	rt = proto.CloneOf(rt)
	rt.Unwatched = unwatched
	return append(edits, revision.Edit{Ref: resource.Ref{Kind: route.Kind, Scope: r.Scope, Name: r.Name}, Spec: rt, Base: r.Revision}), nil
}

// appendDelete deletes a check Dockwell made for a route, from the revision read,
// unless the change already writes it.
func appendDelete(edits []revision.Edit, v *view, c *check) []revision.Edit {
	if writes(edits, c.name) {
		return edits
	}
	c.spec, c.gone = nil, true
	return append(edits, revision.Edit{Ref: resource.Ref{Kind: alert.CheckKind, Scope: v.org, Name: c.name}, Base: c.rev, MadeForRoute: true})
}

// appendMove points a check Dockwell made for a route at the address the route asks
// now, from the revision read, unless the change already writes it.
func appendMove(edits []revision.Edit, v *view, c *check, address string) []revision.Edit {
	if writes(edits, c.name) {
		return edits
	}
	moved := proto.CloneOf(c.spec)
	moved.Address = address
	c.spec = moved
	return append(edits, revision.Edit{Ref: resource.Ref{Kind: alert.CheckKind, Scope: v.org, Name: c.name}, Spec: moved, Base: c.rev, MadeForRoute: true})
}

// writes reports a change that writes the check named.
func writes(edits []revision.Edit, name string) bool {
	return slices.ContainsFunc(edits, func(ed revision.Edit) bool { return ed.Ref.Kind == alert.CheckKind && ed.Ref.Name == name })
}

// hostOf is the host an address asks.
func hostOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// sameBesidesMark reports two checks that differ in nothing but their route's mark.
func sameBesidesMark(a, b *resourcev1.AlertCheck) bool {
	a, b = proto.CloneOf(a), proto.CloneOf(b)
	a.ForRoute, b.ForRoute = "", ""
	return proto.Equal(a, b)
}
