// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// dnsWait bounds one lookup, as a plan waits for it.
const dnsWait = 2 * time.Second

// DNS reads what a domain points at, and what this server's addresses are.
type DNS struct {
	// Lookup finds a domain's addresses. Nil means this server's resolver.
	Lookup func(ctx context.Context, domain string) ([]netip.Addr, error)
	// Addrs finds this server's addresses. Nil means its interfaces.
	Addrs func() ([]netip.Addr, error)
}

// Check says what DNS says of a domain. It never fails: what it can't find
// out, it says it can't.
func (d *DNS) Check(ctx context.Context, domain string) *resource.DomainCheck {
	ctx, cancel := context.WithTimeout(ctx, dnsWait)
	defer cancel()
	out := &resource.DomainCheck{Domain: domain}
	found, err := d.lookup(ctx, domain)
	here, herr := d.addrs()
	if herr == nil {
		out.Here = addrWords(here)
	}
	out.Found = addrWords(found)
	switch {
	case isNoRecord(err) || (err == nil && len(found) == 0):
		out.State = resource.DomainNoRecord
	case err != nil:
		out.State = resource.DomainUnknown
	case herr != nil:
		out.State = resource.DomainUnknown
	case slices.ContainsFunc(found, func(a netip.Addr) bool { return slices.Contains(here, a.Unmap()) }):
		out.State = resource.DomainHere
	case !slices.ContainsFunc(here, isPublic) && slices.ContainsFunc(found, isPublic):
		// Behind NAT, as on many clouds: the public address isn't on an
		// interface, so it may be this server's or not.
		out.State = resource.DomainCantTell
	default:
		out.State = resource.DomainElsewhere
	}
	return out
}

func (d *DNS) lookup(ctx context.Context, domain string) ([]netip.Addr, error) {
	if d.Lookup != nil {
		return d.Lookup(ctx, domain)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", domain)
}

func (d *DNS) addrs() ([]netip.Addr, error) {
	if d.Addrs != nil {
		return d.Addrs()
	}
	return interfaceAddrs()
}

func isNoRecord(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func isPublic(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate()
}

func addrWords(list []netip.Addr) []string {
	var out []string
	for _, a := range list {
		if s := a.Unmap().String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// interfaceAddrs are this server's addresses a domain could point at, without
// loopback, link-local, or Docker's own bridges.
func interfaceAddrs() ([]netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 ||
			i.Name == "docker0" || strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "veth") || strings.HasPrefix(i.Name, "dw-") {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().IsGlobalUnicast() {
				out = append(out, p.Addr().Unmap())
			}
		}
	}
	return out, nil
}
