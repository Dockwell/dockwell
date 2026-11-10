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
	"github.com/dockwell/dockwell/internal/agent/nftables"
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

// listenerKey is a listener's port and protocol, whatever its address.
func listenerKey(l *dockwellv1.HostListener) string {
	return l.GetProtocol() + " " + strconv.Itoa(int(l.GetPort()))
}

// boundOnIPv4 is the ports, by protocol, something holds on every IPv4 address.
// A socket on "::" beside one of them takes IPv6 alone; on its own, it takes
// IPv4 too, as Linux has it by default.
func boundOnIPv4(found []*dockwellv1.HostListener) map[string]bool {
	out := map[string]bool{}
	for _, l := range found {
		if l.GetAddress() == "0.0.0.0" {
			out[listenerKey(l)] = true
		}
	}
	return out
}

// reached is how far a listener reaches past one firewall, or past them all.
type reached struct {
	reach      dockwellv1.ListenerReach
	by         string
	sources    []string
	interfaces []string
	reason     dockwellv1.ListenerReachReason
}

// narrowness ranks a reach from open to closed: a drop in any firewall drops,
// and what one firewall can't say, the whole can't either, unless another
// closes it.
func narrowness(r dockwellv1.ListenerReach) int {
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

// reachOptions are what reach depends on beyond the firewalls.
type reachOptions struct {
	// dualStack is a socket on "::" alone, which takes IPv4 and IPv6 both.
	dualStack bool
	// dockerUnknown is Docker not saying which ports it publishes, so a
	// listener may be its proxy for a published port.
	dockerUnknown bool
	// dockwell is Dockwell's own table, as loaded now. Nil when none is.
	dockwell *nftables.Table
}

// reachOf works out how far a listener reaches past the firewalls here, for
// each family it takes, and the wider of them for a socket that takes both.
// Each firewall is weighed by its own rules: ufw's and firewalld's one by one;
// a table or chain the agent doesn't weigh one by one, or a firewall it
// couldn't read, makes what nothing else closes read "can't tell", never open.
func reachOf(l *dockwellv1.HostListener, tools []*dockwellv1.FirewallTool, opts reachOptions) {
	var r reached
	switch {
	case opts.dualStack:
		r = wider(reachFor(l, tools, opts.dockwell, 4), reachFor(l, tools, opts.dockwell, 6))
	case strings.Contains(l.GetAddress(), ":"):
		r = reachFor(l, tools, opts.dockwell, 6)
	default:
		r = reachFor(l, tools, opts.dockwell, 4)
	}
	// Docker's proxy listens on the ports Docker publishes, which ufw's and
	// firewalld's rules don't cover; without Docker's list, any of these may be one.
	if opts.dockerUnknown && (r.reach == dockwellv1.ListenerReach_LISTENER_REACH_CLOSED || r.reach == dockwellv1.ListenerReach_LISTENER_REACH_SOME) {
		r = reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: r.by, reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_DOCKER_UNKNOWN}
	}
	l.Reach, l.DecidedBy, l.Sources, l.Interfaces, l.Reason = r.reach, r.by, r.sources, r.interfaces, r.reason
}

// reachFor weighs a listener over one family against every firewall that
// covers that family, Dockwell's own with the rest, the narrowest winning.
func reachFor(l *dockwellv1.HostListener, tools []*dockwellv1.FirewallTool, dockwell *nftables.Table, family int) reached {
	out := reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN}
	take := func(r reached) {
		n, have := narrowness(r.reach), narrowness(out.reach)
		// An open that a firewall decided says more than one nothing decided.
		if n > have || (n == have && out.by == "" && r.by != "") {
			out = r
		}
	}
	var zones []*dockwellv1.FirewallTool
	for _, tool := range tools {
		if !coversFamily(tool, family) {
			continue
		}
		if tool.GetUnreadable() != "" {
			take(reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: toolName(tool), reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_UNREADABLE})
			continue
		}
		if !tool.GetActive() {
			continue
		}
		switch tool.GetKind() {
		case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW:
			take(ufwReach(l, tool, family))
		case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD:
			zones = append(zones, tool)
		default:
			if tool.GetInputRules() > 0 || tool.GetInputPolicy() == dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY {
				take(reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: toolName(tool), reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_OTHER_RULES})
			}
		}
	}
	if len(zones) > 0 {
		take(firewalldReach(l, zones, family))
	}
	if dockwell != nil {
		take(dockwellReach(l, dockwell, family))
	}
	return out
}

