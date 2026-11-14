// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// SetCatalogNeed plans Use another database: one of an install's needs met by a server
// Dockwell runs, at the release the install is on, as installing it would have, with
// what met it before left running as it is, data and all (ADR 0084).
func (h *Handler) SetCatalogNeed(ctx context.Context, req *connect.Request[dockwellv1.SetCatalogNeedRequest]) (*connect.Response[dockwellv1.SetCatalogNeedResponse], error) {
	m := req.Msg
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, m.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	pin := install.GetPin()
	entry, ok := s.entryFor(pin)
	switch {
	case !ok:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s came from %s, which has no %s now, so what meets its needs stays as it is", install.GetProject(), originOf(pin).words(), pin.GetEntry()), "not_in_catalog")
	case len(pin.GetFile()) == 0:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s was installed before Dockwell kept what its entry's file gave, so what meets its needs stays as it is", install.GetProject()), "no_record")
	}
	i := slices.IndexFunc(entry.GetNeeds(), func(n *catalogv1.Need) bool { return n.GetName() == m.GetNeed() })
	was, met := pin.GetNeeds()[m.GetNeed()]
	switch {
	case i < 0:
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("%s has no need %s. Name one of what it needs: %s", entry.GetName(), m.GetNeed(), strings.Join(slices.Collect(maps.Keys(pin.GetNeeds())), ", ")), "invalid")
	case !met:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs %s %s, which doesn't need %s. Upgrade it first", install.GetProject(), entry.GetName(), pin.GetVersion(), m.GetNeed()), "upgrade_first")
	case !strings.HasPrefix(m.GetChoice(), "service:"):
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("%s's %s moves to a server Dockwell runs, named as service:<project>/<environment>/<service>, as its install offers them. A new one beside it comes once Dockwell copies the data across", install.GetProject(), m.GetNeed()), "invalid")
	}
	need := entry.GetNeeds()[i]
	// The entry as the install runs it: its file and release, with this release's
	// needs, every one but this met as the pin says.
	ran := proto.CloneOf(entry)
	ran.Compose, ran.Version, ran.Release = string(pin.GetFile()), pin.GetVersion(), pin.GetRelease()
	pinned := maps.Clone(pin.GetNeeds())
	delete(pinned, m.GetNeed())
	before, err := catalog.MetFile(string(pin.GetFile()), pin.GetNeeds())
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	stays, err := catalog.ServiceNames(before)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	server := h.serverOf(ctx, install.GetEnvironmentId())
	candidates := h.candidates(ctx)
	needed, notes, err := h.meetNeeds(ctx, s, ran, nil, map[string]string{m.GetNeed(): m.GetChoice()}, pinned, candidates, install.GetProject(), server, stays...)
	if err != nil {
		return nil, err
	}
	now := needed.Pinned[m.GetNeed()]
	if was.GetEnvironment() != "" && was.GetEnvironment() == now.GetEnvironment() && was.GetMeets() == now.GetMeets() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's %s is %s in that environment already", install.GetProject(), m.GetNeed(), now.GetMeets()), "unchanged")
	}
	choice := catalog.Resolve(needed.Entry, pin)
	chosen, err := catalog.Choose(needed.Entry, choice)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	after, err := catalog.ServiceNames(chosen.GetCompose())
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What met it before leaves the app's file and keeps running, with its data.
	keeps := slices.DeleteFunc(slices.Clone(stays), func(name string) bool { return slices.Contains(after, name) })
	free, err := h.whatsFree(ctx, needed.Entry, "")
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What the needs set is written anew, as the way it's met says, not as the install
	// was given it.
	carried := proto.CloneOf(pin)
	for _, name := range needed.Plain {
		delete(carried.Values, name)
	}
	filled, err := catalog.Carry(chosen, carried, nil, free.values, found.held)
	if err == nil {
		err = filled.Link(needed.Plain)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	// The password the app had is the new user's now, in the server's environment.
	password := catalog.PasswordName(need)
	if _, generated := filled.Secrets[password]; !generated {
		filled.Secrets[password] = filled.Variables[password]
	}
	old, err := ranFile(pin)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	oldVariables, oldStands, err := catalog.Given(old)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	oldLinked, err := catalog.StandFor(oldVariables, needed.Plain)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// The app's own database is copied across where both ends run the same engine on
	// this server; otherwise it starts empty only when asked to (ADR 0084).
	moved, err := h.moveData(ctx, m, entry, install.GetEnvironmentId(), server, needed, after, candidates)
	if err != nil {
		return nil, err
	}
	notes = append(notes, moved.notes...)
	written := writtenEdits(needed, filled)
	shared, _, err := h.sharedEdits(ctx, needed, filled)
	if err != nil {
		return nil, err
	}
	notes = append(notes, shareNotes(needed)...)
	if len(keeps) > 0 {
		notes = append(notes, revision.Note{
			Reason:  "catalog_need_moves",
			Message: strings.Join(keeps, " and ") + " keeps running as it is, with its data. Delete it once you no longer need it",
			Names:   keeps,
		})
	}
	newPin := pinTo(entry, s.origins[entry.GetId()], filled, choice)
	newPin.Version, newPin.Release, newPin.File, newPin.Needs = pin.GetVersion(), pin.GetRelease(), pin.GetFile(), needed.Pinned
	change, err := h.imports.PlanUpgrade(ctx, composeapi.Upgrade{
		Project:     install.GetProjectId(),
		Environment: install.GetEnvironmentId(),
		Summary:     fmt.Sprintf("Use %s for %s's %s", m.GetChoice()[len("service:"):], install.GetProject(), strings.ToLower(need.GetLabel())),
		FromRelease: pin.GetVersion(),
		Release:     pin.GetVersion(),
		Name:        found.display,
		File:        []byte(chosen.GetCompose()),
		Variables:   filled.Variables,
		Secrets:     filled.Secrets,
		Stands:      filled.Stands(),
		Pin:         newPin,
		Companion:   true,
		Keeps:       keeps,

		Old:          old.GetFile(),
		OldVariables: oldVariables,
		OldStands:    oldStands,
		Notes:        notes,
		Hold:         moved.hold,
		Needs:        composeapi.Needs{Variables: filled.Linked, Stands: filled.LinkStands, OldStands: oldLinked, Links: needed.Links, Extra: slices.Concat(shared.Extra, written, moved.holds), Joins: shared.Joins},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCatalogNeedResponse{Change: change}), nil
}

// moving is what moving an app's database asks besides the move: notes for its plan,
// and where it starts empty and its first visitor makes its owner, every way in held:
// its routes, and the ports its services publish (hold).
type moving struct {
	notes []revision.Note
	holds []*dockwellv1.Edit
	hold  bool
}

// moveData has the user the move makes on the new server start as a copy of the
// app's database, where both run the same engine Dockwell copies, on this server; or
// refuses with cant_copy, unless the request asks to start empty. An app that starts
// empty whose first visitor makes its owner has its routes held, so a stranger can't
// make themselves its owner before the person who moved it sets it up.
func (h *Handler) moveData(ctx context.Context, m *dockwellv1.SetCatalogNeedRequest, entry *catalogv1.Entry, env, server string, needed *catalog.Needed, services []string, candidates []catalog.Candidate) (moving, error) {
	var out moving
	i := slices.IndexFunc(needed.Shared, func(w catalog.Way) bool { return w.Need.GetName() == m.GetNeed() })
	if i < 0 {
		return out, nil
	}
	w := &needed.Shared[i]
	app := entry.GetName()
	err := h.db.View(ctx, func(tx *store.Tx) error {
		was := &resourcev1.Link{}
		r, err := tx.GetResource(ctx, string(link.Kind), env, m.GetNeed())
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case r.Spec != nil:
			// A link is never sealed: it names values, and holds none.
			if err := proto.Unmarshal(r.Spec, was); err != nil {
				return err
			}
		}
		from := w.Existing.Service + " in " + w.Existing.Project
		why := ""
		switch {
		case was.GetDatabaseName() == "" || !database.Copies(dockwellv1.DatabaseEngine(was.GetEngine())):
			why = fmt.Sprintf("Dockwell doesn't copy a %s database across yet", catalog.EngineName(catalogv1.Engine(was.GetEngine())))
		case int32(was.GetEngine()) != int32(w.Engine):
			why = fmt.Sprintf("%s runs %s, and %s's database is %s", from, catalog.EngineName(w.Engine), app, catalog.EngineName(catalogv1.Engine(was.GetEngine())))
		case server != "" && !slices.Contains(w.Existing.Servers, server):
			why = fmt.Sprintf("%s runs on another server than %s", from, app)
		}
		switch {
		case !m.GetStartEmpty() && why != "":
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s, so %s's data can't be copied across. Start it empty there, or pick a server that runs %s", why, app, catalog.EngineName(catalogv1.Engine(was.GetEngine()))), "cant_copy")
		case !m.GetStartEmpty():
			w.CopyFrom = &resourcev1.DatabaseCopy{Environment: cmp.Or(was.GetMeetsEnvironment(), env), Service: was.GetService(), Database: was.GetDatabaseName()}
			size := ""
			if n, ok := sizeOf(candidates, w.CopyFrom); ok {
				size = " of " + human.Size(n)
			}
			out.notes = append(out.notes, revision.Note{
				Reason: "catalog_need_copies",
				Message: fmt.Sprintf("Dockwell copies %s's database%s from %s to %s first. %s keeps serving from %s until the copy is done, then starts on the copy; what it writes while the copy runs, which takes longer the larger it is, stays on %s",
					app, size, was.GetService(), from, app, was.GetService(), was.GetService()),
			})
		default:
			msg := fmt.Sprintf("%s starts on an empty database on %s", app, from)
			if entry.GetFirstVisitorOwns() {
				held, err := holdRoutes(ctx, tx, env, services)
				if err != nil {
					return err
				}
				out.holds, out.hold = held, true
				msg += ". Its first visitor makes its owner, so it's held before it restarts: its domains answer that it isn't open, and the ports it publishes listen on this server alone, until you open it from its page. Set it up first, through the panel"
			}
			out.notes = append(out.notes, revision.Note{Reason: "catalog_need_starts_empty", Message: msg})
		}
		out.notes = append(out.notes, revision.Note{
			Reason: "catalog_need_undo",
			Message: fmt.Sprintf("Undoing this puts %s back on %s, and drops the database Dockwell makes it on %s, with what %s wrote there after the move",
				app, was.GetService(), from, app),
		})
		return nil
	})
	return out, err
}

