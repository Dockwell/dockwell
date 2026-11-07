// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"fmt"
	"net/netip"
	"slices"
)

// TLSOff is what a route's tls says when TLS ends before this server.
const TLSOff = "off"

// PresetCloudflare names Cloudflare's published ranges among a route's trusted
// proxies. A request from them has its address read from CF-Connecting-IP, as
// Cloudflare documents for an origin. X-Forwarded-For isn't read past them: those
// ranges are shared by all of Cloudflare's customers, and a Worker's cross-zone
// request comes from one of them with whatever X-Forwarded-For its author wrote
// (ADR 0030).
const PresetCloudflare = "cloudflare"

// cloudflareRanges are Cloudflare's published ranges, from
// https://www.cloudflare.com/ips-v4 and ips-v6, as they stood on 2026-11-05. A
// range Cloudflare adds later isn't believed until a release has it, so what a
// service reads from there is Cloudflare's address, not a forged one, and a route
// with no certificate doesn't answer it. A route's config holds these, so a new
// list reads as drift on a route that names the preset, and reverting the drift
// applies the new list. .github/workflows/cloudflare-ranges.yml says when they've
// changed, nightly.
var cloudflareRanges = []string{
	"103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "104.16.0.0/13", "104.24.0.0/14",
	"108.162.192.0/18", "131.0.72.0/22", "141.101.64.0/18", "162.158.0.0/15", "172.64.0.0/13",
	"173.245.48.0/20", "188.114.96.0/20", "190.93.240.0/20", "197.234.240.0/22", "198.41.128.0/17",
	"2400:cb00::/32", "2405:8100::/32", "2405:b500::/32", "2606:4700::/32", "2803:f800::/32",
	"2a06:98c0::/29", "2c0f:f248::/32",
}

// IsCloudflare reports an address in Cloudflare's ranges, which everyone behind its
// proxy shares, so a record that points there says nothing of where a server is.
func IsCloudflare(a netip.Addr) bool {
	return slices.ContainsFunc(cloudflareRanges, func(r string) bool { return netip.MustParsePrefix(r).Contains(a.Unmap()) })
}

// Limits on what a route may name as a proxy.
const (
	// maxTrustedProxies is more than any route needs, past which a token that
	// plans them costs a person a long read.
	maxTrustedProxies = 32
	// minPrivateBits is the widest range that can be someone's proxy on a private
	// network, which 10.0.0.0/8 and Docker's address pools are.
	minPrivateBits = 8
	// minPublicBits4 and minPublicBits6 are the widest on the internet. Past them,
	// so many addresses of so many owners could say whatever they like of a client
	// that it isn't naming a proxy at all. Cloudflare's own widest are /13 and /29.
	minPublicBits4 = 12
	minPublicBits6 = 28
)

// mappedIPv4 is where IPv6 spells IPv4 addresses. A range over it would name
// IPv4's whole internet in another way.
var mappedIPv4 = netip.MustParseAddr("::ffff:0.0.0.0")

// privateSpace is what isn't anyone's on the internet: a wide range here still
// names one owner's network.
var privateSpace = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("::1/128"),
}

// NormalizeTrustedProxies checks what a route names as its proxies, and returns
// it canonical, sorted and without repeats, so that it compares equal to what the
// proxy reads back. Nothing a token sends becomes config without going through
// this: what comes out is a preset's name or a prefix netip wrote.
func NormalizeTrustedProxies(list []string) ([]string, error) {
	if len(list) > maxTrustedProxies {
		return nil, fmt.Errorf("a route names %d trusted proxies, and it takes at most %d. Name a range that covers them", len(list), maxTrustedProxies)
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		canon, err := normalizeProxy(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, canon)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func normalizeProxy(entry string) (string, error) {
	if entry == PresetCloudflare {
		return entry, nil
	}
	prefix, err := netip.ParsePrefix(entry)
	if err != nil {
		addr, aerr := netip.ParseAddr(entry)
		if aerr != nil {
			return "", fmt.Errorf("the trusted proxy %q isn't an address, a range such as 203.0.113.0/24, or %s. Write one of those", entry, PresetCloudflare)
		}
		if addr.Zone() != "" {
			return "", fmt.Errorf("the trusted proxy %q names a network interface. Write the address alone", entry)
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	if prefix.Addr().Is4In6() {
		return "", fmt.Errorf("the trusted proxy %s spells an IPv4 address in IPv6. Write it as IPv4", entry)
	}
	prefix = prefix.Masked()
	if tooWide(prefix) {
		return "", fmt.Errorf("the trusted proxy %s covers too much to be someone's proxy: anything in it could say any address is its client's, and services would believe it. Name your proxy's own address, or a range of /%d or narrower, /%d in IPv6, or /%d on a private network", entry, minPublicBits4, minPublicBits6, minPrivateBits)
	}
	if slices.ContainsFunc(cloudflareRanges, func(r string) bool { return netip.MustParsePrefix(r).Overlaps(prefix) }) {
		return "", fmt.Errorf("the trusted proxy %s is in Cloudflare's ranges, which all its customers share, so X-Forwarded-For read from there can be written by anyone's Worker. Name %s instead, which reads the address Cloudflare documents for an origin", entry, PresetCloudflare)
	}
	return prefix.String(), nil
}

// tooWide reports a range that can't be naming one proxy.
func tooWide(p netip.Prefix) bool {
	a := p.Addr()
	switch {
	case a.IsUnspecified(), a.IsMulticast(), a.Is6() && p.Contains(mappedIPv4):
		return true
	case slices.ContainsFunc(privateSpace, func(s netip.Prefix) bool { return s.Contains(a) && s.Bits() <= max(p.Bits(), minPrivateBits) }):
		return p.Bits() < minPrivateBits
	case a.Is4():
		return p.Bits() < minPublicBits4
	}
	return p.Bits() < minPublicBits6
}

// splitProxies divides what a spec names into what has its X-Forwarded-For read,
// and the ranges of a network that says who its client is in CF-Connecting-IP.
func splitProxies(names []string) (named, edge []string) {
	for _, name := range names {
		if name == PresetCloudflare {
			edge = slices.Clone(cloudflareRanges)
			continue
		}
		named = append(named, name)
	}
	return named, edge
}

// proxyNames is splitProxies the other way. A list that is the preset's ranges, no
// more and no fewer, reads as its name, so it compares equal to the spec that named
// it. Any other reads as what it holds, which is a difference, and shows as drift: a
// range missing, as when Cloudflare adds one, a range too many, as when it gives one
// up, and a range added by hand alike.
func proxyNames(named, edge []string) []string {
	if len(named)+len(edge) == 0 {
		return nil
	}
	out := slices.Clone(named)
	if len(edge) > 0 {
		if isPreset(edge) {
			out = append(out, PresetCloudflare)
		} else {
			out = append(out, edge...)
		}
	}
	slices.Sort(out)
	return out
}

// isPreset reports a list of ranges that is the preset's, in any order.
func isPreset(edge []string) bool {
	have := slices.Compact(slices.Sorted(slices.Values(edge)))
	return slices.Equal(have, slices.Sorted(slices.Values(cloudflareRanges)))
}
