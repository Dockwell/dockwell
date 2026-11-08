// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// testdata reads a tool's output from testdata/firewall: the reviewer's
// captures in each tool's own format, and outputs written in the formats
// ufw's backend_iptables.py, firewall-cmd, nft -j and iptables-save print.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "firewall", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ufwRowOf is a row of `ufw status verbose` as ufw's backend prints it.
func ufwRowOf(to, action, from string) string {
	return fmt.Sprintf("%-26s %-12s%-26s", to, action, from)
}

func ufwStatus(rows ...string) []byte {
	return []byte("Status: active\nLogging: on (low)\nDefault: deny (incoming), allow (outgoing), disabled (routed)\nNew profiles: skip\n\nTo                         Action      From\n--                         ------      ----\n" + strings.Join(rows, "\n") + "\n")
}

func ufwOf(t *testing.T, out []byte) *dockwellv1.FirewallTool {
	t.Helper()
	tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW}
	if err := parseUFW(out, tool); err != nil {
		t.Fatal(err)
	}
	return tool
}

// reachIs says how far a listener reaches past tools, as one string to compare.
func reachIs(tools []*dockwellv1.FirewallTool, addr string, port uint32, proto string, opts reachOptions) string {
	l := &dockwellv1.HostListener{Address: addr, Port: port, Protocol: proto}
	reachOf(l, tools, opts)
	out := strings.TrimPrefix(l.GetReach().String(), "LISTENER_REACH_")
	if l.GetDecidedBy() != "" {
		out += " by " + l.GetDecidedBy()
	}
	if len(l.GetSources()) > 0 {
		out += " from " + strings.Join(l.GetSources(), ",")
	}
	if len(l.GetInterfaces()) > 0 {
		out += " on " + strings.Join(l.GetInterfaces(), ",")
	}
	if l.GetReason() != dockwellv1.ListenerReachReason_LISTENER_REACH_REASON_UNSPECIFIED {
		out += " (" + strings.TrimPrefix(l.GetReason().String(), "LISTENER_REACH_REASON_") + ")"
	}
	return out
}

// ufw's table reads as rules by port and source, inbound alone, with its
// default for what comes in, its limits, comments and IPv6 rules.
func TestParseUFW(t *testing.T) {
	tool := ufwOf(t, testdata(t, "ufw-ports.txt"))
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
		"FIREWALL_VERDICT_ALLOW|5432/tcp|203.0.113.0/24|office|",
		"FIREWALL_VERDICT_DENY|9100/tcp|||",
		"FIREWALL_VERDICT_LIMIT|22/tcp|||v6",
		"FIREWALL_VERDICT_ALLOW|80,443/tcp||web|v6",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	inactive := ufwOf(t, []byte("Status: inactive\n"))
	if inactive.GetActive() || len(inactive.GetRules()) != 0 {
		t.Errorf("an inactive ufw reads as %v", inactive)
	}
}

