// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// firewallWait bounds each command the agent reads a firewall with.
const firewallWait = 5 * time.Second

// firewallReader says what filters the server's inbound traffic, reading ufw,
// firewalld and nftables with their own commands and Docker's published ports,
// and changes nothing (ADR 0052).
type firewallReader struct {
	docker *docker.Client
	// run runs a command and returns what it printed, for a test. Nil runs it.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// has says whether a command is installed, for a test. Nil looks on PATH.
	has func(name string) bool
	// proc reads /proc/net, for a test. Nil reads the real one.
	proc fs.FS
	now  func() time.Time
}

func (f *firewallReader) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, firewallWait)
	defer cancel()
	if f.run != nil {
		return f.run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // only the fixed commands below
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

func (f *firewallReader) installed(name string) bool {
	if f.has != nil {
		return f.has(name)
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// read reads the firewalls the server has, and the ports Docker publishes.
func (f *firewallReader) read(ctx context.Context) (*dockwellv1.ServerFirewall, error) {
	if f == nil {
		return nil, errors.New("this agent can't read the firewall")
	}
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	out := &dockwellv1.ServerFirewall{Time: timestamppb.New(now())}
	if f.installed("ufw") {
		out.Tools = append(out.Tools, f.ufw(ctx))
	}
	if f.installed("firewall-cmd") {
		out.Tools = append(out.Tools, f.firewalld(ctx))
	}
	if f.installed("nft") {
		out.Tools = append(out.Tools, f.nftables(ctx)...)
	}
	if f.installed("iptables-legacy-save") {
		if legacy := f.iptablesLegacy(ctx); legacy != nil {
			out.Tools = append(out.Tools, legacy)
		}
	}
	// A Docker that doesn't answer leaves the firewalls to say: the ports it
	// publishes are what's missing.
	if f.docker != nil {
		ports, err := dockerPublished(ctx, f.docker)
		if err != nil {
			out.PublishedUnreadable = fmt.Sprintf("Docker didn't say which ports it publishes: %v", err)
		}
		out.Published = ports
	}
	proc := f.proc
	if proc == nil {
		proc = os.DirFS("/proc")
	}
	for _, l := range listeners(proc) {
		// Docker's proxy listens on each port Docker publishes, but what reaches
		// that port is forwarded past ufw's and firewalld's input rules, so the
		// port is said with what Docker publishes, not weighed against them.
		if publishes(out.Published, l) {
			continue
		}
		reachOf(l, out.Tools)
		out.Listening = append(out.Listening, l)
	}
	return out, nil
}

// publishes says whether Docker publishes the port a listener holds.
func publishes(ports []*dockwellv1.PublishedPort, l *dockwellv1.HostListener) bool {
	return slices.ContainsFunc(ports, func(p *dockwellv1.PublishedPort) bool {
		return p.GetHostPort() == l.GetPort() && p.GetProtocol() == l.GetProtocol()
	})
}

// unreadable says why a command couldn't read a firewall, in words a person acts on.
func unreadable(name string, err error) string {
	msg := err.Error()
	if strings.Contains(msg, "root") || strings.Contains(msg, "ermission") || strings.Contains(msg, "not permitted") {
		return name + " needs root to say its rules, and the agent doesn't run as root here"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("%s didn't answer within %s", name, firewallWait)
	}
	return fmt.Sprintf("%s failed: %s", name, msg)
}

// ufw reads ufw's status, defaults and rules from `ufw status verbose`.
func (f *firewallReader) ufw(ctx context.Context) *dockwellv1.FirewallTool {
	tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_UFW}
	out, err := f.command(ctx, "ufw", "status", "verbose")
	if err != nil {
		tool.Unreadable = unreadable("ufw", err)
		return tool
	}
	parseUFW(out, tool)
	return tool
}

// ufwColumns splits a row of ufw's table at runs of two spaces or more.
var ufwColumns = regexp.MustCompile(`\s{2,}`)

// parseUFW reads `ufw status verbose`:
//
//	Status: active
//	Default: deny (incoming), allow (outgoing), disabled (routed)
//	To                         Action      From
//	--                         ------      ----
//	22/tcp                     ALLOW IN    203.0.113.0/24             # office
func parseUFW(out []byte, tool *dockwellv1.FirewallTool) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	table := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " ")
		switch {
		case strings.HasPrefix(line, "Status:"):
			tool.Active = strings.TrimSpace(strings.TrimPrefix(line, "Status:")) == "active"
		case strings.HasPrefix(line, "Default:"):
			for part := range strings.SplitSeq(strings.TrimPrefix(line, "Default:"), ",") {
				if verdict, ok := strings.CutSuffix(strings.TrimSpace(part), " (incoming)"); ok {
					tool.DefaultInbound = ufwVerdict(verdict)
				}
			}
		case strings.HasPrefix(line, "--"):
			table = true
		case table && line != "":
			if rule, ok := parseUFWRule(line); ok {
				tool.Rules = append(tool.Rules, rule)
			}
		}
	}
}

