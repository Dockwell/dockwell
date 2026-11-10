// SPDX-License-Identifier: AGPL-3.0-only

// Package host is a server's host, kind "server.host": what Dockwell does to the
// server itself. Its spec is empty; installing the server's updates is a flagged
// edit of it, which makes no revision (ADR 0064).
package host

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is a server's host.
const Kind resource.Kind = "server.host"

// Name is the name a server's one host goes by.
const Name = "host"

// Backend installs a server's updates: the agent's package manager and init
// system. Nil on the control plane, which plans through the agent.
type Backend interface {
	// Plan simulates installing the updates waiting, the security fixes or all.
	Plan(ctx context.Context, all bool) (*resource.Install, error)
	// Left are the planned packages not yet at their planned version, and
	// whether an install for the change still runs.
	Left(ctx context.Context, change string, planned []resource.InstallPackage) (left []resource.InstallPackage, running bool, err error)
	// Install installs the planned packages for the change, saying each line.
	Install(ctx context.Context, change string, planned []resource.InstallPackage, say func(string)) error
}

// Driver is a server's host.
type Driver struct {
	Backend Backend
}

var (
	_ resource.Driver    = (*Driver)(nil)
	_ resource.Installer = (*Driver)(nil)
)

// Kind returns "server.host".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the server.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty ServerHost.
func (*Driver) New() proto.Message { return &resourcev1.ServerHost{} }

// CheckName takes the one name a server's host goes by.
func (*Driver) CheckName(name string) error {
	if name != Name {
		return fmt.Errorf("a server has one host, named %q, not %q", Name, name)
	}
	return nil
}

// Validate has nothing to check: the spec is empty.
func (*Driver) Validate(proto.Message) error { return nil }

// Observe reads a host that's always there: the server itself.
func (*Driver) Observe(context.Context, resource.Ref) (proto.Message, error) {
	return &resourcev1.ServerHost{}, nil
}

// Plan has nothing to change: the spec is empty, and a host is never deleted
// from the server, only from what Dockwell keeps.
func (*Driver) Plan(_, _ proto.Message, _ []resource.Change) (resource.Decision, error) {
	return resource.Decision{Action: resource.ActionNone}, nil
}

// Apply has nothing to do.
func (*Driver) Apply(context.Context, resource.Step, proto.Message) error { return nil }

// Verify has nothing to check.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

var errNoBackend = errors.New("this agent can't install updates")

// JudgeInstall plans an install: at planning, the exact command simulated; on a
// resume, what's left of the planned packages, nothing when every one is at its
// version and no install for the change runs.
func (d *Driver) JudgeInstall(ctx context.Context, _ resource.Ref, ask resource.InstallAsked) (resource.Decision, bool, error) {
	if d.Backend == nil {
		return resource.Decision{}, false, errNoBackend
	}
	dec := resource.Decision{Action: resource.ActionUpdate, Effect: resource.EffectNone}
	if len(ask.Packages) == 0 {
		in, err := d.Backend.Plan(ctx, ask.All)
		if err != nil {
			return resource.Decision{}, false, err
		}
		if len(in.Packages) == 0 {
			return resource.Decision{}, false, nil
		}
		dec.Install = in
		return dec, true, nil
	}
	left, running, err := d.Backend.Left(ctx, ask.Change, ask.Packages)
	if err != nil {
		return resource.Decision{}, false, err
	}
	if len(left) == 0 && !running {
		return resource.Decision{}, false, nil
	}
	// The step carries the whole plan: what's done reads done again as it applies.
	// What it needs after is each package's, as the plan said: an apply plans
	// again, and its fingerprint has to read as the plan's did.
	dec.Install = &resource.Install{Packages: ask.Packages}
	for _, p := range ask.Packages {
		dec.Install.Reboot = dec.Install.Reboot || p.Reboot
		dec.Install.DockerRestarts = dec.Install.DockerRestarts || p.Docker
	}
	return dec, true, nil
}

// ApplyInstall installs the step's packages, for the change applying it.
func (d *Driver) ApplyInstall(ctx context.Context, step resource.Step) error {
	if d.Backend == nil {
		return errNoBackend
	}
	change := resource.DeployOf(ctx).Change
	if change == "" {
		return errors.New("an install needs its change, which names its unit")
	}
	return d.Backend.Install(ctx, change, step.Install.Packages, func(line string) { resource.Progress(ctx, line) })
}
