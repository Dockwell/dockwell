// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// What Swarm gives a task's container, an address pinned on each network and a
// stack's volume's options, doesn't keep its copy from being made; anything else
// a take-on refuses still does.
func TestWhatSwarmGivesATaskIsLeftBehind(t *testing.T) {
	refused := []string{
		"ipv4_address on dokploy-network", "VolumeOptions.Labels on /var/lib/postgresql/data", "VolumeOptions.DriverConfig on /data",
		"VolumeOptions.Subpath on /srv", "VolumeOptions.NoCopy on /cache", "driver_opts on dokploy-network", "a tmpfs mount at /run",
	}
	if got := slices.DeleteFunc(slices.Clone(refused), swarmOnly); !slices.Equal(got, []string{"VolumeOptions.Subpath on /srv", "VolumeOptions.NoCopy on /cache", "driver_opts on dokploy-network", "a tmpfs mount at /run"}) {
		t.Errorf("left %v", got)
	}
}

// A copy's names on one network alone go as its aliases do, so it never keeps a
// name on a stack's network it doesn't join, which would fail its spec (the narrow
// check of ADR 0097).
func TestACopyKeepsNoNameOnANetworkItLeaves(t *testing.T) {
	app := &resourcev1.Container{
		Image: "nginx:1.27", ExternalNetworks: []string{"dokploy-network", "shop_back"},
		Aliases: []string{"web"}, NetworkAliases: []string{"shop_back=storage"},
	}
	remade(app, Remake{Container: "shop_web.1.abc", Service: "web", Stack: "shop"})
	if len(app.GetNetworkAliases()) != 0 || !slices.Equal(app.GetExternalNetworks(), []string{"dokploy-network"}) {
		t.Errorf("the copy keeps %v on %v", app.GetNetworkAliases(), app.GetExternalNetworks())
	}
	if err := container.Validate(app); err != nil {
		t.Errorf("the copy's spec = %v", err)
	}
}
