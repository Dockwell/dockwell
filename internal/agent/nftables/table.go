// SPDX-License-Identifier: AGPL-3.0-only

// Package nftables writes and reads Dockwell's own nftables table, inet
// dockwell, as ADR 0052 lays it out: what keeps the server reachable first,
// then what's pinned, every allow, every deny, and the policy, each in the
// input chain and, for ports Docker publishes, the forward chain. The whole
// table is replaced in one nft transaction, so the server is never between
// two rulesets.
package nftables

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The table Dockwell writes, and nothing else.
const (
	Family = "inet"
	Name   = "dockwell"
)

// Rule is one rule of the table: a protocol's ports, from some sources or
// from anywhere.
type Rule struct {
	// Name is what the rule is, kept in its comment, such as "rule office-ssh"
	// or "pin ssh". It names the rule on the server, as nft lists it.
	Name string
	// Protocol is "tcp" or "udp".
	Protocol string
	// Ports are the ports on the server it covers, one at least.
	Ports []uint16
	// Sources are where it covers traffic from. Empty is anywhere.
	Sources []netip.Prefix
}

// Table is what inet dockwell holds, beyond what keeps the server reachable,
// which every table holds first.
type Table struct {
	// Pins are always allowed, before every rule: SSH from where people come
	// in now, and on a control server the agent listener and the panel's ports.
	Pins []Rule
	// Allows let traffic in, before every deny.
	Allows []Rule
	// Denies drop traffic, after every allow.
	Denies []Rule
	// DenyRest drops what nothing above let in: the policy, deny everything
	// else, which a person turns on.
	DenyRest bool
	// DockerInterfaces are Docker's networks' interfaces, from Docker's network
	// list. Traffic arriving on them, from containers, isn't filtered.
	DockerInterfaces []string
	// NoDocker is a server without Docker: nothing it forwards is a port Docker
	// publishes, so the table leaves forwarding alone, as libvirt's or LXD's.
	NoDocker bool
}

// keepsReachable is what every table lets in first, so the server stays
// reachable whatever the rules say: connections already open, loopback,
// ICMP's errors, IPv6's neighbour and router discovery, MLD from the link,
// which keeps a snooping switch sending the host its neighbour discovery,
// DHCP's replies, and ping, at most 5 a second, as RFC 4890 keeps echo; with
// invalid packets dropped.
var keepsReachable = []string{
	"ct state established,related accept",
	"ct state invalid drop",
	`iifname "lo" accept`,
	"icmp type { destination-unreachable, time-exceeded, parameter-problem } accept",
	"icmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem } accept",
	"icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert, nd-redirect } ip6 hoplimit 255 accept",
	"icmpv6 type { mld-listener-query, mld-listener-report, mld-listener-done, mld2-listener-report } ip6 saddr fe80::/10 ip6 hoplimit 1 accept",
	"udp sport 67 udp dport 68 accept",
	"ip6 saddr fe80::/10 udp sport 547 udp dport 546 accept",
	"icmp type echo-request limit rate 5/second burst 10 packets accept",
	"icmpv6 type echo-request limit rate 5/second burst 10 packets accept",
}

