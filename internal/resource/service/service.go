// SPDX-License-Identifier: AGPL-3.0-only

// Package service is the "service" kind: anything that runs in an
// environment, placed on servers. Each revision is deployed on the
// servers it is placed on, and the agent on each runs it.
//
// For now a service is an app from an image, which the driver runs as one
// container through the container driver's Runtime. Compose stacks and
// the real Docker backend replace that in M3 (#26) behind the same kind.
package service

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Kind is the service kind.
const Kind resource.Kind = "service"

// Driver manages services. On the control plane, where it only reads and
// checks specs, its Containers needs no Runtime.
type Driver struct {
	Containers container.Driver
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "service".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the environment.
func (*Driver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "svc".
func (*Driver) IDPrefix() string { return id.Service }

// New returns an empty Service.
func (*Driver) New() proto.Message { return &resourcev1.Service{} }

// CheckName requires a DNS label, such as web or api-v2, since a service's
// name goes in its URL and in the name of its container.
func (*Driver) CheckName(name string) error { return resource.CheckLabel("service", name, "api-v2") }

// Validate requires something to run, and checks it as a container.
func (d *Driver) Validate(spec proto.Message) error {
	app := spec.(*resourcev1.Service).GetApp()
	if app == nil {
		return errors.New("a service needs something to run: an app from an image, such as nginx:1.27")
	}
	return d.Containers.Validate(app)
}

// ContainerName is the container a service runs as on a server: its name
// and environment, as <name>.env_<ID>, so services of one name in two
// environments can share a server. Containers run by hand can't take names
// of that form (container.Driver.CheckName), so neither manages the other's.
func ContainerName(ref resource.Ref) string { return ref.Name + "." + ref.Scope }

func (d *Driver) containerRef(ref resource.Ref) resource.Ref {
	return resource.Ref{Kind: container.Kind, Scope: ref.Scope, Name: ContainerName(ref)}
}

// Observe reads the service's container.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	c, err := d.Containers.Observe(ctx, d.containerRef(ref))
	if err != nil || c == nil {
		return nil, err
	}
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: c.(*resourcev1.Container)}}, nil
}

// Plan judges the step as the container driver does for the app.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	app := func(m proto.Message) proto.Message {
		if m == nil {
			return nil
		}
		return m.(*resourcev1.Service).GetApp()
	}
	inner := make([]resource.Change, len(changes))
	for i, c := range changes {
		c.Path = strings.TrimPrefix(c.Path, "app.")
		inner[i] = c
	}
	return d.Containers.Plan(app(actual), app(desired), inner)
}

// Apply runs, updates or removes the service's container.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	step.Ref = d.containerRef(step.Ref)
	var app proto.Message
	if desired != nil {
		app = desired.(*resourcev1.Service).GetApp()
	}
	return d.Containers.Apply(ctx, step, app)
}

// Verify checks the service's container is healthy.
func (d *Driver) Verify(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	return d.Containers.Verify(ctx, d.containerRef(ref), desired)
}
