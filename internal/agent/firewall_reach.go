// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io/fs"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// listeners reads what listens on the server beyond loopback, from /proc/net's
// tcp, tcp6, udp and udp6: TCP sockets listening, and UDP sockets bound with no
// peer.
func listeners(proc fs.FS) []*dockwellv1.HostListener {
	seen := map[string]bool{}
	var out []*dockwellv1.HostListener
	for _, f := range []struct{ name, protocol, state string }{
		{"net/tcp", "tcp", "0A"},
		{"net/tcp6", "tcp", "0A"},
		{"net/udp", "udp", "07"},
		{"net/udp6", "udp", "07"},
	} {
		b, err := fs.ReadFile(proc, f.name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || fields[3] != f.state {
				continue
			}
			addr, port, ok := procAddress(fields[1])
			if !ok || addr.IsLoopback() {
				continue
			}
			if f.protocol == "udp" {
				if _, peer, ok := procAddress(fields[2]); !ok || peer != 0 {
					continue
				}
			}
			key := f.protocol + " " + netip.AddrPortFrom(addr, port).String()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, &dockwellv1.HostListener{Address: addr.String(), Port: uint32(port), Protocol: f.protocol})
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.HostListener) int {
		if a.GetPort() != b.GetPort() {
			return int(a.GetPort()) - int(b.GetPort())
		}
		if c := strings.Compare(a.GetProtocol(), b.GetProtocol()); c != 0 {
			return c
		}
		return strings.Compare(a.GetAddress(), b.GetAddress())
	})
	return out
}

// procAddress reads /proc/net's "0100007F:1F90": the address in the kernel's own
// byte order, four bytes at a time, and the port.
func procAddress(field string) (netip.Addr, uint16, bool) {
	hexAddr, hexPort, ok := strings.Cut(field, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.Addr{}, 0, false
	}
	// Each four bytes are a little-endian word.
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	addr, _ := netip.AddrFromSlice(raw)
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	return addr.Unmap(), uint16(port), true
}

// reachOf works out how far a listener reaches past the firewalls that cover it:
// ufw's and firewalld's, read by their own tools. nftables's other tables and
// iptables-legacy aren't weighed rule by rule, which the tab says.
func reachOf(l *dockwellv1.HostListener, tools []*dockwellv1.FirewallTool) {
	l.Reach = dockwellv1.ListenerReach_LISTENER_REACH_OPEN
	for _, tool := range tools {
		if !tool.GetActive() && tool.GetUnreadable() == "" {
			continue
		}
		var reach dockwellv1.ListenerReach
		var sources []string
		switch {
		case tool.GetUnreadable() != "" && (tool.GetKind() == dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW || tool.GetKind() == dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD):
			reach = dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN
		case tool.GetKind() == dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW:
			reach, sources = ufwReach(l, tool)
		case tool.GetKind() == dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD:
			reach = firewalldReach(l, tool)
		default:
			continue
		}
		// The narrowest of the firewalls wins: a drop anywhere drops.
		if reachRank(reach) > reachRank(l.GetReach()) {
			l.Reach, l.DecidedBy, l.Sources = reach, toolName(tool), sources
		}
	}
}

func reachRank(r dockwellv1.ListenerReach) int {
	switch r {
	case dockwellv1.ListenerReach_LISTENER_REACH_SOME:
		return 1
	case dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN:
		return 2
	case dockwellv1.ListenerReach_LISTENER_REACH_CLOSED:
		return 3
	default:
		return 0
	}
}

func toolName(tool *dockwellv1.FirewallTool) string {
	switch tool.GetKind() {
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW:
		return "ufw"
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD:
		return "firewalld"
	default:
		return tool.GetName()
	}
}