// matchable says whether nft can match an interface by name: Linux's 15 bytes
// at most, in quotes, which may hold anything but a quote or a control
// character, and not end in *, which nft reads as a wildcard.
func matchable(name string) bool {
	if name == "" || len(name) > 15 || strings.HasSuffix(name, "*") {
		return false
	}
	for _, r := range name {
		if r == '"' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// SkippedWords says what it means that one of Docker's interfaces has a name
// the table can't match, and how to fix it.
func SkippedWords(name string) string {
	return fmt.Sprintf("Docker's interface %q has a name nftables can't match: it holds a quote or a control character, or ends in *. "+
		"Its containers' traffic to the server is treated as from outside, and the ports they publish aren't filtered. "+
		"Rename its network's bridge, with com.docker.network.bridge.name", name)
}

// Skipped are Docker's interfaces the table leaves out, as nft can't match their names.
func (t Table) Skipped() []string {
	var out []string
	for _, name := range t.DockerInterfaces {
		if !matchable(name) {
			out = append(out, name)
		}
	}
	return out
}

// ruleName is what a rule's comment may hold: printable, without what nft
// would read as the comment's end, and shorter than nft's 128 bytes.
var ruleName = regexp.MustCompile(`^[A-Za-z0-9 _.:/@-]{1,100}$`)

// Validate refuses a table nft would misread: a rule with no ports, a port of
// 0, a protocol other than TCP and UDP, or a name or interface that would need
// quoting.
func (t Table) Validate() error {
	for _, list := range [][]Rule{t.Pins, t.Allows, t.Denies} {
		for _, r := range list {
			if !ruleName.MatchString(r.Name) {
				return fmt.Errorf("a rule's name %q has what the table can't keep in a comment: use letters, digits, spaces and - _ . : / @", r.Name)
			}
			if r.Protocol != "tcp" && r.Protocol != "udp" {
				return fmt.Errorf("rule %s is for %q, and a rule is for tcp or udp", r.Name, r.Protocol)
			}
			if len(r.Ports) == 0 || slices.Contains(r.Ports, 0) {
				return fmt.Errorf("rule %s covers no port, or port 0: give it ports from 1 to 65535", r.Name)
			}
			for _, p := range r.Sources {
				if !p.IsValid() {
					return fmt.Errorf("rule %s has a source that isn't an address or a range", r.Name)
				}
			}
		}
	}
	return nil
}

// Script is the nft transaction that replaces inet dockwell with this table:
// it makes the table if it isn't there, deletes it, and makes it again, all in
// one transaction, so a failed load changes nothing.
func (t Table) Script() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table %s %s {}\n", Family, Name)
	fmt.Fprintf(&b, "delete table %s %s\n", Family, Name)
	fmt.Fprintf(&b, "table %s %s {\n", Family, Name)

	// What listens on the host, and what Docker's proxy accepts.
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority filter; policy accept;\n")
	for _, line := range keepsReachable {
		fmt.Fprintf(&b, "\t\t%s\n", line)
	}
	// Containers reach the host, whatever the rules say.
	if ifs := t.dockerSet(); ifs != "" {
		fmt.Fprintf(&b, "\t\tiifname %s accept\n", ifs)
	}
	t.rules(&b, inputMatch)
	b.WriteString("\t}\n")

	// Ports Docker publishes: traffic Docker translated, arriving from outside
	// its networks, matched by the port it arrived on.
	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority filter; policy accept;\n")
	// Docker's alone, so other port forwards on the server, such as libvirt's,
	// LXD's or k3s's, aren't filtered as published ports; with Docker's networks
	// unknown, every one is; with no Docker, none is.
	switch ifs := t.dockerSet(); {
	case t.NoDocker:
	case ifs != "":
		fmt.Fprintf(&b, "\t\tct status dnat iifname != %s oifname %s jump published\n", ifs, ifs)
	default:
		b.WriteString("\t\tct status dnat jump published\n")
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain published {\n")
	b.WriteString("\t\tct state established,related accept\n")
	t.rules(&b, publishedMatch)
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// DeleteScript removes inet dockwell, whether it's there or not.
func DeleteScript() string {
	return fmt.Sprintf("table %s %s {}\ndelete table %s %s\n", Family, Name, Family, Name)
}

// rules writes the pins, allows, denies and policy, in that order, each line
// matching ports as match says.
func (t Table) rules(b *strings.Builder, match func(Rule) string) {
	for _, r := range t.Pins {
		writeRule(b, r, match, "accept")
	}
	for _, r := range t.Allows {
		writeRule(b, r, match, "accept")
	}
	for _, r := range t.Denies {
		writeRule(b, r, match, "drop")
	}
	if t.DenyRest {
		b.WriteString("\t\tdrop comment \"policy deny everything else\"\n")
	}
}

// writeRule writes a rule as one line, or one line for each family its
// sources hold, as ip and ip6 match their own family alone.
func writeRule(b *strings.Builder, r Rule, match func(Rule) string, verdict string) {
	comment := fmt.Sprintf(" comment %q", r.Name)
	if len(r.Sources) == 0 {
		fmt.Fprintf(b, "\t\t%s %s%s\n", match(r), verdict, comment)
		return
	}
	var v4, v6 []string
	for _, p := range r.Sources {
		if p.Addr().Is4In6() {
			// ::ffff:203.0.113.0/120 is 203.0.113.0/24, as IPv4 traffic carries it.
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		p = p.Masked()
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	if len(v4) > 0 {
		fmt.Fprintf(b, "\t\tip saddr %s %s %s%s\n", set(v4), match(r), verdict, comment)
	}
	if len(v6) > 0 {
		fmt.Fprintf(b, "\t\tip6 saddr %s %s %s%s\n", set(v6), match(r), verdict, comment)
	}
}

// inputMatch matches a rule's ports as the host receives them.
func inputMatch(r Rule) string {
	return fmt.Sprintf("%s dport %s", r.Protocol, set(ports(r.Ports)))
}

// publishedMatch matches a rule's ports as the port traffic arrived on, before
// Docker translated it to its container's.
func publishedMatch(r Rule) string {
	return fmt.Sprintf("meta l4proto %s ct original proto-dst %s", r.Protocol, set(ports(r.Ports)))
}

func ports(list []uint16) []string {
	sorted := slices.Clone(list)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	out := make([]string, len(sorted))
	for i, p := range sorted {
		out[i] = strconv.Itoa(int(p))
	}
	return out
}

// set is an nft set of items, or the item alone.
func set(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return "{ " + strings.Join(items, ", ") + " }"
}

// dockerSet is Docker's interfaces as an nft set of names, or empty.
func (t Table) dockerSet() string {
	var names []string
	for _, n := range t.DockerInterfaces {
		if matchable(n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)
	names = slices.Compact(names)
	quoted := make([]string, len(names))
	for i, n := range names {
		// nft takes what's between the quotes as it is, backslashes too.
		quoted[i] = `"` + n + `"`
	}
	return "{ " + strings.Join(quoted, ", ") + " }"
}
