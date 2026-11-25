// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
)

var _ firewall.Reacher = (*Manager)(nil)

// The ways in over TCP are SSH's ports where sshd holds them, those Dockwell
// keeps pinned, the control plane's own and those in front of it; over UDP,
// every port bound beyond loopback and the tunnels' ports. Each is unknown
// where it can't be read, so the firewall's plan warns as before (#850, its
// review's H1 and M1, and its re-check's N1 to N3).
func TestTheWaysInAreSSHsPortsTheControlPlanesAndTheTunnels(t *testing.T) {
	pins := func(p Pins, err error) func(context.Context) (Pins, error) {
		return func(context.Context) (Pins, error) { return p, err }
	}
	tunnels := func(ts ...Tunnel) func(context.Context) ([]Tunnel, error) {
		return func(context.Context) ([]Tunnel, error) { return ts, nil }
	}
	front := []uint16{8443, 7443}
	m := &Manager{
		Dir: t.TempDir(), Table: &table{},
		Pins:    pins(Pins{SSHPorts: []uint16{2222}, Own: []uint16{7600, 7601}, UDPPorts: []uint16{1194, 68}, UDPRead: true}, nil),
		Proxied: func() []uint16 { return front },
		Tunnels: tunnels(Tunnel{Name: "wg0", Port: 51820}),
	}
	m.confirmed.Pinned.SSHPorts = []uint16{2200}
	ways := m.WaysIn(t.Context())
	slices.Sort(ways.TCP)
	if !ways.TCPKnown || !slices.Equal(ways.TCP, []uint16{2200, 2222, 7443, 7600, 7601, 8443}) {
		t.Errorf("the ways in over TCP are %v, %v; want SSH's, the pinned, the control plane's and those in front of it", ways.TCP, ways.TCPKnown)
	}
	slices.Sort(ways.UDP)
	if !ways.UDPKnown || !slices.Equal(ways.UDP, []uint16{68, 1194, 51820}) {
		t.Errorf("the ways in over UDP are %v, %v; want what's bound beyond loopback and wg0's", ways.UDP, ways.UDPKnown)
	}
	// A port a request names later is a way in from then.
	front = append(front, 9443)
	if ways := m.WaysIn(t.Context()); !slices.Contains(ways.TCP, 9443) {
		t.Errorf("a port seen later isn't a way in: %v", ways.TCP)
	}

	for name, m := range map[string]*Manager{
		"pins that can't be read": {Pins: pins(Pins{}, errors.New("no /proc"))},
		"no SSH port found":       {Pins: pins(Pins{Own: []uint16{7600}}, nil)},
		"SSH's port only as sshd -T names it, held by something else": {
			Pins: pins(Pins{SSHPorts: []uint16{22}, SSHByConfig: true, UDPRead: true}, nil),
		},
		"no way to read pins": {},
	} {
		m.Dir, m.Table = t.TempDir(), &table{}
		if ways := m.WaysIn(t.Context()); ways.TCPKnown {
			t.Errorf("%s: known over TCP as %v, want unknown", name, ways.TCP)
		}
	}
	for name, m := range map[string]*Manager{
		"a tunnel whose port can't be read": {Pins: pins(Pins{SSHPorts: []uint16{22}, UDPRead: true}, nil), Tunnels: tunnels(Tunnel{Name: "wg0"})},
		"tunnels that can't be read": {Pins: pins(Pins{SSHPorts: []uint16{22}, UDPRead: true}, nil), Tunnels: func(context.Context) ([]Tunnel, error) {
			return nil, errors.New("no /sys/class/net")
		}},
		"no way to read tunnels":         {Pins: pins(Pins{SSHPorts: []uint16{22}, UDPRead: true}, nil)},
		"UDP sockets that can't be read": {Pins: pins(Pins{SSHPorts: []uint16{22}}, nil), Tunnels: tunnels()},
	} {
		m.Dir, m.Table = t.TempDir(), &table{}
		if ways := m.WaysIn(t.Context()); ways.UDPKnown {
			t.Errorf("%s: known over UDP as %v, want unknown", name, ways.UDP)
		}
	}
	none := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: pins(Pins{SSHPorts: []uint16{22}, UDPRead: true}, nil), Tunnels: tunnels()}
	if ways := none.WaysIn(t.Context()); !ways.UDPKnown || len(ways.UDP) != 0 {
		t.Errorf("with nothing bound and no tunnel, the ways in over UDP are %v, %v; want none, known", ways.UDP, ways.UDPKnown)
	}
}

// A plan reads the server once, for every check of every rule in it, and each
// plan call reads its own; a check with no plan's context reads anew, so a
// plan after someone signs in sees them (#850's review, L3, and its re-check, N4).
func TestEachPlanReadsTheServerOnce(t *testing.T) {
	reads := 0
	m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: func(context.Context) (Pins, error) {
		reads++
		return Pins{SSHPorts: []uint16{22}, Own: []uint16{7600}, UDPRead: true}, nil
	}}
	rule := func(port uint32) *resourcev1.FirewallRule {
		return &resourcev1.FirewallRule{Port: port, Action: resourcev1.FirewallAction_FIREWALL_ACTION_DENY}
	}
	plan := func(ctx context.Context, port uint32) {
		m.WaysIn(ctx)
		_ = m.CheckRule(ctx, resource.Ref{Kind: firewall.Kind, Name: "test"}, nil, rule(port))
		m.StaysAfterRule(ctx, resource.Ref{Kind: firewall.Kind, Name: "test"}, nil, rule(port))
	}
	one := m.Planning(t.Context())
	plan(one, 5432)
	plan(one, 5433)
	if reads != 1 {
		t.Errorf("a plan of two rules read the server %d times, want once", reads)
	}
	// Another plan call, even while the first is under way, reads its own.
	two := m.Planning(t.Context())
	plan(two, 5432)
	plan(one, 5434)
	if reads != 2 {
		t.Errorf("two plans read the server %d times in all, want twice", reads)
	}
	plan(t.Context(), 5432)
	if reads != 5 {
		t.Errorf("checks with no plan read the server %d times in all, want 3 more", reads)
	}
}
