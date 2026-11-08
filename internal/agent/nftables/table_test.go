// SPDX-License-Identifier: AGPL-3.0-only

package nftables

import (
	"net/netip"
	"strings"
	"testing"
)

func prefixes(t *testing.T, list ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// The table goes in the order ADR 0052 sets: what keeps the server reachable,
// containers, pins, allows, denies and the policy, each in the input chain and,
// matched by the port it arrived on, the chain for what Docker publishes.
func TestScriptLaysOutTheTable(t *testing.T) {
	table := Table{
		Pins:             []Rule{{Name: "pin ssh", Protocol: "tcp", Ports: []uint16{22}, Sources: prefixes(t, "203.0.113.7/32", "2001:db8::7/128")}},
		Allows:           []Rule{{Name: "rule web", Protocol: "tcp", Ports: []uint16{443, 80, 443}}},
		Denies:           []Rule{{Name: "rule no-dns", Protocol: "udp", Ports: []uint16{53}, Sources: prefixes(t, "198.51.100.0/24")}},
		DenyRest:         true,
		DockerInterfaces: []string{"docker0", "br-1a2b3c4d5e6f", "docker0"},
	}
	got, err := table.Script()
	if err != nil {
		t.Fatal(err)
	}
	want := `table inet dockwell {}
delete table inet dockwell
table inet dockwell {
	chain input {
		type filter hook input priority filter; policy accept;
		ct state established,related accept
		ct state invalid drop
		iifname "lo" accept
		icmp type { destination-unreachable, time-exceeded, parameter-problem } accept
		icmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem } accept
		icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert, nd-redirect } ip6 hoplimit 255 accept
		icmpv6 type { mld-listener-query, mld-listener-report, mld-listener-done, mld2-listener-report } ip6 saddr fe80::/10 ip6 hoplimit 1 accept
		udp sport 67 udp dport 68 accept
		ip6 saddr fe80::/10 udp sport 547 udp dport 546 accept
		icmp type echo-request limit rate 5/second burst 10 packets accept
		icmpv6 type echo-request limit rate 5/second burst 10 packets accept
		iifname { "br-1a2b3c4d5e6f", "docker0" } accept
		ip saddr 203.0.113.7/32 tcp dport 22 accept comment "pin ssh"
		ip6 saddr 2001:db8::7/128 tcp dport 22 accept comment "pin ssh"
		tcp dport { 80, 443 } accept comment "rule web"
		ip saddr 198.51.100.0/24 udp dport 53 drop comment "rule no-dns"
		drop comment "policy deny everything else"
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		ct status dnat iifname != { "br-1a2b3c4d5e6f", "docker0" } oifname { "br-1a2b3c4d5e6f", "docker0" } jump published
	}
	chain published {
		ct state established,related accept
		ip saddr 203.0.113.7/32 meta l4proto tcp ct original proto-dst 22 accept comment "pin ssh"
		ip6 saddr 2001:db8::7/128 meta l4proto tcp ct original proto-dst 22 accept comment "pin ssh"
		meta l4proto tcp ct original proto-dst { 80, 443 } accept comment "rule web"
		ip saddr 198.51.100.0/24 meta l4proto udp ct original proto-dst 53 drop comment "rule no-dns"
		drop comment "policy deny everything else"
	}
}
`
	if got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}
}

// With the policy off and Docker's networks unknown, nothing is dropped past
// what's invalid, and every translated packet goes through the rules.
func TestScriptWithNoPolicyAndNoDocker(t *testing.T) {
	got, err := Table{Denies: []Rule{{Name: "rule x", Protocol: "tcp", Ports: []uint16{3306}}}}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "policy deny") || strings.Contains(got, "iifname {") || strings.Contains(got, "iifname !=") {
		t.Errorf("script has a policy or Docker's interfaces:\n%s", got)
	}
	if !strings.Contains(got, "\t\tct status dnat jump published\n") {
		t.Errorf("script doesn't send every translated packet to its rules:\n%s", got)
	}
}

// An IPv4 address written as IPv6, as ::ffff:203.0.113.0/120, matches IPv4
// traffic, as that's how it arrives.
func TestScriptMatchesMappedAddressesAsIPv4(t *testing.T) {
	got, err := Table{Allows: []Rule{{Name: "rule m", Protocol: "tcp", Ports: []uint16{22}, Sources: prefixes(t, "::ffff:203.0.113.0/120")}}}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ip saddr 203.0.113.0/24 tcp dport 22 accept") || strings.Contains(got, "ip6 saddr ::ffff") {
		t.Errorf("a mapped range isn't matched as IPv4:\n%s", got)
	}
}

// What nft would read wrong, or a name that would end its comment, is refused
// before nft sees it.
func TestValidateRefusesWhatNftWouldMisread(t *testing.T) {
	ok := Rule{Name: "rule ok", Protocol: "tcp", Ports: []uint16{22}}
	cases := map[string]Table{
		"a quote in a name":       {Allows: []Rule{{Name: `x" accept`, Protocol: "tcp", Ports: []uint16{22}}}},
		"a protocol nft can't":    {Allows: []Rule{{Name: "rule a", Protocol: "icmp", Ports: []uint16{1}}}},
		"no port":                 {Denies: []Rule{{Name: "rule b", Protocol: "tcp"}}},
		"port 0":                  {Denies: []Rule{{Name: "rule c", Protocol: "udp", Ports: []uint16{0}}}},
		"a source that isn't one": {Pins: []Rule{{Name: "pin ssh", Protocol: "tcp", Ports: []uint16{22}, Sources: []netip.Prefix{{}}}}},
	}
	for what, table := range cases {
		if _, err := table.Script(); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
	if _, err := (Table{Allows: []Rule{ok}, DockerInterfaces: []string{"docker0"}}).Script(); err != nil {
		t.Errorf("a plain table: %v", err)
	}
}

// An interface name nft can match in quotes stays, whatever it holds; one it
// can't is left out and said, rather than refuse the whole table, which would
// stop a rollback too (review of #306, finding 7).
func TestInterfacesNftCantMatchAreSkipped(t *testing.T) {
	table := Table{DockerInterfaces: []string{"br+lan", `br\lan`, "brücke", "docker0", `br"x`, "br\tx", "br*", "br-0123456789abcdef"}}
	got, err := table.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `iifname { "br+lan", "br\lan", "brücke", "docker0" } accept`) {
		t.Errorf("script:\n%s", got)
	}
	if skipped := table.Skipped(); strings.Join(skipped, "|") != `br"x|br`+"\t"+`x|br*|br-0123456789abcdef` {
		t.Errorf("skipped %q", skipped)
	}
	if words := SkippedWords("br*"); !strings.Contains(words, "Rename its network's bridge") || !strings.Contains(words, "aren't filtered") {
		t.Errorf("skipped words %q", words)
	}
}
