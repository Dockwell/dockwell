// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/updates"
)

// UpdatesTimeout is what a read of a server's package updates gets: a refresh of
// its lists, then as many as four reads, as dnf's takes, each within the agent's
// own deadline and its grace, and a minute more (ADR 0043).
const UpdatesTimeout = updates.RefreshTimeout + 4*updates.ReadTimeout + 5*updates.KillGrace + time.Minute

// ReadsUpdates reports an agent that reads its server's package updates.
func (s *Session) ReadsUpdates() bool { return s.readsUpdates }

// ListUpdates asks the agent which package updates wait. With refresh, it
// refreshes the server's package lists first when they're more than a day old,
// and with force whatever their age.
func (s *Session) ListUpdates(ctx context.Context, refresh, force bool) (*dockwellv1.ServerUpdates, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ListUpdates{ListUpdates: &agentv1.ListUpdatesCall{Refresh: refresh, Force: force}}}, UpdatesTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetListUpdates().GetUpdates(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a read of its updates with nothing", s.name)
}
