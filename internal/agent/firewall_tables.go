// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// ruleSet is one nftables table, or one family of iptables, as the agent weighs
// it: its chains' names, its input chain's policy and rules, and how many rules
// it holds in all.
type ruleSet struct {
	name string
	// family is 4 or 6, or 0 for both, as an inet table holds.
	family int
	chains []string
	rules  uint32
	// filters says whether it has a chain that filters, on any hook; a table of
	// nat chains alone filters nothing.
	filters    bool
	inputDrops bool
	input      bool
	inputRules uint32
	// dockerUser is how many rules DOCKER-USER holds beyond a return.
	dockerUser uint32
}

// ufws says whether ufw made the table: its chains are ufw's.
func (r *ruleSet) ufws() bool {
	for _, c := range r.chains {
		if strings.HasPrefix(c, "ufw-") || strings.HasPrefix(c, "ufw6-") {
			return true
		}
	}
	return false
}

// dockers says whether Docker made the table: it has Docker's chains.
func (r *ruleSet) dockers() bool {
	for _, c := range r.chains {
		if strings.HasPrefix(c, "DOCKER") {
			return true
		}
	}
	return false
}

// filtersInput says whether its input chain could drop what comes in: it drops
// what no rule matches, or holds rules.
func (r *ruleSet) filtersInput() bool { return r.inputDrops || r.inputRules > 0 }

// tool is how the tab shows a table that isn't ufw's or Docker's own, or nil for
// one that says nothing: empty, filtering nothing, ufw's while ufw is there to
// say it, or Docker's while its input chain lets everything in.
func (r *ruleSet) tool(kind dockwellv1.FirewallToolKind, ufwThere bool) *dockwellv1.FirewallTool {
	switch {
	case ufwThere && r.ufws():
		return nil
	case !r.filtersInput() && (r.dockers() || r.rules == 0 || !r.filters):
		return nil
	}
	t := &dockwellv1.FirewallTool{Kind: kind, Name: r.name, Active: true, RuleCount: r.rules, InputRules: r.inputRules}
	if r.input {
		t.InputPolicy = dockwellv1.FirewallVerdict_FIREWALL_VERDICT_ALLOW
		if r.inputDrops {
			t.InputPolicy = dockwellv1.FirewallVerdict_FIREWALL_VERDICT_DENY
		}
	}
	return t
}

// kernelRules reads the kernel's rules beyond ufw's and firewalld's: every
// nftables table, and iptables-legacy's, whose drops win beside nftables's; or,
// on a server without nft, iptables-save's. It returns the tables that may
// filter what comes in, and how many rules Docker's DOCKER-USER chains hold.
func (f *firewallReader) kernelRules(ctx context.Context, ufwThere bool) ([]*dockwellv1.FirewallTool, uint32) {
	var tools []*dockwellv1.FirewallTool
	var dockerUser uint32
	add := func(kind dockwellv1.FirewallToolKind, sets []*ruleSet) {
		for _, s := range sets {
			dockerUser += s.dockerUser
			if t := s.tool(kind, ufwThere); t != nil {
				tools = append(tools, t)
			}
		}
	}
	iptables := func(kind dockwellv1.FirewallToolKind, label, v4, v6 string) {
		for _, fam := range []struct {
			cmd, name string
			family    int
		}{{v4, "IPv4", 4}, {v6, "IPv6", 6}} {
			if !f.installed(fam.cmd) {
				continue
			}
			out, err := f.output(ctx, fam.cmd)
			if err != nil {
				tools = append(tools, &dockwellv1.FirewallTool{Kind: kind, Name: fam.name, Unreadable: f.unreadable(label+" ("+fam.name+")", err)})
				continue
			}
			add(kind, []*ruleSet{parseIptablesSave(out, fam.name, fam.family)})
		}
	}
	switch {
	case f.installed("nft"):
		// -t leaves out sets' elements, which a blocklist fills by the thousand.
		out, err := f.output(ctx, "nft", "-j", "-t", "list", "ruleset")
		if err == nil {
			var sets []*ruleSet
			if sets, err = parseNftables(out); err == nil {
				add(dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES, sets)
			}
		}
		if err != nil {
			tools = append(tools, &dockwellv1.FirewallTool{Kind: dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES, Unreadable: f.unreadable("nftables", err)})
		}
		iptables(dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES_LEGACY, "iptables-legacy", "iptables-legacy-save", "ip6tables-legacy-save")
	case f.installed("iptables-save") || f.installed("ip6tables-save"):
		iptables(dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_IPTABLES, "iptables", "iptables-save", "ip6tables-save")
	default:
		tools = append(tools, &dockwellv1.FirewallTool{
			Kind:       dockwellv1.FirewallToolKind_FIREWALL_TOOL_KIND_NFTABLES,
			Unreadable: "Dockwell can't read the kernel's rules: this server has neither nft nor iptables-save. Install the nftables package, and Dockwell reads them",
		})
	}
	return tools, dockerUser
}

