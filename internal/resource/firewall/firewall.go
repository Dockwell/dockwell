// SPDX-License-Identifier: AGPL-3.0-only

// Package firewall is the driver for inbound firewall rules, the
// "firewall.rule" kind. It talks to the firewall through Backend, which the
// agent implements with nftables and tests with Fake. Rules change
// atomically, so no step has downtime.
package firewall

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the firewall rule kind.
const Kind resource.Kind = "firewall.rule"

// sshPort is the port a rule must not close by accident.
const sshPort = 22

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
// 0052). It judges the state the edit leaves: actual is what's there now, nil
// for nothing, and desired what it becomes, nil to delete it.
type Checker interface {
	CheckRule(actual, desired *resourcev1.FirewallRule) error
	CheckPolicy(desired *resourcev1.FirewallPolicy) error
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

// Validate requires a port and valid sources, and puts sources in a
// canonical order.
func (*Driver) Validate(spec proto.Message) error {
	r := spec.(*resourcev1.FirewallRule)
	if r.GetPort() == 0 || r.GetPort() > 65535 {
		return fmt.Errorf("a rule's port runs from 1 to 65535, not %d", r.GetPort())
	}
	if r.Action == resourcev1.FirewallAction_FIREWALL_ACTION_UNSPECIFIED {
		r.Action = resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW
	}
	if r.Protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED {
		r.Protocol = resourcev1.Protocol_PROTOCOL_TCP
	}
	sources := make([]string, 0, len(r.GetSources()))
	for _, s := range r.GetSources() {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			addr, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return fmt.Errorf("%q isn't an address or a CIDR range, such as 203.0.113.0/24", s)
			}
			if addr.Zone() != "" {
				return fmt.Errorf("%q names a network interface, which a rule can't match; drop the %%%s", s, addr.Zone())
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		sources = append(sources, p.Masked().String())
	}
	slices.Sort(sources)
	r.Sources = slices.Compact(sources)
	return nil
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
// let in. It lets the backend refuse what leaves the server unsafe to reach.
func (d *Driver) Plan(actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	var dec resource.Decision
	switch {
	case actual == nil:
		dec.Action = resource.ActionCreate
	case desired == nil:
		dec.Action = resource.ActionDelete
	default:
		dec.Action = resource.ActionUpdate
	}
	was, _ := actual.(*resourcev1.FirewallRule)
	r, _ := desired.(*resourcev1.FirewallRule)
	if blocksSSH(r) || r.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY || narrows(was, r) {
		dec.Warnings = append(dec.Warnings, resource.WarningMayLockOut)
	}
	if c, ok := d.Backend.(Checker); ok {
		if err := c.CheckRule(was, r); err != nil {
			return dec, err
		}
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
		r.GetPort() != was.GetPort() || r.GetProtocol() != was.GetProtocol() {
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

// blocksSSH reports a rule that denies SSH from anywhere: no sources, or a
// source covering every address.
func blocksSSH(r *resourcev1.FirewallRule) bool {
	if r.GetAction() != resourcev1.FirewallAction_FIREWALL_ACTION_DENY || r.GetPort() != sshPort ||
		r.GetProtocol() != resourcev1.Protocol_PROTOCOL_TCP {
		return false
	}
	if len(r.GetSources()) == 0 {
		return true
	}
	for _, s := range r.GetSources() {
		if p, err := netip.ParsePrefix(s); err == nil && p.Bits() == 0 {
			return true
		}
	}
	return false
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