// sizeOf is how much a database to copy takes, as its server's reading says.
func sizeOf(candidates []catalog.Candidate, c *resourcev1.DatabaseCopy) (int64, bool) {
	for i := range candidates {
		if candidates[i].EnvironmentID == c.GetEnvironment() && candidates[i].Service == c.GetService() {
			n, ok := candidates[i].Sizes[c.GetDatabase()]
			return n, ok
		}
	}
	return 0, false
}

// holdRoutes holds each route of env that sends to one of services, as an edit of
// its spec.
func holdRoutes(ctx context.Context, tx *store.Tx, env string, services []string) ([]*dockwellv1.Edit, error) {
	routes, err := tx.ListResources(ctx, env, "route")
	if err != nil {
		return nil, err
	}
	var out []*dockwellv1.Edit
	for _, r := range routes {
		if r.Spec == nil {
			continue
		}
		// A route is sealed since it could hold users' password hashes (ADR 0087).
		plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		spec := &resourcev1.Route{}
		if proto.Unmarshal(plain, spec) != nil || !slices.Contains(services, spec.GetService()) || spec.GetHeld() {
			continue
		}
		spec.Held = true
		out = append(out, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: env, Name: r.Name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: spec}},
		})
	}
	return out, nil
}

// OpenCatalogInstall plans opening an install that SetCatalogNeed held as it started
// empty: each held route answers again, and each held port listens where it did, as
// the person who set it up asks (ADR 0084).
func (h *Handler) OpenCatalogInstall(ctx context.Context, req *connect.Request[dockwellv1.OpenCatalogInstallRequest]) (*connect.Response[dockwellv1.OpenCatalogInstallResponse], error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	env := install.GetEnvironmentId()
	var edits []*dockwellv1.Edit
	err = h.db.View(ctx, func(tx *store.Tx) error {
		for _, kind := range []string{"route", string(service.Kind)} {
			list, err := tx.ListResources(ctx, env, kind)
			if err != nil {
				return err
			}
			for i := range list {
				r := list[i]
				if r.Spec == nil {
					continue
				}
				spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					return err
				}
				var out *resourcev1.Spec
				switch v := spec.(type) {
				case *resourcev1.Route:
					if v.GetHeld() {
						v.Held = false
						out = &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: v}}
					}
				case *resourcev1.Service:
					opened := false
					for _, p := range v.GetApp().GetPorts() {
						opened = opened || p.GetHeld()
						p.Held = false
					}
					if opened {
						out = &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: v}}
					}
				}
				if out != nil {
					edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: kind, Scope: env, Name: r.Name}, Spec: out})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if len(edits) == 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't held, so there's nothing to open", install.GetProject()), "not_held")
	}
	change, err := h.changes.Propose(ctx, "Open "+install.GetProject()+" to everyone", edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.OpenCatalogInstallResponse{Change: change}), nil
}
