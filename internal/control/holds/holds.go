// SPDX-License-Identifier: AGPL-3.0-only

// Package holds keeps a held catalog install held through every write: its pin's
// hold and its sign-ups' choice, a route to it, a route to one of its ports, and its
// ports, whatever an edit says, until its owner is made or a person opens it. Held
// is worked out from the install's own state, never from the entry it's pinned to
// now: its pin's held, or an app.owner of its whose owner isn't made or opened yet.
// See docs/adr/0105-a-catalog-apps-owner-is-made-at-install.md.
package holds

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Decoder reads a stored spec, opening a sealed one.
type Decoder interface {
	Decode(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) (proto.Message, error)
}

// Holds keeps held installs held.
type Holds struct {
	Keeper Decoder
}

// Hook is the engine's edit hook (revision.Engine.SetEditHook): it keeps every
// write of a pin from dropping its hold or its sign-ups' choice, but an edit that
// unholds it; has deleting an owner not yet made hold its install; and holds every
// route and port of a held install that an edit writes.
func (h *Holds) Hook(ctx context.Context, tx *store.Tx, edits []revision.Edit) ([]revision.Edit, []revision.Note, error) {
	if !slices.ContainsFunc(edits, func(ed revision.Edit) bool {
		switch ed.Ref.Kind {
		case scope.ProjectKind, appowner.Kind, route.Kind, service.Kind:
			return true
		}
		return false
	}) {
		return edits, nil, nil
	}
	v, err := h.view(ctx, tx, edits)
	if err != nil {
		return nil, nil, err
	}
	out := slices.Clone(edits)
	var notes []revision.Note
	// A write of a project that leaves its pin out keeps the pin, as the engine's
	// Keep will: the hold is decided on that, never on a pin the write didn't say
	// (the review's H1).
	for i := range out {
		ed := &out[i]
		p, ok := ed.Spec.(*resourcev1.Project)
		if ed.Ref.Kind != scope.ProjectKind || !ok || p.GetCatalog() != nil {
			continue
		}
		old, err := v.storedPin(ed.Ref.Name)
		if err != nil {
			return nil, nil, err
		}
		if old != nil {
			p = proto.CloneOf(p)
			p.Catalog = proto.CloneOf(old)
			ed.Spec = p
		}
	}
	// Only the catalog's own write of a pin, an undo or a rollback changes what it says
	// the install runs and where it's reached, which the owner's run reads: any other
	// write keeps them as stored (the re-check's N2).
	for i := range out {
		ed := &out[i]
		p, ok := ed.Spec.(*resourcev1.Project)
		if ed.Ref.Kind != scope.ProjectKind || !ok || p.GetCatalog() == nil || ed.Pin || ed.Reverses || ed.Restores {
			continue
		}
		old, err := v.storedPin(ed.Ref.Name)
		if err != nil {
			return nil, nil, err
		}
		if old == nil || keepsIdentity(p.GetCatalog(), old) {
			continue
		}
		p = proto.CloneOf(p)
		c, o := p.GetCatalog(), old
		c.Entry, c.Version, c.Release, c.Catalog, c.Repository, c.Commit, c.Path, c.BuiltInCommit = o.GetEntry(), o.GetVersion(), o.GetRelease(), o.GetCatalog(), o.GetRepository(), o.GetCommit(), o.GetPath(), o.GetBuiltInCommit()
		c.Values, c.File, c.Address = maps.Clone(o.GetValues()), slices.Clone(o.GetFile()), o.GetAddress()
		ed.Spec = p
	}
	// Only the catalog writes an app's owner: an install makes it, and giving it its
	// email changes that and its user alone. Any other write of one is refused, so
	// no change can say who the owner is but the person installing it (the review's
	// H2).
	for i := range out {
		if err := v.ownersWrite(&out[i]); err != nil {
			return nil, nil, err
		}
	}
	v.edits = out
	// A pin keeps its hold and its sign-ups' choice, and an owner deleted before it's
	// made holds its install.
	for i := range out {
		ed := &out[i]
		if ed.Ref.Kind != scope.ProjectKind || ed.Spec == nil {
			continue
		}
		old, err := v.storedPin(ed.Ref.Name)
		if err != nil {
			return nil, nil, err
		}
		p, ok := ed.Spec.(*resourcev1.Project)
		if !ok || p.GetCatalog() == nil || old == nil {
			continue
		}
		held := p.GetCatalog().GetHeld() || (old.GetHeld() && !ed.Unholds)
		// Once opened, always: only an Open sets it, and nothing takes it back (L10).
		opened := old.GetOpened() || ed.Unholds && p.GetCatalog().GetOpened()
		if held == p.GetCatalog().GetHeld() && p.GetCatalog().GetSignupsOpen() == old.GetSignupsOpen() && opened == p.GetCatalog().GetOpened() {
			continue
		}
		p = proto.CloneOf(p)
		p.Catalog.Held, p.Catalog.SignupsOpen, p.Catalog.Opened = held, old.GetSignupsOpen(), opened
		ed.Spec = p
	}
	for i := range edits {
		ed := &edits[i]
		if ed.Ref.Kind != appowner.Kind || ed.Spec != nil {
			continue
		}
		final, err := v.ownerFinal(ed.Ref)
		if err != nil {
			return nil, nil, err
		}
		if final {
			continue
		}
		project, ok := v.projectOf(ed.Ref.Scope)
		if !ok {
			continue
		}
		if out, err = v.holdPin(out, project); err != nil {
			return nil, nil, err
		}
		notes = append(notes, revision.Note{
			Reason:  "owner_held",
			Message: project + " stays held: its owner isn't made, so deleting the owner leaves it held until you open it",
			Ref:     ed.Ref,
		})
	}
	v.edits = out
	// Routes and ports of a held install are held, whatever an edit says.
	heldNoted := map[string]bool{}
	// published are the host ports a held install's services publish in this change.
	published := map[uint32]bool{}
	for i := range out {
		ed := &out[i]
		if ed.Spec == nil {
			continue
		}
		switch spec := ed.Spec.(type) {
		case *resourcev1.Route:
			if spec.GetHeld() || spec.GetRedirect() != nil {
				continue
			}
			hold := false
			if spec.GetService() != "" {
				if hold, err = v.held(ed.Ref.Scope); err != nil {
					return nil, nil, err
				}
			} else if spec.GetHostPort() > 0 {
				if hold, err = v.heldHostPort(spec.GetHostPort()); err != nil {
					return nil, nil, err
				}
			}
			if hold {
				spec = proto.CloneOf(spec)
				spec.Held = true
				ed.Spec = spec
				if !heldNoted[ed.Ref.Scope] {
					heldNoted[ed.Ref.Scope] = true
					notes = append(notes, revision.Note{
						Reason:  "owner_held",
						Message: spec.GetDomain() + " answers that the app isn't open yet: its install is held until its owner is made or you open it",
						Ref:     ed.Ref,
					})
				}
			}
		case *resourcev1.Service:
			held, err := v.held(ed.Ref.Scope)
			if err != nil {
				return nil, nil, err
			}
			if held && spec.GetApp().GetHostNetwork() {
				return nil, nil, fault.New(revision.ErrInvalid, "%s is held until its owner is made or a person opens it, and on the server's own network every port it listens on would answer everyone. Open it first, or keep it on its own network", ed.Ref.Name)
			}
			if held {
				for _, p := range spec.GetApp().GetPorts() {
					if p.GetHostPort() > 0 {
						published[p.GetHostPort()] = true
					}
				}
			}
			if !held || !holdsPorts(spec) {
				continue
			}
			spec = proto.CloneOf(spec)
			for _, p := range spec.GetApp().GetPorts() {
				if !Loopback(p.GetHostIp()) {
					p.Held = true
				}
			}
			ed.Spec = spec
		}
	}
	// A route to a port on the server, made before a held install published on it,
	// dials the held app on 127.0.0.1 from now on, so it's held too, in this change
	// (the re-check's N1). Loopback ports count, as the route dials loopback.
	if len(published) > 0 {
		stale, err := v.hostPortRoutes(published, false)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range stale {
			if slices.ContainsFunc(out, func(ed revision.Edit) bool { return ed.Ref == r.Ref }) {
				continue
			}
			spec := proto.CloneOf(r.Spec)
			spec.Held = true
			out = append(out, revision.Edit{Ref: r.Ref, Spec: spec, Base: r.Revision})
			notes = append(notes, revision.Note{
				Reason:  "owner_held",
				Message: spec.GetDomain() + " sends to a port a held app publishes now, so it answers that the app isn't open yet, until its owner is made or you open it",
				Ref:     r.Ref,
			})
		}
	}
	return out, notes, nil
}

