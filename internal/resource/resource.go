// SPDX-License-Identifier: AGPL-3.0-only

// Package resource is the model every managed thing shares: a kind, the
// scope it lives in, a name, and a spec. A kind's level says what its
// scope is: a server for host-level kinds, an environment for services, a
// project for environments, and the organization for projects. Each kind
// that runs on servers has a driver that observes, plans, applies and
// verifies it there. The revision package builds history, plans, undo and
// drift on top, the same way for every kind. See
// docs/adr/0009-resources-revisions-and-plans.md and
// docs/adr/0014-object-model-and-store.md.
package resource

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
)

// Kind names a type of resource, such as "container" or "firewall.rule".
type Kind string

// Ref names one resource.
type Ref struct {
	Kind Kind
	// Scope is the ID of what the resource lives in, which its kind's level
	// says: a server (srv_…), an environment (env_…), a project (prj_…) or
	// the organization (org_…).
	Scope string
	Name  string // unique among resources of the kind in the scope
}

func (r Ref) String() string { return fmt.Sprintf("%s %s in %s", r.Kind, r.Name, r.Scope) }

// Level says what a kind's resources live in, and so where applying them
// runs.
type Level int

// Levels.
const (
	// LevelServer kinds are host-level: each lives on one server and
	// applies there, such as a firewall rule or a container run by hand.
	LevelServer Level = iota + 1
	// LevelEnvironment kinds live in an environment and are placed on
	// servers, where each revision is deployed, such as a service.
	LevelEnvironment
	// LevelProject kinds live in a project, and apply on the control plane
	// alone, such as an environment.
	LevelProject
	// LevelOrganization kinds live in the organization, and apply on the
	// control plane alone, such as a project.
	LevelOrganization
)

var levelNames = [...]string{"", "server", "environment", "project", "organization"}

