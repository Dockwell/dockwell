// SPDX-License-Identifier: AGPL-3.0-only

// Package container is the driver for Docker containers, the "container"
// kind. It talks to Docker through Runtime, which the agent implements
// with the Docker Engine API and tests with Fake.
package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the container kind.
const Kind resource.Kind = "container"

// Runtime is the part of Docker the driver needs.
type Runtime interface {
	// Inspect returns the container's spec as Docker has it, or nil if
	// there is no such container.
	Inspect(ctx context.Context, name string) (*resourcev1.Container, error)
	// Run creates and starts a container, replacing any with the name.
	Run(ctx context.Context, name string, spec *resourcev1.Container) error
	// SetRestart changes the restart policy of a running container.
	SetRestart(ctx context.Context, name string, policy resourcev1.RestartPolicy) error
	// Remove stops and removes the container.
	Remove(ctx context.Context, name string) error
	// Healthy reports whether the container is running and passes its
	// health check.
	Healthy(ctx context.Context, name string) error
}

// Driver manages containers.
type Driver struct {
	Runtime Runtime
	// RecreateDowntime is how long a recreated container is down: stopping
	// the old one and starting the new. Rolling deploys with two replicas
	// avoid it; they come with services.
	RecreateDowntime time.Duration
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "container".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the server: a container run by hand is host-level. Services
// run containers too, as part of their environment.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty Container.
func (*Driver) New() proto.Message { return &resourcev1.Container{} }

// Validate requires an image and valid ports, and fills in defaults.
func (*Driver) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.Container)
	if strings.TrimSpace(c.GetImage()) == "" {
		return errors.New("a container needs an image, such as nginx:1.27")
	}
	if c.Restart == resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED
	}
	seen := map[string]bool{}
	for _, p := range c.GetPorts() {
		if p.Protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED {
			p.Protocol = resourcev1.Protocol_PROTOCOL_TCP
		}
		if p.GetHostPort() == 0 || p.GetHostPort() > 65535 || p.GetContainerPort() == 0 || p.GetContainerPort() > 65535 {
			return fmt.Errorf("ports run from 1 to 65535, not %d:%d", p.GetHostPort(), p.GetContainerPort())
		}
		key := fmt.Sprintf("%d/%s", p.GetHostPort(), p.GetProtocol())
		if seen[key] {
			return fmt.Errorf("server port %d is published twice; give each mapping its own port", p.GetHostPort())
		}
		seen[key] = true
	}
	// Order doesn't change what ports mean, so it mustn't show as a change.
	slices.SortFunc(c.Ports, func(a, b *resourcev1.PortMapping) int {
		if a.GetHostPort() != b.GetHostPort() {
			return int(a.GetHostPort()) - int(b.GetHostPort())
		}
		return int(a.GetProtocol()) - int(b.GetProtocol())
	})
	return nil
}

// Observe asks Docker.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	c, err := d.Runtime.Inspect(ctx, ref.Name)
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

// Plan recreates the container for any change Docker can't make in place,
// which is all of them except the restart policy.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete}, nil
	}
	// Only a change that is nothing but the restart policy happens in place.
	inPlace := len(changes) > 0
	for _, c := range changes {
		if c.Path != "restart" {
			inPlace = false
			break
		}
	}
	if inPlace {
		return resource.Decision{Action: resource.ActionUpdate}, nil
	}
	return resource.Decision{Action: resource.ActionReplace, Effect: resource.EffectRecreate, Downtime: d.RecreateDowntime}, nil
}

// Apply runs, updates or removes the container.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	name := step.Ref.Name
	switch step.Action {
	case resource.ActionCreate, resource.ActionReplace:
		return d.Runtime.Run(ctx, name, desired.(*resourcev1.Container))
	case resource.ActionUpdate:
		return d.Runtime.SetRestart(ctx, name, desired.(*resourcev1.Container).GetRestart())
	case resource.ActionDelete:
		return d.Runtime.Remove(ctx, name)
	default:
		return nil
	}
}

// Verify checks the container is running and passes its health check.
func (d *Driver) Verify(ctx context.Context, ref resource.Ref, _ proto.Message) error {
	return d.Runtime.Healthy(ctx, ref.Name)
}
