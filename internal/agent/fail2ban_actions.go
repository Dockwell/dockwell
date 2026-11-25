// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func (f *fail2banReader) setBan(ctx context.Context, req *v1.SetFail2BanAddressBanRequest) error {
	if f == nil {
		return errors.New("this agent can't change fail2ban bans")
	}
	addr, err := netip.ParseAddr(req.GetAddress())
	name := req.GetJail()
	if err != nil || addr.Zone() != "" || !isJailArg(name) {
		return banRefused(errors.New("choose a running jail and enter one IP address"))
	}
	addr = addr.Unmap()
	status, err := f.client(ctx, "status")
	if err != nil {
		return err
	}
	if !slices.Contains(jailList(status), name) {
		return banRefused(errors.New("this jail is no longer running. Refresh the list and choose a running jail"))
	}
	command := "unbanip"
	if req.GetBanned() {
		if err := f.checkManualBan(ctx, name, addr); err != nil {
			return banRefused(err)
		}
		command = "banip"
	}
	defer f.forget()
	_, err = f.client(ctx, "set", name, command, addr.String())
	return err
}

func banRefused(err error) error {
	return resource.PlainlyBecause(resource.ReasonSSHRefused, err.Error(), err)
}

func (f *fail2banReader) checkManualBan(ctx context.Context, name string, addr netip.Addr) error {
	if !addr.IsGlobalUnicast() {
		return errors.New("choose a remote unicast address")
	}
	if f.protected == nil {
		return errors.New("the agent can't check the control server's addresses. Try again after it reconnects")
	}
	control, err := f.protected(ctx)
	if err != nil {
		return fmt.Errorf("the agent can't check the control server's addresses: %w", err)
	}
	if slices.ContainsFunc(control, func(text string) bool { return containsAddress(text, addr) }) {
		return errors.New("this address belongs to the control server. Choose another address")
	}
	local, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("the agent can't check this server's addresses: %w", err)
	}
	for _, own := range local {
		address, _, _ := strings.Cut(own.String(), "/")
		if containsAddress(address, addr) {
			return errors.New("this address belongs to the server. Choose another address")
		}
	}
	if f.sessions != nil && slices.ContainsFunc(f.sessions(ctx), func(text string) bool { return containsAddress(text, addr) }) {
		return errors.New("this address has an open SSH session. Choose another address")
	}
	ignore, err := f.client(ctx, "get", name, "ignoreip")
	if err != nil {
		return fmt.Errorf("the agent can't check the jail's ignored addresses: %w", err)
	}
	if slices.ContainsFunc(ignored(ignore), func(text string) bool { return containsAddress(text, addr) }) {
		return errors.New("this address is on the jail's ignore list. Choose another address")
	}
	return nil
}

func containsAddress(text string, addr netip.Addr) bool {
	if a, err := netip.ParseAddr(text); err == nil {
		return a.Unmap() == addr
	}
	p, err := netip.ParsePrefix(text)
	if err != nil {
		return false
	}
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Contains(addr)
}
