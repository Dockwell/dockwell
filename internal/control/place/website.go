// SPDX-License-Identifier: AGPL-3.0-only

package place

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

// Website resolves the project a website's ID, name or domain names, as
// WebsiteService does. Projects hold no sealed fields, and a route is opened as
// the store's keeper opens it, as one may hold users' password hashes (ADR 0087).
// Only live website projects with an environment count, as in WebsiteService's list.
func Website(ctx context.Context, tx *store.Tx, ref string) (Place, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return Place{}, err
	}
	for _, p := range tree.Projects {
		r, err := tx.GetResourceByID(ctx, p.ID)
		if err != nil {
			return Place{}, err
		}
		var spec resourcev1.Project
		if err := proto.Unmarshal(r.Spec, &spec); err != nil {
			return Place{}, fmt.Errorf("can't read website project %s: %w", p.ID, err)
		}
		if spec.GetWebsite() == nil || len(p.Environments) == 0 {
			continue
		}
		env := p.Environments[0]
		found := ref == p.ID || ref == p.Name
		if !found {
			routes, err := tx.ListResources(ctx, env.ID, string(route.Kind))
			if err != nil {
				return Place{}, err
			}
			var selected *store.Resource
			for i := range routes {
				if selected == nil || routes[i].Name == website.RouteName {
					selected = &routes[i]
				}
			}
			if selected != nil {
				plain, err := tx.Open(ctx, store.InRevision(selected.ID, selected.Kind, selected.Revision), selected.Spec)
				if err != nil {
					return Place{}, fmt.Errorf("can't read website route %s: %w", selected.ID, err)
				}
				var rt resourcev1.Route
				if err := proto.Unmarshal(plain, &rt); err != nil {
					return Place{}, fmt.Errorf("can't read website route %s: %w", selected.ID, err)
				}
				found = rt.GetDomain() == ref
			}
		}
		if found {
			at, _, err := Environment(ctx, tx, p.ID, env.ID)
			return at, err
		}
	}
	return Place{}, store.ErrNotFound
}
