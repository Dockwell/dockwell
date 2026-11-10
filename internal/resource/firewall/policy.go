// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// PolicyKind is a server's firewall policy: what it does with what no rule lets in.
const PolicyKind resource.Kind = "firewall.policy"

// PolicyName is the name a server's one policy goes by.
const PolicyName = "default"

// PolicyBackend is the part of the firewall the policy driver needs. Each call
// replaces the ruleset atomically.
type PolicyBackend interface {
	// Policy returns the server's policy, or nil if it has none.
	Policy(ctx context.Context) (*resourcev1.FirewallPolicy, error)
	// PutPolicy sets it.
	PutPolicy(ctx context.Context, policy *resourcev1.FirewallPolicy) error
	// DeletePolicy removes it, which lets in what no rule drops, as with none.
	DeletePolicy(ctx context.Context) error
}

// Closer is a backend that says what turning deny everything else on would
// close, for the plan to list (ADR 0052).
type Closer interface {
	Closes(desired *resourcev1.FirewallPolicy) []string
}

// Stayer is a backend that says who still reaches SSH once an edit that narrows
// it is kept, for the plan to name (ADR 0052).
type Stayer interface {
	StaysAfterPolicy(desired *resourcev1.FirewallPolicy) *resource.AccessStays
	StaysAfterRule(actual, desired *resourcev1.FirewallRule) *resource.AccessStays
}

// PolicyDriver manages a server's firewall policy.
type PolicyDriver struct {
	Backend PolicyBackend
}

var (
	_ resource.Driver      = (*PolicyDriver)(nil)
	_ resource.AccessGuard = (*PolicyDriver)(nil)
	_ resource.Readier     = (*PolicyDriver)(nil)
)

// Kind returns "firewall.policy".
func (*PolicyDriver) Kind() resource.Kind { return PolicyKind }

// Level is the server: a policy is host-level.
func (*PolicyDriver) Level() resource.Level { return resource.LevelServer }

// New returns an empty FirewallPolicy.
func (*PolicyDriver) New() proto.Message { return &resourcev1.FirewallPolicy{} }

// GuardsAccess marks the policy as deciding who reaches the server.
func (*PolicyDriver) GuardsAccess() {}

// Ready says whether the server can hold Dockwell's firewall, when the backend
// can say.
func (d *PolicyDriver) Ready(ctx context.Context) error { return ready(ctx, d.Backend) }

// CheckName takes the one name a server's policy goes by.
func (*PolicyDriver) CheckName(name string) error {
	if name != PolicyName {
		return fmt.Errorf("a server has one firewall policy, named %q, not %q", PolicyName, name)
	}
	return nil
}

// Validate has nothing to check: the policy is on or off.
func (*PolicyDriver) Validate(proto.Message) error { return nil }

// Observe asks the firewall.
func (d *PolicyDriver) Observe(ctx context.Context, _ resource.Ref) (proto.Message, error) {
	p, err := d.Backend.Policy(ctx)
	if err != nil || p == nil {
		return nil, err
	}
	return p, nil
}

// Plan flags turning deny everything else on as able to cut off access, and
// lets the backend refuse a policy that leaves the server unsafe to reach.
func (d *PolicyDriver) Plan(actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	var dec resource.Decision
	switch {
	case actual == nil:
		dec.Action = resource.ActionCreate
	case desired == nil:
		dec.Action = resource.ActionDelete
	default:
		dec.Action = resource.ActionUpdate
	}
	p, _ := desired.(*resourcev1.FirewallPolicy)
	if p.GetDenyEverythingElse() {
		dec.Warnings = append(dec.Warnings, resource.WarningMayLockOut)
		if c, ok := d.Backend.(Closer); ok {
			dec.Closes = c.Closes(p)
		}
	}
	if c, ok := d.Backend.(Checker); ok {
		if err := c.CheckPolicy(p); err != nil {
			return dec, err
		}
	}
	if s, ok := d.Backend.(Stayer); ok {
		dec.Stays = s.StaysAfterPolicy(p)
	}
	return dec, nil
}

// Apply sets or removes the policy.
func (d *PolicyDriver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	switch step.Action {
	case resource.ActionCreate, resource.ActionUpdate, resource.ActionReplace:
		return d.Backend.PutPolicy(ctx, desired.(*resourcev1.FirewallPolicy))
	case resource.ActionDelete:
		return d.Backend.DeletePolicy(ctx)
	default:
		return nil
	}
}

// Verify waits, on a backend whose changes wait for their confirmation, for
// the change to be kept.
func (d *PolicyDriver) Verify(ctx context.Context, _ resource.Ref, _ proto.Message) error {
	return wait(ctx, d.Backend)
}
