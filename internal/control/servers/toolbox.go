// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
)

// GetHostToolbox returns the settings the server reports for its toolbox.
func (s *Service) GetHostToolbox(ctx context.Context, req *connect.Request[v1.GetHostToolboxRequest]) (*connect.Response[v1.GetHostToolboxResponse], error) {
	srv, err := s.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	var session *agentapi.Session
	if s.agents != nil {
		session, _ = s.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	}
	if session == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s's agent isn't connected. Check the server, then try again", srv.GetName()), authapi.ReasonUnavailable)
	}
	status, err := session.HostToolbox(ctx)
	if errors.Is(err, agentapi.ErrOlderAgent) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, authapi.ReasonAgentOutdated)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&v1.GetHostToolboxResponse{Status: status}), nil
}
