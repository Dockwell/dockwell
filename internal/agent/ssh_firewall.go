// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"errors"
	"strings"
)

type inputRule struct {
	line string
	expr []json.RawMessage
}

// Follow input jumps that can see either SSH port. Rules that treat them
// alike can stay in place. Unknown matches are reported with their chain.
func (s *ruleSet) sshPortConflict(from, to uint16, movingJail bool) string {
	seen := map[string]bool{}
	var scan func(string) string
	scan = func(chain string) string {
		if seen[chain] {
			return ""
		}
		seen[chain] = true
		for _, rule := range s.chainRules[chain] {
			old, next, target, known := rule.sshPorts(from, to)
			if movingJail && target == "f2b-sshd" {
				continue
			}
			_, isChain := s.chainRules[target]
			if !known || old != next || (old && target != "" && !isChain) {
				if rule.line != "" {
					return "filter/" + chain + " " + rule.line
				}
				body, _ := json.Marshal(rule.expr)
				return chain + " " + string(body)
			}
			if old && isChain {
				if detail := scan(target); detail != "" {
					return detail
				}
			}
		}
		return ""
	}
	for _, chain := range s.inputChains {
		if detail := scan(chain); detail != "" {
			return detail
		}
	}
	return ""
}

func (r inputRule) sshPorts(from, to uint16) (old, next bool, target string, known bool) {
	if r.line != "" {
		return iptablesSSHPorts(r.line, from, to)
	}
	old, next, known = true, true, len(r.expr) > 0
	for _, raw := range r.expr {
		var expr map[string]json.RawMessage
		if json.Unmarshal(raw, &expr) != nil || len(expr) != 1 {
			return old, next, target, false
		}
		for kind, body := range expr {
			switch kind {
			case "counter", "accept", "drop", "reject", "return", "continue", "log", "limit", "comment":
			case "jump", "goto":
				var branch struct{ Target string }
				if json.Unmarshal(body, &branch) != nil || branch.Target == "" {
					return old, next, target, false
				}
				target = branch.Target
			case "match":
				a, b, ok := nftSSHMatch(body, from, to)
				old, next, known = old && a, next && b, known && ok
			default:
				known = false
			}
		}
	}
	// A known exclusion means the rest of the rule cannot affect either port.
	if !old && !next {
		known = true
	}
	return old, next, target, known
}

func nftSSHMatch(body json.RawMessage, from, to uint16) (bool, bool, bool) {
	var match struct {
		Op   string
		Left struct {
			Payload *struct{ Protocol, Field string }
			Meta    *struct{ Key string }
			CT      *struct{ Key string }
		}
		Right json.RawMessage
	}
	if json.Unmarshal(body, &match) != nil {
		return true, true, false
	}
	if p := match.Left.Payload; p != nil {
		switch p.Protocol {
		case "udp", "udplite", "sctp", "dccp", "icmp", "icmpv6":
			return false, false, true
		case "tcp", "th":
			if p.Field == "dport" {
				a, aKnown := nftPortMatch(match.Right, match.Op, from)
				b, bKnown := nftPortMatch(match.Right, match.Op, to)
				return a, b, aKnown && bKnown
			}
			return true, true, p.Field != ""
		case "ip", "ip6", "ether", "vlan":
			return true, true, p.Field != ""
		}
	}
	if match.Left.Meta != nil {
		return true, true, match.Left.Meta.Key != ""
	}
	if c := match.Left.CT; c != nil {
		switch c.Key {
		case "state", "status", "direction", "mark", "label", "zone":
			return true, true, true
		}
	}
	return true, true, false
}

