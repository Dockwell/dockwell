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

// Driver manages firewall rules.
type Driver struct {
	Backend Backend
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "firewall.rule".
func (*Driver) Kind() resource.Kind { return Kind }

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
func (d *Driver) Observe(ctx context.Context, name string) (proto.Message, error) {
	r, err := d.Backend.Rule(ctx, name)
	if err != nil || r == nil {
		return nil, err
	}
	return r, nil
}

// Plan never has downtime, but flags a rule that may cut off SSH.
func (*Driver) Plan(actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	var dec resource.Decision
	switch {
	case actual == nil:
		dec.Action = resource.ActionCreate
	case desired == nil:
		dec.Action = resource.ActionDelete
	default:
		dec.Action = resource.ActionUpdate
	}
	if r, ok := desired.(*resourcev1.FirewallRule); ok && blocksSSH(r) {
		dec.Warnings = append(dec.Warnings, resource.WarningMayLockOut)
	}
	return dec, nil
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

// Verify has nothing to check beyond the rule being there: a rule has no
// health of its own.
func (*Driver) Verify(context.Context, string, proto.Message) error { return nil }
