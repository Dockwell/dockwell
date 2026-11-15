// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/resource"
)

// ConnectionProbeTimeout is how long a connection's check may take on its agent:
// the probe's own bound, with the time its container takes to be made and taken away.
const ConnectionProbeTimeout = probe.CheckWait + probe.ConnectWait + 30*time.Second

// ProbesConnections reports an agent that checks a connection from where an app is
// (ADR 0099).
func (s *Session) ProbesConnections() bool { return s.probesConnections }

// ProbeConnection asks the agent to check a connection from an app's networks,
// environment's service, or from a network of the check's own where service is
// empty: req, its keys among it, goes sealed to the app's environment, and what the
// check may reach is the agent's to say.
func (s *Session) ProbeConnection(ctx context.Context, environment, service string, req probe.Request) (*agentv1.ProbeConnectionResult, error) {
	if !s.probesConnections {
		return nil, fmt.Errorf("%s's agent can't check a connection from where an app is yet. Update its agent, then try again", s.name)
	}
	req.Allow, req.Refuse = nil, nil
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	call := &agentv1.ProbeConnectionCall{Environment: environment, Service: service}
	seal := func(on *Session) error {
		values := map[string]string{"request": string(raw)}
		defer clear(values)
		sealed, err := on.seal(resource.ProbeRef(environment), values)
		call.Request = sealed
		return err
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ProbeConnection{ProbeConnection: call}}, ConnectionProbeTimeout, seal)
	clear(raw)
	if err != nil {
		return nil, err
	}
	if r.GetProbeConnection() == nil {
		return nil, ErrOlderAgent
	}
	return r.GetProbeConnection(), nil
}
