// SPDX-License-Identifier: AGPL-3.0-only

package place_test

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/registrylogin"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/task"
)

// Every kind of resource the control plane runs can be placed by its ID, so a
// list of a project's objects of that kind isn't emptied for a project role,
// and a call naming one isn't refused as unknown. The kinds are those of
// controlKinds in internal/cli/run_control.go, with projects and environments.
func TestEveryKindOfResourceCanBePlaced(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	kinds := []resource.Type{
		&container.Driver{}, &firewall.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, &route.Driver{},
		variable.Type{},
		variable.SecretType{},
		backuppolicy.Type{},
		backuppolicy.Destination{},
		registrylogin.Type{},
		scope.ProjectType{},
		scope.EnvironmentType{},
		&task.Driver{},
		&database.UserDriver{},
		alert.Rule{},
		alert.Channel{},
		alert.Check{},
	}
	err := db.View(t.Context(), func(tx *store.Tx) error {
		for _, k := range kinds {
			prefix := id.Resource
			if p, ok := k.(resource.IDPrefixer); ok {
				prefix = p.IDPrefix()
			}
			// One that isn't there is not found, which a known kind can be.
			if _, err := place.Of(t.Context(), tx, prefix+"_01JD6Z3R8M9QF2K7T4XW5BNCAA"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a %s's ID, %s_…: %v, want not found, as a kind the resolver knows", k.Kind(), prefix, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
