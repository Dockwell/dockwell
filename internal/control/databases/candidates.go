// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// linkKind is the kind a link is, which says who shares a server (ADR 0084).
const linkKind = "link"

// pinnedApart reports a service a catalog install's pin keeps in a file of its own,
// as it does what a need's new one from the catalog added (CatalogPin.needs), so the
// app's upgrades leave it as it ran.
//
// pinnable reports the entry's own service for a need of its name, which the plan
// that first shares it pins apart from the app, so the app's upgrades leave it as it
// runs (ADR 0084).
func pinnedApart(ctx context.Context, tx *store.Tx, environment, service string) (kept, pinnable bool, err error) {
	env, err := tx.GetResourceByID(ctx, environment)
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	project, err := tx.GetResourceByID(ctx, env.Scope)
	if errors.Is(err, store.ErrNotFound) || err == nil && project.Spec == nil {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	// A project is never sealed (ADR 0022).
	spec := &resourcev1.Project{}
	if err := proto.Unmarshal(project.Spec, spec); err != nil {
		return false, false, fmt.Errorf("can't read the project %s: %w", project.Name, err)
	}
	n, ok := spec.GetCatalog().GetNeeds()[service]
	if !ok || n.GetEnvironment() != "" {
		return false, false, nil
	}
	return len(n.GetFile()) > 0, len(n.GetFile()) == 0 && len(spec.GetCatalog().GetFile()) > 0, nil
}

// databaseEntries are the catalog's entries that install a database of its own.
var databaseEntries = map[string]bool{"postgres": true, "mysql": true, "mariadb": true, "redis": true}

// Candidates are the database servers Dockwell runs, as ways to meet an app's need
// (ADR 0084): each with its engine, the version it says of itself where a reading
// says it, its server, what its reading lists, the projects whose apps link to it
// already, and a service outside it that signs in as its administrator, where one
// does, which keeps it from being shared.
func (h *Handler) Candidates(ctx context.Context) ([]catalog.Candidate, error) {
	list, err := h.list(ctx)
	if err != nil {
		return nil, err
	}
	h.read(ctx, list)
	var out []catalog.Candidate
	err = h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		names := map[string][2]string{} // by environment ID: its project's and its own name
		projectIDs := map[string]string{}
		for _, p := range tree.Projects {
			for _, e := range p.Environments {
				names[e.ID] = [2]string{p.Name, e.Name}
				projectIDs[e.ID] = p.ID
			}
		}
		shared := map[string][]string{} // by environment ID and service: the projects linking to it
		links, err := tx.ListKind(ctx, linkKind)
		if err != nil {
			return err
		}
		var stored []store.StoredSpec
		var live []store.Resource
		for _, r := range links {
			if r.Spec != nil {
				stored = append(stored, store.StoredSpec{At: store.InRevision(r.ID, r.Kind, r.Revision), Spec: r.Spec})
				live = append(live, r)
			}
		}
		decoded, _, err := h.keeper.DecodeEach(ctx, tx, stored)
		if err != nil {
			return err
		}
		for i, spec := range decoded {
			r := live[i]
			if l, ok := spec.(*resourcev1.Link); ok && l.GetMeetsEnvironment() != "" {
				key := l.GetMeetsEnvironment() + "/" + l.GetService()
				if p := names[r.Scope][0]; p != "" && !slices.Contains(shared[key], p) {
					shared[key] = append(shared[key], p)
				}
			}
		}
		for _, p := range list {
			db := p.db
			env := p.ref.Scope
			c := catalog.Candidate{
				ServiceID: db.GetId(), Service: p.ref.Name,
				Project: names[env][0], Environment: names[env][1], ProjectID: projectIDs[env], EnvironmentID: env,
				Engine: catalogv1.Engine(db.GetEngine()), Version: db.GetVersion(), SharedWith: shared[env+"/"+p.ref.Name],
				JoinsInPlace: len(p.servers) > 0,
			}
			// A database a catalog install brings in its app's file follows that app's
			// upgrades; one installed as a database of its own is its own, and so is one
			// a need's new one from the catalog added, which the install's pin keeps as
			// it ran.
			if entry := db.GetCatalogEntry(); entry != "" && !databaseEntries[entry] {
				kept, pinnable, err := pinnedApart(ctx, tx, env, p.ref.Name)
				if err != nil {
					return err
				}
				if !kept {
					c.PartOf, c.Pinnable = entry, pinnable
				}
			}
			for i := range p.servers {
				s := &p.servers[i]
				c.Servers, c.ServerNames = append(c.Servers, s.ID), append(c.ServerNames, s.Name)
				if sess, ok := h.agents.Session(s.ID); !ok || !sess.Links() {
					c.JoinsInPlace = false
				}
			}
			for _, r := range db.GetReadings() {
				if r.GetUnread() != "" {
					continue
				}
				if r.GetVersion() != "" {
					c.Version = r.GetVersion()
				}
				for _, d := range r.GetDatabases() {
					c.Databases = append(c.Databases, d.GetName())
				}
				for _, u := range r.GetUsers() {
					c.Users = append(c.Users, u.GetName())
				}
				break
			}
			// The administrator's secret: what else uses it signs in as the administrator.
			if secret := db.GetPasswordSecret(); secret != "" {
				users, err := tx.UsersOf(ctx, env, string(variable.SecretKind), secret)
				if err != nil {
					return err
				}
				for i := range users {
					u := &users[i]
					if u.Kind == "service" && (u.Scope != env || u.Name != p.ref.Name) {
						c.AdministeredBy = u.Name
						break
					}
				}
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}
