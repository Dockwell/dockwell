// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// HostMatches says a rule's hostname takes a request's host, as cloudflared reads
// it: no hostname takes every host, "*.example.com" every name under example.com at
// any depth but not example.com itself, and anything else the host alone, any case.
func HostMatches(ruleHost, host string) bool {
	ruleHost, host = strings.ToLower(ruleHost), strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case ruleHost == "" || ruleHost == "*":
		return true
	case strings.HasPrefix(ruleHost, "*."):
		return strings.HasSuffix(host, ruleHost[1:])
	}
	return ruleHost == host
}

// Way is where a name goes in a document: the rules that take its host, in order,
// up to the first that takes every path. Two documents send a name the same way
// when its Way is the same in both.
type Way []Rule

// Goes is the service a name's root path reaches: the first rule of its way whose
// path, if any, takes "/".
func (w Way) Goes() string {
	for _, r := range w {
		if r.Path == "" || pathTakes(r.Path, "/") {
			return r.Service
		}
	}
	return ""
}

func pathTakes(pattern, path string) bool {
	re, err := regexp.Compile(pattern)
	return err == nil && re.MatchString(path)
}

// WayOf is the way name goes in rules.
func WayOf(rules []Rule, name string) Way {
	var w Way
	for _, r := range rules {
		if !HostMatches(r.Hostname, name) {
			continue
		}
		w = append(w, r)
		if r.Path == "" {
			break
		}
	}
	return w
}

// FirstFor is the index of the first rule that takes every path of name, which is
// the rule that serves it, or -1.
func FirstFor(rules []Rule, name string) int {
	for i, r := range rules {
		if HostMatches(r.Hostname, name) && r.Path == "" {
			return i
		}
	}
	return -1
}

// SameService says two services are the same place, as "http://127.0.0.1:80",
// "http://localhost" and "http://127.0.0.1" all reach the same proxy.
func SameService(a, b string) bool { return normalService(a) == normalService(b) }

