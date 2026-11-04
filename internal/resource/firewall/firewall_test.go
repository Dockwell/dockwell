// SPDX-License-Identifier: AGPL-3.0-only

package firewall_test

import (
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

func TestValidateMakesEqualRulesEqual(t *testing.T) {
	var d firewall.Driver
	r := &resourcev1.FirewallRule{Port: 443, Sources: []string{"203.0.113.9/24", "198.51.100.4", "203.0.113.0/24"}}
	if err := d.Validate(r); err != nil {
		t.Fatal(err)
	}
	if want := []string{"198.51.100.4/32", "203.0.113.0/24"}; !slices.Equal(r.GetSources(), want) {
		t.Errorf("sources %v, want %v", r.GetSources(), want)
	}
	if r.GetAction() != resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW || r.GetProtocol() != resourcev1.Protocol_PROTOCOL_TCP {
		t.Errorf("defaults: %v %v", r.GetAction(), r.GetProtocol())
	}
	for _, bad := range []*resourcev1.FirewallRule{{}, {Port: 70000}, {Port: 80, Sources: []string{"office"}}} {
		if err := d.Validate(bad); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
}
