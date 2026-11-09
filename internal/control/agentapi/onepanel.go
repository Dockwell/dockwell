// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// OnePanelTimeout is how long a read of 1Panel's records may take: a few tables
// of a database on the server's own disk, read twice where 1Panel writes as it goes.
const OnePanelTimeout = time.Minute

// ReadsOnePanel reports an agent that reads what 1Panel keeps (ADR 0056).
func (s *Session) ReadsOnePanel() bool { return s.readsOnePanel }

// ReadOnePanel asks the agent what 1Panel keeps on its server, read-only.
func (s *Session) ReadOnePanel(ctx context.Context) (*agentv1.ReadOnePanelResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ReadOnepanel{ReadOnepanel: &agentv1.ReadOnePanelCall{}}}, OnePanelTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetReadOnepanel(); out.GetFound() != nil || out.GetNotHere() != "" {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a read of 1Panel's records with nothing", s.name)
}
