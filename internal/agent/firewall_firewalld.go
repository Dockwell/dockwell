// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// firewalldZoneName is a zone's name as firewalld allows it, which is all the
// agent passes to firewall-cmd, and maxFirewalldZoneName how long one gets. The
// length is checked apart from the form: a counted repetition, such as {1,64},
// compiles to that many copies of what it repeats, which every agent pays for as
// it starts (ADR 0060).
var firewalldZoneName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const maxFirewalldZoneName = 64

// firewalld reads whether firewalld runs and, when it does, every zone that
// takes traffic: those bound to interfaces or sources, and the default zone,
// which takes what no other does. Docker's own zone, for its interfaces, is
// traffic from containers, and is left out.
func (f *firewallReader) firewalld(ctx context.Context) []*dockwellv1.FirewallTool {
	off := func(err error) []*dockwellv1.FirewallTool {
		return []*dockwellv1.FirewallTool{{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD, Unreadable: f.unreadable("firewalld", err)}}
	}
	// --state says "not running" and exits 252 when firewalld isn't running:
	// that's an answer. Anything else it says, it couldn't.
	res, err := f.command(ctx, "firewall-cmd", "--state")
	said := strings.TrimSpace(string(res.Out) + "\n" + res.Errs)
	switch {
	case err != nil:
		return off(err)
	case res.Code == 0 && strings.TrimSpace(string(res.Out)) == "running":
	case res.Code == 252 && strings.Contains(said, "not running"):
		return []*dockwellv1.FirewallTool{{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD}}
	default:
		return off(fmt.Errorf("firewall-cmd --state ended with status %d: %s", res.Code, cmp.Or(said, "it said nothing")))
	}
	def, err := f.output(ctx, "firewall-cmd", "--get-default-zone")
	if err != nil {
		return off(err)
	}
	active, err := f.output(ctx, "firewall-cmd", "--get-active-zones")
	if err != nil {
		return off(err)
	}
	defaultZone := strings.TrimSpace(string(def))
	zones := activeZones(active)
	if !slices.Contains(zones, defaultZone) {
		zones = append(zones, defaultZone)
	}
	var tools []*dockwellv1.FirewallTool
	for _, zone := range zones {
		if zone == "docker" {
			continue
		}
		if len(zone) > maxFirewalldZoneName || !firewalldZoneName.MatchString(zone) {
			return off(fmt.Errorf("firewalld named a zone %q, which Dockwell doesn't pass on", zone))
		}
		out, err := f.output(ctx, "firewall-cmd", "--list-all", "--zone="+zone)
		if err != nil {
			return off(err)
		}
		tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD, Active: true, DefaultZone: zone == defaultZone}
		parseFirewalld(out, tool)
		tool.Name = zone
		tools = append(tools, tool)
	}
	return tools
}

// activeZones reads `firewall-cmd --get-active-zones`: each zone's name on a
// line of its own, maybe with "(default)" after it, and what it's bound to
// indented below.
func activeZones(out []byte) []string {
	var zones []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		zones = append(zones, name)
	}
	return zones
}

// parseFirewalld reads `firewall-cmd --list-all --zone=Z`:
//
//	public (active)
//	  target: default
//	  interfaces: eth0
//	  sources:
//	  services: dhcpv6-client ssh
//	  ports: 8080/tcp
//	  protocols:
//	  rich rules:
//		rule family="ipv6" source address="2001:db8::/32" port port="22" protocol="tcp" accept
//
// Rich rules are each a line indented with a tab, whatever they hold, colons
// included.
func parseFirewalld(out []byte, tool *dockwellv1.FirewallTool) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	rich, first := false, true
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if first {
			tool.Name, _, _ = strings.Cut(line, " ")
			first = false
			continue
		}
		if rich && strings.HasPrefix(raw, "\t") {
			tool.Other = append(tool.Other, line)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		rich = false
		value = strings.TrimSpace(value)
		switch key {
		case "target":
			// "default" rejects what no rule allows, but for ICMP.
			switch value {
			case "default", "%%REJECT%%", "REJECT":
				tool.DefaultInbound = dockwellv1.FirewallVerdict_FIREWALL_VERDICT_REJECT
			case "DROP":
				tool.DefaultInbound = dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY
			case "ACCEPT":
				tool.DefaultInbound = dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW
			}
		case "interfaces":
			tool.Interfaces = strings.Fields(value)
		case "sources":
			tool.Sources = strings.Fields(value)
		case "services", "ports":
			for item := range strings.FieldsSeq(value) {
				tool.Rules = append(tool.Rules, &dockwellv1.FirewallSeenRule{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: item})
			}
		case "protocols":
			// A protocol allowed whole opens every port of it.
			for item := range strings.FieldsSeq(value) {
				tool.Rules = append(tool.Rules, &dockwellv1.FirewallSeenRule{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: "Anywhere/" + item})
			}
		case "rich rules":
			rich = true
			if value != "" {
				tool.Other = append(tool.Other, value)
			}
		}
	}
}

