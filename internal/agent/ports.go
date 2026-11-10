// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/ingress"
)

// maxPortsAsked bounds the ports one call asks about.
const maxPortsAsked = 64

// portsHeld says what holds each port asked, as the kernel and Docker say, in
// the order asked, for a form that offers a free port and says what holds a
// typed one. It reads when asked, and keeps nothing.
func portsHeld(ctx context.Context, h *ingress.Holders, asked []*dockwellv1.ServerPortAsk) (*agentv1.PortsResult, error) {
	if len(asked) > maxPortsAsked {
		return nil, fmt.Errorf("%d ports were asked about, and an agent answers %d at most. Ask for fewer", len(asked), maxPortsAsked)
	}
	ports := make([]ingress.Port, 0, len(asked))
	for _, a := range asked {
		if a.GetPort() == 0 || a.GetPort() > 65535 {
			return nil, fmt.Errorf("%d isn't a port. Use one from 1 to 65535", a.GetPort())
		}
		ports = append(ports, ingress.Port{Number: int(a.GetPort()), UDP: a.GetUdp()})
	}
	holders, err := h.Of(ctx, ports)
	if err != nil {
		return nil, fmt.Errorf("the server can't say what holds its ports: %w", err)
	}
	by := make(map[ingress.Port]ingress.Holder, len(holders))
	for _, held := range holders {
		by[held.Port] = held
	}
	out := &agentv1.PortsResult{}
	for _, a := range asked {
		res := &dockwellv1.ServerPortHeld{Port: a.GetPort(), Udp: a.GetUdp()}
		if held, ok := by[ingress.Port{Number: int(a.GetPort()), UDP: a.GetUdp()}]; ok {
			res.Held = true
			if !held.Unknown {
				res.By = held.Words
			}
		}
		out.Ports = append(out.Ports, res)
	}
	return out, nil
}

// errNoPorts is what an agent with no Docker says of a port check, as what
// containers publish is half of what it reads.
var errNoPorts = errors.New("this agent can't say what holds its ports, as it has no Docker driver")
