// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"errors"
	"fmt"
	"strings"
)

// Need is what a tunnel needs for a name to reach the server's proxy, as the plan
// works it out from the document as read (ADR 0104, section 4).
type Need struct {
	// Nothing says the name reaches the proxy already: through the catch-all, a
	// wildcard, or a rule for it. Publishing it writes its DNS record alone.
	Nothing bool
	// Rule is the rule to add: the zone's wildcard, or its apex.
	Rule Rule
	// Edit is where it goes, and After the document with it.
	Edit  Edit
	After *Document
	// Refused says why no rule can go in, naming what's in the way; nil when one
	// can, or none is needed.
	Refused error
}

// ErrServesPanel is a rule Dockwell would add that serves the panel's own name.
var ErrServesPanel = errors.New("the rule would serve the panel's own name, which Dockwell never touches")

// Caught is a name an earlier rule takes, and sends elsewhere than the proxy.
type Caught struct {
	Name string
	Rule Rule
}

func (c Caught) Error() string {
	host := c.Rule.Hostname
	if c.Rule.Path != "" {
		host += " " + c.Rule.Path
	}
	return fmt.Sprintf("%s in the tunnel catches %s first, to %s", host, c.Name, c.Rule.Service)
}

// NeedFor works out what doc needs for name, in zone, to reach the proxy, keeping
// every other name rt knows where it goes. name is zone itself for the apex, which
// the zone's wildcard doesn't take, and takes a rule of its own.
func NeedFor(doc *Document, rt Routing, zone, name string) Need {
	zone, name = strings.ToLower(zone), strings.ToLower(name)
	if rt.Reaches(doc.Rules, name) {
		return Need{Nothing: true}
	}
	if i := rt.CaughtBy(doc.Rules, name); i >= 0 {
		return Need{Refused: Caught{Name: name, Rule: doc.Rules[i]}}
	}
	rule := Rule{Hostname: "*." + zone, Service: rt.Proxy}
	if name == zone {
		rule.Hostname = zone
	}
	if rt.Serves(rule) {
		return Need{Refused: ErrServesPanel}
	}
	after, at, err := doc.Insert(rule)
	if err != nil {
		return Need{Refused: err}
	}
	edit := Edit{At: at, Inserted: true}
	if moved := rt.Check(doc.Rules, after.Rules, edit); len(moved) > 0 {
		return Need{Refused: moved[0]}
	}
	if !rt.Reaches(after.Rules, name) {
		return Need{Refused: fmt.Errorf("%s still wouldn't reach the server's proxy with %s added", name, rule.Hostname)}
	}
	return Need{Rule: rule, Edit: edit, After: after}
}

// Unless says what's left of every other name when a rule goes in: the words a
// plan shows beside it, naming the panel where a rule of the tunnel serves one of its
// names.
func (rt Routing) Unless(tunnel string, rules []Rule) string {
	for _, p := range rt.Panel {
		if i := FirstFor(rules, probeName(p)); i >= 0 && rules[i].Hostname != "" {
			return "Every other name in " + tunnel + ", the panel's among them, goes where it went"
		}
	}
	return "Every other name in " + tunnel + " goes where it went"
}
