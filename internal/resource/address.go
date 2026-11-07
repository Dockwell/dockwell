// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/dockwell/dockwell/internal/human"
)

// sharedAddresses are carrier-grade NAT's (RFC 6598).
var sharedAddresses = netip.MustParsePrefix("100.64.0.0/10")

// notReachable and reachableInside are the ranges that decide whether the internet
// can reach an address, from the "Globally Reachable" column of IANA's special-purpose
// registries, as read on 2026-11-07, both last updated 2025-10-09, with multicast,
// which has a registry of its own. The most specific range that holds an address
// decides, which is why reachableInside exists: it's what the registries say is
// reachable inside a range that isn't. A range they leave open, as N/A or blank,
// decides nothing, so Teredo goes by 2001::/23 around it, and 6to4 has none around it.
//
// Three documentation ranges are left out of notReachable against the registry, and
// so count as public, because this repo's tests and examples use them as a server's
// public address: 198.51.100.0/24, 203.0.113.0/24 and 2001:db8::/32. The other two,
// 192.0.2.0/24 and 3fff::/20, aren't used that way, so they follow the registry.
//
// testdata/iana-special-registry-*.csv are those registries, and a test holds this
// to them at both ends of every range. 198.18.0.0/15 matters most in practice:
// mihomo gives its TUN 198.18.0.1 by default, and it's on many servers in China.
var (
	notReachable = prefixes(
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.2/32", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23", "3fff::/20", "5f00::/16",
		"fc00::/7", "fe80::/10", "ff00::/8",
	)
	reachableInside = prefixes(
		"192.0.0.9/32", "192.0.0.10/32",
		"2001:1::1/128", "2001:1::2/128", "2001:1::3/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28", "2001:30::/28",
	)
)

func prefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// mostSpecific is how long the longest prefix of list that holds a is, or -1.
func mostSpecific(list []netip.Prefix, a netip.Addr) int {
	bits := -1
	for _, p := range list {
		if p.Bits() > bits && p.Contains(a) {
			bits = p.Bits()
		}
	}
	return bits
}

// IsPublicAddress reports whether a is an address the internet can route to, so
// the only kind a domain's DNS record can usefully point at. It isn't for a
// private, shared, link-local or loopback address, which are all a server behind
// NAT has of its own, or a benchmarking one, which a TUN takes. This is the one
// definition: isPrivateAddress in the panel's server.ts mirrors it, and
// testdata/addresses.json holds them to the same table.
func IsPublicAddress(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	return mostSpecific(reachableInside, a) >= mostSpecific(notReachable, a)
}

// IsSharedAddress reports an address in carrier-grade NAT's range.
func IsSharedAddress(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && sharedAddresses.Contains(a.Unmap())
}

// PublicAddresses keeps the public addresses of list, as they're written, leaving
// out those that aren't and anything that isn't an address.
func PublicAddresses(list []string) []string {
	var out []string
	for _, s := range list {
		if a, err := netip.ParseAddr(s); err == nil && IsPublicAddress(a) {
			out = append(out, s)
		}
	}
	return out
}

// PointHere says where to point a domain's DNS, given a server's own addresses.
// It names a public address or none, as a record pointed at a private one can't
// work: with only private addresses, it says so, and that the public one is what's
// wanted.
func PointHere(here []string) string {
	public := PublicAddresses(here)
	if len(public) > 0 {
		return "Point an A or AAAA record at this server, " + human.Either(public) + "."
	}
	if len(here) == 0 {
		return "Point an A or AAAA record at this server's public address."
	}
	are := "are"
	if len(here) == 1 {
		are = "is"
	}
	return fmt.Sprintf("Point an A record at this server's public address. Dockwell only sees %s here, which %s private, so the internet can't reach it.", human.List(here), are)
}

// BehindNAT reports whether a server's own addresses are all private, so what the
// internet sees of it isn't on any interface. With no addresses, nothing is known.
func BehindNAT(here []string) bool {
	return len(here) > 0 && len(PublicAddresses(here)) == 0
}

// BehindCarrierNAT reports a server whose every address is carrier-grade NAT's, so
// no port forwarded anywhere reaches it. A VPN's address in that range beside one of
// its own, as Tailscale's, says nothing of that.
func BehindCarrierNAT(here []string) bool {
	return len(here) > 0 && !slices.ContainsFunc(here, func(s string) bool { return !IsSharedAddress(s) })
}

// ForwardTo keeps the addresses of here a router could forward a port to: the
// server's own private ones, leaving out a VPN's in the shared range.
func ForwardTo(here []string) []string {
	var out []string
	public := PublicAddresses(here)
	for _, s := range here {
		if !IsSharedAddress(s) && !slices.Contains(public, s) {
			out = append(out, s)
		}
	}
	return out
}