func normalService(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(s))
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if host == "localhost" || host == "::1" {
		host = "127.0.0.1"
	}
	if port == "" {
		switch u.Scheme {
		case "http", "ws":
			port = "80"
		case "https", "wss":
			port = "443"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + host + ":" + port + strings.TrimSuffix(u.Path, "/")
}

// Routing is what's known of the names a tunnel's document routes, for the routing
// check (ADR 0104, section 4).
type Routing struct {
	// Panel are every name the panel answers to: in use, saved and waiting, the old
	// one during a move, and the one the control plane was started with.
	Panel []string
	// PanelService is the panel's own listener, as a rule would name it, such as
	// "http://127.0.0.1:7600"; empty if no rule could.
	PanelService string
	// Names are every other name to keep where it goes: those the tunnel's rules
	// name, and those in the zone whose DNS points at the tunnel.
	Names []string
	// Proxy is how the tunnel reaches the server's proxy, such as
	// "http://127.0.0.1:80".
	Proxy string
	// Ours are names Dockwell routes on this server, which may come to reach the
	// proxy: that's them being published.
	Ours []string
}

// Moved is a name a change would send somewhere else.
type Moved struct {
	Name, From, To string
	// Panel says it's a name the panel answers to.
	Panel bool
}

// Error says what moved, in a line, naming the first.
func (m Moved) Error() string {
	from := m.From
	if from == "" {
		from = "nothing"
	}
	if m.Panel {
		return fmt.Sprintf("the change would move the panel's name %s, which goes to %s now", m.Name, from)
	}
	return fmt.Sprintf("the change would move %s, which goes to %s now", m.Name, from)
}

// Edit is the one rule a change adds or takes away, by its index: in the new list
// for an insert, in the old one for a removal.
type Edit struct {
	At       int
	Inserted bool
}

// moved maps a rule's index before the edit to after it, or -1 for the rule taken
// away.
func (e Edit) moved(i int) int {
	switch {
	case e.Inserted && i >= e.At:
		return i + 1
	case !e.Inserted && i == e.At:
		return -1
	case !e.Inserted && i > e.At:
		return i - 1
	}
	return i
}

// sameWay says two ways take a name to the same places, path for path.
func sameWay(a, b Way) bool {
	return slices.EqualFunc(a, b, func(x, y Rule) bool { return x.Path == y.Path && SameService(x.Service, y.Service) })
}

// Check compares where every known name goes before and after edit, and returns
// each one that would go elsewhere. A panel name must still be served by the very
// rule that served it. Any other name must go to the same places, except that a name
// answered 404 before may reach the proxy, which answers it 404 too while it has no
// route for it, and Dockwell's own names may come to reach the proxy, which is them
// being published.
func (rt Routing) Check(before, after []Rule, edit Edit) []Moved {
	var out []Moved
	for _, panel := range rt.Panel {
		name := probeName(panel)
		bi, ai := FirstFor(before, name), FirstFor(after, name)
		if bi < 0 || ai != edit.moved(bi) || !sameWay(WayOf(before, name), WayOf(after, name)) {
			out = append(out, Moved{Name: panel, From: WayOf(before, name).Goes(), To: WayOf(after, name).Goes(), Panel: true})
		}
	}
	for _, name := range rt.Names {
		if slices.ContainsFunc(rt.Panel, func(p string) bool { return strings.EqualFold(p, name) }) {
			continue
		}
		bw, aw := WayOf(before, name), WayOf(after, name)
		if sameWay(bw, aw) {
			continue
		}
		// What comes before its last rule, its paths sent elsewhere, stays as it was.
		samePaths := len(bw) == len(aw) && len(bw) > 0 && sameWay(bw[:len(bw)-1], aw[:len(aw)-1])
		toProxy := samePaths && SameService(aw[len(aw)-1].Service, rt.Proxy)
		was404 := samePaths && bw[len(bw)-1].Service == CatchAll.Service
		ours := slices.ContainsFunc(rt.Ours, func(o string) bool { return strings.EqualFold(o, name) })
		if toProxy && (was404 || ours) {
			continue
		}
		out = append(out, Moved{Name: name, From: bw.Goes(), To: aw.Goes()})
	}
	return out
}

// Serves says a rule serves a panel name: its hostname is one, or names something
// beneath one, or its service is the panel's own listener. Such a rule is never
// added, changed or removed. A zone's wildcard, as "*.example.com" beside a panel at
// "example.com" or "panel.example.com", matches no panel name by itself, so it
// doesn't serve one: the routing check says whether it may go in (ADR 0104, section
// 4).
func (rt Routing) Serves(r Rule) bool {
	if rt.PanelService != "" && SameService(r.Service, rt.PanelService) {
		return true
	}
	host := strings.ToLower(r.Hostname)
	base, wildcard := strings.CutPrefix(host, "*.")
	for _, p := range rt.Panel {
		p = strings.ToLower(p)
		switch {
		case host == p:
			return true
		case wildcard && base == strings.TrimPrefix(p, "*."):
			// "*.example.com" beside the panel's "example.com": not the panel's.
			if !strings.HasPrefix(p, "*.") {
				continue
			}
			return true
		case strings.HasSuffix(base, "."+strings.TrimPrefix(p, "*.")):
			return true
		}
	}
	return false
}

// probeName is a name to route in place of a wildcard panel name, such as the
// panel's apps domain, "*.apps-example.net": a label beneath it no one uses.
func probeName(name string) string {
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		return "dockwell-routing-check." + rest
	}
	return name
}

// Reaches says name reaches the server's proxy already: its way's root goes there.
func (rt Routing) Reaches(rules []Rule, name string) bool {
	return SameService(WayOf(rules, name).Goes(), rt.Proxy)
}

// CaughtBy is the first rule for name, by its own hostname or a wildcard, that
// sends it, or a path of it, elsewhere than the proxy before any rule sends it there:
// what the live check names as catching it first, and what refuses the plan. -1 when
// none does. The catch-all never catches: a rule goes in before it.
func (rt Routing) CaughtBy(rules []Rule, name string) int {
	for i, r := range rules {
		if r.Hostname == "" || !HostMatches(r.Hostname, name) {
			continue
		}
		if !SameService(r.Service, rt.Proxy) {
			return i
		}
		if r.Path == "" {
			return -1
		}
	}
	return -1
}
