// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// A database server other projects' apps share is restored whole from its own page
// alone, and only by someone with a role in each of those projects, or an admin, who
// types its name: a whole restore takes back what each app wrote since, and each app
// restores its own database from its own page instead (ADR 0084).

// linkedApp is an app of another environment that shares a server.
type linkedApp struct {
	project, projectID string
	// env is the app's environment, which may have several links to the server.
	env string
	// service is the server's service, which the link meets.
	service string
}

// linkedApps are the apps of other environments whose links meet a server keeping its
// data in volume ref.
func (h *Handler) linkedApps(ctx context.Context, tx *store.Tx, ref resource.Ref) ([]linkedApp, error) {
	links, err := tx.ListKind(ctx, "link")
	if err != nil {
		return nil, err
	}
	var out []linkedApp
	var tree *scope.Tree
	mounts := map[string]bool{} // by service: it keeps its data in ref
	for _, r := range links {
		l := &resourcev1.Link{}
		// A link is never sealed: it names values, and holds none.
		if r.Spec == nil || r.Scope == ref.Scope || proto.Unmarshal(r.Spec, l) != nil || cmp.Or(l.GetMeetsEnvironment(), r.Scope) != ref.Scope {
			continue
		}
		in, seen := mounts[l.GetService()]
		if !seen {
			svc, err := tx.GetResource(ctx, "service", ref.Scope, l.GetService())
			if err == nil && svc.Spec != nil {
				spec, err := h.keeper.Decode(ctx, tx, store.InRevision(svc.ID, svc.Kind, svc.Revision), svc.Spec)
				if err != nil {
					return nil, err
				}
				in = slices.ContainsFunc(h.engine.Kinds().References("service", spec), func(u resource.Reference) bool {
					return u.Kind == ref.Kind && u.Name == ref.Name
				})
			}
			mounts[l.GetService()] = in
		}
		if !in {
			continue
		}
		if tree == nil {
			t, err := scope.Load(ctx, tx)
			if err != nil {
				return nil, err
			}
			tree = &t
		}
		a := linkedApp{service: l.GetService(), env: r.Scope}
		for _, p := range tree.Projects {
			for _, env := range p.Environments {
				if env.ID == r.Scope {
					a.project, a.projectID = p.Name, p.ID
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// wholeServer checks a whole restore of a server other projects' apps share: the
// caller holds a role in each of those projects, or is an admin, and typed the
// server's service's name in confirm. It returns what the plan says of each app.
func (h *Handler) wholeServer(ctx context.Context, ref resource.Ref, b *store.Backup, confirm string) ([]revision.Note, error) {
	var apps []linkedApp
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		apps, err = h.linkedApps(ctx, tx, ref)
		return err
	}); err != nil {
		return nil, h.fail(ctx, err)
	}
	if len(apps) == 0 {
		return nil, nil
	}
	service := apps[0].service
	if caller, ok := place.CallerFrom(ctx); ok && store.RoleRank(place.Holds(caller.Grants, place.Install)) < store.RoleRank(store.RoleAdmin) {
		for _, a := range apps {
			if store.RoleRank(place.Holds(caller.Grants, place.Place{Project: a.projectID})) < store.RoleRank(store.RoleDeveloper) {
				return nil, authapi.ReasonError(connect.CodePermissionDenied, fmt.Errorf("restoring %s whole takes back what %s of other projects wrote to it since, so it takes a role in each of their projects, or an admin. "+
					"Restore your app's database alone from its page instead", service, appsWord(appCount(apps))), "needs_role")
			}
		}
	}
	projects := make([]string, 0, len(apps))
	for _, a := range apps {
		if !slices.Contains(projects, a.project) {
			projects = append(projects, a.project)
		}
	}
	slices.Sort(projects)
	taken := b.StartedAt.UTC().Format("2006-01-02 15:04 UTC")
	loses := fmt.Sprintf("Restoring %s whole takes back what %s wrote to it since %s", service, human.List(projects), taken)
	if confirm != service {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s. Type %s to confirm, or restore each app's database alone from its page", loses, service), "confirm_name")
	}
	return []revision.Note{{Reason: "restore_linked_apps", Message: loses, Names: projects}}, nil
}

// appCount is how many apps the links are of: each environment's, once.
func appCount(apps []linkedApp) int {
	envs := map[string]bool{}
	for _, a := range apps {
		envs[a.env] = true
	}
	return len(envs)
}

// appsWord is how many apps, in words.
func appsWord(n int) string {
	if n == 1 {
		return "an app"
	}
	return fmt.Sprintf("%d apps", n)
}
