// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// ListSSHKeys forwards an SSH request to the server.
func (s *Session) ListSSHKeys(ctx context.Context, req *dockwellv1.ListServerSshKeysRequest) (*dockwellv1.ListServerSshKeysResponse, error) {
	if !s.managesSSH {
		return nil, ErrAgentOutdated
	}
	got, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ListSshKeys{ListSshKeys: req}}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	out := got.GetListSshKeys()
	if out == nil {
		return nil, errors.New("the agent returned no SSH result")
	}
	return out, nil
}

// EditSSHKey forwards an SSH request to the server.
func (s *Session) EditSSHKey(ctx context.Context, req *dockwellv1.EditServerSshKeyRequest) (*dockwellv1.EditServerSshKeyResponse, error) {
	if !s.managesSSH {
		return nil, ErrAgentOutdated
	}
	got, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_EditSshKey{EditSshKey: req}}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	out := got.GetEditSshKey()
	if out == nil {
		return nil, errors.New("the agent returned no SSH result")
	}
	return out, nil
}

// ListSSHSessions forwards an SSH request to the server.
func (s *Session) ListSSHSessions(ctx context.Context, req *dockwellv1.ListServerSshSessionsRequest) (*dockwellv1.ListServerSshSessionsResponse, error) {
	if !s.managesSSH {
		return nil, ErrAgentOutdated
	}
	got, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ListSshSessions{ListSshSessions: req}}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	out := got.GetListSshSessions()
	if out == nil {
		return nil, errors.New("the agent returned no SSH result")
	}
	return out, nil
}

// DisconnectSSHSession forwards an SSH request to the server.
func (s *Session) DisconnectSSHSession(ctx context.Context, req *dockwellv1.DisconnectServerSshSessionRequest) (*dockwellv1.DisconnectServerSshSessionResponse, error) {
	if !s.managesSSH {
		return nil, ErrAgentOutdated
	}
	got, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_DisconnectSshSession{DisconnectSshSession: req}}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	out := got.GetDisconnectSshSession()
	if out == nil {
		return nil, errors.New("the agent returned no SSH result")
	}
	return out, nil
}

// ListSSHLogins forwards an SSH request to the server.
func (s *Session) ListSSHLogins(ctx context.Context, req *dockwellv1.ListServerSshLoginsRequest) (*dockwellv1.ListServerSshLoginsResponse, error) {
	if !s.managesSSH {
		return nil, ErrAgentOutdated
	}
	got, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ListSshLogins{ListSshLogins: req}}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	out := got.GetListSshLogins()
	if out == nil {
		return nil, errors.New("the agent returned no SSH result")
	}
	return out, nil
}

// BanSSHAddress forwards an SSH request to the server.
func (s *Session) BanSSHAddress(ctx context.Context, req *dockwellv1.BanServerSshAddressRequest) (*dockwellv1.BanServerSshAddressResponse, error) {
	if !s.managesSSH {
		return nil, ErrAgentOutdated
	}
	got, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_BanSshAddress{BanSshAddress: req}}, ObserveTimeout, nil)
	if unknownCall(err) {
		return nil, ErrAgentOutdated
	}
	if err != nil {
		return nil, err
	}
	out := got.GetBanSshAddress()
	if out == nil {
		return nil, errors.New("the agent returned no SSH result")
	}
	return out, nil
}
