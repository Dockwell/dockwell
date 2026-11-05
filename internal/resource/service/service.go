// SPDX-License-Identifier: AGPL-3.0-only

// Package service is the "service" kind: anything that runs in an
// environment, placed on servers. Each revision is deployed on the
// servers it is placed on, and the agent on each runs it.
//
// For now a service is an app from an image, which the driver runs as one
// container through the container driver's Runtime, named and labelled as
// Docker Compose names and labels a service's first container. Its
// environment is its Compose project. A Compose file becomes services,
// with volumes and networks of their own kinds.
package service

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/volume"
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
	return container.Validate(app)
}

// References are the volumes the service mounts and the networks it
// joins, in its environment.
func (*Driver) References(spec proto.Message) []resource.Reference {
	app := spec.(*resourcev1.Service).GetApp()
	var out []resource.Reference
	for _, m := range app.GetMounts() {
		if v := m.GetVolume(); v != "" {
			out = append(out, resource.Reference{Kind: volume.Kind, Name: v, Verb: "mount", ReadOnly: m.GetReadOnly()})
		}
	}
	for _, n := range app.GetNetworks() {
		out = append(out, resource.Reference{Kind: network.Kind, Name: n, Verb: "join"})
	}
	return out
}

// Target is where a service runs on a server: in its environment's
// Compose project, as that project's service of the same name, in a
// container named as Compose names a service's first, such as
// env_01kat…-web-1. Containers run by hand can't take names of that form
// (container.Driver.CheckName), so neither manages the other's.
func Target(ref resource.Ref) container.Target {
	p := docker.Project(ref.Scope)
	return container.Target{Name: docker.ContainerName(p, ref.Name), Project: p, Service: ref.Name, Scoped: true}
}

// ContainerName is the name of a service's container on a server.
func ContainerName(ref resource.Ref) string { return Target(ref).Name }

// Observe reads the service's container.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	c, err := d.Containers.ObserveAt(ctx, Target(ref))
	if err != nil || c == nil {
		return nil, err
	}
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: c.(*resourcev1.Container)}}, nil
}

// Plan judges the step as the container driver does for the app.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.Containers.Plan(app(actual), app(desired), inner(changes))
}

// Judge judges the step as the container driver does for the app, with
// what its server has.
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.Containers.JudgeAt(ctx, Target(ref), app(actual), app(desired), inner(changes))
}

// Check checks the app on its server, as the container driver does.
func (d *Driver) Check(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	return d.Containers.CheckAt(ctx, Target(ref), desired.(*resourcev1.Service).GetApp())
}

// HostAccess is what the app itself reaches on its server: the server
// paths it mounts. Its networks are its environment's, never the server's
// own. The engine adds what its volumes and networks reach (References).
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	return container.ServiceAccess(spec.(*resourcev1.Service).GetApp())
}

// Normalize orders the app, as Validate would.
func (*Driver) Normalize(spec proto.Message) {
	container.Normalize(spec.(*resourcev1.Service).GetApp())
}

// app is a service's app, or nil.
func app(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	return m.(*resourcev1.Service).GetApp()
}

// inner are changes to a service as changes to its app.
func inner(changes []resource.Change) []resource.Change {
	out := make([]resource.Change, len(changes))
	for i, c := range changes {
		c.Path = strings.TrimPrefix(c.Path, "app.")
		out[i] = c
	}
	return out
}

// Apply runs, updates or removes the service's container.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	var app proto.Message
	if desired != nil {
		app = desired.(*resourcev1.Service).GetApp()
	}
	return d.Containers.ApplyAt(ctx, Target(step.Ref), step, app)
}

// Verify checks the service's container is healthy.
func (d *Driver) Verify(ctx context.Context, ref resource.Ref, _ proto.Message) error {
	return d.Containers.VerifyAt(ctx, Target(ref))
}

// Ready says why services can't run now, as their containers can't.
func (d *Driver) Ready(ctx context.Context) error { return d.Containers.Ready(ctx) }

// ReadyKey is the containers' runtime's.
func (d *Driver) ReadyKey() any { return d.Containers.ReadyKey() }
