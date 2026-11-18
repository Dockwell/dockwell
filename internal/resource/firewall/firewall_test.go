// SPDX-License-Identifier: AGPL-3.0-only

package firewall_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
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
	for _, bad := range []*resourcev1.FirewallRule{{}, {Port: 70000}, {Port: 80, Sources: []string{"office"}}, {Port: 80, Sources: []string{"fe80::1%eth0"}}} {
		if err := d.Validate(bad); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

// reaching is a firewall that knows how the server is reached, or doesn't.
type reaching struct {
	*firewall.Fake
	ports []uint16
	known bool
}

func (r reaching) WaysIn() ([]uint16, bool) { return r.ports, r.known }

// "May cut off access" is for a rule that can: a deny of a port the server is
// reached by, SSH's as the server has it or the control plane's own, or an allow
// of one deleted or narrowed. A deny of another port, as a database's external
// access keeps, says nothing; and where the ports can't be read, every deny and
// narrowing still warns (#850).
func TestOnlyARuleThatCanCutAccessWarns(t *testing.T) {
	rule := func(action resourcev1.FirewallAction, port uint32, protocol resourcev1.Protocol, sources ...string) *resourcev1.FirewallRule {
		return &resourcev1.FirewallRule{Action: action, Port: port, Protocol: protocol, Sources: sources}
	}
	deny, allow := resourcev1.FirewallAction_FIREWALL_ACTION_DENY, resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW
	tcp, udp := resourcev1.Protocol_PROTOCOL_TCP, resourcev1.Protocol_PROTOCOL_UDP
	// SSH on 2222, read from the server, and the panel and its agents on 7600 and 7601.
	known := reaching{Fake: firewall.NewFake(), ports: []uint16{2222, 7600, 7601}, known: true}
	unknown := reaching{Fake: firewall.NewFake()}
	for _, tt := range []struct {
		name    string
		backend firewall.Backend
		was, r  *resourcev1.FirewallRule
		warns   bool
	}{
		{"a deny of a database's port, from anywhere", known, nil, rule(deny, 5432, tcp), false},
		{"external access's deny, beside its allow", known, nil, rule(deny, 5432, tcp, "0.0.0.0/0"), false},
		{"a deny of 22, where SSH listens on 2222", known, nil, rule(deny, 22, tcp), false},
		{"a deny of SSH's port", known, nil, rule(deny, 2222, tcp), true},
		{"a deny of SSH's port from one range", known, nil, rule(deny, 2222, tcp, "198.51.100.0/24"), true},
		{"a deny of the panel's port", known, nil, rule(deny, 7600, tcp), true},
		{"a deny of the agents' port", known, nil, rule(deny, 7601, tcp), true},
		{"a deny of SSH's port over UDP", known, nil, rule(deny, 2222, udp), false},
		{"deleting an allow of SSH's port", known, rule(allow, 2222, tcp), nil, true},
		{"narrowing an allow of SSH's port", known, rule(allow, 2222, tcp), rule(allow, 2222, tcp, "203.0.113.0/24"), true},
		{"moving an allow off SSH's port", known, rule(allow, 2222, tcp), rule(allow, 8080, tcp), true},
		{"deleting an allow of an app's port", known, rule(allow, 8080, tcp), nil, false},
		{"an allow of anything", known, nil, rule(allow, 2222, tcp), false},
		{"a deny of a database's port, where SSH's can't be read", unknown, nil, rule(deny, 5432, tcp), true},
		{"deleting an app's allow, where SSH's can't be read", unknown, rule(allow, 8080, tcp), nil, true},
		{"a deny of a database's port, on a backend that can't say", firewall.NewFake(), nil, rule(deny, 5432, tcp), true},
	} {
		var actual, desired proto.Message
		if tt.was != nil {
			actual = tt.was
		}
		if tt.r != nil {
			desired = tt.r
		}
		dec, err := (&firewall.Driver{Backend: tt.backend}).Plan(actual, desired, nil)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := slices.Contains(dec.Warnings, resource.WarningMayLockOut); got != tt.warns {
			t.Errorf("%s: may cut off access is %v, want %v", tt.name, got, tt.warns)
		}
	}
}