// wellKnownServices are the firewalld services a server's SSH and web ports go
// by, and those AlmaLinux's and Fedora's default zone turns on, so they don't
// leave a listener unknown.
var wellKnownServices = map[string]string{
	"ssh": "22/tcp", "http": "80/tcp", "https": "443/tcp",
	"dhcpv6-client": "546/udp", "cockpit": "9090/tcp", "mdns": "5353/udp",
}

// zoneReach is how far a listener reaches through one zone: open when the zone
// accepts everything, or its ports, services or protocols cover it and no rich
// rule could say otherwise first; can't tell where a rich rule or a service it
// doesn't know might; and the zone's target otherwise.
func zoneReach(l *dockwellv1.HostListener, zone *dockwellv1.FirewallTool) reached {
	if zone.GetDefaultInbound() == dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW {
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN, by: "firewalld"}
	}
	covers, named := false, false
	for _, r := range zone.GetRules() {
		c, known := coversSpec(r.GetPort(), l, "-")
		if !known {
			if svc, ok := wellKnownServices[r.GetPort()]; ok {
				c, known = coversSpec(svc, l, "-")
			}
		}
		if !known {
			named = true
			continue
		}
		covers = covers || c
	}
	switch {
	case slices.ContainsFunc(zone.GetOther(), func(rule string) bool { return richMayCover(rule, l) }):
		// Rich rules run before the zone's ports and services, and may reject
		// what those let in, or let in what they don't.
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "firewalld", reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_RICH_RULE}
	case covers:
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_OPEN, by: "firewalld"}
	case named:
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "firewalld", reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_SERVICE}
	}
	return reached{reach: byDefault(zone.GetDefaultInbound()), by: "firewalld"}
}

// richPort and richService are what a rich rule names: port port="5432"
// protocol="tcp", or service name="ssh".
var (
	richPort    = regexp.MustCompile(`\bport port="([0-9-]+)" protocol="([a-z]+)"`)
	richService = regexp.MustCompile(`\bservice name="([^"]+)"`)
)

// richMayCover says whether a rich rule may decide for a listener: it names its
// port, or a service on it, or names neither and so may cover every port.
func richMayCover(rule string, l *dockwellv1.HostListener) bool {
	if m := richPort.FindStringSubmatch(rule); m != nil {
		covers, known := coversSpec(m[1]+"/"+m[2], l, "-")
		return covers || !known
	}
	if m := richService.FindStringSubmatch(rule); m != nil {
		if svc, ok := wellKnownServices[m[1]]; ok {
			covers, _ := coversSpec(svc, l, "-")
			return covers
		}
		return true
	}
	return true
}

// firewalldReach weighs a listener against firewalld's zones over one family.
// Traffic goes through the zone of the interface it arrives on, or the default
// zone, unless its source is bound to a zone, which comes first. Where the zones
// it could arrive through disagree, which interface it arrives on decides, and
// the agent can't tell which.
func firewalldReach(l *dockwellv1.HostListener, zones []*dockwellv1.FirewallTool, family int) reached {
	var through []reached
	for _, z := range zones {
		if len(z.GetInterfaces()) > 0 || z.GetDefaultZone() {
			through = append(through, zoneReach(l, z))
		}
	}
	if len(through) == 0 {
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "firewalld", reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_ZONES}
	}
	base := through[0]
	for _, r := range through[1:] {
		if r.reach != base.reach || r.reason != base.reason {
			return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN, by: "firewalld", reason: dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_ZONES}
		}
	}
	if base.reach != dockwellv1.ListenerReach_LISTENER_REACH_CLOSED {
		return base
	}
	// Sources bound to a zone that lets it in reach it, though the rest can't.
	var sources []string
	for _, z := range zones {
		if len(z.GetSources()) == 0 || zoneReach(l, z).reach != dockwellv1.ListenerReach_LISTENER_REACH_OPEN {
			continue
		}
		for _, s := range z.GetSources() {
			if isIPv6(s) == (family == 6) {
				sources = appendNew(sources, s)
			}
		}
	}
	if len(sources) > 0 {
		return reached{reach: dockwellv1.ListenerReach_LISTENER_REACH_SOME, by: "firewalld", sources: sources}
	}
	return base
}
