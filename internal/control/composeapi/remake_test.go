// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"slices"
	"testing"
)

// What Swarm gives a task's container, an address pinned on each network and a
// stack's volume's options, doesn't keep its copy from being made; anything else
// a take-on refuses still does.
func TestWhatSwarmGivesATaskIsLeftBehind(t *testing.T) {
	refused := []string{"ipv4_address on dokploy-network", "VolumeOptions on /var/lib/postgresql/data", "driver_opts on dokploy-network", "a tmpfs mount at /run"}
	if got := slices.DeleteFunc(slices.Clone(refused), swarmOnly); !slices.Equal(got, []string{"driver_opts on dokploy-network", "a tmpfs mount at /run"}) {
		t.Errorf("left %v", got)
	}
}