// dockwellReach weighs a listener against Dockwell's own table as nft runs it:
// its pins, its allows, its denies, then its policy, the first that matches
// deciding for those it covers. A rule from some addresses lets those in, or
// keeps them out, and leaves the rest to what follows. Traffic from Docker's
// networks, which the table leaves to their containers, isn't from outside.
func dockwellReach(l *dockwellv1.HostListener, t *nftables.Table, family int) reached {
	const by = "Dockwell"
	var allowed []string
	matches := func(r nftables.Rule) (anywhere bool, sources []string, covers bool) {
		if r.Protocol != l.GetProtocol() || !slices.Contains(r.Ports, uint16(l.GetPort())) { //nolint:gosec // a port
			return false, nil, false
		}
		if len(r.Sources) == 0 {
			return true, nil, true
		}
		for _, p := range r.Sources {
			if (family == 4) == p.Addr().Unmap().Is4() {
				sources = append(sources, p.Masked().String())
			}
		}
		return false, sources, len(sources) > 0
	}
	accepts := slices.Concat(t.Pins, t.Allows)
	for _, r := range accepts {
		anywhere, sources, covers := matches(r)
		switch {
		case !covers:
		case anywhere:
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN, by: by}
		default:
			for _, s := range sources {
				allowed = appendNew(allowed, s)
			}
		}
	}
	for _, r := range t.Denies {
		if anywhere, _, covers := matches(r); covers && anywhere {
			if len(allowed) > 0 {
				return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_SOME, by: by, sources: allowed}
			}
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_CLOSED, by: by}
		}
	}
	if t.DenyRest {
		if len(allowed) > 0 {
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_SOME, by: by, sources: allowed}
		}
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_CLOSED, by: by}
	}
	// Nothing here closes it: what decides is the others', or nothing.
	return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN}
}

// wider is how far a socket that takes both families reaches: as far as the
// farther of the two.
func wider(a, b reached) reached {
	if narrowness(b.reach) < narrowness(a.reach) {
		a, b = b, a
	}
	if a.reach == dockwellv1.ListenerReach_LISTENER_REACH_SOME && b.reach == dockwellv1.ListenerReach_LISTENER_REACH_SOME {
		for _, s := range b.sources {
			a.sources = appendNew(a.sources, s)
		}
		for _, i := range b.interfaces {
			a.interfaces = appendNew(a.interfaces, i)
		}
	}
	return a
}

// coversFamily says whether a firewall filters a family: ufw and firewalld do
// both, an nftables table its own, and iptables its own.
func coversFamily(tool *dockwellv1.FirewallTool, family int) bool {
	switch tool.GetKind() {
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES:
		f, _, _ := strings.Cut(tool.GetName(), " ")
		n := nftFamily(f)
		return n == 0 || n == family
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES, dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES_LEGACY:
		switch tool.GetName() {
		case "IPv4":
			return family == 4
		case "IPv6":
			return family == 6
		}
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW, dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD,
		dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UNSPECIFIED:
	}
	return true
}

func toolName(tool *dockwellv1.FirewallTool) string {
	switch tool.GetKind() {
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW:
		return "ufw"
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD:
		return "firewalld"
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES_LEGACY:
		return strings.TrimSpace("iptables-legacy " + tool.GetName())
	case dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES:
		return strings.TrimSpace("iptables " + tool.GetName())
	default:
		return strings.TrimSpace("nftables " + tool.GetName())
	}
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

// coversSpec says whether a rule's port covers a listener: "Anywhere" covers
// every port, "Anywhere/tcp" every port of a protocol, and a port spec as
// portCovers reads it.
func coversSpec(spec string, l *dockwellv1.HostListener, ranges string) (covers, known bool) {
	if spec == "Anywhere" {
		return true, true
	}
	if proto, ok := strings.CutPrefix(spec, "Anywhere/"); ok {
		return proto == l.GetProtocol(), true
	}
	return portCovers(spec, l.GetPort(), l.GetProtocol(), ranges)
}

// portCovers says whether a rule's port, such as "22", "22/tcp", "80,443/tcp" or
// "9000:9100/udp", covers port and protocol; known is false for a name, such as
// firewalld's "ssh". ranges are the characters a range is written with: ufw
// writes "9000:9100", firewalld "9000-9100".
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
