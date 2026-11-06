// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"fmt"
	"net/netip"
	"strings"
)

// sharedAddresses are carrier-grade NAT's (RFC 6598): not public, though not
// private by net/netip's reckoning either.
var sharedAddresses = netip.MustParsePrefix("100.64.0.0/10")

// IsPublicAddress reports whether a is an address the internet can route to, so
// the only kind a domain's DNS record can usefully point at. It isn't for a
// private, shared, link-local or loopback address, which are all a server behind
// NAT has of its own. This is the one definition: isPrivateAddress in the panel's
// server.ts mirrors it, and testdata/addresses.json holds them to the same table.
func IsPublicAddress(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !sharedAddresses.Contains(a) && a.Zone() == ""
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
		return "Point an A or AAAA record at this server, " + strings.Join(public, " or ") + "."
	}
	if len(here) == 0 {
		return "Point an A or AAAA record at this server's public address."
	}
	which, are := strings.Join(here, " and "), "are"
	if len(here) == 1 {
		are = "is"
	}
	return fmt.Sprintf("Point an A record at this server's public address. Dockwell only sees %s here, which %s private, so the internet can't reach it.", which, are)
}

// BehindNAT reports whether a server's own addresses are all private, so what the
// internet sees of it isn't on any interface. With no addresses, nothing is known.
func BehindNAT(here []string) bool {
	return len(here) > 0 && len(PublicAddresses(here)) == 0
}