// parseNftables reads `nft -j -t list ruleset` into its tables, leaving out
// Dockwell's own and firewalld's, which say themselves.
func parseNftables(out []byte) ([]*ruleSet, error) {
	var doc struct {
		Nftables []struct {
			Table *struct {
				Family string `json:"family"`
				Name   string `json:"name"`
			} `json:"table"`
			Chain *struct {
				Family string `json:"family"`
				Table  string `json:"table"`
				Name   string `json:"name"`
				Hook   string `json:"hook"`
				Type   string `json:"type"`
				Policy string `json:"policy"`
			} `json:"chain"`
			Rule *struct {
				Family string            `json:"family"`
				Table  string            `json:"table"`
				Chain  string            `json:"chain"`
				Expr   []json.RawMessage `json:"expr"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("nft listed its rules in a way Dockwell can't read: %w", err)
	}
	var sets []*ruleSet
	byName := map[string]*ruleSet{}
	inputs := map[string]bool{}
	for _, item := range doc.Nftables {
		switch {
		case item.Table != nil:
			name := item.Table.Family + " " + item.Table.Name
			if ownedElsewhere(name) {
				continue
			}
			s := &ruleSet{name: name, family: nftFamily(item.Table.Family)}
			byName[name] = s
			sets = append(sets, s)
		case item.Chain != nil:
			s := byName[item.Chain.Family+" "+item.Chain.Table]
			if s == nil {
				continue
			}
			s.chains = append(s.chains, item.Chain.Name)
			if item.Chain.Hook != "" && cmp.Or(item.Chain.Type, "filter") == "filter" {
				s.filters = true
			}
			if item.Chain.Hook == "input" && cmp.Or(item.Chain.Type, "filter") == "filter" {
				s.input = true
				s.inputDrops = s.inputDrops || item.Chain.Policy == "drop"
				inputs[s.name+" "+item.Chain.Name] = true
			}
		case item.Rule != nil:
			s := byName[item.Rule.Family+" "+item.Rule.Table]
			if s == nil {
				continue
			}
			s.rules++
			if inputs[s.name+" "+item.Rule.Chain] {
				s.inputRules++
			}
			if item.Rule.Chain == "DOCKER-USER" && !onlyReturns(item.Rule.Expr) {
				s.dockerUser++
			}
		}
	}
	return sets, nil
}

// onlyReturns says whether a rule does nothing but return, maybe counting, as
// the rule Docker puts in DOCKER-USER does.
func onlyReturns(expr []json.RawMessage) bool {
	returns := false
	for _, e := range expr {
		var stmt map[string]json.RawMessage
		if json.Unmarshal(e, &stmt) != nil {
			return false
		}
		for k := range stmt {
			switch k {
			case "return":
				returns = true
			case "counter":
			default:
				return false
			}
		}
	}
	return returns
}

func nftFamily(family string) int {
	switch family {
	case "ip":
		return 4
	case "ip6":
		return 6
	}
	return 0
}

// parseIptablesSave reads iptables-save's filter table, as iptables-legacy-save
// and ip6tables-save print it too:
//
//	*filter
//	:INPUT DROP [0:0]
//	:DOCKER-USER - [0:0]
//	-A INPUT -i lo -j ACCEPT
//	-A DOCKER-USER -j RETURN
//	COMMIT
func parseIptablesSave(out []byte, name string, family int) *ruleSet {
	s := &ruleSet{name: name, family: family}
	table := ""
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "*"):
			table = strings.TrimPrefix(line, "*")
		case table != "filter":
		case strings.HasPrefix(line, ":"):
			fields := strings.Fields(strings.TrimPrefix(line, ":"))
			if len(fields) == 0 {
				continue
			}
			s.chains = append(s.chains, fields[0])
			s.filters = true
			if fields[0] == "INPUT" {
				s.input = true
				s.inputDrops = len(fields) > 1 && fields[1] != "ACCEPT"
			}
		case strings.HasPrefix(line, "-A "):
			s.rules++
			chain := strings.Fields(line)[1]
			switch chain {
			case "INPUT":
				s.inputRules++
			case "DOCKER-USER":
				if line != "-A DOCKER-USER -j RETURN" {
					s.dockerUser++
				}
			}
		}
	}
	return s
}

// ownedElsewhere is a table Dockwell or firewalld keeps, which say themselves.
func ownedElsewhere(name string) bool {
	family, table, _ := strings.Cut(name, " ")
	return (family == "inet" && table == "dockwell") || table == "firewalld"
}