// What ufw status verbose prints for app profiles, interfaces, addresses,
// logging and IPv6 reads whole, and what it prints too long for its columns
// isn't dropped (review of #300, P1 and P3).
func TestParseUFWReadsEveryVerboseRow(t *testing.T) {
	tool := ufwOf(t, testdata(t, "ufw-long.txt"))
	var got []string
	for _, r := range tool.GetRules() {
		got = append(got, strings.Join([]string{r.GetPort(), r.GetApp(), r.GetInterface(), r.GetDestination(), r.GetSource(), r.GetComment(), map[bool]string{true: "v6"}[r.GetIpv6()]}, "|"))
	}
	want := []string{
		"22/tcp|OpenSSH|||||",
		"443/tcp||eth0||||",
		"8000:8100/tcp||tailscale0||||",
		"9000/tcp|||203.0.113.5|||",
		"Anywhere||||198.51.100.9|monitoring|",
		"443/tcp|||2001:db8:abcd:12::/64|||v6",
		"5432/tcp||||2001:db8::/32||v6",
		"22/tcp|OpenSSH|||||v6",
		"80,443/tcp|Nginx Full|||||v6",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// What ufw says that Dockwell can't read makes the read fail, rather than read
// as ufw off, or leave a rule out (P3, P4).
func TestParseUFWRefusesWhatItCantRead(t *testing.T) {
	for what, out := range map[string][]byte{
		"a status in another language": []byte(strings.Replace(string(ufwStatus(ufwRowOf("22/tcp", "ALLOW IN", "Anywhere"))), "Status: active", "Status: aktiv", 1)),
		"no status at all":             []byte("ERROR: problem running iptables\n"),
		"a row with no action":         ufwStatus("22/tcp                     ALLOW-ish   Anywhere"),
	} {
		if err := parseUFW(out, &dockwellv1.FirewallTool{}); err == nil {
			t.Errorf("%s: read without an error", what)
		}
	}
}

// How far a listener reaches past ufw, by its own rules, as the review of #300
// found it read wrong: app profiles (P1), interfaces (P2), long rows (P3), and
// a family's rules on the other's listeners (P5).
func TestUFWReach(t *testing.T) {
	apps := []*dockwellv1.FirewallTool{ufwOf(t, testdata(t, "ufw-apps.txt"))}
	long := []*dockwellv1.FirewallTool{ufwOf(t, testdata(t, "ufw-long.txt"))}
	mixed := []*dockwellv1.FirewallTool{ufwOf(t, ufwStatus(ufwRowOf("8080/tcp", "DENY IN", "Anywhere"), ufwRowOf("8080/tcp (v6)", "ALLOW IN", "Anywhere (v6)")))}
	v6From := []*dockwellv1.FirewallTool{ufwOf(t, ufwStatus(ufwRowOf("5432/tcp", "ALLOW IN", "2001:db8::/32")))}
	dual := reachOptions{dualStack: true}
	cases := []struct {
		name  string
		tools []*dockwellv1.FirewallTool
		addr  string
		port  uint32
		proto string
		opts  reachOptions
		want  string
	}{
		{"ufw allow OpenSSH", apps, "0.0.0.0", 22, "tcp", reachOptions{}, "OPEN by ufw"},
		{`ufw allow "Nginx Full"`, apps, "0.0.0.0", 443, "tcp", reachOptions{}, "OPEN by ufw"},
		{"OpenSSH on IPv6", apps, "::", 22, "tcp", reachOptions{}, "OPEN by ufw"},
		{"from one range", apps, "0.0.0.0", 5432, "tcp", reachOptions{}, "SOME by ufw from 203.0.113.0/24"},
		{"nothing allows it", apps, "0.0.0.0", 3306, "tcp", reachOptions{}, "CLOSED by ufw"},
		{"an allow on eth0", long, "10.0.0.5", 443, "tcp", reachOptions{}, "SOME by ufw from 198.51.100.9 on eth0"},
		{"a range on tailscale0, in a long row", long, "100.64.0.1", 8050, "tcp", reachOptions{}, "SOME by ufw from 198.51.100.9 on tailscale0"},
		{"a rule to this listener's own address", long, "203.0.113.5", 9000, "tcp", reachOptions{}, "OPEN by ufw"},
		{"Nginx Full on IPv6, in a long row", long, "::", 80, "tcp", reachOptions{}, "OPEN by ufw"},
		{"an IPv4 deny before an IPv6 allow, on IPv4", mixed, "0.0.0.0", 8080, "tcp", reachOptions{}, "CLOSED by ufw"},
		{"an IPv4 deny before an IPv6 allow, on a socket taking both", mixed, "::", 8080, "tcp", dual, "OPEN by ufw"},
		{"an IPv4 deny before an IPv6 allow, on IPv6 alone", mixed, "::", 8080, "tcp", reachOptions{}, "OPEN by ufw"},
		{"an IPv6 source doesn't let IPv4 in", v6From, "0.0.0.0", 5432, "tcp", reachOptions{}, "CLOSED by ufw"},
		{"an IPv6 source lets IPv6 in", v6From, "::", 5432, "tcp", reachOptions{}, "SOME by ufw from 2001:db8::/32"},
		{"DHCP replies, by ufw's before.rules", apps, "0.0.0.0", 68, "udp", reachOptions{}, "OPEN by ufw (BUILT_IN)"},
		{"mDNS, by ufw's before.rules", apps, "::", 5353, "udp", reachOptions{}, "OPEN by ufw (BUILT_IN)"},
	}
	for _, c := range cases {
		if got := reachIs(c.tools, c.addr, c.port, c.proto, c.opts); got != c.want {
			t.Errorf("%s: %s %d/%s reaches %q, want %q", c.name, c.addr, c.port, c.proto, got, c.want)
		}
	}
	// An app profile ufw doesn't say the ports of, as plain ufw status lists
	// it, leaves what it might cover unknown, and says why.
	named := []*dockwellv1.FirewallTool{{
		Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW, Active: true, DefaultInbound: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY,
		Rules: []*dockwellv1.FirewallSeenRule{{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: "Nginx Full"}},
	}}
	if got := reachIs(named, "0.0.0.0", 80, "tcp", reachOptions{}); got != "UNKNOWN by ufw (APP_PROFILE)" {
		t.Errorf("behind an app profile: %s", got)
	}
	if got := reachIs(nil, "0.0.0.0", 80, "tcp", reachOptions{}); got != "OPEN" {
		t.Errorf("with no firewall: %s", got)
	}
}

// fakeTools answers the reader's commands from a table of outputs, each by the
// command and its arguments, and counts each run.
type fakeTools struct {
	mu      sync.Mutex
	outputs map[string]hosttool.Result
	errs    map[string]error
	ran     []string
}

func (f *fakeTools) run(_ context.Context, name string, args ...string) (hosttool.Result, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.ran = append(f.ran, key)
	f.mu.Unlock()
	if err := f.errs[key]; err != nil {
		return hosttool.Result{Code: -1}, err
	}
	res, ok := f.outputs[key]
	if !ok {
		return hosttool.Result{Code: 127, Errs: key + ": not faked"}, nil
	}
	return res, nil
}

func (f *fakeTools) has(name string) bool {
	for key := range f.outputs {
		if strings.HasPrefix(key, name+" ") || key == name {
			return true
		}
	}
	for key := range f.errs {
		if strings.HasPrefix(key, name+" ") {
			return true
		}
	}
	return false
}

func ok(out []byte) hosttool.Result { return hosttool.Result{Out: out} }

// proc is /proc/net with listeners on these TCP ports, on every IPv4 address.
func proc(ports ...uint16) fstest.MapFS {
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st\n")
	for i, p := range ports {
		fmt.Fprintf(&b, "   %d: 00000000:%04X 00000000:0000 0A\n", i, p)
	}
	return fstest.MapFS{"net/tcp": {Data: []byte(b.String())}}
}

func reader(f *fakeTools, p fstest.MapFS) *firewallReader {
	return &firewallReader{run: f.run, has: f.has, proc: p, euid: func() int { return 0 }}
}

func listening(t *testing.T, read *dockwellv1.ServerFirewall) map[uint32]string {
	t.Helper()
	out := map[uint32]string{}
	for _, l := range read.GetListening() {
		out[l.GetPort()] = reachIs(read.GetTools(), l.GetAddress(), l.GetPort(), l.GetProtocol(), reachOptions{})
	}
	return out
}

// firewalld is read zone by zone: every zone that takes traffic, and the
// default one, with rich rules whatever they hold (P6); a source bound to a
// zone that lets it in reaches what the rest can't (H5).
func TestFirewalldIsReadByZone(t *testing.T) {
	tools := &fakeTools{outputs: map[string]hosttool.Result{
		"firewall-cmd --state":                   ok([]byte("running\n")),
		"firewall-cmd --get-default-zone":        ok([]byte("public\n")),
		"firewall-cmd --get-active-zones":        ok(testdata(t, "firewalld-active-zones.txt")),
		"firewall-cmd --list-all --zone=public":  ok(testdata(t, "firewalld-public.txt")),
		"firewall-cmd --list-all --zone=trusted": ok(testdata(t, "firewalld-trusted.txt")),
		"nft -j -t list ruleset":                 ok([]byte(`{"nftables": []}`)),
	}}
	read, err := reader(tools, proc(22, 3306, 5432, 8080, 9090)).read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var zones []string
	for _, tool := range read.GetTools() {
		zones = append(zones, tool.GetName())
	}
	if strings.Join(zones, " ") != "public trusted" {
		t.Errorf("zones %v, want public and trusted, without Docker's", zones)
	}
	for _, ran := range tools.ran {
		if strings.Contains(ran, "--zone=docker") {
			t.Errorf("read Docker's zone: %s", ran)
		}
	}
	got := listening(t, read)
	want := map[uint32]string{
		22:   "OPEN by firewalld",
		8080: "OPEN by firewalld",
		9090: "OPEN by firewalld",
		5432: "UNKNOWN by firewalld (RICH_RULE)",
		3306: "SOME by firewalld from 10.0.0.0/8",
	}
	for port, w := range want {
		if got[port] != w {
			t.Errorf("%d reaches %q, want %q", port, got[port], w)
		}
	}
	// Rich rules with IPv6 addresses in them are each a rule (P6).
	zone := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD, Active: true, DefaultZone: true}
	parseFirewalld(testdata(t, "firewalld-rich-ipv6.txt"), zone)
	if len(zone.GetOther()) != 2 {
		t.Fatalf("rich rules %q, want both", zone.GetOther())
	}
	if got := reachIs([]*dockwellv1.FirewallTool{zone}, "::", 5432, "tcp", reachOptions{}); got != "UNKNOWN by firewalld (RICH_RULE)" {
		t.Errorf("5432 behind rich rules reaches %q", got)
	}
	if got := reachIs([]*dockwellv1.FirewallTool{zone}, "::", 22, "tcp", reachOptions{}); got != "OPEN by firewalld" {
		t.Errorf("22 beside rich rules for 5432 reaches %q", got)
	}
	// A port the zone opens, with a rich rule for it, which runs first and may reject it.
	zone.Rules = append(zone.Rules, &dockwellv1.FirewallSeenRule{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: "5432/tcp"})
	if got := reachIs([]*dockwellv1.FirewallTool{zone}, "::", 5432, "tcp", reachOptions{}); got != "UNKNOWN by firewalld (RICH_RULE)" {
		t.Errorf("5432 opened beside a rich rule for it reaches %q", got)
	}
}

// firewalld that isn't running says so by "not running" and status 252;
// anything else --state does means it can't say, and nothing reads open (P7,
// M11).
func TestFirewalldThatCantSay(t *testing.T) {
	empty := ok([]byte(`{"nftables": []}`))
	notRunning := &fakeTools{outputs: map[string]hosttool.Result{
		"firewall-cmd --state":   {Errs: "not running\n", Code: 252},
		"nft -j -t list ruleset": empty,
	}}
	read, _ := reader(notRunning, proc(5432)).read(t.Context())
	if fw := read.GetTools()[0]; fw.GetActive() || fw.GetUnreadable() != "" {
		t.Errorf("not running reads as %v", fw)
	}
	for what, tools := range map[string]*fakeTools{
		"a timeout": {errs: map[string]error{"firewall-cmd --state": context.DeadlineExceeded}, outputs: map[string]hosttool.Result{"nft -j -t list ruleset": empty}},
		"a D-Bus error": {outputs: map[string]hosttool.Result{
			"firewall-cmd --state":   {Errs: "Error: org.freedesktop.DBus.Error.AccessDenied\n", Code: 1},
			"nft -j -t list ruleset": empty,
		}},
	} {
		read, err := reader(tools, proc(5432)).read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if fw := read.GetTools()[0]; fw.GetUnreadable() == "" {
			t.Errorf("%s: firewalld reads as %v", what, fw)
		}
		if got := listening(t, read)[5432]; got != "UNKNOWN by firewalld (UNREADABLE)" {
			t.Errorf("%s: 5432 reaches %q", what, got)
		}
	}
}

// The kernel's other rules: ufw's and Docker's tables fold into theirs, a
// person's table or iptables-legacy chain that may drop makes what nothing
// else closes read "can't tell" (P8, P9), and a ruleset that can't be read
// does too (H7).
func TestOtherRulesNeverReadOpen(t *testing.T) {
	cases := []struct {
		name    string
		outputs map[string]hosttool.Result
		errs    map[string]error
		tables  string
		reach   string
		user    uint32
	}{
		{
			name: "Ubuntu's ufw and Docker on iptables-nft",
			outputs: map[string]hosttool.Result{
				"ufw status verbose":     ok(testdata(t, "ufw-ports.txt")),
				"nft -j -t list ruleset": ok(testdata(t, "nft-ubuntu-ufw-docker.json")),
			},
			tables: "ufw", reach: "OPEN by ufw",
		},
		{
			name: "ufw-docker's rules in DOCKER-USER",
			outputs: map[string]hosttool.Result{
				"ufw status verbose":     ok(testdata(t, "ufw-ports.txt")),
				"nft -j -t list ruleset": ok(testdata(t, "nft-ufw-docker-user.json")),
			},
			tables: "ufw", reach: "OPEN by ufw", user: 2,
		},
		{
			name:    "Debian's default ruleset beside Docker's, and Dockwell's own",
			outputs: map[string]hosttool.Result{"nft -j -t list ruleset": ok(testdata(t, "nft-debian-docker.json"))},
			tables:  "", reach: "OPEN",
		},
		{
			name:    "a person's inet filter that drops",
			outputs: map[string]hosttool.Result{"nft -j -t list ruleset": ok(testdata(t, "nft-filter-drops.json"))},
			tables:  "nftables inet filter", reach: "UNKNOWN by nftables inet filter (OTHER_RULES)",
		},
		{
			name:    "iptables-nft's ip filter that drops, with no ufw",
			outputs: map[string]hosttool.Result{"nft -j -t list ruleset": ok(testdata(t, "nft-iptables-input-drops.json"))},
			tables:  "nftables ip filter,nftables ip6 filter", reach: "UNKNOWN by nftables ip filter (OTHER_RULES)",
		},
		{
			name: "iptables-legacy's INPUT that drops, with no rules",
			outputs: map[string]hosttool.Result{
				"nft -j -t list ruleset": ok([]byte(`{"nftables": []}`)),
				"iptables-legacy-save":   ok(testdata(t, "iptables-input-drop.txt")),
				"ip6tables-legacy-save":  ok(nil),
			},
			tables: "iptables-legacy IPv4", reach: "UNKNOWN by iptables-legacy IPv4 (OTHER_RULES)",
		},
		{
			name: "ufw on iptables-legacy, as Ubuntu 20.04 has it",
			outputs: map[string]hosttool.Result{
				"ufw status verbose":     ok(testdata(t, "ufw-ports.txt")),
				"nft -j -t list ruleset": ok([]byte(`{"nftables": []}`)),
				"iptables-legacy-save":   ok(testdata(t, "iptables-legacy-ufw.txt")),
			},
			tables: "ufw", reach: "OPEN by ufw",
		},
		{
			name: "ufw-docker's rules in DOCKER-USER, on iptables-legacy",
			outputs: map[string]hosttool.Result{
				"ufw status verbose":     ok(testdata(t, "ufw-ports.txt")),
				"nft -j -t list ruleset": ok([]byte(`{"nftables": []}`)),
				"iptables-legacy-save":   ok(testdata(t, "iptables-legacy-ufw-docker.txt")),
			},
			tables: "ufw", reach: "OPEN by ufw", user: 2,
		},
		{
			name:    "no nft, so iptables-save",
			outputs: map[string]hosttool.Result{"iptables-save": ok(testdata(t, "iptables-input-drop.txt"))},
			tables:  "iptables IPv4", reach: "UNKNOWN by iptables IPv4 (OTHER_RULES)",
		},
		{
			name:   "neither nft nor iptables-save",
			tables: "nftables", reach: "UNKNOWN by nftables (UNREADABLE)",
		},
		{
			name:   "nft that can't be read",
			errs:   map[string]error{"nft -j -t list ruleset": errors.New("nft ended with status 1: netlink: Error: cache initialization failed: Operation not permitted")},
			tables: "nftables", reach: "UNKNOWN by nftables (UNREADABLE)",
		},
	}
	for _, c := range cases {
		tools := &fakeTools{outputs: c.outputs, errs: c.errs}
		read, err := reader(tools, proc(22)).read(t.Context())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var names []string
		for _, tool := range read.GetTools() {
			names = append(names, toolName(tool))
		}
		if got := strings.Join(names, ","); got != c.tables {
			t.Errorf("%s: tables %q, want %q", c.name, got, c.tables)
		}
		if got := listening(t, read)[22]; got != c.reach {
			t.Errorf("%s: 22 reaches %q, want %q", c.name, got, c.reach)
		}
		if read.GetDockerUserRules() != c.user {
			t.Errorf("%s: DOCKER-USER holds %d, want %d", c.name, read.GetDockerUserRules(), c.user)
		}
	}
}

// A firewall that needs root says the agent doesn't run as root only when it
// doesn't, and otherwise quotes what refused it (L15); its words start with
// Dockwell, so the tool's name keeps its case (L16).
func TestUnreadableSaysWhy(t *testing.T) {
	err := errors.New("ufw ended with status 1: ERROR: You need to be root to run this script")
	f := &firewallReader{euid: func() int { return 1000 }}
	if got := f.unreadable("ufw", err); !strings.HasPrefix(got, "Dockwell can't read ufw: it needs root, and the agent doesn't run as root here") {
		t.Errorf("not root: %q", got)
	}
	f.euid = func() int { return 0 }
	if got := f.unreadable("ufw", err); !strings.Contains(got, "though the agent runs as root") || !strings.Contains(got, "You need to be root") || !strings.Contains(got, "LXC") {
		t.Errorf("root, refused: %q", got)
	}
}

// The agent reads what's installed, and lists the ports Docker publishes,
// which ufw's input rules don't see; IPv6 to a container without IPv6 comes
// through Docker's proxy, which they do (M10).
func TestFirewallReaderReadsWhatsThere(t *testing.T) {
	e := dockertest.New(t)
	e.AddContainer(docker.Container{ID: "a1", Name: "/shop-web-1", HostConfig: docker.HostConfig{
		PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8080"}}},
	}})
	// sshd on 22, and Docker's proxy on 8080, on IPv4 and IPv6 apart.
	p := fstest.MapFS{
		"net/tcp":  {Data: []byte("  sl  local_address rem_address   st\n   0: 00000000:0016 00000000:0000 0A\n   1: 00000000:1F90 00000000:0000 0A\n")},
		"net/tcp6": {Data: []byte("  sl  local_address rem_address   st\n   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A\n")},
	}
	tools := &fakeTools{outputs: map[string]hosttool.Result{
		"ufw status verbose":     ok(testdata(t, "ufw-apps.txt")),
		"nft -j -t list ruleset": ok(testdata(t, "nft-ubuntu-ufw-docker.json")),
	}}
	f := reader(tools, p)
	f.docker = e.Client()
	read, err := f.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var pub []string
	for _, port := range read.GetPublished() {
		pub = append(pub, fmt.Sprintf("%s %s:%d proxied=%v", port.GetContainer(), port.GetHostIp(), port.GetHostPort(), port.GetProxied()))
	}
	if strings.Join(pub, ", ") != "shop-web-1 0.0.0.0:8080 proxied=false, shop-web-1 :::8080 proxied=true" {
		t.Errorf("published %v", pub)
	}
	var got []string
	for _, l := range read.GetListening() {
		got = append(got, fmt.Sprintf("%s:%d %s", l.GetAddress(), l.GetPort(), strings.TrimPrefix(l.GetReach().String(), "LISTENER_REACH_")))
	}
	// 8080 on IPv4 goes past ufw, so it isn't weighed; on IPv6 Docker's proxy
	// takes it, behind ufw's default deny.
	if strings.Join(got, ", ") != "0.0.0.0:22 OPEN, :::8080 CLOSED" {
		t.Errorf("listening %v", got)
	}
}

// A Docker that doesn't answer leaves the firewalls to say, says why its ports
// are missing, and what ufw would close reads "can't tell", as it may be a port
// Docker publishes past ufw (H4).
func TestDockerNotAnsweringIsNeverClosed(t *testing.T) {
	e := dockertest.New(t)
	e.Intercept(func(*http.Request) error { return errors.New("the engine is restarting") })
	tools := &fakeTools{outputs: map[string]hosttool.Result{
		"ufw status verbose":     ok(testdata(t, "ufw-apps.txt")),
		"nft -j -t list ruleset": ok(testdata(t, "nft-ubuntu-ufw-docker.json")),
	}}
	f := reader(tools, proc(22, 8090))
	f.docker = e.Client()
	read, err := f.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.GetPublishedUnreadable(), "the engine is restarting") {
		t.Errorf("why %q", read.GetPublishedUnreadable())
	}
	got := map[uint32]string{}
	for _, l := range read.GetListening() {
		got[l.GetPort()] = l.GetReach().String() + " " + l.GetReason().String()
	}
	if got[8090] != "LISTENER_REACH_UNKNOWN LISTENER_REACH_REASON_DOCKER_UNKNOWN" || got[22] != "LISTENER_REACH_OPEN LISTENER_REACH_REASON_UNSPECIFIED" {
		t.Errorf("with Docker not answering: %v", got)
	}
}