// Opened says whether a person opened an environment's install, as its pin says.
func (h *Holds) Opened(ctx context.Context, tx *store.Tx, environment string) (bool, error) {
	v, err := h.view(ctx, tx, nil)
	if err != nil {
		return false, err
	}
	project, ok := v.projectOf(environment)
	if !ok {
		return false, nil
	}
	pin, _, err := v.pin(project)
	return pin.GetOpened(), err
}

// HostPortRoute is a route to a port on the server, as stored.
type HostPortRoute struct {
	Ref      resource.Ref
	Spec     *resourcev1.Route
	Revision int
}

// HeldHostPortRoutes are the held routes, in any environment, to a port on the
// server that environment's services publish, which opening it, or its owner being
// made, releases with the rest (the re-check's N1).
func (h *Holds) HeldHostPortRoutes(ctx context.Context, tx *store.Tx, environment string) ([]HostPortRoute, error) {
	v, err := h.view(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	ports := map[uint32]bool{}
	list, err := tx.ListResources(ctx, environment, string(service.Kind))
	if err != nil {
		return nil, err
	}
	for i := range list {
		r := list[i]
		spec, err := h.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		s, _ := spec.(*resourcev1.Service)
		for _, p := range s.GetApp().GetPorts() {
			if p.GetHostPort() > 0 {
				ports[p.GetHostPort()] = true
			}
		}
	}
	if len(ports) == 0 {
		return nil, nil
	}
	return v.hostPortRoutes(ports, true)
}

// keepsIdentity says whether a pin says the same of what the install runs and where
// it's reached as old.
func keepsIdentity(c, old *resourcev1.CatalogPin) bool {
	return c.GetEntry() == old.GetEntry() && c.GetVersion() == old.GetVersion() && c.GetRelease() == old.GetRelease() &&
		c.GetCatalog() == old.GetCatalog() && c.GetRepository() == old.GetRepository() && c.GetCommit() == old.GetCommit() &&
		c.GetPath() == old.GetPath() && c.GetBuiltInCommit() == old.GetBuiltInCommit() && c.GetAddress() == old.GetAddress() &&
		maps.Equal(c.GetValues(), old.GetValues()) && bytes.Equal(c.GetFile(), old.GetFile())
}

// holdsPorts says whether a held service has a port to hold that isn't held yet.
func holdsPorts(s *resourcev1.Service) bool {
	return slices.ContainsFunc(s.GetApp().GetPorts(), func(p *resourcev1.PortMapping) bool {
		return !Loopback(p.GetHostIp()) && !p.GetHeld()
	})
}

// Loopback says whether a port listens on the server alone.
func Loopback(ip string) bool { return ip == "127.0.0.1" || ip == "::1" || ip == "localhost" }

// Held says whether an environment's install is held now, as it's stored.
func (h *Holds) Held(ctx context.Context, tx *store.Tx, environment string) (bool, error) {
	v, err := h.view(ctx, tx, nil)
	if err != nil {
		return false, err
	}
	return v.held(environment)
}

// view is what a proposal's edits leave, over what's stored.
type view struct {
	ctx   context.Context
	tx    *store.Tx
	h     *Holds
	tree  scope.Tree
	edits []revision.Edit
	// projects is each environment's project, by its name, the edits' own new ones
	// among them.
	projects  map[string]string
	heldEnv   map[string]bool
	hostPorts map[uint32]bool
}

func (h *Holds) view(ctx context.Context, tx *store.Tx, edits []revision.Edit) (*view, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	v := &view{ctx: ctx, tx: tx, h: h, tree: tree, edits: edits, projects: map[string]string{}, heldEnv: map[string]bool{}}
	byID := map[string]string{}
	for _, p := range tree.Projects {
		byID[p.ID] = p.Name
		for _, e := range p.Environments {
			v.projects[e.ID] = p.Name
		}
	}
	// A new project and its environment, as an install makes them, are named by
	// the IDs their edits give them.
	for i := range edits {
		if ed := &edits[i]; ed.Ref.Kind == scope.ProjectKind && ed.ID != "" {
			byID[ed.ID] = ed.Ref.Name
		}
	}
	for i := range edits {
		if ed := &edits[i]; ed.Ref.Kind == scope.EnvironmentKind && ed.ID != "" {
			if name, ok := byID[ed.Ref.Scope]; ok {
				v.projects[ed.ID] = name
			}
		}
	}
	return v, nil
}

func (v *view) projectOf(environment string) (string, bool) {
	p, ok := v.projects[environment]
	return p, ok
}

func (v *view) projectRef(name string) resource.Ref {
	return resource.Ref{Kind: scope.ProjectKind, Scope: v.tree.Organization.ID, Name: name}
}

// storedPin is a project's pin as it's stored, nil for one that isn't an install or
// isn't there yet.
func (v *view) storedPin(name string) (*resourcev1.CatalogPin, error) {
	r, err := v.tx.GetResource(v.ctx, string(scope.ProjectKind), v.tree.Organization.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil || r.Spec == nil {
		return nil, err
	}
	spec, err := v.h.Keeper.Decode(v.ctx, v.tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	p, _ := spec.(*resourcev1.Project)
	return p.GetCatalog(), nil
}

// pin is a project's pin as the edits leave it, and whether an edit unholds it.
func (v *view) pin(name string) (*resourcev1.CatalogPin, bool, error) {
	ref := v.projectRef(name)
	if i := slices.IndexFunc(v.edits, func(ed revision.Edit) bool { return ed.Ref == ref }); i >= 0 {
		ed := v.edits[i]
		if ed.Spec == nil {
			return nil, false, nil
		}
		if p, _ := ed.Spec.(*resourcev1.Project); p.GetCatalog() != nil {
			return p.GetCatalog(), ed.Unholds, nil
		}
		pin, err := v.storedPin(name)
		return pin, ed.Unholds, err
	}
	pin, err := v.storedPin(name)
	return pin, false, err
}

// held says whether an environment's install is held, as the edits leave it: its
// pin's held, or an owner of its not yet made or opened. An edit that unholds its
// pin unholds it, as an applied OpenCatalogInstall does.
func (v *view) held(environment string) (bool, error) {
	if held, ok := v.heldEnv[environment]; ok {
		return held, nil
	}
	held, err := v.workOut(environment)
	if err != nil {
		return false, err
	}
	v.heldEnv[environment] = held
	return held, nil
}

func (v *view) workOut(environment string) (bool, error) {
	project, ok := v.projectOf(environment)
	if !ok {
		return false, nil
	}
	pin, unholds, err := v.pin(project)
	if err != nil || unholds {
		return false, err
	}
	if pin.GetHeld() {
		return true, nil
	}
	// With no pin, its owner still holds it: a write that leaves the pin out never
	// reads as not held.
	owners, err := v.tx.ListResources(v.ctx, environment, string(appowner.Kind))
	if err != nil {
		return false, err
	}
	var ids []string
	for i := range owners {
		ref := resource.Ref{Kind: appowner.Kind, Scope: environment, Name: owners[i].Name}
		if slices.ContainsFunc(v.edits, func(ed revision.Edit) bool { return ed.Ref == ref && ed.Spec == nil }) {
			continue
		}
		ids = append(ids, owners[i].ID)
	}
	// An owner a proposal makes isn't made yet.
	for i := range v.edits {
		if ed := &v.edits[i]; ed.Ref.Kind == appowner.Kind && ed.Ref.Scope == environment && ed.Spec != nil &&
			!slices.ContainsFunc(owners, func(r store.Resource) bool { return r.Name == ed.Ref.Name }) {
			return true, nil
		}
	}
	states, err := v.tx.AppOwnerStates(v.ctx, ids)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if s, ok := states[id]; !ok || !store.OwnerFinal(s.State) {
			return true, nil
		}
	}
	return false, nil
}

// ownersWrite refuses a write of an app's owner the catalog didn't make, and one of
// the catalog's that changes anything but its email and its user. A deletion holds
// its install, as the hook says.
func (v *view) ownersWrite(ed *revision.Edit) error {
	// An undo or a rollback puts back a revision Dockwell recorded, which only the
	// catalog wrote.
	if ed.Ref.Kind != appowner.Kind || ed.Spec == nil || ed.Reverses || ed.Restores {
		return nil
	}
	if !ed.Owner {
		return fault.New(revision.ErrInvalid, "only the catalog writes an app's owner: an install makes it, and setting the owner's email gives it that. Delete %s's owner, which keeps the app held, or install the app again", ed.Ref.Scope)
	}
	r, err := v.tx.GetResource(v.ctx, string(ed.Ref.Kind), ed.Ref.Scope, ed.Ref.Name)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return nil
	}
	if err != nil {
		return err
	}
	stored, err := v.h.Keeper.Decode(v.ctx, v.tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return err
	}
	was, _ := stored.(*resourcev1.AppOwner)
	now, _ := ed.Spec.(*resourcev1.AppOwner)
	if was.GetService() != now.GetService() || was.GetPassword() != now.GetPassword() || was.GetApp() != now.GetApp() {
		return fault.New(revision.ErrInvalid, "an app's owner keeps its service, its password's secret and its app; only its email and user change")
	}
	return nil
}

// ownerFinal says whether a stored owner is made or opened.
func (v *view) ownerFinal(ref resource.Ref) (bool, error) {
	r, err := v.tx.GetResource(v.ctx, string(ref.Kind), ref.Scope, ref.Name)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	states, err := v.tx.AppOwnerStates(v.ctx, []string{r.ID})
	if err != nil {
		return false, err
	}
	s, ok := states[r.ID]
	return ok && store.OwnerFinal(s.State), nil
}

// holdPin has the proposal set a project's pin held, in its own edit or one added
// for it, as deleting an owner not yet made does. A project the proposal deletes
// needs none.
func (v *view) holdPin(edits []revision.Edit, project string) ([]revision.Edit, error) {
	ref := v.projectRef(project)
	if i := slices.IndexFunc(edits, func(ed revision.Edit) bool { return ed.Ref == ref }); i >= 0 {
		if edits[i].Spec == nil {
			return edits, nil
		}
		p, ok := edits[i].Spec.(*resourcev1.Project)
		if !ok || p.GetCatalog() == nil || p.GetCatalog().GetHeld() {
			return edits, nil
		}
		p = proto.CloneOf(p)
		p.Catalog.Held = true
		edits[i].Spec, edits[i].Unholds = p, false
		return edits, nil
	}
	r, err := v.tx.GetResource(v.ctx, string(scope.ProjectKind), ref.Scope, ref.Name)
	if err != nil {
		return nil, err
	}
	spec, err := v.h.Keeper.Decode(v.ctx, v.tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	p, ok := spec.(*resourcev1.Project)
	if !ok || p.GetCatalog() == nil || p.GetCatalog().GetHeld() {
		return edits, nil
	}
	p.Catalog.Held = true
	return append(edits, revision.Edit{Ref: ref, Spec: p, Base: r.Revision}), nil
}

// heldHostPort says whether a held install publishes a port on the server at port,
// which a route to that port dials.
func (v *view) heldHostPort(port uint32) (bool, error) {
	if v.hostPorts == nil {
		v.hostPorts = map[uint32]bool{}
		for _, environment := range slices.Sorted(maps.Keys(v.projects)) {
			held, err := v.held(environment)
			if err != nil {
				return false, err
			}
			if !held {
				continue
			}
			list, err := v.tx.ListResources(v.ctx, environment, string(service.Kind))
			if err != nil {
				return false, err
			}
			for i := range list {
				r := list[i]
				spec, err := v.h.Keeper.Decode(v.ctx, v.tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					return false, err
				}
				s, _ := spec.(*resourcev1.Service)
				for _, p := range s.GetApp().GetPorts() {
					v.hostPorts[p.GetHostPort()] = true
				}
			}
			for i := range v.edits {
				if s, ok := v.edits[i].Spec.(*resourcev1.Service); ok && v.edits[i].Ref.Scope == environment {
					for _, p := range s.GetApp().GetPorts() {
						v.hostPorts[p.GetHostPort()] = true
					}
				}
			}
		}
	}
	return v.hostPorts[port], nil
}

// hostPortRoutes are the stored routes, in every environment, to one of ports on the
// server, with no service, and held or not as held says. A redirect sends nowhere.
func (v *view) hostPortRoutes(ports map[uint32]bool, held bool) ([]HostPortRoute, error) {
	var out []HostPortRoute
	for _, environment := range slices.Sorted(maps.Keys(v.projects)) {
		list, err := v.tx.ListResources(v.ctx, environment, string(route.Kind))
		if err != nil {
			return nil, err
		}
		for i := range list {
			r := list[i]
			if r.Spec == nil {
				continue
			}
			spec, err := v.h.Keeper.Decode(v.ctx, v.tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return nil, err
			}
			rt, _ := spec.(*resourcev1.Route)
			if rt.GetService() != "" || rt.GetRedirect() != nil || !ports[rt.GetHostPort()] || rt.GetHeld() != held {
				continue
			}
			out = append(out, HostPortRoute{Ref: resource.Ref{Kind: route.Kind, Scope: environment, Name: r.Name}, Spec: rt, Revision: r.Revision})
		}
	}
	return out, nil
}