func parseUFWRule(line string) (*dockwellv1.FirewallSeenRule, bool) {
	comment := ""
	if i := strings.Index(line, " # "); i >= 0 {
		line, comment = line[:i], strings.TrimSpace(line[i+3:])
	}
	cols := ufwColumns.Split(strings.TrimSpace(line), -1)
	if len(cols) < 3 {
		return nil, false
	}
	to, action, from := cols[0], cols[1], cols[2]
	// Only what comes in: ufw lists outbound and routed rules in the same table.
	if strings.HasSuffix(action, " OUT") || strings.HasSuffix(action, " FWD") {
		return nil, false
	}
	rule := &dockwellv1.FirewallSeenRule{Comment: comment}
	if cut, ok := strings.CutSuffix(to, " (v6)"); ok {
		to, rule.Ipv6 = cut, true
	}
	from = strings.TrimSuffix(from, " (v6)")
	if from != "Anywhere" {
		rule.Source = from
	}
	rule.Port = to
	rule.Verdict = ufwVerdict(strings.TrimSuffix(action, " IN"))
	return rule, true
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

// firewalld reads whether firewalld runs and, when it does, its default zone
// from `firewall-cmd --list-all`.
func (f *firewallReader) firewalld(ctx context.Context) *dockwellv1.FirewallTool {
	tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_FIREWALLD}
	// --state says "not running" and exits 252 when it isn't: that's an answer.
	if out, _ := f.command(ctx, "firewall-cmd", "--state"); strings.TrimSpace(string(out)) != "running" {
		return tool
	}
	tool.Active = true
	out, err := f.command(ctx, "firewall-cmd", "--list-all")
	if err != nil {
		tool.Unreadable = unreadable("firewalld", err)
		return tool
	}
	parseFirewalld(out, tool)
	return tool
}

// parseFirewalld reads `firewall-cmd --list-all`:
//
//	public (active)
//	  target: default
//	  services: dhcpv6-client ssh
//	  ports: 8080/tcp
//	  rich rules:
//		rule family="ipv4" source address="203.0.113.0/24" port port="22" protocol="tcp" accept
func parseFirewalld(out []byte, tool *dockwellv1.FirewallTool) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	rich := false
	first := true
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if first && line != "" {
			tool.Name, _, _ = strings.Cut(line, " ")
			first = false
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		switch {
		case rich && line != "" && !ok:
			tool.Other = append(tool.Other, line)
			continue
		case !ok:
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
		case "services", "ports":
			for item := range strings.FieldsSeq(value) {
				tool.Rules = append(tool.Rules, &dockwellv1.FirewallSeenRule{Verdict: dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW, Port: item})
			}
		case "rich rules":
			rich = true
			if value != "" {
				tool.Other = append(tool.Other, value)
			}
		}
	}
}

// nftables lists the nftables tables something other than firewalld or Dockwell
// made, with how many rules each holds, from `nft -j list ruleset`.
func (f *firewallReader) nftables(ctx context.Context) []*dockwellv1.FirewallTool {
	out, err := f.command(ctx, "nft", "-j", "list", "ruleset")
	if err != nil {
		return []*dockwellv1.FirewallTool{{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES, Unreadable: unreadable("nftables", err)}}
	}
	tools, err := parseNftables(out)
	if err != nil {
		return []*dockwellv1.FirewallTool{{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES, Unreadable: "nftables said what the agent can't read: " + err.Error()}}
	}
	return tools
}

// parseNftables counts the rules in each table of `nft -j list ruleset`, leaving
// out Dockwell's own and firewalld's, which the tab says otherwise.
func parseNftables(out []byte) ([]*dockwellv1.FirewallTool, error) {
	var doc struct {
		Nftables []struct {
			Table *struct {
				Family string `json:"family"`
				Name   string `json:"name"`
			} `json:"table"`
			Rule *struct {
				Family string `json:"family"`
				Table  string `json:"table"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var tools []*dockwellv1.FirewallTool
	byName := map[string]*dockwellv1.FirewallTool{}
	for _, item := range doc.Nftables {
		switch {
		case item.Table != nil:
			name := item.Table.Family + " " + item.Table.Name
			if ownedElsewhere(name) {
				continue
			}
			tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES, Name: name, Active: true}
			byName[name] = tool
			tools = append(tools, tool)
		case item.Rule != nil:
			if tool := byName[item.Rule.Family+" "+item.Rule.Table]; tool != nil {
				tool.RuleCount++
			}
		}
	}
	return tools, nil
}

// iptablesLegacy counts iptables-legacy's rules, which run beside nftables's, or
// nothing when it has none.
func (f *firewallReader) iptablesLegacy(ctx context.Context) *dockwellv1.FirewallTool {
	tool := &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES_LEGACY}
	out, err := f.command(ctx, "iptables-legacy-save")
	if err != nil {
		tool.Unreadable = unreadable("iptables-legacy", err)
		return tool
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "-A ") {
			tool.RuleCount++
		}
	}
	if tool.RuleCount == 0 {
		return nil
	}
	tool.Active = true
	return tool
}

// ownedElsewhere is a table Dockwell or firewalld keeps, which the tab says otherwise.
func ownedElsewhere(name string) bool {
	family, table, _ := strings.Cut(name, " ")
	return (family == "inet" && table == "dockwell") || table == "firewalld"
}

// dockerPublished lists the ports Docker publishes for its containers now.
func dockerPublished(ctx context.Context, client *docker.Client) ([]*dockwellv1.PublishedPort, error) {
	var out []*dockwellv1.PublishedPort
	err := client.EachContainer(ctx, nil, func(s *docker.ContainerSummary) error {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		for _, p := range s.Ports {
			if p.PublicPort == 0 {
				continue
			}
			out = append(out, &dockwellv1.PublishedPort{
				Container: name, HostIp: p.IP, HostPort: uint32(p.PublicPort), Protocol: p.Type, ContainerPort: uint32(p.PrivatePort),
			})
		}
		return nil
	})
	slices.SortFunc(out, func(a, b *dockwellv1.PublishedPort) int {
		if a.GetHostPort() != b.GetHostPort() {
			return int(a.GetHostPort()) - int(b.GetHostPort())
		}
		return strings.Compare(a.GetHostIp(), b.GetHostIp())
	})
	return out, err
}
