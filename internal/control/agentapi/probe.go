// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// ProbeTimeout is how long a server has to say whether what a website sends to
// answers: the agent's own wait, and a little for the stream.
const ProbeTimeout = 10 * time.Second

// Probe asks the agent whether what a website sends to answers, from its server.
func (s *Session) Probe(ctx context.Context, call *agentv1.ProbeCall) (*agentv1.ProbeResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Probe{Probe: call}}, ProbeTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetProbe(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a probe with something else", s.name)
}