// ufwReach takes ufw's first rule that covers the listener, as ufw does, or its
// default for what comes in. A rule naming an app profile, which the agent
// doesn't resolve, leaves it unknown when nothing else decides.
func ufwReach(l *dockwellv1.HostListener, tool *dockwellv1.FirewallTool) (dockwellv1.ListenerReach, []string) {
	ipv6 := strings.Contains(l.GetAddress(), ":")
	named := false
	var allowedFrom []string
	for _, r := range tool.GetRules() {
		if r.GetIpv6() && !ipv6 {
			continue
		}
		covers, known := ufwCovers(r.GetPort(), l)
		if !known {
			named = true
			continue
		}
		if !covers {
			continue
		}
		switch r.GetVerdict() {
		case dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, dockwellv1.FirewallVerdict_FIREWALL_VERDICT_LIMIT:
			if r.GetSource() == "" {
				// Whoever an earlier rule let in, this lets in everyone else.
				return dockwellv1.ListenerReach_LISTENER_REACH_OPEN, nil
			}
			allowedFrom = append(allowedFrom, r.GetSource())
		case dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY, dockwellv1.FirewallVerdict_FIREWALL_VERDICT_REJECT:
			if r.GetSource() == "" {
				if len(allowedFrom) > 0 {
					return dockwellv1.ListenerReach_LISTENER_REACH_SOME, allowedFrom
				}
				return dockwellv1.ListenerReach_LISTENER_REACH_CLOSED, nil
			}
		default:
			// A verdict this agent doesn't know decides nothing here.
		}
	}
	if named {
		return dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, nil
	}
	// What no rule matched goes by the default. When that allows, everyone gets
	// in, not only the sources a rule named.
	reach := byDefault(tool.GetDefaultInbound())
	if len(allowedFrom) > 0 && reach == dockwellv1.ListenerReach_LISTENER_REACH_CLOSED {
		return dockwellv1.ListenerReach_LISTENER_REACH_SOME, allowedFrom
	}
	return reach, nil
}

// ufwCovers is portCovers for a ufw rule, whose "To" is "Anywhere" for every port.
func ufwCovers(to string, l *dockwellv1.HostListener) (covers, known bool) {
	if to == "Anywhere" {
		return true, true
	}
	return portCovers(to, l.GetPort(), l.GetProtocol(), "-:")
}

// firewalldReach is open when a port of the zone covers the listener, unknown
// when a service or rich rule might and the agent doesn't resolve them, and the
// zone's target otherwise.
func firewalldReach(l *dockwellv1.HostListener, tool *dockwellv1.FirewallTool) dockwellv1.ListenerReach {
	named := len(tool.GetOther()) > 0
	for _, r := range tool.GetRules() {
		covers, known := portCovers(r.GetPort(), l.GetPort(), l.GetProtocol(), "-")
		if !known {
			if svc, ok := wellKnownServices[r.GetPort()]; ok {
				covers, known = portCovers(svc, l.GetPort(), l.GetProtocol(), "-")
			}
		}
		if !known {
			named = true
			continue
		}
		if covers {
			return dockwellv1.ListenerReach_LISTENER_REACH_OPEN
		}
	}
	if named {
		return dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN
	}
	return byDefault(tool.GetDefaultInbound())
}

// wellKnownServices are the firewalld services a server's SSH and web ports go
// by, and those AlmaLinux's and Fedora's default zone turns on, so they don't
// leave a listener unknown.
var wellKnownServices = map[string]string{
	"ssh": "22/tcp", "http": "80/tcp", "https": "443/tcp",
	"dhcpv6-client": "546/udp", "cockpit": "9090/tcp", "mdns": "5353/udp",
}

func byDefault(v dockwellv1.FirewallVerdict) dockwellv1.ListenerReach {
	switch v {
	case dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY, dockwellv1.FirewallVerdict_FIREWALL_VERDICT_REJECT:
		return dockwellv1.ListenerReach_LISTENER_REACH_CLOSED
	case dockwellv1.FirewallVerdict_FIREWALL_VERDICT_UNSPECIFIED:
		return dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN
	default:
		return dockwellv1.ListenerReach_LISTENER_REACH_OPEN
	}
}

// portCovers says whether a rule's port, such as "22", "22/tcp", "80,443/tcp" or
// "9000:9100/udp", covers port and protocol; known is false for a name, such as
// ufw's "OpenSSH" or firewalld's "ssh". ranges are the characters a range is
// written with: ufw writes "9000:9100", firewalld "9000-9100".
func portCovers(spec string, port uint32, protocol, ranges string) (covers, known bool) {
	ports, proto, hasProto := strings.Cut(spec, "/")
	if hasProto && proto != protocol {
		_, known = parsePorts(ports, ranges, port)
		return false, known
	}
	return parsePorts(ports, ranges, port)
}

func parsePorts(list, ranges string, port uint32) (covers, known bool) {
	for part := range strings.SplitSeq(list, ",") {
		lo, hi, isRange := strings.Cut(part, ranges[:1])
		if !isRange && len(ranges) > 1 {
			lo, hi, isRange = strings.Cut(part, ranges[1:2])
		}
		from, err := strconv.ParseUint(lo, 10, 16)
		if err != nil {
			return false, false
		}
		to := from
		if isRange {
			if to, err = strconv.ParseUint(hi, 10, 16); err != nil {
				return false, false
			}
		}
		if uint64(port) >= from && uint64(port) <= to {
			covers = true
		}
	}
	return covers, true
}
