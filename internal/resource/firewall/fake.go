// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Fake is an in-memory Backend.
type Fake struct {
	mu    sync.Mutex
	rules map[string]*resourcev1.FirewallRule
}

// NewFake returns a firewall with no rules.
func NewFake() *Fake { return &Fake{rules: map[string]*resourcev1.FirewallRule{}} }

var _ Backend = (*Fake)(nil)

// Rule returns a copy of the named rule.
func (f *Fake) Rule(_ context.Context, name string) (*resourcev1.FirewallRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rules[name]
	if !ok {
		return nil, nil
	}
	return proto.Clone(r).(*resourcev1.FirewallRule), nil
}

// Put stores a copy of rule.
func (f *Fake) Put(_ context.Context, name string, rule *resourcev1.FirewallRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[name] = proto.Clone(rule).(*resourcev1.FirewallRule)
	return nil
}

// Delete removes the named rule.
func (f *Fake) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, name)
	return nil
}

// Len is the number of rules.
func (f *Fake) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rules)
}
