// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/dockwell/dockwell/internal/lazyre"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// ufw reads ufw's status, defaults and rules from `ufw status verbose`.
func (f *firewallReader) ufw(ctx context.Context) *dockwellv1.FirewallTool {
	tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW}
	out, err := f.output(ctx, "ufw", "status", "verbose")
	if err == nil {
		err = parseUFW(out, tool)
	}
	if err != nil {
		// What was read before a row it can't read would say too little, so none
		// of it stands.
		*tool = dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW, Unreadable: f.unreadable("ufw", err)}
	}
	return tool
}

// ufwRow is a row of ufw's rules, as ufw's backend prints it with
// "%-26s %-12s%-26s%s%s": what it covers, the action and its direction, where
// from, " (log)" when it logs, and " # comment". It's split on the action, as a
// long first column leaves a single space before it.
var ufwRow = lazyre.Regexp(`^(.+?) +(ALLOW|DENY|REJECT|LIMIT)(?: (IN|OUT|FWD))?(?: +(.*))?$`)

// parseUFW reads `ufw status verbose`, run in the C locale:
//
//	Status: active
//	Default: deny (incoming), allow (outgoing), disabled (routed)
//	To                         Action      From
//	--                         ------      ----
//	22/tcp (OpenSSH)           ALLOW IN    Anywhere
//	443/tcp on eth0            ALLOW IN    203.0.113.0/24             # office
//
// It fails on a status it doesn't know, and on any row it can't read, rather
// than leave a rule out: a rule left out could read as a port closed.
func parseUFW(out []byte, tool *dockwellv1.FirewallTool) error {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	table, status := false, false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " ")
		switch {
		case strings.HasPrefix(line, "Status:"):
			switch s := strings.TrimSpace(strings.TrimPrefix(line, "Status:")); s {
			case "active":
				tool.Active, status = true, true
			case "inactive":
				status = true
			default:
				return fmt.Errorf("ufw says its status is %q, which Dockwell doesn't know", s)
			}
		case strings.HasPrefix(line, "Default:"):
			for part := range strings.SplitSeq(strings.TrimPrefix(line, "Default:"), ",") {
				if verdict, ok := strings.CutSuffix(strings.TrimSpace(part), " (incoming)"); ok {
					tool.DefaultInbound = ufwVerdict(verdict)
				}
			}
		case strings.HasPrefix(line, "--"):
			table = true
		case table && line != "":
			rule, inbound, err := parseUFWRule(line)
			if err != nil {
				return err
			}
			if inbound {
				tool.Rules = append(tool.Rules, rule)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !status {
		return fmt.Errorf("ufw didn't say its status: %q", firstLine(out))
	}
	return nil
}

// parseUFWRule reads one row, and says whether it's about what comes in to the
// server: outbound and routed rules aren't.
func parseUFWRule(line string) (*dockwellv1.FirewallSeenRule, bool, error) {
	comment := ""
	if i := strings.Index(line, " # "); i >= 0 {
		line, comment = line[:i], strings.TrimSpace(line[i+3:])
	}
	m := ufwRow().FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return nil, false, fmt.Errorf("ufw listed a rule Dockwell can't read: %q", line)
	}
	to, action, direction, from := strings.TrimSpace(m[1]), m[2], m[3], strings.TrimSpace(m[4])
	if direction == "OUT" || direction == "FWD" {
		return nil, false, nil
	}
	rule := &dockwellv1.FirewallSeenRule{Verdict: ufwVerdict(action), Comment: comment}
	var v6 bool
	rule.Port, rule.App, rule.Interface, rule.Destination, v6 = parseUFWTo(to)
	source, fromV6, ok := parseUFWFrom(from)
	if !ok {
		return nil, false, fmt.Errorf("ufw listed a rule from %q, which Dockwell can't read", from)
	}
	rule.Source = source
	rule.Ipv6 = v6 || fromV6 || isIPv6(rule.Destination)
	return rule, true, nil
}

