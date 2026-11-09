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

// Resource kinds whose IDs say what they are, beside internal/id's: each is a
// resource, placed where its scope is. A destination's scope is the
// organization, so it's the install's.
const (
	volumePrefix      = "vol"
	networkPrefix     = "net"
	policyPrefix      = "bpl"
	destinationPrefix = "bds"
)

// Named reports an object seen by name on the install, as servers and backup
// destinations are, whichever projects name it (ADR 0049).
func Named(objectID string) bool {
	prefix, _, _ := strings.Cut(objectID, "_")
	return prefix == id.Server || prefix == destinationPrefix
}

// Of says where the object with objectID belongs. A change is where each of its
// items is, so it can be in several places; anything else is in one. An ID that
// names nothing is store.ErrNotFound.
func Of(ctx context.Context, tx *store.Tx, objectID string) ([]Place, error) {
	return NewPlacer(tx).Of(ctx, objectID)
}

// Placer places objects with one transaction, reading each environment once,
// for a list whose elements share them.
type Placer struct {
	tx     *store.Tx
	scopes map[string]Place
}

// NewPlacer returns a Placer reading with tx.
func NewPlacer(tx *store.Tx) *Placer { return &Placer{tx: tx, scopes: map[string]Place{}} }

// Of is the package's Of, with what p read before.
func (pl *Placer) Of(ctx context.Context, objectID string) ([]Place, error) {
	tx := pl.tx
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
			p, err := pl.itemPlace(ctx, it, own)
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
		p, err := pl.scoped(ctx, b.EnvironmentID)
		return []Place{p}, err
	case id.Project, id.Environment, id.Service, id.Variable, id.Secret, id.Route, id.Resource,
		volumePrefix, networkPrefix, policyPrefix, destinationPrefix, (task.Type{}).IDPrefix():
		r, err := tx.GetResourceByID(ctx, objectID)
		if err != nil {
			return nil, err
		}
		p, err := pl.resourcePlace(ctx, r)
		return []Place{p}, err
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknown, objectID)
}

// itemPlace is where a change's item is: an environment's item is in that
// environment, and anything else is where its scope is, among the change's own
// items first, then in the store.
func (pl *Placer) itemPlace(ctx context.Context, it *store.ChangeItem, own map[string]*store.ChangeItem) (Place, error) {
	switch it.Kind {
	case string(scope.EnvironmentKind):
		return Place{Project: it.Scope, Environment: it.ResourceID, Protected: protectedByName(it.Name)}, nil
	case string(scope.ProjectKind):
		return Place{Project: it.ResourceID}, nil
	}
	if parent, ok := own[it.Scope]; ok && parent != it {
		return pl.itemPlace(ctx, parent, own)
	}
	return pl.scoped(ctx, it.Scope)
}

// resourcePlace is where a resource is: a project or an environment is its
// own place, and anything else is where its scope is.
func (pl *Placer) resourcePlace(ctx context.Context, r store.Resource) (Place, error) {
	switch r.Kind {
	case string(scope.ProjectKind):
		return Place{Project: r.ID}, nil
	case string(scope.EnvironmentKind):
		return Place{Project: r.Scope, Environment: r.ID, Protected: protectedByName(r.Name)}, nil
	}
	return pl.scoped(ctx, r.Scope)
}

// scoped is the place a scope ID makes: an environment's, a project's, or the
// install's for a server or the organization.
func (pl *Placer) scoped(ctx context.Context, scopeID string) (Place, error) {
	prefix, _, _ := strings.Cut(scopeID, "_")
	switch prefix {
	case id.Environment:
		if p, ok := pl.scopes[scopeID]; ok {
			return p, nil
		}
		env, err := pl.tx.GetResourceByID(ctx, scopeID)
		if err != nil {
			return Place{}, err
		}
		p := Place{Project: env.Scope, Environment: env.ID, Protected: protectedByName(env.Name)}
		pl.scopes[scopeID] = p
		return p, nil
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
		p, err := NewPlacer(tx).resourcePlace(ctx, found[0])
		return p, err == nil, err
	case len(found) == 0:
		return Place{}, false, store.ErrNotFound
	}
	return Place{}, false, nil
}

// Found is a place a name was found in, with its project's and environment's
// names, for saying which, and the resource's ID when one was named.
type Found struct {
	Place
	ResourceID                   string
	ProjectName, EnvironmentName string
}

// Find finds what a request names by name without its project, among the
// places reaches lets in, as the API finds it: a service by its name, in an
// environment of that name when env is given, or an environment by its name.
// Places out of reach are passed over, so what it finds says nothing of them.
func Find(ctx context.Context, tx *store.Tx, reaches func(Place) bool, service, env string) ([]Found, error) {
	return findResource(ctx, tx, reaches, serviceKind, service, "", env)
}

// FindTask finds a live task by ID, or by name within the projects reaches
// allows. Project and environment narrow a name, as TaskService does; an ID
// already names one task. Missing and unreachable tasks both find nothing.
func FindTask(ctx context.Context, tx *store.Tx, reaches func(Place) bool, named, project, env string) ([]Found, error) {
	if strings.HasPrefix(named, (task.Type{}).IDPrefix()+"_") {
		r, err := tx.GetResourceByID(ctx, named)
		if err != nil {
			return nil, err
		}
		if r.Kind != string(task.Kind) || r.Spec == nil {
			return nil, store.ErrNotFound
		}
		p, err := NewPlacer(tx).resourcePlace(ctx, r)
		if err != nil {
			return nil, err
		}
		if !reaches(p) {
			return nil, nil
		}
		return []Found{{Place: p, ResourceID: r.ID}}, nil
	}
	return findResource(ctx, tx, reaches, string(task.Kind), named, project, env)
}

// findResource finds an environment, or a live resource of kind in it, within
// reachable projects. It reads no resource specs beyond checking they're live.
func findResource(ctx context.Context, tx *store.Tx, reaches func(Place) bool, kind, named, project, env string) ([]Found, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []Found
	for _, p := range tree.Projects {
		if !reaches(Place{Project: p.ID}) || project != "" && project != p.ID && project != p.Name {
			continue
		}
		for _, e := range p.Environments {
			if env != "" && env != e.Name && env != e.ID {
				continue
			}
			var resourceID string
			if named != "" {
				r, err := tx.GetResource(ctx, kind, e.ID, named)
				if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
					continue
				}
				if err != nil {
					return nil, err
				}
				resourceID = r.ID
			}
			at, _, err := Environment(ctx, tx, p.ID, e.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, Found{
				Place:       at,
				ResourceID:  resourceID,
				ProjectName: p.Name, EnvironmentName: e.Name,
			})
		}
	}
	return out, nil
}

// serviceKind is a service's kind, as internal/resource/service names it.
const serviceKind = "service"