// The tools run from their fixed paths, never PATH, in the C locale with
// nothing of the agent's environment, and two tabs asking at once share one
// read (M8, H6; P14 to P17).
func TestTheReaderRunsToolsAsADR0043Has(t *testing.T) {
	root := t.TempDir()
	count := filepath.Join(root, "count")
	sbin := filepath.Join(root, "usr", "sbin")
	if err := os.MkdirAll(sbin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A ufw that answers in German unless it's in the C locale, and counts its runs.
	ufw := "#!/bin/sh\necho run >> " + count + "\nsleep 0.3\n" +
		`if [ "$LC_ALL" = C ] && [ -z "$DOCKWELL_SECRET" ]; then echo "Status: active"; echo "Default: deny (incoming), allow (outgoing), disabled (routed)"; else echo "Status: aktiv"; fi` + "\n"
	if err := os.WriteFile(filepath.Join(sbin, "ufw"), []byte(ufw), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("DOCKWELL_SECRET", "leaks")
	f := &firewallReader{tools: &hosttool.Runner{Root: root}, proc: proc(22), euid: func() int { return 0 }}
	var wg sync.WaitGroup
	var reads atomic.Int32
	for range 2 {
		wg.Go(func() {
			read, err := f.read(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if ufw := read.GetTools()[0]; !ufw.GetActive() || ufw.GetUnreadable() != "" {
				t.Errorf("ufw in the C locale reads as %v", ufw)
			}
			reads.Add(1)
		})
	}
	wg.Wait()
	runs, _ := os.ReadFile(count)
	if n := strings.Count(string(runs), "run"); n != 1 || reads.Load() != 2 {
		t.Errorf("two reads at once ran ufw %d times", n)
	}
	// A ufw on PATH alone isn't ufw.
	pathOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathOnly, "ufw"), []byte("#!/bin/sh\necho Status: inactive\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathOnly+":"+os.Getenv("PATH"))
	if (&firewallReader{tools: &hosttool.Runner{Root: t.TempDir()}}).installed("ufw") {
		t.Error("a ufw on PATH reads as installed")
	}
}

// An agent given no tools, as in a test, runs none of the machine's to read its
// firewall, and says it can't read the kernel's rules rather than that nothing
// filters.
func TestAnAgentWithNoToolsRunsNone(t *testing.T) {
	f := newFirewallReader(Config{})
	if !f.tools.Off {
		t.Fatalf("an agent with no tools runs %+v", f.tools)
	}
	f.proc = proc(22)
	read, err := f.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(read.GetTools()) != 1 || !strings.Contains(read.GetTools()[0].GetUnreadable(), "neither nft nor iptables-save") {
		t.Errorf("read %v", read.GetTools())
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
	p := fstest.MapFS{
		"net/tcp":  {Data: []byte(procTCP)},
		"net/tcp6": {Data: []byte(procTCP6)},
		"net/udp":  {Data: []byte(procUDP)},
	}
	var got []string
	for _, l := range listeners(p) {
		got = append(got, l.GetProtocol()+" "+l.GetAddress()+":"+strconv.Itoa(int(l.GetPort())))
	}
	want := "tcp 0.0.0.0:22 udp 0.0.0.0:68 tcp :::443 tcp 0.0.0.0:5432"
	if strings.Join(got, " ") != want {
		t.Errorf("listening:\n%s\nwant:\n%s", strings.Join(got, " "), want)
	}
}

// nftables's tables count their rules, leave out Dockwell's and firewalld's
// own, and note what their input chain does.
func TestParseNftables(t *testing.T) {
	sets, err := parseNftables(testdata(t, "nft-debian-docker.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sets {
		got = append(got, fmt.Sprintf("%s rules=%d input=%v drops=%v docker=%v", s.name, s.rules, s.input, s.inputDrops, s.dockers()))
	}
	if strings.Join(got, "; ") != "inet filter rules=0 input=true drops=false docker=false; ip filter rules=2 input=true drops=false docker=true" {
		t.Errorf("tables %v", got)
	}
}
