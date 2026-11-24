// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"maps"
	"slices"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource/certificate"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// providerReachNotes says who gains or loses the right to publish and which
// published names are left outside a provider's new reach (ADR 0104).
func (e *Engine) providerReachNotes(ctx context.Context, items []item) ([]Note, error) {
	var out []Note
	for i := range items {
		it := &items[i]
		after, ok := it.spec.(*resourcev1.DnsProvider)
		if !ok || after == nil {
			continue
		}
		before, _ := it.current.(*resourcev1.DnsProvider)
		provider := it.ref.Name
		zones := human.List(after.GetZones())
		switch {
		case after.GetEveryProject():
			if !before.GetEveryProject() {
				out = append(out, Note{
					Reason: "provider_serves_every_project", Ref: it.ref, Names: append([]string{provider}, after.GetZones()...),
					Message: provider + " serves every project from now on, including new ones. Their developers can publish names in " + zones + " and prove the names they publish there by DNS",
				})
			}
			// A project it stops listing keeps publishing, but proves by DNS only the
			// names its own routes publish here.
			for _, project := range before.GetProjects() {
				if slices.Contains(after.GetProjects(), project) {
					continue
				}
				name := e.projectName(ctx, items, project)
				out = append(out, Note{
					Reason: "provider_stops_proving", Ref: it.ref, Names: []string{provider, name},
					Message: name + " can prove by DNS through " + provider + " only the names its routes publish there from now on. Certificates for its other names in " + zones + " may stop renewing",
				})
			}
			continue
		case before.GetEveryProject():
			out = append(out, Note{
				Reason: "provider_serves_selected_projects", Ref: it.ref, Names: []string{provider},
				Message: provider + " no longer serves every project. New projects need an admin to add them before they can publish names through it",
			})
		default:
			for _, project := range after.GetProjects() {
				if dnsprovider.Serves(before, project) {
					continue
				}
				name := e.projectName(ctx, items, project)
				out = append(out, Note{
					Reason: "provider_serves", Ref: it.ref, Names: []string{provider, name},
					Message: provider + " serves " + name + " from now on, so its developers can publish names in " + zones,
				})
			}
		}
		if !before.GetEveryProject() && !slices.ContainsFunc(before.GetProjects(), func(p string) bool { return !dnsprovider.Serves(after, p) }) {
			continue
		}
		lost, names, err := e.reachLost(ctx, items, provider, before, after)
		if err != nil {
			return nil, err
		}
		for _, project := range slices.Sorted(maps.Keys(lost)) {
			name := lost[project]
			note := Note{
				Reason: "provider_stops_serving", Ref: it.ref, Names: []string{provider, name},
				Message: provider + " no longer serves " + name,
			}
			if domains := names[project]; len(domains) > 0 {
				slices.Sort(domains)
				domains = slices.Compact(domains)
				note.Names = append(note.Names, domains...)
				note.Message += ". Its DNS records for " + human.List(domains) + " stay in place. Editing those routes while they publish through " + provider +
					" fails until an admin restores access. Certificates proved through it may stop renewing"
			}
			out = append(out, note)
		}
	}
	return out, nil
}

func (e *Engine) reachLost(ctx context.Context, items []item, provider string, before, after *resourcev1.DnsProvider) (map[string]string, map[string][]string, error) {
	lost, names := map[string]string{}, map[string][]string{}
	for _, project := range before.GetProjects() {
		if !dnsprovider.Serves(after, project) {
			lost[project] = project
		}
	}
	editedRoutes := map[string]*resourcev1.Route{}
	for i := range items {
		it := &items[i]
		if it.ref.Kind == routeKind {
			editedRoutes[it.id], _ = it.spec.(*resourcev1.Route)
		}
	}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		projects, err := tx.ListKind(ctx, string(scope.ProjectKind))
		if err != nil {
			return err
		}
		for _, project := range projects {
			if project.Spec != nil && dnsprovider.Serves(before, project.ID) && !dnsprovider.Serves(after, project.ID) {
				lost[project.ID] = project.Name
			}
		}
		if len(lost) == 0 {
			return nil
		}
		envs, err := tx.ListKind(ctx, string(scope.EnvironmentKind))
		if err != nil {
			return err
		}
		projectsOf := map[string]string{}
		for _, env := range envs {
			if _, ok := lost[env.Scope]; ok {
				projectsOf[env.ID] = env.Scope
			}
		}
		routes, err := tx.ListKind(ctx, string(routeKind))
		if err != nil {
			return err
		}
		for _, r := range routes {
			project := projectsOf[r.Scope]
			if project == "" || r.Spec == nil {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			rt, _ := spec.(*resourcev1.Route)
			if certificate.RoutePublisher(rt) != provider {
				continue
			}
			if edited, ok := editedRoutes[r.ID]; ok && (certificate.RoutePublisher(edited) != provider || edited.GetDomain() != rt.GetDomain()) {
				continue
			}
			names[project] = append(names[project], rt.GetDomain())
		}
		return nil
	})
	return lost, names, err
}

// projectName is a project's name, including projects created in the change.
func (e *Engine) projectName(ctx context.Context, items []item, project string) string {
	for i := range items {
		if items[i].id == project && items[i].ref.Kind == scope.ProjectKind {
			return items[i].ref.Name
		}
	}
	name := project
	_ = e.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, project)
		if err == nil {
			name = r.Name
		}
		return err
	})
	return name
}