// parseUFWTo reads ufw's "To": a port, an address, or both, then the app
// profile in parentheses with "(v6)" inside it, or "(v6)" alone, then " on" and
// the interface. The port is "Anywhere" for every port, or "Anywhere/tcp" for
// every port of one protocol.
func parseUFWTo(to string) (port, app, iface, destination string, v6 bool) {
	if i := strings.LastIndex(to, " on "); i >= 0 {
		to, iface = to[:i], strings.TrimSpace(to[i+4:])
	}
	if cut, ok := strings.CutSuffix(to, " (v6)"); ok {
		to, v6 = cut, true
	}
	if strings.HasSuffix(to, ")") {
		if i := strings.Index(to, " ("); i >= 0 {
			app = to[i+2 : len(to)-1]
			to = to[:i]
			if cut, ok := strings.CutSuffix(app, " (v6)"); ok {
				app, v6 = cut, true
			}
		}
	}
	fields := strings.Fields(to)
	switch {
	case len(fields) == 2:
		return fields[1], app, iface, fields[0], v6
	case len(fields) == 1 && strings.HasPrefix(fields[0], "Anywhere"):
		return fields[0], app, iface, "", v6
	case len(fields) == 1:
		// An address alone, or with its protocol, as "203.0.113.5/tcp", covers
		// every port; a range of addresses is "203.0.113.0/24".
		addr, proto, hasProto := strings.Cut(fields[0], "/")
		if _, err := netip.ParseAddr(addr); err == nil {
			if !hasProto {
				return "Anywhere", app, iface, addr, v6
			}
			if _, err := netip.ParsePrefix(fields[0]); err == nil {
				return "Anywhere", app, iface, fields[0], v6
			}
			return "Anywhere/" + proto, app, iface, addr, v6
		}
	}
	return to, app, iface, "", v6
}

// parseUFWFrom reads ufw's "From": "Anywhere", or an address or range, maybe
// with a port and an app profile, then " on" an outbound interface and " (log)".
// A source port, or an app profile there, doesn't narrow where from.
func parseUFWFrom(from string) (source string, v6, ok bool) {
	for _, attrib := range []string{"(log)", "(log-all)"} {
		from = strings.TrimSpace(strings.TrimSuffix(from, attrib))
	}
	if i := strings.LastIndex(from, " on "); i >= 0 {
		from = from[:i]
	}
	if cut, ok := strings.CutSuffix(from, " (v6)"); ok {
		from, v6 = cut, true
	}
	if i := strings.Index(from, " ("); i >= 0 && strings.HasSuffix(from, ")") {
		from = from[:i]
	}
	fields := strings.Fields(from)
	if len(fields) == 0 {
		return "", v6, false
	}
	first := fields[0]
	if strings.HasPrefix(first, "Anywhere") {
		return "", v6, true
	}
	if p, err := netip.ParsePrefix(first); err == nil {
		return p.String(), v6 || p.Addr().Is6(), true
	}
	if a, err := netip.ParseAddr(first); err == nil {
		return a.String(), v6 || a.Is6(), true
	}
	// A source port alone, as "53/udp", is from anywhere.
	if _, known := portCovers(first, 0, "", ":"); known {
		return "", v6, true
	}
	return "", v6, false
}

func ufwVerdict(word string) dockwellv1.FirewallVerdict {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "allow":
		return dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW
	case "deny":
		return dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY
	case "reject":
		return dockwellv1.FirewallVerdict_FIREWALL_VERDICT_REJECT
	case "limit":
		return dockwellv1.FirewallVerdict_FIREWALL_VERDICT_LIMIT
	}
	return dockwellv1.FirewallVerdict_FIREWALL_VERDICT_UNSPECIFIED
}