func nftPortMatch(raw json.RawMessage, op string, port uint16) (bool, bool) {
	if n := uint16(0); json.Unmarshal(raw, &n) == nil && string(raw) != "null" {
		switch op {
		case "==", "in":
			return port == n, true
		case "!=":
			return port != n, true
		case "<":
			return port < n, true
		case "<=":
			return port <= n, true
		case ">":
			return port > n, true
		case ">=":
			return port >= n, true
		default:
			return true, false
		}
	}
	var values struct {
		Set   []json.RawMessage
		Range []uint16
	}
	if json.Unmarshal(raw, &values) != nil {
		return true, false
	}
	switch op {
	case "==", "!=", "in":
	default:
		return true, false
	}
	var matched bool
	switch {
	case len(values.Range) == 2:
		matched = values.Range[0] <= port && port <= values.Range[1]
	case values.Set != nil:
		for _, item := range values.Set {
			hit, known := nftPortMatch(item, "==", port)
			if !known {
				return true, false
			}
			matched = matched || hit
		}
	default:
		return true, false
	}
	return matched != (op == "!="), true
}

func iptablesSSHPorts(line string, from, to uint16) (old, next bool, target string, known bool) {
	args, err := iptablesWords(line)
	if err != nil || len(args) < 3 {
		return true, true, "", false
	}
	old, next, known = true, true, true
	negated := false
	for i := 2; i < len(args); i++ {
		option := args[i]
		if option == "!" {
			negated = true
			continue
		}
		if i+1 == len(args) {
			return old, next, target, false
		}
		i++
		value := args[i]
		switch option {
		case "-p", "--protocol":
			if value != "all" && (value == "tcp" || value == "6") == negated {
				return false, false, "", true
			}
		case "--dport", "--destination-port", "--dports", "--destination-ports":
			a, aKnown := parsePorts(value, ":", uint32(from))
			b, bKnown := parsePorts(value, ":", uint32(to))
			old, next, known = old && (a != negated), next && (b != negated), known && aKnown && bKnown
			if !aKnown || !bKnown {
				return true, true, target, false
			}
		case "-j", "--jump", "-g", "--goto":
			switch value {
			case "ACCEPT", "DROP", "REJECT", "RETURN", "LOG", "NFLOG", "MARK", "CONNMARK":
			default:
				target = value
			}
		case "-m", "--match":
			switch value {
			case "tcp", "udp", "multiport", "comment", "conntrack", "state", "mark", "addrtype", "pkttype", "iprange", "limit", "icmp", "icmp6", "icmpv6":
			default:
				known = false
			}
		case "-s", "-d", "-i", "-o", "--source", "--destination", "--in-interface", "--out-interface",
			"--sport", "--source-port", "--sports", "--source-ports", "--comment", "--ctstate", "--state", "--ctstatus",
			"--mark", "--set-mark", "--set-xmark", "--src-type", "--dst-type", "--pkt-type", "--src-range", "--dst-range",
			"--limit", "--limit-burst", "--reject-with", "--log-prefix", "--log-level", "--icmp-type", "--icmpv6-type":
		default:
			// Unknown options may be flags rather than taking an argument.
			// Do not consume the next token and guess what the rest means.
			return true, true, "", false
		}
		negated = false
	}
	return old, next, target, known || (!old && !next)
}

func seenSSHRule(rule interface {
	GetPort() string
	GetSource() string
	GetDestination() string
	GetInterface() string
},
) string {
	var detail strings.Builder
	detail.WriteString(rule.GetPort())
	for _, field := range []struct{ name, value string }{
		{"from", rule.GetSource()}, {"to", rule.GetDestination()}, {"on", rule.GetInterface()},
	} {
		if field.value != "" {
			detail.WriteByte(' ')
			detail.WriteString(field.name)
			detail.WriteByte(' ')
			detail.WriteString(field.value)
		}
	}
	return detail.String()
}

// iptablesWords splits a line of iptables-save into its words: iptables quotes
// a word with spaces in double quotes and escapes a quote or backslash inside
// it. The agent build links no shell parser for this (ADR 0023).
func iptablesWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
		case c == '"':
			quoted, inWord = !quoted, true
		case !quoted && (c == ' ' || c == '\t'):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		return nil, errors.New("an iptables rule ends inside a quoted word")
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}
