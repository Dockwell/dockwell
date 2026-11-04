// SPDX-License-Identifier: AGPL-3.0-only

// Package resource is the model every managed thing shares: a kind, the
// server it lives on, a name, and a spec. Each kind has a driver that
// observes, plans, applies and verifies it on its server. The revision
// package builds history, plans, undo and drift on top, the same way for
// every kind. See docs/adr/0009-resources-revisions-and-plans.md.
package resource

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
)

// Kind names a type of resource, such as "container" or "firewall.rule".
type Kind string

// Ref names one resource.
type Ref struct {
	Kind   Kind
	Server string // the server's ID
	Name   string // unique among resources of the kind on the server
}

func (r Ref) String() string { return fmt.Sprintf("%s %s on %s", r.Kind, r.Name, r.Server) }

// Action is what applying does to one resource.
type Action int

// Actions, from least to most disruptive.
const (
	ActionNone    Action = iota // already as desired
	ActionCreate                // made for the first time
	ActionUpdate                // changed in place
	ActionReplace               // removed and made again, such as a recreated container
	ActionDelete                // removed
)

var actionNames = [...]string{"none", "create", "update", "replace", "delete"}

func (a Action) String() string {
	if int(a) < len(actionNames) {
		return actionNames[a]
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Effect is what a step does to whatever the resource serves, beyond the
// change itself. The panel words it ("Recreate", "Restart").
type Effect int

// Effects.
const (
	EffectNone     Effect = iota // nothing stops
	EffectRestart                // the process restarts in place
	EffectRecreate               // it is removed and made again
)

// Warning flags a step that may hurt in a way the action alone doesn't
// say. The panel words it.
type Warning int

// Warnings.
const (
	// WarningMayLockOut means the step may cut off remote access to the
	// server, such as a rule that blocks SSH from everywhere.
	WarningMayLockOut Warning = iota + 1
)

// Decision is a driver's judgement of one step.
type Decision struct {
	Action Action
	Effect Effect
	// Downtime is how long what the resource serves is unavailable; zero
	// means none. Drivers must not understate it.
	Downtime time.Duration
	// Destroys is true when the step removes data the spec can't bring
	// back.
	Destroys bool
	Warnings []Warning
}

// Step is what applying a change does to one resource.
type Step struct {
	Ref Ref
	Decision
	// Changes are the fields that differ between what the server has and
	// what is desired.
	Changes []Change
}

// Driver manages one kind of resource on a server. Drivers run where the
// resource is: on the agent.
type Driver interface {
	// Kind is the kind this driver manages.
	Kind() Kind
	// New returns an empty spec of the kind's message type.
	New() proto.Message
	// Validate checks a spec and fills in its defaults in place, so specs
	// that mean the same thing compare equal.
	Validate(spec proto.Message) error
	// Observe returns the resource as it is on the server, as a spec, or
	// nil if it doesn't exist. It holds intent only: nothing the server
	// adds, such as IDs or start times, or every check would see drift.
	Observe(ctx context.Context, name string) (proto.Message, error)
	// Plan judges moving the resource from actual to desired, which differ
	// in changes. A nil actual means create, a nil desired delete.
	Plan(actual, desired proto.Message, changes []Change) (Decision, error)
	// Apply carries out a step; desired is nil for a deletion.
	Apply(ctx context.Context, step Step, desired proto.Message) error
	// Verify confirms the resource is healthy, once the caller has checked
	// it matches desired. It isn't called after a deletion.
	Verify(ctx context.Context, name string, desired proto.Message) error
}

// Registry holds the drivers for each kind.
type Registry struct {
	drivers map[Kind]Driver
}

// NewRegistry returns a registry of drivers.
func NewRegistry(drivers ...Driver) *Registry {
	r := &Registry{drivers: make(map[Kind]Driver, len(drivers))}
	for _, d := range drivers {
		r.drivers[d.Kind()] = d
	}
	return r
}

// Driver returns the driver for kind.
func (r *Registry) Driver(kind Kind) (Driver, error) {
	d, ok := r.drivers[kind]
	if !ok {
		return nil, fmt.Errorf("dockwell doesn't manage resources of kind %q", kind)
	}
	return d, nil
}

// Validate checks spec against its kind: the right message type, valid
// values, and defaults filled in.
func (r *Registry) Validate(kind Kind, spec proto.Message) error {
	d, err := r.Driver(kind)
	if err != nil {
		return err
	}
	want := d.New().ProtoReflect().Descriptor().FullName()
	if got := spec.ProtoReflect().Descriptor().FullName(); got != want {
		return fmt.Errorf("a %s spec must be a %s, not a %s", kind, want, got)
	}
	return d.Validate(spec)
}

// Decode reads a stored spec of kind.
func (r *Registry) Decode(kind Kind, b []byte) (proto.Message, error) {
	d, err := r.Driver(kind)
	if err != nil {
		return nil, err
	}
	spec := d.New()
	if err := proto.Unmarshal(b, spec); err != nil {
		return nil, fmt.Errorf("can't read a stored %s spec: %w", kind, err)
	}
	return spec, nil
}

// Encode writes a spec deterministically, so equal specs encode equal.
func Encode(spec proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(spec)
}
