// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// ReadTunnelsTimeout is how long reading a server's tunnels may take: a look through
// /proc, and a /ready asked of each cloudflared.
const ReadTunnelsTimeout = 20 * time.Second

// ReadsTunnels reports an agent that reads the Cloudflare tunnels cloudflared runs on
// its server (ADR 0104).
func (s *Session) ReadsTunnels() bool { return s.readsTunnels }

// ReadTunnels asks the agent which Cloudflare tunnels cloudflared runs on its server
// now, each with the connector ID of its run, which changes as cloudflared restarts:
// what Cloudflare's connections list proves the tunnel by. No token is in what it
// answers.
func (s *Session) ReadTunnels(ctx context.Context) ([]*dockwellv1.CloudflaredTunnel, error) {
	if !s.readsTunnels {
		return nil, fmt.Errorf("%s's agent can't read Cloudflare tunnels yet. Update its agent, then try again", s.name)
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ReadTunnels{ReadTunnels: &agentv1.ReadTunnelsCall{}}}, ReadTunnelsTimeout, nil)
	if err != nil {
		return nil, err
	}
	if r.GetReadTunnels() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetReadTunnels().GetTunnels(), nil
}
