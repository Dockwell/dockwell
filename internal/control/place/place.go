// SPDX-License-Identifier: AGPL-3.0-only

// Package place says where an object belongs, by its ID: its project, its
// environment, and whether that environment is protected. Roles are checked
// against it, for every call that names an object, however the object is
// reached (docs/adr/0048-members-as-far-as-one-owner-goes.md,
// docs/adr/0049-roles-and-invites-by-project.md).
package place

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/task"
)

// Place is where an object belongs.
type Place struct {
	// Project is the project's ID, or empty for what belongs to no project:
	// the install.
	Project string
	// Environment is the environment's ID, or empty for what belongs to a
	// whole project, or to the install.
	Environment string
	// Protected is true in a protected environment.
	Protected bool
}

// Install is the place of what belongs to no project, such as a server.
var Install = Place{}

// ErrUnknown is an ID of a kind this package can't place. A call naming one is
// refused, so a new kind of object is closed until it's placed.
var ErrUnknown = errors.New("can't say where this belongs")

// Of says where the object with objectID belongs. A change is where each of its
// items is, so it can be in several places; anything else is in one. An ID that
// names nothing is store.ErrNotFound.
func Of(ctx context.Context, tx *store.Tx, objectID string) ([]Place, error) {
	prefix, _, _ := strings.Cut(objectID, "_")
	switch prefix {
	case id.Server, id.Organization:
		return []Place{Install}, nil
	case id.Change:
		c, err := tx.GetChange(ctx, objectID)
		if err != nil {
			return nil, err
		}
		// A change can make what its other items live in, such as a new
		// environment, which isn't in the store until it's applied.
		own := make(map[string]*store.ChangeItem, len(c.Items))
		for i := range c.Items {
			own[c.Items[i].ResourceID] = &c.Items[i]
		}
		var out []Place
		for i := range c.Items {
			it := &c.Items[i]
			p, err := itemPlace(ctx, tx, it, own)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
		return out, nil
	case id.Backup:
		b, err := tx.GetBackup(ctx, objectID)
		if err != nil {
			return nil, err
		}
		p, err := scoped(ctx, tx, b.EnvironmentID)
		return []Place{p}, err
	case id.Project, id.Environment, id.Service, id.Variable, id.Secret, id.Route, id.Resource, (task.Type{}).IDPrefix():
		r, err := tx.GetResourceByID(ctx, objectID)
		if err != nil {
			return nil, err
		}
		p, err := resourcePlace(ctx, tx, r)
		return []Place{p}, err
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknown, objectID)
}

// itemPlace is where a change's item is: an environment's item is in that
// environment, and anything else is where its scope is, among the change's own
// items first, then in the store.
func itemPlace(ctx context.Context, tx *store.Tx, it *store.ChangeItem, own map[string]*store.ChangeItem) (Place, error) {
	switch it.Kind {
	case string(scope.EnvironmentKind):
		return Place{Project: it.Scope, Environment: it.ResourceID, Protected: protectedByName(it.Name)}, nil
	case string(scope.ProjectKind):
		return Place{Project: it.ResourceID}, nil
	}
	if parent, ok := own[it.Scope]; ok && parent != it {
		return itemPlace(ctx, tx, parent, own)
	}
	return scoped(ctx, tx, it.Scope)
}

// resourcePlace is where a resource is: a project or an environment is its
// own place, and anything else is where its scope is.
func resourcePlace(ctx context.Context, tx *store.Tx, r store.Resource) (Place, error) {
	switch r.Kind {
	case string(scope.ProjectKind):
		return Place{Project: r.ID}, nil
	case string(scope.EnvironmentKind):
		return Place{Project: r.Scope, Environment: r.ID, Protected: protectedByName(r.Name)}, nil
	}
	return scoped(ctx, tx, r.Scope)
}

// scoped is the place a scope ID makes: an environment's, a project's, or the
// install's for a server or the organization.
func scoped(ctx context.Context, tx *store.Tx, scopeID string) (Place, error) {
	prefix, _, _ := strings.Cut(scopeID, "_")
	switch prefix {
	case id.Environment:
		env, err := tx.GetResourceByID(ctx, scopeID)
		if err != nil {
			return Place{}, err
		}
		return Place{Project: env.Scope, Environment: env.ID, Protected: protectedByName(env.Name)}, nil
	case id.Project:
		return Place{Project: scopeID}, nil
	case id.Server, id.Organization, "":
		return Install, nil
	}
	return Place{}, fmt.Errorf("%w: a scope %s", ErrUnknown, scopeID)
}

// protectedByName is an environment's protection while it has none written: an
// environment named production is protected (ADR 0049). Once Environment
// carries protection, a written one is read first, and this only for one
// without.
func protectedByName(name string) bool { return name == "production" }

// Holds is the role grants give at a place: the highest of a grant on the
// install and one on the place's project. The install's own place counts only
// grants on the install. Empty is none: the place is out of reach.
func Holds(grants []store.Grant, p Place) string {
	best := ""
	for _, g := range grants {
		if g.ProjectID != store.Install && (p.Project == "" || g.ProjectID != p.Project) {
			continue
		}
		if store.RoleRank(g.Role) > store.RoleRank(best) {
			best = g.Role
		}
	}
	return best
}

// Project finds a project by its ID or its name, or ErrNotFound.
func Project(ctx context.Context, tx *store.Tx, ref string) (string, error) {
	if strings.HasPrefix(ref, id.Project+"_") {
		return ref, nil
	}
	all, err := tx.ListKind(ctx, string(scope.ProjectKind))
	if err != nil {
		return "", err
	}
	for i := range all {
		if all[i].Name == ref {
			return all[i].ID, nil
		}
	}
	return "", store.ErrNotFound
}

// Environment finds an environment of a project by its ID or its name, or, with
// none named, the project's only one. It's ErrNotFound for none, and ok false
// when the project has several and none is named.
func Environment(ctx context.Context, tx *store.Tx, projectID, ref string) (Place, bool, error) {
	if strings.HasPrefix(ref, id.Environment+"_") {
		got, err := Of(ctx, tx, ref)
		if err != nil || len(got) != 1 {
			return Place{}, false, err
		}
		return got[0], true, nil
	}
	envs, err := tx.ListResources(ctx, projectID, string(scope.EnvironmentKind))
	if err != nil {
		return Place{}, false, err
	}
	var found []store.Resource
	for i := range envs {
		if ref == "" || envs[i].Name == ref {
			found = append(found, envs[i])
		}
	}
	switch {
	case len(found) == 1:
		p, err := resourcePlace(ctx, tx, found[0])
		return p, err == nil, err
	case len(found) == 0:
		return Place{}, false, store.ErrNotFound
	}
	return Place{}, false, nil
}