func (l Level) String() string {
	if l > 0 && int(l) < len(levelNames) {
		return levelNames[l]
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// RunsOnServers reports whether the level's resources are applied on
// servers, rather than on the control plane alone.
func (l Level) RunsOnServers() bool { return l == LevelServer || l == LevelEnvironment }

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

// Type is what the control plane knows of a kind: its spec's message, its
// level, and how to check a spec.
type Type interface {
	// Kind is the kind this type describes.
	Kind() Kind
	// Level says what the kind's resources live in.
	Level() Level
	// New returns an empty spec of the kind's message type.
	New() proto.Message
	// Validate checks a spec and fills in its defaults in place, so specs
	// that mean the same thing compare equal.
	Validate(spec proto.Message) error
}

// IDPrefixer is a Type whose resources' IDs say what they are, such as
// "env" for environments. Others get "res".
type IDPrefixer interface {
	IDPrefix() string
}

// NameChecker is a Type that takes only some names, such as names that fit
// in a URL. Kinds without one take any name that isn't blank.
type NameChecker interface {
	// CheckName says why a name won't do, with an example of one that
	// would.
	CheckName(name string) error
}

var labelRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckLabel requires a name that is a DNS label, so it fits in URLs, in
// hostnames such as a service's URL, and on the command line: 1 to 63
// lowercase letters, digits and dashes, starting with a letter and ending
// with a letter or digit. Projects, environments and services take such
// names. what names the kind, and example is a name that would do.
func CheckLabel(what, name, example string) error {
	if !labelRE.MatchString(name) {
		return fmt.Errorf("%q isn't a valid %s name: use up to 63 lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, such as %s",
			name, what, example)
	}
	return nil
}

// Driver manages one kind of resource on a server. Drivers run where the
// resource is: on the agent. Kinds whose level doesn't run on servers have
// a Type and no driver.
type Driver interface {
	Type
	// Observe returns the resource as it is on the server, as a spec, or
	// nil if it doesn't exist. It holds intent only: nothing the server
	// adds, such as IDs or start times, or every check would see drift.
	Observe(ctx context.Context, ref Ref) (proto.Message, error)
	// Plan judges moving the resource from actual to desired, which differ
	// in changes. A nil actual means create, a nil desired delete.
	Plan(actual, desired proto.Message, changes []Change) (Decision, error)
	// Apply carries out a step; desired is nil for a deletion.
	Apply(ctx context.Context, step Step, desired proto.Message) error
	// Verify confirms the resource is healthy, once the caller has checked
	// it matches desired. It isn't called after a deletion.
	Verify(ctx context.Context, ref Ref, desired proto.Message) error
}

// Registry holds the type, and the driver where there is one, of each
// kind.
type Registry struct {
	types map[Kind]Type
}

// NewRegistry returns a registry of types. Drivers are types too.
func NewRegistry(types ...Type) *Registry {
	r := &Registry{types: make(map[Kind]Type, len(types))}
	for _, t := range types {
		r.types[t.Kind()] = t
	}
	return r
}

// Readier is a driver that can't always run, such as one that needs
// Docker. An agent offers its kind only while Ready says it can.
type Readier interface {
	// Ready says why the driver can't run now, for people, with how to
	// fix it, such as "Docker isn't running. Start it with …".
	Ready(ctx context.Context) error
}

// Ready lists the kinds whose drivers can run now, sorted, and why each
// other kind with a driver can't.
func (r *Registry) Ready(ctx context.Context) (ready []Kind, unavailable map[Kind]string) {
	for _, k := range r.Kinds() {
		d, _ := r.types[k].(Readier)
		if d == nil {
			ready = append(ready, k)
			continue
		}
		if err := d.Ready(ctx); err != nil {
			if unavailable == nil {
				unavailable = map[Kind]string{}
			}
			unavailable[k] = err.Error()
			continue
		}
		ready = append(ready, k)
	}
	return ready, unavailable
}

// With returns a registry with more types, which replace any of the same
// kind.
func (r *Registry) With(types ...Type) *Registry {
	all := make([]Type, 0, len(r.types)+len(types))
	for _, t := range r.types {
		all = append(all, t)
	}
	return NewRegistry(append(all, types...)...)
}

// Type returns the type of kind.
func (r *Registry) Type(kind Kind) (Type, error) {
	t, ok := r.types[kind]
	if !ok {
		return nil, fmt.Errorf("dockwell doesn't manage resources of kind %q", kind)
	}
	return t, nil
}

// ErrNoDriver means a server's agent has no driver for a kind, so it can't
// plan or apply resources of that kind.
var ErrNoDriver = errors.New("no driver for this kind")

// Kinds lists the kinds the registry has drivers for, sorted.
func (r *Registry) Kinds() []Kind {
	var out []Kind
	for k, t := range r.types {
		if _, ok := t.(Driver); ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// Driver returns the driver for kind.
func (r *Registry) Driver(kind Kind) (Driver, error) {
	t, err := r.Type(kind)
	if err != nil {
		return nil, err
	}
	d, ok := t.(Driver)
	if !ok {
		return nil, fmt.Errorf("%s resources apply on the control plane, not on a server", kind)
	}
	return d, nil
}

// KindOf returns the kind whose spec is spec's message type.
func (r *Registry) KindOf(spec proto.Message) (Kind, bool) {
	name := spec.ProtoReflect().Descriptor().FullName()
	for k, t := range r.types {
		if t.New().ProtoReflect().Descriptor().FullName() == name {
			return k, true
		}
	}
	return "", false
}

// Validate checks spec against its kind: the right message type, valid
// values, and defaults filled in.
func (r *Registry) Validate(kind Kind, spec proto.Message) error {
	d, err := r.Type(kind)
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
	d, err := r.Type(kind)
	if err != nil {
		return nil, err
	}
	spec := d.New()
	if err := proto.Unmarshal(b, spec); err != nil {
		return nil, fmt.Errorf("can't read a stored %s spec: %w", kind, err)
	}
	return spec, nil
}

// Encode writes a spec deterministically, so equal specs encode equal. An
// empty spec encodes to no bytes, but never to nil, which the store keeps
// for a deletion.
func Encode(spec proto.Message) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err == nil && b == nil {
		b = []byte{}
	}
	return b, err
}