// ufwBuiltIn is what ufw's before.rules let in ahead of every rule a person
// makes: DHCP replies, and mDNS and UPnP discovery to their multicast groups.
func ufwBuiltIn(l *dockwellv1.HostListener, family int) bool {
	if l.GetProtocol() != "udp" {
		return false
	}
	switch l.GetPort() {
	case 5353, 1900:
		return true
	case 68:
		return family == 4
	case 546:
		return family == 6
	}
	return false
}

// ufwReach works out how far a listener reaches over one family past ufw, as
// ufw decides: its own built-in rules, then the first of a person's rules that
// covers it, then its default. A rule on an interface or to one address
// narrows where it's open, but never closes it for everyone.
func ufwReach(l *dockwellv1.HostListener, tool *dockwellv1.FirewallTool, family int) reached {
	if ufwBuiltIn(l, family) {
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN, by: "ufw", reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_BUILT_IN}
	}
	var unknown dockwellv1.ListenerReachReason
	var sources, ifaces []string
	allowedTo := false
	closed := func() reached {
		switch {
		case len(sources) > 0 || len(ifaces) > 0:
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_SOME, by: "ufw", sources: sources, interfaces: ifaces}
		case allowedTo:
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "ufw", reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_DESTINATION}
		}
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_CLOSED, by: "ufw"}
	}
	for _, r := range tool.GetRules() {
		if ruleFamily(r) != family {
			continue
		}
		covers, known := coversSpec(r.GetPort(), l, ":")
		if !known {
			unknown = dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_APP_PROFILE
			continue
		}
		if !covers {
			continue
		}
		if d := r.GetDestination(); d != "" && d != l.GetAddress() {
			if !isUnspecified(l.GetAddress()) {
				// A rule to another of the server's addresses isn't about this listener.
				continue
			}
			allowedTo = allowedTo || allows(r)
			continue
		}
		if r.GetInterface() != "" {
			if allows(r) {
				ifaces = appendNew(ifaces, r.GetInterface())
			}
			continue
		}
		switch {
		case allows(r) && r.GetSource() == "":
			// Whoever an earlier rule let in, this lets in everyone else.
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN, by: "ufw"}
		case allows(r):
			sources = appendNew(sources, r.GetSource())
		case denies(r) && r.GetSource() == "":
			return closed()
		}
	}
	if unknown != dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_UNSPECIFIED {
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "ufw", reason: unknown}
	}
	// What no rule matched goes by the default. When that allows, everyone gets
	// in, not only the sources a rule named.
	switch def := byDefault(tool.GetDefaultInbound()); def {
	case dockwellv1.ListenerReach_LISTENER_REACH_CLOSED:
		return closed()
	case dockwellv1.ListenerReach_LISTENER_REACH_OPEN, dockwellv1.ListenerReach_LISTENER_REACH_SOME,
		dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, dockwellv1.ListenerReach_LISTENER_REACH_UNSPECIFIED:
		return reached{reach: def, by: "ufw"}
	}
	return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "ufw"}
}

// ruleFamily is 6 for a rule ufw keeps for IPv6, and 4 otherwise.
func ruleFamily(r *dockwellv1.FirewallSeenRule) int {
	if r.GetIpv6() || isIPv6(r.GetSource()) || isIPv6(r.GetDestination()) {
		return 6
	}
	return 4
}

func allows(r *dockwellv1.FirewallSeenRule) bool {
	return r.GetVerdict() == dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW || r.GetVerdict() == dockwellv1.FirewallVerdict_FIREWALL_VERDICT_LIMIT
}

func denies(r *dockwellv1.FirewallSeenRule) bool {
	return r.GetVerdict() == dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY || r.GetVerdict() == dockwellv1.FirewallVerdict_FIREWALL_VERDICT_REJECT
}

func appendNew(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}

// isIPv6 says whether an address or a range is IPv6.
func isIPv6(s string) bool {
	if s == "" {
		return false
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr().Is6() && !p.Addr().Is4In6()
	}
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6() && !a.Is4In6()
}

func isUnspecified(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.IsUnspecified()
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}
