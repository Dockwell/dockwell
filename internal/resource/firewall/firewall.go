// SPDX-License-Identifier: AGPL-3.0-only

// Package firewall is the driver for inbound firewall rules, the
// "firewall.rule" kind. It talks to the firewall through Backend, which the
// agent implements with nftables and tests with Fake. Rules change
// atomically, so no step has downtime.
package firewall

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the firewall rule kind.
const Kind resource.Kind = "firewall.rule"

// Backend is the part of the firewall the driver needs. Each call replaces
// the ruleset atomically.
type Backend interface {
	// Rule returns the named rule, or nil if there is none.
	Rule(ctx context.Context, name string) (*resourcev1.FirewallRule, error)
	// Put adds or replaces the named rule.
	Put(ctx context.Context, name string, rule *resourcev1.FirewallRule) error
	// Delete removes the named rule.
	Delete(ctx context.Context, name string) error
}

// Checker is a backend that refuses, when a change is planned, a rule or a
// policy that leaves the server unsafe to reach: one that narrows SSH with
// nobody's address to keep, or denies the control plane's own ports (ADR
// 0052). Each check sees the other edits in the plan.
type Checker interface {
	CheckRule(ctx context.Context, ref resource.Ref, actual, desired *resourcev1.FirewallRule) error
	CheckPolicy(ctx context.Context, desired *resourcev1.FirewallPolicy) error
}

// Ways are the ports someone reaches a server by, over TCP and over UDP, each
// known or not. Where one isn't known, every deny and narrowing over it warns,
// as not knowing is what the warning is for.
type Ways struct {
	// TCP are SSH's ports, and on the control server the control plane's own,
	// with those a proxy holds for the panel and its agents.
	TCP      []uint16
	TCPKnown bool
	// UDP are the tunnels' ports, such as WireGuard's.
	UDP      []uint16
	UDPKnown bool
}

// Reacher is a backend that knows the ways into the server. Plan warns only for
// a rule that may close one of them (#850, ADR 0052).
type Reacher interface {
	WaysIn(ctx context.Context) Ways
}

// Waiter is a backend whose changes wait for their confirmation: the change's
// last step on the server ends once it's kept, or fails once the server put
// back what it had (ADR 0052).
type Waiter interface {
	Wait(ctx context.Context) error
}

// Driver manages firewall rules.
type Driver struct {
	Backend Backend
}

var (
	_ resource.Driver      = (*Driver)(nil)
	_ resource.AccessGuard = (*Driver)(nil)
	_ resource.Readier     = (*Driver)(nil)
)

// GuardsAccess marks a rule as deciding who reaches the server: it applies
// under the firewall's timer.
func (*Driver) GuardsAccess() {}

// Ready says whether the server can hold Dockwell's firewall, when the backend
// can say.
func (d *Driver) Ready(ctx context.Context) error { return ready(ctx, d.Backend) }

func ready(ctx context.Context, backend any) error {
	if r, ok := backend.(resource.Readier); ok {
		return r.Ready(ctx)
	}
	return nil
}

// Kind returns "firewall.rule".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the server: a firewall rule is host-level.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty FirewallRule.
func (*Driver) New() proto.Message { return &resourcev1.FirewallRule{} }

// Validate requires a port or a source and puts sources in a canonical order.
func (*Driver) Validate(spec proto.Message) error {
	r := spec.(*resourcev1.FirewallRule)
	if r.GetPort() > 65535 {
		return fmt.Errorf("a rule's port runs from 1 to 65535, not %d", r.GetPort())
	}
	if r.GetPort() == 0 && len(r.GetSources()) == 0 {
		return errors.New("a rule needs a port or addresses to cover on every port. Add one")
	}
	if r.Action == resourcev1.FirewallAction_FIREWALL_ACTION_UNSPECIFIED {
		r.Action = resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW
	}
	if r.Protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED && r.GetPort() != 0 {
		r.Protocol = resourcev1.Protocol_PROTOCOL_TCP
	}
	if r.Action != resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW && r.Action != resourcev1.FirewallAction_FIREWALL_ACTION_DENY {
		return errors.New("choose allow or deny for the firewall rule")
	}
	switch r.Protocol {
	case resourcev1.Protocol_PROTOCOL_UNSPECIFIED, resourcev1.Protocol_PROTOCOL_TCP, resourcev1.Protocol_PROTOCOL_UDP:
	default:
		return errors.New("choose TCP, UDP or all protocols for the firewall rule")
	}
	sources := make([]string, 0, len(r.GetSources()))
	for _, s := range r.GetSources() {
		p, err := sourcePrefix(s)
		if err != nil {
			return err
		}
		if r.GetPort() == 0 && r.Action == resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW && p.Bits() == 0 {
			return errors.New("this range allows every address on every matching port. Narrow the range or turn off deny everything else")
		}
		sources = append(sources, p.String())
	}
	slices.Sort(sources)
	r.Sources = slices.Compact(sources)
	return nil
}

// SourceContains reports whether one firewall source covers an address.
func SourceContains(source string, addr netip.Addr) bool {
	p, err := sourcePrefix(source)
	return err == nil && p.Contains(addr.Unmap())
}

func sourcePrefix(source string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(source)
	if err != nil {
		addr, err := netip.ParseAddr(source)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q isn't an address or a CIDR range, such as 203.0.113.0/24", source)
		}
		if addr.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%q names a network interface, which a rule can't match; drop the %%%s", source, addr.Zone())
		}
		p = netip.PrefixFrom(addr, addr.BitLen())
	}
	if p.Addr().Is4In6() {
		p = netip.PrefixFrom(p.Addr().Unmap(), max(0, p.Bits()-96))
	}
	return p.Masked(), nil
}

