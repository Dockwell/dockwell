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
// proxies.
const PresetCloudflare = "cloudflare"

// cloudflareRanges are Cloudflare's published ranges, from
// https://www.cloudflare.com/ips-v4 and ips-v6, as they stood on 2026-11-05. A
// range Cloudflare adds later isn't believed until a release has it, so what a
// service reads from there is Cloudflare's address, not a forged one. A route's
// config holds these, so a new list reads as drift on a route that names the
// preset, and reverting the drift applies the new list.
var cloudflareRanges = []string{
	"103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "104.16.0.0/13", "104.24.0.0/14",
	"108.162.192.0/18", "131.0.72.0/22", "141.101.64.0/18", "162.158.0.0/15", "172.64.0.0/13",
	"173.245.48.0/20", "188.114.96.0/20", "190.93.240.0/20", "197.234.240.0/22", "198.41.128.0/17",
	"2400:cb00::/32", "2405:8100::/32", "2405:b500::/32", "2606:4700::/32", "2803:f800::/32",
	"2a06:98c0::/29", "2c0f:f248::/32",
}

// Limits on what a route may name as a proxy.
const (
	// maxTrustedProxies is more than any route needs, past which a token that
	// plans them costs a person a long read.
	maxTrustedProxies = 32
	// minTrustedBits is the widest range that can be someone's proxy. Past it, so
	// many addresses could say whatever they like of a client that it isn't naming
	// a proxy at all. Docker's address pools and 10.0.0.0/8 fit within it.
	minTrustedBits = 8
)

// mappedIPv4 is where IPv6 spells IPv4 addresses. A range over it would name
// IPv4's whole internet in another way.
var mappedIPv4 = netip.MustParseAddr("::ffff:0.0.0.0")

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
	// As it reads back from the proxy, however it was written: a preset's ranges
	// spelled out are the preset, and a range in a preset beside its name is the name.
	return proxyNames(expandProxies(out)), nil
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
	if prefix.Bits() < minTrustedBits || prefix.Addr().IsUnspecified() || (prefix.Addr().Is6() && prefix.Contains(mappedIPv4)) {
		return "", fmt.Errorf("the trusted proxy %s covers too much to be someone's proxy: anything in it could say any address is its client's, and services would believe it. Name your proxy's own address, or a range of /%d or narrower", entry, minTrustedBits)
	}
	return prefix.String(), nil
}

// expandProxies turns what a spec names into the ranges a proxy takes.
func expandProxies(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == PresetCloudflare {
			out = append(out, cloudflareRanges...)
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// proxyNames is expandProxies the other way: a list that holds every range of a
// preset reads as its name, and anything else as itself, so a preset compares
// equal to the spec that named it. A list that lacks one of them reads as what it
// holds, which is a difference, and shows as drift.
func proxyNames(ranges []string) []string {
	if len(ranges) == 0 {
		return nil
	}
	out := slices.Clone(ranges)
	if !slices.ContainsFunc(cloudflareRanges, func(r string) bool { return !slices.Contains(ranges, r) }) {
		out = slices.DeleteFunc(out, func(r string) bool { return slices.Contains(cloudflareRanges, r) })
		out = append(out, PresetCloudflare)
	}
	slices.Sort(out)
	return out
}
