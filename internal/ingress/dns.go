// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
	// Told is what the control plane knows of this server's public addresses, beyond
	// its interfaces.
	Told []*agentv1.KnownAddress
	// Carrier says this server is behind carrier-grade NAT, with no way in known.
	Carrier bool
	// Firewalls says which of them to open, on a cloud whose provider is known.
	Firewalls string
}

// firewallWords say where a provider's own firewall is, beside the machine's.
var firewallWords = map[dockwellv1.CloudProvider]string{
	dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE: "Oracle has two firewalls, and most people only open one: the VCN's security list, or a network security group in its place, and the machine's own. " +
		"On Oracle's Ubuntu images, insert the rule before the REJECT one with iptables -I, as one appended with -A comes after it and does nothing. On Oracle Linux, open both ports with firewall-cmd.",
	dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS:          "Open both ports in the instance's security group, and in the machine's own firewall.",
	dockwellv1.CloudProvider_CLOUD_PROVIDER_GCP:          "Open both ports in the VPC's firewall rules, or tick Allow HTTP traffic and Allow HTTPS traffic on the VM, and in the machine's own firewall.",
	dockwellv1.CloudProvider_CLOUD_PROVIDER_AZURE:        "Open both ports in the network security group on the VM's network interface and in the one on its subnet, as either can block them, and in the machine's own firewall.",
	dockwellv1.CloudProvider_CLOUD_PROVIDER_DIGITALOCEAN: "Open both ports in DigitalOcean's Cloud Firewall, if the Droplet has one, and in the machine's own firewall.",
	dockwellv1.CloudProvider_CLOUD_PROVIDER_HETZNER:      "Open both ports in Hetzner's Cloud Firewall, if the server has one, and in the machine's own firewall.",
}

// Know takes what the control plane says of this server's public addresses.
func (d *DNS) Know(known *agentv1.PublicAddress) {
	d.Told, d.Carrier = known.GetAddresses(), known.GetCarrierNat()
	d.Firewalls = firewallWords[known.GetProvider()]
}

// Check says what DNS says of a domain. It never fails: what it can't find
// out, it says it can't.
func (d *DNS) Check(ctx context.Context, domain string) *resource.DomainCheck {
	ctx, cancel := context.WithTimeout(ctx, dnsWait)
	defer cancel()
	out := &resource.DomainCheck{Domain: domain, Carrier: d.Carrier, Firewalls: d.Firewalls, ReadAt: time.Now()}
	found, err := d.lookup(ctx, domain)
	own, herr := d.addrs()
	here := own
	var wayIn []netip.Addr
	for _, known := range d.Told {
		a, perr := netip.ParseAddr(known.GetAddress())
		if perr != nil || !resource.IsPublicAddress(a) {
			continue
		}
		here = append(slices.Clip(here), a.Unmap())
		out.Public = append(out.Public, a.Unmap().String())
		if known.GetWayIn() {
			wayIn = append(wayIn, a.Unmap())
		} else {
			out.Unproven = append(out.Unproven, a.Unmap().String())
		}
	}
	for _, a := range own {
		if resource.IsPublicAddress(a) {
			wayIn = append(wayIn, a)
		}
	}
	if herr == nil {
		out.Here = addrWords(here)
	}
	out.Found = addrWords(found)
	// A record can only be wrong against a way in of its own family: an A record
	// says nothing of a server whose way in is known only for IPv6.
	noWayIn := func(a netip.Addr) bool {
		return resource.IsPublicAddress(a) && !slices.ContainsFunc(wayIn, func(in netip.Addr) bool { return in.Is4() == a.Unmap().Is4() })
	}
	out.Proxied = slices.ContainsFunc(found, IsCloudflare)
	switch {
	case isNoRecord(err) || (err == nil && len(found) == 0):
		out.State = resource.DomainNoRecord
	case err != nil:
		out.State = resource.DomainUnknown
	case herr != nil:
		out.State = resource.DomainUnknown
	case slices.ContainsFunc(found, func(a netip.Addr) bool { return slices.Contains(here, a.Unmap()) }):
		out.State = resource.DomainHere
	case slices.ContainsFunc(found, noWayIn):
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

// sysClassNet is where the kernel lists network interfaces.
var sysClassNet = "/sys/class/net"

// isTun reports a TUN or TAP device, by kind: the kernel gives only those a
// tun_flags file. Such a device is a program's, as mihomo's or a VPN's, and the
// internet doesn't reach the server through it, whatever address it took. Being
// point-to-point doesn't tell, as a PPPoE link is too, with the server's real address.
func isTun(name string) bool {
	_, err := os.Stat(filepath.Join(sysClassNet, name, "tun_flags"))
	return err == nil
}

// ownInterface is one interface and its addresses.
type ownInterface struct {
	name  string
	addrs []netip.Addr
}

// ownInterfaces are the interfaces that are up, with their addresses beyond
// loopback and link-local.
func ownInterfaces() ([]ownInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ownInterface
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		own := ownInterface{name: i.Name}
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().IsGlobalUnicast() {
				own.addrs = append(own.addrs, p.Addr().Unmap())
			}
		}
		out = append(out, own)
	}
	return out, nil
}

// serving keeps the addresses of all a domain could point at, without Docker's own
// bridges or a TUN's.
func serving(all []ownInterface) []netip.Addr {
	var out []netip.Addr
	for _, i := range all {
		if i.name == "docker0" || strings.HasPrefix(i.name, "br-") || strings.HasPrefix(i.name, "veth") || strings.HasPrefix(i.name, "dw-") || isTun(i.name) {
			continue
		}
		out = append(out, i.addrs...)
	}
	return out
}

// interfaceAddrs are this server's addresses a domain could point at.
func interfaceAddrs() ([]netip.Addr, error) {
	all, err := ownInterfaces()
	if err != nil {
		return nil, err
	}
	return serving(all), nil
}

// OwnAddress is outbound, the address this server's route to the internet leaves
// from, unless that's a TUN's, as it is with mihomo routing everything: then it's
// this server's own, as nothing else can reach it at the TUN's.
func OwnAddress(outbound string) string {
	all, err := ownInterfaces()
	if err != nil {
		return outbound
	}
	return ownAddress(outbound, all)
}

func ownAddress(outbound string, all []ownInterface) string {
	a, err := netip.ParseAddr(outbound)
	if err != nil {
		return outbound
	}
	onTun := slices.ContainsFunc(all, func(i ownInterface) bool { return slices.Contains(i.addrs, a.Unmap()) && isTun(i.name) })
	if !onTun {
		return outbound
	}
	own := serving(all)
	if at := slices.IndexFunc(own, netip.Addr.Is4); at >= 0 {
		return own[at].String()
	}
	if len(own) > 0 {
		return own[0].String()
	}
	return outbound
}
