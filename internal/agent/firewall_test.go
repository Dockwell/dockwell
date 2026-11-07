// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

const ufwVerbose = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     LIMIT IN    Anywhere
80,443/tcp                 ALLOW IN    Anywhere                   # web
5432/tcp                   ALLOW IN    203.0.113.0/24
8080                       DENY IN     Anywhere
53                         ALLOW OUT   Anywhere
Anywhere on docker0        ALLOW FWD   Anywhere on eth0
22/tcp (v6)                LIMIT IN    Anywhere (v6)
`

const firewalldList = `public (active)
  target: default
  icmp-block-inversion: no
  interfaces: eth0
  sources:
  services: dhcpv6-client ssh
  ports: 8080/tcp 9000-9100/udp
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
	rule family="ipv4" source address="203.0.113.0/24" port port="22" protocol="tcp" accept
`

const nftRuleset = `{"nftables": [{"metainfo": {"version": "1.0.9", "json_schema_version": 1}},
 {"table": {"family": "inet", "name": "filter", "handle": 1}},
 {"chain": {"family": "inet", "table": "filter", "name": "input", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "accept"}},
 {"set": {"family": "inet", "name": "blocked", "table": "filter", "type": "ipv4_addr", "handle": 2, "elem": ["198.51.100.7"]}},
 {"rule": {"family": "inet", "table": "filter", "chain": "input", "handle": 3, "expr": []}},
 {"rule": {"family": "inet", "table": "filter", "chain": "input", "handle": 4, "expr": []}},
 {"table": {"family": "inet", "name": "dockwell", "handle": 2}},
 {"rule": {"family": "inet", "table": "dockwell", "chain": "input", "handle": 5, "expr": []}},
 {"table": {"family": "inet", "name": "firewalld", "handle": 3}},
 {"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "handle": 6, "expr": []}},
 {"table": {"family": "ip", "name": "nat", "handle": 4}},
 {"rule": {"family": "ip", "table": "nat", "chain": "DOCKER", "handle": 7, "expr": []}}]}`

// ufw's table reads as rules by port and source, inbound alone, with its
// default for what comes in, its limits, comments and IPv6 rules.
func TestParseUFW(t *testing.T) {
	tool := &dockwellv1.FirewallTool{}
	parseUFW([]byte(ufwVerbose), tool)
	if !tool.GetActive() || tool.GetDefaultInbound() != dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY {
		t.Errorf("active %v, default %v", tool.GetActive(), tool.GetDefaultInbound())
	}
	var got []string
	for _, r := range tool.GetRules() {
		got = append(got, strings.Join([]string{r.GetVerdict().String(), r.GetPort(), r.GetSource(), r.GetComment(), map[bool]string{true: "v6"}[r.GetIpv6()]}, "|"))
	}
	want := []string{
		"FIREWALL_VERDICT_LIMIT|22/tcp|||",
		"FIREWALL_VERDICT_ALLOW|80,443/tcp||web|",
		"FIREWALL_VERDICT_ALLOW|5432/tcp|203.0.113.0/24||",
		"FIREWALL_VERDICT_DENY|8080|||",
		"FIREWALL_VERDICT_LIMIT|22/tcp|||v6",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	inactive := &dockwellv1.FirewallTool{}
	parseUFW([]byte("Status: inactive\n"), inactive)
	if inactive.GetActive() || len(inactive.GetRules()) != 0 {
		t.Errorf("an inactive ufw reads as %v", inactive)
	}
}

// firewalld's default zone reads as its services and ports allowed, its target as
// its default, and its rich rules as it says them.
func TestParseFirewalld(t *testing.T) {
	tool := &dockwellv1.FirewallTool{}
	parseFirewalld([]byte(firewalldList), tool)
	if tool.GetName() != "public" || tool.GetDefaultInbound() != dockwellv1.FirewallVerdict_FIREWALL_VERDICT_REJECT {
		t.Errorf("zone %q, default %v", tool.GetName(), tool.GetDefaultInbound())
	}
	var ports []string
	for _, r := range tool.GetRules() {
		ports = append(ports, r.GetPort())
	}
	if strings.Join(ports, " ") != "dhcpv6-client ssh 8080/tcp 9000-9100/udp" {
		t.Errorf("allowed %v", ports)
	}
	if len(tool.GetOther()) != 1 || !strings.Contains(tool.GetOther()[0], `source address="203.0.113.0/24"`) {
		t.Errorf("rich rules %v", tool.GetOther())
	}
}

// nftables's tables count their rules, sets aside, and Dockwell's and firewalld's
// own are left to what says them otherwise.
func TestParseNftables(t *testing.T) {
	tools, err := parseNftables([]byte(nftRuleset))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range tools {
		got = append(got, tool.GetName()+"="+string(rune('0'+tool.GetRuleCount())))
	}
	if strings.Join(got, " ") != "inet filter=2 ip nat=1" {
		t.Errorf("tables %v, want inet filter with 2 rules and ip nat with 1", got)
	}
}

// The agent reads what's installed, says why it couldn't read what it couldn't,
// and lists the ports Docker publishes, which a firewall's input rules don't see.
func TestFirewallReaderReadsWhatsThere(t *testing.T) {
	e := dockertest.New(t)
	e.AddContainer(docker.Container{ID: "a1", Name: "/shop-web-1", HostConfig: docker.HostConfig{
		PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "8080"}}},
	}})
	// sshd on 22, and Docker's proxy on the 8080 it publishes.
	proc := fstest.MapFS{"net/tcp": {Data: []byte(`  sl  local_address rem_address   st
   0: 00000000:0016 00000000:0000 0A
   1: 00000000:1F90 00000000:0000 0A
`)}}
	f := &firewallReader{
		docker: e.Client(),
		proc:   proc,
		has:    func(name string) bool { return name == "ufw" || name == "nft" },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch name {
			case "ufw":
				return []byte(ufwVerbose), nil
			case "nft":
				return nil, errors.New("Operation not permitted (you must be root)")
			}
			return nil, errors.New("no such command")
		},
	}
	read, err := f.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(read.GetTools()) != 2 {
		t.Fatalf("tools %v, want ufw and nftables", read.GetTools())
	}
	ufw, nft := read.GetTools()[0], read.GetTools()[1]
	if ufw.GetKind() != dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW || len(ufw.GetRules()) != 5 {
		t.Errorf("ufw %v", ufw)
	}
	if nft.GetKind() != dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES || !strings.Contains(nft.GetUnreadable(), "needs root") {
		t.Errorf("nftables without root says %q", nft.GetUnreadable())
	}
	pub := read.GetPublished()
	if len(pub) != 1 || pub[0].GetContainer() != "shop-web-1" || pub[0].GetHostPort() != 8080 || pub[0].GetContainerPort() != 80 {
		t.Errorf("published %v", pub)
	}
	// ufw's deny by default doesn't close 8080: Docker forwards it past ufw's
	// input rules, so it's said as published, not as listening behind ufw.
	listening := read.GetListening()
	if len(listening) != 1 || listening[0].GetPort() != 22 || listening[0].GetReach() != dockwellv1.ListenerReach_LISTENER_REACH_OPEN {
		t.Errorf("listening %v, want sshd on 22 alone, open", listening)
	}

	// A Docker that doesn't answer leaves the firewalls to say, and says why its ports are missing.
	e.Intercept(func(*http.Request) error { return errors.New("the engine is restarting") })
	read, err = f.read(t.Context())
	if err != nil {
		t.Fatalf("with Docker not answering: %v", err)
	}
	if len(read.GetTools()) != 2 || len(read.GetPublished()) != 0 || !strings.Contains(read.GetPublishedUnreadable(), "the engine is restarting") {
		t.Errorf("with Docker not answering: tools %d, published %v, why %q", len(read.GetTools()), read.GetPublished(), read.GetPublishedUnreadable())
	}
	if len(read.GetListening()) != 2 {
		t.Errorf("with Docker not answering, listening %v, want 22 and 8080", read.GetListening())
	}
}

const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001
   1: 0100007F:1DB0 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1002
   2: 0500000A:0016 6400A8C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 1003
   3: 00000000:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 1004
`

const procTCP6 = `  sl  local_address                         remote_address                        st
   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A
   1: 00000000000000000000000001000000:1DB0 00000000000000000000000000000000:0000 0A
`

const procUDP = `  sl  local_address rem_address   st
   0: 00000000:0044 00000000:0000 07
   1: 0500000A:A1B2 08080808:0035 01
`

// What listens beyond loopback, TCP listening and UDP bound with no peer, from
// /proc/net, each once.
func TestListenersComeFromProc(t *testing.T) {
	proc := fstest.MapFS{
		"net/tcp":  {Data: []byte(procTCP)},
		"net/tcp6": {Data: []byte(procTCP6)},
		"net/udp":  {Data: []byte(procUDP)},
	}
	var got []string
	for _, l := range listeners(proc) {
		got = append(got, l.GetProtocol()+" "+l.GetAddress()+":"+strconv.Itoa(int(l.GetPort())))
	}
	want := "tcp 0.0.0.0:22 udp 0.0.0.0:68 tcp :::443 tcp 0.0.0.0:5432"
	if strings.Join(got, " ") != want {
		t.Errorf("listening:\n%s\nwant:\n%s", strings.Join(got, " "), want)
	}
}

// How far each listener reaches is worked out from ufw's own rules, first match
// first, and its default; and from firewalld's zone, its well-known services, and
// what it can't resolve.
func TestReachOfListeners(t *testing.T) {
	ufw := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW}
	parseUFW([]byte(ufwVerbose), ufw)
	firewalld := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD, Active: true}
	parseFirewalld([]byte(firewalldList), firewalld)
	plainFirewalld := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD, Active: true}
	parseFirewalld([]byte(strings.Split(firewalldList, "  rich rules:")[0]), plainFirewalld)
	cases := []struct {
		tool  *dockwellv1.FirewallTool
		port  uint32
		proto string
		want  string
	}{
		{ufw, 22, "tcp", "LISTENER_REACH_OPEN"},
		{ufw, 443, "tcp", "LISTENER_REACH_OPEN"},
		{ufw, 5432, "tcp", "LISTENER_REACH_SOME 203.0.113.0/24"},
		{ufw, 8080, "udp", "LISTENER_REACH_CLOSED"},
		{ufw, 3306, "tcp", "LISTENER_REACH_CLOSED"},
		{firewalld, 22, "tcp", "LISTENER_REACH_OPEN"},
		{firewalld, 9050, "udp", "LISTENER_REACH_OPEN"},
		{firewalld, 5432, "tcp", "LISTENER_REACH_UNKNOWN"},
		{plainFirewalld, 5432, "tcp", "LISTENER_REACH_CLOSED"},
	}
	for _, c := range cases {
		l := &dockwellv1.HostListener{Address: "0.0.0.0", Port: c.port, Protocol: c.proto}
		reachOf(l, []*dockwellv1.FirewallTool{c.tool})
		got := strings.TrimSpace(l.GetReach().String() + " " + strings.Join(l.GetSources(), " "))
		if got != c.want {
			t.Errorf("%s %d/%s reaches %q, want %q", toolName(c.tool), c.port, c.proto, got, c.want)
		}
	}
	// An app profile ufw doesn't say the ports of leaves what it might cover unknown.
	named := &dockwellv1.FirewallTool{
		Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW, Active: true, DefaultInbound: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY,
		Rules: []*dockwellv1.FirewallSeenRule{{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: "Nginx Full"}},
	}
	l := &dockwellv1.HostListener{Address: "0.0.0.0", Port: 80, Protocol: "tcp"}
	reachOf(l, []*dockwellv1.FirewallTool{named})
	if l.GetReach() != dockwellv1.ListenerReach_LISTENER_REACH_UNKNOWN || l.GetDecidedBy() != "ufw" {
		t.Errorf("behind an app profile: %v by %q", l.GetReach(), l.GetDecidedBy())
	}
	// ufw's "Anywhere" covers every port, and a default that allows lets in
	// everyone, not only the sources a rule named.
	anywhere := &dockwellv1.FirewallTool{
		Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW, Active: true, DefaultInbound: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY,
		Rules: []*dockwellv1.FirewallSeenRule{{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: "Anywhere", Source: "198.51.100.9"}},
	}
	l = &dockwellv1.HostListener{Address: "0.0.0.0", Port: 3306, Protocol: "tcp"}
	reachOf(l, []*dockwellv1.FirewallTool{anywhere})
	if l.GetReach() != dockwellv1.ListenerReach_LISTENER_REACH_SOME || strings.Join(l.GetSources(), " ") != "198.51.100.9" {
		t.Errorf("behind an allow from one address to anywhere: %v from %v", l.GetReach(), l.GetSources())
	}
	anywhere.DefaultInbound = dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW
	l = &dockwellv1.HostListener{Address: "0.0.0.0", Port: 3306, Protocol: "tcp"}
	reachOf(l, []*dockwellv1.FirewallTool{anywhere})
	if l.GetReach() != dockwellv1.ListenerReach_LISTENER_REACH_OPEN {
		t.Errorf("with a default that allows: %v from %v", l.GetReach(), l.GetSources())
	}

	// With nothing filtering, everything is open.
	l = &dockwellv1.HostListener{Address: "0.0.0.0", Port: 80, Protocol: "tcp"}
	reachOf(l, nil)
	if l.GetReach() != dockwellv1.ListenerReach_LISTENER_REACH_OPEN {
		t.Errorf("with no firewall: %v", l.GetReach())
	}
}
