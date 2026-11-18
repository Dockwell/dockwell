// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// A database's external access is a port and two rules that go together (ADR 0038,
// "External access, as built"): its service publishes a port tagged external
// (PortMapping.external), and on each server it runs on, two firewall rules let that
// port in from the addresses listed and from nowhere else. The rules are named for
// the service's ID, so two databases of one name on a server don't share them. The
// engine keeps them together on every path: an edit, a move, a server added, an
// undo, a rollback, a removal and an automatic update all come through here.
const (
	// ExternalAllow names the rule that lets the listed addresses in.
	ExternalAllow = "allow"
	// ExternalDeny names the rule that keeps everyone else out.
	ExternalDeny = "deny"
)

// firewallRuleKind is the kind of a server's firewall rule.
const firewallRuleKind resource.Kind = "firewall.rule"

// ExternalRule is the name of the firewall rule a database's external access keeps
// on its server: which is ExternalAllow or ExternalDeny.
func ExternalRule(serviceID, which string) string {
	return "external-" + strings.ToLower(strings.TrimPrefix(serviceID, id.Service+"_")) + "-" + which
}

// ExternalOf is the service a firewall rule's name says it keeps external access
// for, and whether it's one.
func ExternalOf(rule, which string) (string, bool) {
	rest, ok := strings.CutPrefix(rule, "external-")
	if !ok {
		return "", false
	}
	svc, ok := strings.CutSuffix(rest, "-"+which)
	if !ok || svc == "" || strings.Contains(svc, "-") {
		return "", false
	}
	return id.Service + "_" + strings.ToUpper(svc), true
}

// Public says a published port answers beyond its server: on every address, or on
// any address but loopback. An address that doesn't read as one is taken as public.
func Public(hostIP string) bool {
	ip := strings.Trim(hostIP, "[]")
	if ip == "" {
		return true
	}
	a, err := netip.ParseAddr(ip)
	return err != nil || !a.Unmap().IsLoopback()
}

// The widest a range external access lets in may be, and how many it may list.
// A /8 is the widest block one network holds; a /32 is what an ISP is given for
// IPv6. With at most 32 of them, the most a list can reach is 32 /8s, an eighth of
// IPv4, so no list covers a whole family, however its ranges are cut.
const (
	widestV4      = 8
	widestV6      = 32
	mostAllowFrom = 32
)

