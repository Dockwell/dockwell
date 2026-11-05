// SPDX-License-Identifier: AGPL-3.0-only

package network_test

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
)

// A macvlan network puts a container on the server's own LAN segment, past
// its firewall, and needs no person.
func TestNetworksThatReachPastTheBridgeAreAccess(t *testing.T) {
	r := resource.NewRegistry(&network.Driver{})
	for _, n := range []*resourcev1.Network{
		{Driver: "macvlan", Options: map[string]string{"parent": "eth0"}},
		{Driver: "ipvlan", Options: map[string]string{"parent": "eth0", "ipvlan_mode": "l2"}},
		{Driver: "bridge", Options: map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "nat-unprotected"}},
	} {
		if err := r.Validate(network.Kind, n); err != nil {
			t.Fatal(err)
		}
		if len(r.HostAccess(network.Kind, n)) == 0 {
			t.Errorf("a %s network with %v counts as no host access", n.Driver, n.Options)
		}
	}
}
