// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
)

// Fail2ban operations recorded on the server activity timeline.
const (
	EventFail2banBanned      = "jail.ban"
	EventFail2banUnbanned    = "jail.unban"
	EventFail2banUnconfirmed = "jail.unconfirmed"
)

// SetFail2BanAddressBan changes one address's ban. Only banning needs a
// passkey. The proof is for this server, jail and canonical address.
func (h *Handler) SetFail2BanAddressBan(ctx context.Context, req *connect.Request[v1.SetFail2BanAddressBanRequest]) (*connect.Response[v1.SetFail2BanAddressBanResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Token != nil || h.db == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("sign in to manage fail2ban bans"))
	}
	m := req.Msg
	addr, err := netip.ParseAddr(m.GetAddress())
	if err != nil || addr.Zone() != "" {
		return nil, authapi.Invalid(errors.New("enter one IP address, such as 203.0.113.7"))
	}
	address := addr.Unmap().String()
	if m.GetBanned() {
		if address == callersAddress(ctx) {
			return nil, authapi.Invalid(errors.New("this is your current address. Choose another address to ban"))
		}
		if !addr.Unmap().IsGlobalUnicast() {
			return nil, authapi.Invalid(errors.New("choose a remote unicast address"))
		}
	}
	jail := m.GetJail()
	if !validJailName(jail) {
		return nil, authapi.Invalid(errors.New("choose a jail from the list"))
	}
	srv, session, err := h.session(ctx, m.GetServerId(), "which fail2ban jails are running")
	if err != nil {
		return nil, err
	}
	action := EventFail2banUnbanned
	if m.GetBanned() {
		if srv.GetId() == h.fleet.ControlServer().GetId() {
			if name := h.agents.ServerAtAddress(addr, srv.GetId()); name != "" {
				return nil, authapi.Invalid(fmt.Errorf("this address is how %s's agent connects to the control server. Choose another address", name))
			}
		}
		_, err = h.sshWrite(ctx, m.GetProof(), srv.GetId()+":"+jail+":"+address,
			auth.StepUpBanFail2banAddress, v1.StepUpAction_STEP_UP_ACTION_BAN_FAIL2BAN_ADDRESS)
		if err != nil {
			return nil, err
		}
		action = EventFail2banBanned
	}
	ctx = context.WithoutCancel(ctx)
	out, err := session.SetFail2banBan(ctx, &v1.SetFail2BanAddressBanRequest{Jail: jail, Address: address, Banned: m.GetBanned()})
	if err != nil {
		if resource.ReasonOf(err) == resource.ReasonSSHRefused {
			return nil, h.sshWriteError(ctx, p, srv, action, address, "fail2ban", "change this ban", err)
		}
		if !errors.Is(err, agentapi.ErrAgentOutdated) {
			operation := "unban"
			if m.GetBanned() {
				operation = "ban"
			}
			h.sshRecord(ctx, p, srv.GetId(), EventFail2banUnconfirmed, address, operation+" "+jail)
		}
		return nil, readError(srv, "fail2ban", "change this ban", err)
	}
	h.sshRecord(ctx, p, srv.GetId(), action, address, jail)
	return connect.NewResponse(out), nil
}

func validJailName(name string) bool {
	if name == "" || len(name) > 128 || !letterOrDigit(name[0]) {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if !letterOrDigit(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func letterOrDigit(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
