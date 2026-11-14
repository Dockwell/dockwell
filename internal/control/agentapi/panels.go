// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// PanelTimeout is how long a read of what a panel runs may take: Docker's
// containers and services, and a few small files.
const PanelTimeout = time.Minute

// ReadsPanels reports an agent that reads what Coolify or Dokploy runs (ADR 0089).
func (s *Session) ReadsPanels() bool { return s.readsPanels }

// StopsPanels reports an agent that stops and starts Coolify's or Dokploy's own
// containers as a step of a change.
func (s *Session) StopsPanels() bool { return s.stopsPanels }

// ReadPanel asks the agent what a panel runs on its server, read-only, from Docker
// and the panel's folders.
func (s *Session) ReadPanel(ctx context.Context, kind dockwellv1.PanelKind) (*agentv1.ReadPanelResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ReadPanel{ReadPanel: &agentv1.ReadPanelCall{Panel: kind}}}, PanelTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetReadPanel(); out.GetFound() != nil || out.GetNotHere() != "" {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a read of what a panel runs with nothing", s.name)
}