// Observe asks the firewall.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	r, err := d.Backend.Rule(ctx, ref.Name)
	if err != nil || r == nil {
		return nil, err
	}
	return r, nil
}

// Plan never has downtime, but flags a rule that may cut off access: a deny, or
// an allow deleted or narrowed, which under deny everything else drops what it
// let in, of a port someone reaches the server by, as a Reacher says. Without
// one, or where it can't say, every deny and narrowing is flagged. It lets the
// backend refuse what leaves the server unsafe to reach.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.Judge(context.Background(), resource.Ref{Kind: Kind}, actual, desired, changes)
}

// Judge is Plan within a plan's context, whose checks share one read of the
// server (#850's re-check, N4).
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	var dec resource.Decision
	was, _ := actual.(*resourcev1.FirewallRule)
	r, _ := desired.(*resourcev1.FirewallRule)
	switch {
	case was == nil:
		dec.Action = resource.ActionCreate
	case r == nil:
		dec.Action = resource.ActionDelete
	default:
		dec.Action = resource.ActionUpdate
	}
	if mayCutOff(ctx, d.Backend, was, r) {
		dec.Warnings = append(dec.Warnings, resource.WarningMayLockOut)
	}
	if c, ok := d.Backend.(Checker); ok {
		if err := c.CheckRule(ctx, ref, was, r); err != nil {
			return dec, err
		}
	}
	if s, ok := d.Backend.(Stayer); ok {
		dec.Stays = s.StaysAfterRule(ctx, ref, was, r)
	}
	if len(dec.Warnings) == 0 && dec.Stays != nil && len(dec.Stays.SSHSessionsCovered) > 0 {
		dec.Warnings = append(dec.Warnings, resource.WarningMayLockOut)
	}
	return dec, nil
}

// narrows says whether an edit takes away some of what an allow let in: it
// deletes it, makes it a deny, moves it, or lets in from fewer places.
func narrows(was, r *resourcev1.FirewallRule) bool {
	if was == nil || was.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY {
		return false
	}
	if r == nil || r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY ||
		r.GetPort() != 0 && r.GetPort() != was.GetPort() ||
		(r.GetPort() != 0 || r.GetProtocol() != resourcev1.Protocol_PROTOCOL_UNSPECIFIED) && r.GetProtocol() != was.GetProtocol() {
		return true
	}
	return !covers(r.GetSources(), was.GetSources())
}

// covers says whether sources take in every address of others; empty is
// anywhere.
func covers(sources, others []string) bool {
	if len(sources) == 0 {
		return true
	}
	if len(others) == 0 {
		return false
	}
	for _, o := range others {
		p, err := netip.ParsePrefix(o)
		if err != nil {
			return false
		}
		if !slices.ContainsFunc(sources, func(s string) bool {
			q, err := netip.ParsePrefix(s)
			return err == nil && q.Bits() <= p.Bits() && q.Contains(p.Addr())
		}) {
			return false
		}
	}
	return true
}

// mayCutOff says whether an edit may close a way into the server: a deny of one
// of the ports it's reached by, over that protocol, or an allow of one that the
// edit deletes or narrows (#850). A deny of another port, such as a database's
// under external access, can't cut anyone off. Where the backend can't say which
// ports those are, any deny or narrowing over that protocol may.
func mayCutOff(ctx context.Context, backend Backend, was, r *resourcev1.FirewallRule) bool {
	deny := r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY
	narrowed := narrows(was, r)
	if !deny && !narrowed {
		return false
	}
	reacher, ok := backend.(Reacher)
	if !ok {
		return true
	}
	ways := reacher.WaysIn(ctx)
	return deny && reaches(r, ways) || narrowed && reaches(was, ways)
}

// reaches says whether a rule's port may be a way in, over its protocol: one
// of the ways' ports, or any where they aren't known.
func reaches(r *resourcev1.FirewallRule, ways Ways) bool {
	if r.GetPort() == 0 {
		switch r.GetProtocol() {
		case resourcev1.Protocol_PROTOCOL_TCP:
			return !ways.TCPKnown || len(ways.TCP) > 0
		case resourcev1.Protocol_PROTOCOL_UDP:
			return !ways.UDPKnown || len(ways.UDP) > 0
		default:
			return !ways.TCPKnown || !ways.UDPKnown || len(ways.TCP)+len(ways.UDP) > 0
		}
	}
	ports, known := ways.TCP, ways.TCPKnown
	if r.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
		ports, known = ways.UDP, ways.UDPKnown
	}
	return !known || slices.ContainsFunc(ports, func(p uint16) bool { return uint32(p) == r.GetPort() })
}

// Apply puts or deletes the rule.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	switch step.Action {
	case resource.ActionCreate, resource.ActionUpdate, resource.ActionReplace:
		return d.Backend.Put(ctx, step.Ref.Name, desired.(*resourcev1.FirewallRule))
	case resource.ActionDelete:
		return d.Backend.Delete(ctx, step.Ref.Name)
	default:
		return nil
	}
}

// Verify waits, on a backend whose changes wait for their confirmation, for
// the change to be kept. A rule has no health of its own.
func (d *Driver) Verify(ctx context.Context, _ resource.Ref, _ proto.Message) error {
	return wait(ctx, d.Backend)
}

func wait(ctx context.Context, backend any) error {
	if w, ok := backend.(Waiter); ok {
		return w.Wait(ctx)
	}
	return nil
}
