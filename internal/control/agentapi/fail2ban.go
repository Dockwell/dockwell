// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// SetFail2banBan changes one ban on the agent's server.
func (s *Session) SetFail2banBan(ctx context.Context, req *v1.SetFail2BanAddressBanRequest) (*v1.SetFail2BanAddressBanResponse, error) {
	if !s.managesFirewall {
		return nil, ErrAgentOutdated
	}
	result, err := s.call(ctx, &agentv1.Call{
		Call: &agentv1.Call_SetFail2BanAddressBan{SetFail2BanAddressBan: req},
	}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	if out := result.GetSetFail2BanAddressBan(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent returned no fail2ban result")
}
