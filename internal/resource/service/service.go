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
//
// A service uses its environment's variables and secrets by reference
// (resource.Binder): the revision store pins each to its revision, and the
// driver builds the container's environment from the values the agent
// stream sent for the step, in memory (package binding).
package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// Kind is the service kind.
const Kind resource.Kind = "service"

// Driver manages services. On the control plane, where it only reads and
// checks specs, its Containers needs no Runtime.
type Driver struct {
	Containers container.Driver
}

var (
	_ resource.Driver   = (*Driver)(nil)
	_ resource.Binder   = (*Driver)(nil)
	_ resource.Rebinder = (*Driver)(nil)
)

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

// Validate requires something to run, and checks it as a container. It
// checks what the service uses, writes an empty name as the name the
// service sees, and drops a revision for a name it doesn't use, so specs
// that mean the same compare equal.
func (d *Driver) Validate(spec proto.Message) error {
	svc := spec.(*resourcev1.Service)
	app := svc.GetApp()
	if app == nil {
		return errors.New("a service needs something to run: an app from an image, such as nginx:1.27")
	}
	if _, ok := app.GetLabels()[binding.LabelKey]; ok {
		return fmt.Errorf("the label %s is Dockwell's own, which records what a service uses. Use another", binding.LabelKey)
	}
	for _, m := range []struct {
		what  string
		names map[string]string
	}{{"variable", svc.GetVariables()}, {"secret", svc.GetSecrets()}} {
		for as, name := range m.names {
			if err := variable.CheckName("environment variable", as); err != nil {
				return err
			}
			if name == "" {
				m.names[as] = as
			} else if err := variable.CheckName(m.what, name); err != nil {
				return err
			}
			if _, ok := app.GetEnv()[as]; ok {
				return fmt.Errorf("%s is set in the app's environment and from a %s too. Keep one", as, m.what)
			}
		}
	}
	for as := range svc.GetSecrets() {
		if _, ok := svc.GetVariables()[as]; ok {
			return fmt.Errorf("%s comes from a variable and a secret. Keep one", as)
		}
	}
	maps.DeleteFunc(svc.Revisions, func(as string, rev int64) bool {
		_, v := svc.GetVariables()[as]
		_, s := svc.GetSecrets()[as]
		return !v && !s || rev <= 0
	})
	if len(svc.Revisions) == 0 {
		svc.Revisions = nil
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

// ReferenceKinds are volumes and networks.
func (*Driver) ReferenceKinds() []resource.Kind { return []resource.Kind{volume.Kind, network.Kind} }

// Answers are the names the service answers to on each of its networks: its own
// and any others its app takes.
func (*Driver) Answers(ref resource.Ref, spec proto.Message) []resource.Answer {
	return container.Answers(ref.Name, spec.(*resourcev1.Service).GetApp())
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

// Uses lists the variables and secrets the service uses.
func (*Driver) Uses(spec proto.Message) []resource.Use {
	return binding.Uses(spec.(*resourcev1.Service))
}

// Pin sets the revision of each variable and secret the service uses.
func (*Driver) Pin(spec proto.Message, revisions map[string]int) {
	svc := spec.(*resourcev1.Service)
	svc.Revisions = nil
	for _, u := range binding.Uses(svc) {
		if rev := revisions[u.As]; rev > 0 {
			if svc.Revisions == nil {
				svc.Revisions = map[string]int64{}
			}
			svc.Revisions[u.As] = int64(rev)
		}
	}
}

// Rebind moves each use of the variable or secret name, of kind from, to
// the one of kind to, under the same name the service sees it as.
func (*Driver) Rebind(spec proto.Message, name string, from, to resource.Kind) {
	svc := spec.(*resourcev1.Service)
	src, dst := usesOfKind(svc, from), usesOfKind(svc, to)
	if from == to || !known(from) || !known(to) {
		return
	}
	for as, n := range src {
		if n == name || n == "" && as == name {
			delete(src, as)
			if dst == nil {
				dst = map[string]string{}
			}
			dst[as] = name
		}
	}
	if len(src) == 0 {
		src = nil
	}
	setUses(svc, from, src)
	setUses(svc, to, dst)
}

func known(kind resource.Kind) bool {
	return kind == variable.VariableKind || kind == variable.SecretKind
}

// usesOfKind is the map of a service's uses of kind.
func usesOfKind(svc *resourcev1.Service, kind resource.Kind) map[string]string {
	if kind == variable.SecretKind {
		return svc.GetSecrets()
	}
	return svc.GetVariables()
}

// setUses sets the map of a service's uses of kind.
func setUses(svc *resourcev1.Service, kind resource.Kind, uses map[string]string) {
	if kind == variable.SecretKind {
		svc.Secrets = uses
	} else {
		svc.Variables = uses
	}
}

// Observe reads the service's container, and what it uses from the
// container's label. The values themselves never leave the driver.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	c, err := d.Containers.ObserveAt(ctx, Target(ref))
	if err != nil || c == nil {
		return nil, err
	}
	return binding.Restore(c.(*resourcev1.Container))
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

// CheckPlan refuses a port something else on the server holds, or another service
// in the plan wants, for a service about to start.
func (d *Driver) CheckPlan(ctx context.Context, want resource.Desired, all []resource.Desired) error {
	var planned []container.Planned
	for _, o := range all {
		switch o.Ref.Kind {
		case Kind:
			var c *resourcev1.Container
			if s, ok := o.Spec.(*resourcev1.Service); ok {
				c = s.GetApp()
			}
			planned = append(planned, container.Planned{Name: Target(o.Ref).Name, Words: o.Ref.Name, Spec: c})
		case container.Kind:
			c, _ := o.Spec.(*resourcev1.Container)
			planned = append(planned, container.Planned{Name: container.HostTarget(o.Ref).Name, Words: o.Ref.Name, Spec: c})
		}
	}
	return d.Containers.CheckPortsAt(ctx, Target(want.Ref), want.Spec.(*resourcev1.Service).GetApp(), planned)
}

// ImageOf is the image a service's app runs, as it is written.
func (*Driver) ImageOf(spec proto.Message) string {
	return spec.(*resourcev1.Service).GetApp().GetImage()
}

// SetImage changes the image a service's app runs.
func (*Driver) SetImage(spec proto.Message, image string) {
	if app := spec.(*resourcev1.Service).GetApp(); app != nil {
		app.Image = image
	}
}

// Check checks the app on its server, as the container driver does.
func (d *Driver) Check(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	return d.Containers.CheckAt(ctx, Target(ref), desired.(*resourcev1.Service).GetApp())
}

// HostAccess is what the app itself reaches on its server: the server paths it
// mounts, the networks it joins that aren't its environment's, the server's own
// network, and what it runs with beyond what Docker gives a container. The
// engine adds what its volumes and networks reach (References).
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

// Apply runs, updates or removes the service's container. Its
// environment holds the values of what the service uses, which the call
// brought (resource.WithValues), and a label records what they came from.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if desired == nil {
		return d.Containers.ApplyAt(ctx, Target(step.Ref), step, nil)
	}
	svc := desired.(*resourcev1.Service)
	env, err := binding.Environment(ctx, svc)
	if err != nil {
		return err
	}
	app := binding.App(svc, env)
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
