// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

var _ firewall.Reacher = (*Manager)(nil)

// The ways in over TCP are SSH's ports as the server has them, those Dockwell
// keeps pinned, the control plane's own and those a proxy holds for it; over
// UDP, the tunnels' ports. Each is unknown where it can't be read, so the
// firewall's plan warns as before (#850, and its review's H1 and M1).
func TestTheWaysInAreSSHsPortsTheControlPlanesAndTheTunnels(t *testing.T) {
	pins := func(p Pins, err error) func(context.Context) (Pins, error) {
		return func(context.Context) (Pins, error) { return p, err }
	}
	tunnels := func(ts ...Tunnel) func(context.Context) []Tunnel {
		return func(context.Context) []Tunnel { return ts }
	}
	m := &Manager{
		Dir: t.TempDir(), Table: &table{},
		Pins:    pins(Pins{SSHPorts: []uint16{2222}, Own: []uint16{7600, 7601}}, nil),
		Proxied: []uint16{443, 80},
		Tunnels: tunnels(Tunnel{Name: "wg0", Port: 51820}),
	}
	m.confirmed.Pinned.SSHPorts = []uint16{2200}
	ways := m.WaysIn()
	slices.Sort(ways.TCP)
	if !ways.TCPKnown || !slices.Equal(ways.TCP, []uint16{80, 443, 2200, 2222, 7600, 7601}) {
		t.Errorf("the ways in over TCP are %v, %v; want SSH's, the pinned, the control plane's and the proxy's", ways.TCP, ways.TCPKnown)
	}
	if !ways.UDPKnown || !slices.Equal(ways.UDP, []uint16{51820}) {
		t.Errorf("the ways in over UDP are %v, %v; want wg0's", ways.UDP, ways.UDPKnown)
	}

	for name, m := range map[string]*Manager{
		"pins that can't be read": {Pins: pins(Pins{}, errors.New("no /proc"))},
		"no SSH port found":       {Pins: pins(Pins{Own: []uint16{7600}}, nil)},
		"no way to read pins":     {},
	} {
		m.Dir, m.Table = t.TempDir(), &table{}
		if ways := m.WaysIn(); ways.TCPKnown {
			t.Errorf("%s: known over TCP as %v, want unknown", name, ways.TCP)
		}
	}
	for name, read := range map[string]func(context.Context) []Tunnel{
		"a tunnel whose port can't be read": tunnels(Tunnel{Name: "wg0"}),
		"no way to read tunnels":            nil,
	} {
		m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: pins(Pins{SSHPorts: []uint16{22}}, nil), Tunnels: read}
		if ways := m.WaysIn(); ways.UDPKnown {
			t.Errorf("%s: known over UDP as %v, want unknown", name, ways.UDP)
		}
	}
	none := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: pins(Pins{SSHPorts: []uint16{22}}, nil), Tunnels: tunnels()}
	if ways := none.WaysIn(); !ways.UDPKnown || len(ways.UDP) != 0 {
		t.Errorf("with no tunnel, the ways in over UDP are %v, %v; want none, known", ways.UDP, ways.UDPKnown)
	}
}

// A plan reads the server once, for every check of every rule in it; outside
// one, each check reads it anew, so a plan after someone signs in sees them
// (#850's review, L3).
func TestAPlanReadsTheServerOnce(t *testing.T) {
	reads := 0
	m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: func(context.Context) (Pins, error) {
		reads++
		return Pins{SSHPorts: []uint16{22}, Own: []uint16{7600}}, nil
	}}
	rule := func(port uint32) *resourcev1.FirewallRule {
		return &resourcev1.FirewallRule{Port: port, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	}
	plan := func(port uint32) {
		m.WaysIn()
		_ = m.CheckRule(nil, rule(port))
		m.StaysAfterRule(nil, rule(port))
	}
	done := m.Planning()
	plan(5432)
	plan(5433)
	done()
	if reads != 1 {
		t.Errorf("a plan of two rules read the server %d times, want once", reads)
	}
	plan(5432)
	if reads != 4 {
		t.Errorf("checks outside a plan read the server %d times in all, want 3 more", reads)
	}
	done() // twice is once
	if m.planning != 0 {
		t.Errorf("planning is %d after done, want 0", m.planning)
	}
}
