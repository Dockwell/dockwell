// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/resource/firewall"
)

var _ firewall.Reacher = (*Manager)(nil)

// The ways in are SSH's ports as the server has them, those Dockwell keeps
// pinned, and the control plane's own; and nothing known where SSH's ports
// can't be read, so the firewall's plan warns as before (#850).
func TestTheWaysInAreSSHsPortsAndTheControlPlanes(t *testing.T) {
	pins := func(p Pins, err error) func(context.Context) (Pins, error) {
		return func(context.Context) (Pins, error) { return p, err }
	}
	m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: pins(Pins{SSHPorts: []uint16{2222}, Own: []uint16{7600, 7601}}, nil)}
	m.confirmed.Pinned.SSHPorts = []uint16{2200}
	ports, known := m.WaysIn()
	slices.Sort(ports)
	if !known || !slices.Equal(ports, []uint16{2200, 2222, 7600, 7601}) {
		t.Errorf("the ways in are %v, %v; want SSH's, the pinned and the control plane's", ports, known)
	}
	for name, read := range map[string]func(context.Context) (Pins, error){
		"pins that can't be read": pins(Pins{}, errors.New("no /proc")),
		"no SSH port found":       pins(Pins{Own: []uint16{7600}}, nil),
	} {
		m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: read}
		if ports, known := m.WaysIn(); known {
			t.Errorf("%s: known as %v, want unknown", name, ports)
		}
	}
	if _, known := (&Manager{Dir: t.TempDir(), Table: &table{}}).WaysIn(); known {
		t.Error("with no way to read pins, the ways in are known")
	}
}
