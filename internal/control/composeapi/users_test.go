// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A database user the plan makes goes right after the server it's made in, where the
// plan makes that too, so it's there before the services that sign in as it start; a
// user in a server that runs already, or in another environment, stays where it is.
func TestAUserGoesAfterItsServer(t *testing.T) {
	edit := func(kind, scope, name, server string) *dockwellv1.Edit {
		e := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: kind, Scope: scope, Name: name}}
		if server != "" {
			e.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: &resourcev1.DatabaseUser{Service: server}}}
		}
		return e
	}
	edits := []*dockwellv1.Edit{
		edit("secret", "env_A", "PASSWORD", ""),
		edit("database.user", "env_A", "blog", "db"),
		edit("database.user", "env_B", "shop", "db"),
		edit("database.user", "env_A", "wiki", "elsewhere"),
		edit("volume", "env_A", "db-data", ""),
		edit("service", "env_A", "db", ""),
		edit("service", "env_A", "app", ""),
	}
	var got []string
	for _, e := range usersAfterTheirServers(edits, "env_A") {
		got = append(got, e.GetResource().GetKind()+" "+e.GetResource().GetScope()+"/"+e.GetResource().GetName())
	}
	want := []string{
		"secret env_A/PASSWORD", "database.user env_B/shop", "database.user env_A/wiki", "volume env_A/db-data",
		"service env_A/db", "database.user env_A/blog", "service env_A/app",
	}
	if !slices.Equal(got, want) {
		t.Errorf("order\n%v\nwant\n%v", got, want)
	}
}