// AllowFrom reads the addresses external access lets in, each an address or a range,
// as ranges in the form a firewall rule takes, without repeats. A range in IPv4 as
// IPv6 writes it, such as ::ffff:0.0.0.0/96, is read as the IPv4 range it is, as the
// firewall does. Anything wider than a /8 for IPv4 or a /32 for IPv6, more than 32
// ranges, or none at all, is refused: external access is never open to everyone.
func AllowFrom(given []string) ([]string, error) {
	var out []string
	for _, g := range given {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		p, err := netip.ParsePrefix(g)
		if err != nil {
			a, aerr := netip.ParseAddr(g)
			if aerr != nil {
				return nil, fmt.Errorf("%s isn't an address or a range. Use one such as 203.0.113.7 or 203.0.113.0/24", g)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if a := p.Addr(); a.Is4In6() {
			p = netip.PrefixFrom(a.Unmap(), max(p.Bits()-96, 0))
		} else if a.Is6() && p.Bits() < 96 && netip.MustParsePrefix("::ffff:0.0.0.0/96").Overlaps(p) {
			return nil, fmt.Errorf("%s takes in every IPv4 address, and external access is never open to everyone. Name the addresses that reach it", g)
		} else if a.Is6() && p.Bits() <= 96 && netip.MustParsePrefix("64:ff9b::/96").Overlaps(p) {
			// Behind a NAT64 gateway, that's every IPv4 host (the re-check's L2).
			return nil, fmt.Errorf("%s is NAT64's prefix, which behind a gateway is every IPv4 address. Name the addresses that reach it", g)
		}
		p = p.Masked()
		switch {
		case p.Addr().Is4() && p.Bits() < widestV4:
			return nil, fmt.Errorf("%s is wider than a /%d, and external access lets in no more than one network's range. Name the addresses that reach it", g, widestV4)
		case p.Addr().Is6() && p.Bits() < widestV6:
			return nil, fmt.Errorf("%s is wider than a /%d, and external access lets in no more than one network's range. Name the addresses that reach it", g, widestV6)
		}
		if s := p.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	switch {
	case len(out) == 0:
		return nil, errors.New("external access lets in only the addresses listed, and none are. Name one at least, such as 203.0.113.7 or 203.0.113.0/24")
	case len(out) > mostAllowFrom:
		return nil, fmt.Errorf("external access lets in at most %d ranges, and %d are listed. Name a range that holds several, such as 203.0.113.0/24", mostAllowFrom, len(out))
	}
	return out, nil
}

// ExternalPorts are the ports a service publishes as its external access, beyond
// its server: tagged, and not on loopback alone.
func ExternalPorts(svc *resourcev1.Service) []*resourcev1.PortMapping {
	return externalPorts(svc, nil)
}

// externalPorts are a service's external ports, beyond its server: those tagged, and
// any its external rules guard, by host port, whether tagged or not. The tag alone is
// never what's relied on: a writer that drops it leaves the rules saying what the
// port is (the re-check's N1).
func externalPorts(svc *resourcev1.Service, guarded map[uint32]bool) []*resourcev1.PortMapping {
	var out []*resourcev1.PortMapping
	for _, p := range svc.GetApp().GetPorts() {
		if (p.GetExternal() || guarded[p.GetHostPort()]) && Public(p.GetHostIp()) && !p.GetHeld() {
			out = append(out, p)
		}
	}
	return out
}

// rulePorts are the ports a service's external rules cover.
func rulePorts(rules []store.Resource) map[uint32]bool {
	out := map[uint32]bool{}
	for i := range rules {
		spec := &resourcev1.FirewallRule{}
		if proto.Unmarshal(rules[i].Spec, spec) == nil && spec.GetPort() != 0 {
			out[spec.GetPort()] = true
		}
	}
	return out
}

// keepExternalTag puts a tagged port's tag back on what a server has, for a port it
// still publishes beyond the server, as drift that's kept becomes the latest revision:
// a container made outside Dockwell, or by an agent from before the tag, reads back
// without it (the re-check's N1).
func keepExternalTag(current, kept proto.Message) proto.Message {
	cur, _ := current.(*resourcev1.Service)
	obs, ok := kept.(*resourcev1.Service)
	if !ok || len(ExternalPorts(cur)) == 0 || obs.GetApp() == nil {
		return kept
	}
	out := proto.CloneOf(obs)
	for _, p := range out.GetApp().GetPorts() {
		for _, t := range ExternalPorts(cur) {
			if p.GetHostPort() == t.GetHostPort() && p.GetProtocol() == t.GetProtocol() && Public(p.GetHostIp()) {
				p.External = true
			}
		}
	}
	return out
}

// externalRules are the stored rules of every service's external access, by the
// service's ID, each with the server it's on.
func externalRules(ctx context.Context, tx *store.Tx) (map[string][]store.Resource, error) {
	rules, err := tx.ListKind(ctx, string(firewallRuleKind))
	if err != nil {
		return nil, err
	}
	out := map[string][]store.Resource{}
	for i := range rules {
		for _, which := range []string{ExternalAllow, ExternalDeny} {
			if svc, ok := ExternalOf(rules[i].Name, which); ok {
				out[svc] = append(out[svc], rules[i])
			}
		}
	}
	return out, nil
}

// withExternalAccessFollowing has a database's external rules follow it: where the
// change runs a service with a tagged port on a server that has no rules for it, as
// a move, a server added beside it or a removal that moves it does, the rules come
// along, as it has them on a server it runs on now; where it no longer runs on a
// server, or is deleted, its rules there go. A change that writes the service's
// rules itself, as SetDatabaseExternalAccess does, is left as it is.
func (e *Engine) withExternalAccessFollowing(ctx context.Context, tx *store.Tx, items []item, named map[resource.Ref]bool) ([]item, error) {
	var services []*item
	writes := map[string]bool{}
	for i := range items {
		it := &items[i]
		switch {
		case it.ref.Kind == serviceKind && it.id != "" && it.current != nil:
			services = append(services, it)
		case it.ref.Kind == firewallRuleKind && it.spec != nil:
			// A delete of them, as a removal makes on the server leaving, writes none.
			for _, which := range []string{ExternalAllow, ExternalDeny} {
				if svc, ok := ExternalOf(it.ref.Name, which); ok {
					writes[svc] = true
				}
			}
		}
	}
	if len(services) == 0 {
		return items, nil
	}
	stored, err := externalRules(ctx, tx)
	if err != nil {
		return items, err
	}
	for _, it := range services {
		if writes[it.id] || len(stored[it.id]) == 0 {
			continue
		}
		svc, _ := it.spec.(*resourcev1.Service)
		keeps := svc != nil && len(externalPorts(svc, rulePorts(stored[it.id]))) > 0
		have := map[string]map[string]*store.Resource{} // by server, then which
		for j := range stored[it.id] {
			r := &stored[it.id][j]
			which := ExternalAllow
			if strings.HasSuffix(r.Name, "-"+ExternalDeny) {
				which = ExternalDeny
			}
			if have[r.Scope] == nil {
				have[r.Scope] = map[string]*store.Resource{}
			}
			have[r.Scope][which] = r
		}
		// What it has on one server is what it takes to another: the first by name.
		var from map[string]*store.Resource
		for _, server := range slices.Sorted(maps.Keys(have)) {
			if len(have[server]) == 2 {
				from = have[server]
				break
			}
		}
		add := func(ref resource.Ref, spec proto.Message) error {
			if named[ref] {
				return nil
			}
			more, err := e.load(ctx, tx, ref)
			if err != nil {
				return err
			}
			if spec == nil && more.current == nil {
				return nil
			}
			more.spec = spec
			if spec != nil && more.id == "" {
				more.id = e.newID(ref.Kind)
			}
			if spec != nil {
				// As an edit of it would be: what's written is what's checked.
				if err := e.kinds.Validate(ref.Kind, more.spec); err != nil {
					return fault.New(ErrInvalid, "%s: %w", ref, err)
				}
				if more.encoded, err = resource.Encode(spec); err != nil {
					return err
				}
				if more.servers, err = e.place(ctx, tx, more, nil); err != nil {
					return err
				}
			}
			named[ref] = true
			items = append(items, more)
			return nil
		}
		if keeps && from != nil {
			for _, server := range it.servers {
				for _, which := range []string{ExternalAllow, ExternalDeny} {
					if have[server][which] != nil {
						continue
					}
					spec := &resourcev1.FirewallRule{}
					if err := proto.Unmarshal(from[which].Spec, spec); err != nil {
						return items, fmt.Errorf("can't read the firewall rule %s: %w", from[which].Name, err)
					}
					if err := add(resource.Ref{Kind: firewallRuleKind, Scope: server, Name: ExternalRule(it.id, which)}, spec); err != nil {
						return items, err
					}
				}
			}
		}
		// Where it no longer runs, or once it's gone, its rules go.
		for _, server := range slices.Sorted(maps.Keys(have)) {
			if svc != nil && slices.Contains(it.servers, server) {
				continue
			}
			for _, which := range []string{ExternalAllow, ExternalDeny} {
				if have[server][which] == nil {
					continue
				}
				if err := add(resource.Ref{Kind: firewallRuleKind, Scope: server, Name: ExternalRule(it.id, which)}, nil); err != nil {
					return items, err
				}
			}
		}
	}
	return items, nil
}

// checkExternalAccess refuses a change that leaves a database's external port
// answering more than its list, however the change comes about: its external port on
// a server without both its rules there, its rules on another port than its tagged
// one, an allow rule that lets in more than a list may, another allow that lets
// anyone in on that port, past the list, as allows run before denies (ADR 0052),
// whether the change writes it or it's there already, a second public mapping of the
// same port in the app, or the tag dropped from a port its rules still guard.
func (e *Engine) checkExternalAccess(ctx context.Context, tx *store.Tx, items []item) error {
	stored, err := e.externalRulesIfAny(ctx, tx, items)
	if err != nil {
		return err
	}
	// The ports a service's rules guard once the change applies: those stored that it
	// leaves, and those it writes.
	guarded := func(svcID string) map[uint32]bool {
		var after []store.Resource
		for i := range stored[svcID] {
			r := stored[svcID][i]
			gone := slices.ContainsFunc(items, func(it item) bool {
				return it.ref.Kind == firewallRuleKind && it.ref.Scope == r.Scope && it.ref.Name == r.Name && it.spec == nil
			})
			if !gone {
				after = append(after, r)
			}
		}
		out := rulePorts(after)
		for i := range items {
			it := &items[i]
			if it.ref.Kind != firewallRuleKind || it.spec == nil {
				continue
			}
			if id, ok := ExternalOf(it.ref.Name, ExternalAllow); !ok || id != svcID {
				if id, ok := ExternalOf(it.ref.Name, ExternalDeny); !ok || id != svcID {
					continue
				}
			}
			if r, ok := it.spec.(*resourcev1.FirewallRule); ok && r.GetPort() != 0 {
				out[r.GetPort()] = true
			}
		}
		return out
	}
	// What to look at: each service the change runs, on the servers it runs on after
	// it, and each service whose rules the change touches, on that rule's server.
	type target struct {
		name    string
		svc     *resourcev1.Service
		servers []string
		// runsOn is where it runs once the change applies.
		runsOn []string
	}
	targets := map[string]*target{}
	look := func(svcID string, servers ...string) error {
		t := targets[svcID]
		if t == nil {
			name, svc, err := e.serviceAfter(ctx, tx, items, svcID)
			if err != nil {
				return err
			}
			runsOn, err := e.runsOnAfter(ctx, tx, items, svcID)
			if err != nil {
				return err
			}
			t = &target{name: name, svc: svc, runsOn: runsOn}
			targets[svcID] = t
		}
		for _, s := range servers {
			if !slices.Contains(t.servers, s) {
				t.servers = append(t.servers, s)
			}
		}
		return nil
	}
	var allows []*item
	for i := range items {
		it := &items[i]
		switch it.ref.Kind {
		case serviceKind:
			svc, _ := it.spec.(*resourcev1.Service)
			if it.id == "" {
				if len(ExternalPorts(svc)) > 0 {
					return fault.New(ErrInvalid, "%s publishes a port as external access before it exists, which only its page can give it. Make it first, then turn its external access on", it.ref.Name)
				}
				continue
			}
			ports := guarded(it.id)
			if err := untagged(it, ports); err != nil {
				return err
			}
			if len(externalPorts(svc, ports)) == 0 {
				continue
			}
			if err := look(it.id, it.servers...); err != nil {
				return err
			}
		case firewallRuleKind:
			for _, which := range []string{ExternalAllow, ExternalDeny} {
				if svc, ok := ExternalOf(it.ref.Name, which); ok {
					if err := look(svc, it.ref.Scope); err != nil {
						return err
					}
				}
			}
			if r, ok := it.spec.(*resourcev1.FirewallRule); ok && r.GetAction() != resourcev1.FirewallAction_FIREWALL_ACTION_DENY {
				allows = append(allows, it)
			}
		}
	}
	for _, svcID := range slices.Sorted(maps.Keys(targets)) {
		t := targets[svcID]
		ports := externalPorts(t.svc, guarded(svcID))
		if err := secondPublic(t.name, t.svc, ports); err != nil {
			return err
		}
		slices.Sort(t.servers)
		for _, p := range ports {
			for _, server := range t.servers {
				// A server it leaves needs no rules for it: what goes there goes with it.
				if !slices.Contains(t.runsOn, server) {
					continue
				}
				allow, err := e.ruleAfter(ctx, tx, items, resource.Ref{Kind: firewallRuleKind, Scope: server, Name: ExternalRule(svcID, ExternalAllow)})
				if err != nil {
					return err
				}
				deny, err := e.ruleAfter(ctx, tx, items, resource.Ref{Kind: firewallRuleKind, Scope: server, Name: ExternalRule(svcID, ExternalDeny)})
				if err != nil {
					return err
				}
				if missing := externalMissing(p, allow, deny); missing != "" {
					return fault.New(ErrInvalid, "%s publishes port %d to the addresses its external access lets in, and on %s %s, so anyone could reach it there. "+
						"Turn its external access off, or on again, from its page", t.name, p.GetHostPort(), serverWords(ctx, tx, server), missing)
				}
				// An allow there already that lets anyone in reaches past the list (the
				// re-check's N3).
				if err := e.wideAllowOn(ctx, tx, items, server, p.GetHostPort(), t.name); err != nil {
					return err
				}
			}
		}
	}
	// An allow on a server's external port that isn't the list's own rule, and lets
	// in more than a list may, reaches past the list.
	for _, it := range allows {
		r := it.spec.(*resourcev1.FirewallRule)
		if _, own := ExternalOf(it.ref.Name, ExternalAllow); own {
			continue
		}
		if _, err := AllowFrom(r.GetSources()); err == nil {
			continue
		}
		guarded, name, err := e.externalPortOn(ctx, tx, items, it.ref.Scope, r)
		if err != nil {
			return err
		}
		if guarded {
			return fault.New(ErrInvalid, "%s lets anyone reach port %d on %s, past %s's external access list, as an allow comes before every deny. "+
				"Change who reaches %s on its page instead", it.ref.Name, r.GetPort(), serverWords(ctx, tx, it.ref.Scope), name, name)
		}
	}
	return nil
}

// externalRulesIfAny are the stored external rules, by service, read only where the
// change has a service or a firewall rule in it.
func (e *Engine) externalRulesIfAny(ctx context.Context, tx *store.Tx, items []item) (map[string][]store.Resource, error) {
	if !slices.ContainsFunc(items, func(it item) bool { return it.ref.Kind == serviceKind || it.ref.Kind == firewallRuleKind }) {
		return nil, nil
	}
	return externalRules(ctx, tx)
}

// untagged refuses an edit that drops the tag from a port beyond the server that its
// external rules still guard: the tag is what has the rules follow it, so a form that
// rebuilds the port without it isn't to take that away. Turning external access off
// takes the port and the rules together, and a port moved to loopback answers no one
// beyond the server.
func untagged(it *item, guarded map[uint32]bool) error {
	cur, _ := it.current.(*resourcev1.Service)
	svc, _ := it.spec.(*resourcev1.Service)
	if svc == nil || len(guarded) == 0 {
		return nil
	}
	for _, was := range ExternalPorts(cur) {
		for _, p := range svc.GetApp().GetPorts() {
			if p.GetHostPort() == was.GetHostPort() && p.GetProtocol() == was.GetProtocol() && Public(p.GetHostIp()) && !p.GetHeld() && !p.GetExternal() && guarded[p.GetHostPort()] {
				return fault.New(ErrInvalid, "%s's port %d is its external access, and this change drops that while its rules stay. "+
					"Keep the port as it is, or turn its external access off from its page", it.ref.Name, p.GetHostPort())
			}
		}
	}
	return nil
}

// secondPublic refuses another mapping beyond the server of the container port an
// external port maps, which would answer everyone beside the list.
func secondPublic(name string, svc *resourcev1.Service, external []*resourcev1.PortMapping) error {
	for _, p := range svc.GetApp().GetPorts() {
		if slices.Contains(external, p) || !Public(p.GetHostIp()) || p.GetHeld() {
			continue
		}
		for _, x := range external {
			if p.GetContainerPort() == x.GetContainerPort() && p.GetProtocol() == x.GetProtocol() {
				return fault.New(ErrInvalid, "%s publishes port %d to everyone beside its external access on port %d, which would let anyone past its list. "+
					"Put port %d on 127.0.0.1, or remove it", name, p.GetHostPort(), x.GetHostPort(), p.GetHostPort())
			}
		}
	}
	return nil
}

// wideAllowOn refuses where another allow on a server's port, as the change leaves
// it, lets in more than a list may: it reaches past the list.
func (e *Engine) wideAllowOn(ctx context.Context, tx *store.Tx, items []item, server string, port uint32, name string) error {
	stored, err := tx.ListResources(ctx, server, string(firewallRuleKind))
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for i := range stored {
		names[stored[i].Name] = true
	}
	for i := range items {
		if items[i].ref.Kind == firewallRuleKind && items[i].ref.Scope == server {
			names[items[i].ref.Name] = true
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(names)) {
		if _, own := ExternalOf(rule, ExternalAllow); own {
			continue
		}
		if _, own := ExternalOf(rule, ExternalDeny); own {
			continue
		}
		r, err := e.ruleAfter(ctx, tx, items, resource.Ref{Kind: firewallRuleKind, Scope: server, Name: rule})
		if err != nil {
			return err
		}
		if r == nil || r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY || r.GetPort() != port {
			continue
		}
		if _, err := AllowFrom(r.GetSources()); err != nil {
			return fault.New(ErrInvalid, "%s already lets anyone reach port %d on %s, past %s's list, as an allow comes before every deny. Narrow it or delete it first",
				rule, port, serverWords(ctx, tx, server), name)
		}
	}
	return nil
}

// externalMissing says what's wrong with a tagged port's rules on a server, or "".
func externalMissing(p *resourcev1.PortMapping, allow, deny *resourcev1.FirewallRule) string {
	protocol := cmp.Or(p.GetProtocol(), resourcev1.Protocol_PROTOCOL_TCP)
	same := func(r *resourcev1.FirewallRule) bool {
		return r.GetPort() == p.GetHostPort() && cmp.Or(r.GetProtocol(), resourcev1.Protocol_PROTOCOL_TCP) == protocol
	}
	switch {
	case allow == nil && deny == nil:
		return "it has neither of its rules"
	case deny == nil:
		return "its rule that keeps everyone else out is gone"
	case allow == nil:
		return "its rule that lets the list in is gone"
	case deny.GetAction() != resourcev1.FirewallAction_FIREWALL_ACTION_DENY || len(deny.GetSources()) > 0 || !same(deny):
		return fmt.Sprintf("its rule that keeps everyone else out no longer covers port %d from anywhere", p.GetHostPort())
	case allow.GetAction() != resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW || !same(allow):
		return fmt.Sprintf("its rule that lets the list in no longer covers port %d", p.GetHostPort())
	}
	if _, err := AllowFrom(allow.GetSources()); err != nil {
		return "its rule lets in more than a list may: " + err.Error()
	}
	return ""
}

// externalPortOn says whether a rule's port, on a server, is a database's external
// port there as the change leaves it, and that database's name.
func (e *Engine) externalPortOn(ctx context.Context, tx *store.Tx, items []item, server string, r *resourcev1.FirewallRule) (bool, string, error) {
	stored, err := tx.ListResources(ctx, server, string(firewallRuleKind))
	if err != nil {
		return false, "", err
	}
	names := map[string]bool{}
	for i := range stored {
		names[stored[i].Name] = true
	}
	for i := range items {
		if items[i].ref.Kind == firewallRuleKind && items[i].ref.Scope == server {
			names[items[i].ref.Name] = true
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(names)) {
		svcID, ok := ExternalOf(rule, ExternalDeny)
		if !ok {
			continue
		}
		deny, err := e.ruleAfter(ctx, tx, items, resource.Ref{Kind: firewallRuleKind, Scope: server, Name: rule})
		if err != nil {
			return false, "", err
		}
		if deny == nil || deny.GetPort() != r.GetPort() {
			continue
		}
		name, svc, err := e.serviceAfter(ctx, tx, items, svcID)
		if err != nil {
			return false, "", err
		}
		for _, p := range ExternalPorts(svc) {
			if p.GetHostPort() == r.GetPort() {
				return true, name, nil
			}
		}
	}
	return false, "", nil
}

// ruleAfter is a firewall rule as a change leaves it: as the change edits it, or as
// it's stored. One the change deletes, or that isn't there, is nil.
func (e *Engine) ruleAfter(ctx context.Context, tx *store.Tx, items []item, ref resource.Ref) (*resourcev1.FirewallRule, error) {
	for i := range items {
		if items[i].ref == ref {
			r, _ := items[i].spec.(*resourcev1.FirewallRule)
			return r, nil
		}
	}
	r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// A firewall rule holds nothing secret, so it's stored as it is.
	spec := &resourcev1.FirewallRule{}
	if err := proto.Unmarshal(r.Spec, spec); err != nil {
		return nil, fmt.Errorf("can't read the firewall rule %s: %w", ref.Name, err)
	}
	return spec, nil
}

// serverWords names a server by its name, or its ID where it has none.
func serverWords(ctx context.Context, tx *store.Tx, server string) string {
	if s, err := tx.GetServer(ctx, server); err == nil && s.Name != "" {
		return s.Name
	}
	return server
}

// runsOnAfter is where a service runs as a change leaves it, by its ID: where the
// change places it, or where it's deployed now.
func (e *Engine) runsOnAfter(ctx context.Context, tx *store.Tx, items []item, svcID string) ([]string, error) {
	for i := range items {
		if items[i].id == svcID && items[i].ref.Kind == serviceKind {
			if items[i].spec == nil {
				return nil, nil
			}
			return items[i].servers, nil
		}
	}
	r, err := tx.GetResourceByID(ctx, svcID)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it, err := e.load(ctx, tx, resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name})
	if err != nil {
		return nil, err
	}
	return it.placed, nil
}

// serviceAfter is a service as a change leaves it, by its ID: as the change edits it,
// or as it's stored. A service the change deletes, or that isn't there, is nil.
func (e *Engine) serviceAfter(ctx context.Context, tx *store.Tx, items []item, svcID string) (string, *resourcev1.Service, error) {
	for i := range items {
		if items[i].id == svcID && items[i].ref.Kind == serviceKind {
			svc, _ := items[i].spec.(*resourcev1.Service)
			return items[i].ref.Name, svc, nil
		}
	}
	r, err := tx.GetResourceByID(ctx, svcID)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var m proto.Message
	if m, err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
		return "", nil, err
	}
	svc, _ := m.(*resourcev1.Service)
	return r.Name, svc, nil
}

// writesExternalRule says a step's resource is a database's external rule.
func writesExternalRule(ref resource.Ref) bool {
	if ref.Kind != firewallRuleKind {
		return false
	}
	_, allow := ExternalOf(ref.Name, ExternalAllow)
	_, deny := ExternalOf(ref.Name, ExternalDeny)
	return allow || deny
}

// rulesAround moves each server's steps under the firewall's timer, which
// accessBetween puts between a queue's other builds and its other deletes: a server
// whose firewall steps make or change a rule has them run first, so what a change
// opens beyond it opens only once the rules that guard it have loaded and been
// confirmed; a server whose firewall steps only delete has them run last, after the
// database's own step there, so its rules go only once its port is closed, as a
// database moves off it or is deleted (the re-check's N2). Each server's steps stay
// together, under one timer.
func rulesAround[Q any](e *Engine, queue []Q, of func(Q) (server string, kind resource.Kind, builds bool)) []Q {
	guards := func(q Q) bool { _, kind, _ := of(q); return e.guardsAccess(kind) }
	first := slices.IndexFunc(queue, guards)
	if first < 0 {
		return queue
	}
	last := first
	for last+1 < len(queue) && guards(queue[last+1]) {
		last++
	}
	run := queue[first : last+1]
	builds := map[string]bool{}
	for _, q := range run {
		if server, _, b := of(q); b {
			builds[server] = true
		}
	}
	var front, back []Q
	for _, q := range run {
		if server, _, _ := of(q); builds[server] {
			front = append(front, q)
		} else {
			back = append(back, q)
		}
	}
	return slices.Concat(front, queue[:first], queue[last+1:], back)
}
