// SPDX-License-Identifier: AGPL-3.0-only

// Package containertest is an in-memory container runtime for tests, which
// behaves like Docker where the driver can tell. It is never linked into
// either build.
package containertest

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Fake is an in-memory Runtime that behaves like Docker where the driver
// can tell: containers it runs are there, with what was asked for. Images
// listed in Broken start but fail their health check.
type Fake struct {
	mu         sync.Mutex
	containers map[string]*resourcev1.Container
	// Broken images start but never become healthy.
	Broken map[string]bool
	// Stuck containers, by name, can't be removed.
	Stuck map[string]bool
	// Runs counts containers started, to see what was recreated.
	Runs int
	// restarted says which restart each container last had.
	restarted map[string]string
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{containers: map[string]*resourcev1.Container{}, Broken: map[string]bool{}, Stuck: map[string]bool{}, restarted: map[string]string{}}
}

var _ container.Runtime = (*Fake)(nil)

// key is what a Fake keeps a container under: its name, or for a service's, the
// name Compose gives its first, whatever Dockwell names it, as Docker's runtime
// finds a service's container by its labels (ADR 0035). Edit takes it too.
func key(t container.Target) string {
	if t.Scoped {
		return docker.ContainerName(t.Project, t.Service)
	}
	return t.Name
}

// Inspect returns a copy, as Docker would.
func (f *Fake) Inspect(_ context.Context, t container.Target) (*resourcev1.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[key(t)]
	if !ok {
		return nil, nil
	}
	return proto.Clone(c).(*resourcev1.Container), nil
}

// ImageAt is the image the container at t runs: an ID made up of its reference,
// as a Fake keeps no images.
func (f *Fake) ImageAt(_ context.Context, t container.Target) (resource.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[key(t)]
	if !ok {
		return resource.Image{}, nil
	}
	return resource.Image{ID: "sha256:fake-" + c.GetImage()}, nil
}

// Run replaces any container with the name.
func (f *Fake) Run(_ context.Context, t container.Target, spec *resourcev1.Container, dec resource.Decision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[key(t)] = proto.Clone(spec).(*resourcev1.Container)
	f.Runs++
	if dec.Restart != "" {
		f.restarted[key(t)] = dec.Restart
	}
	return nil
}

// Left has a restart to do, unless it has been done: a Fake has nothing left
// over otherwise.
func (f *Fake) Left(_ context.Context, t container.Target, _ *resourcev1.Container, restart string) (resource.Decision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if restart == "" || f.restarted[key(t)] == restart {
		return resource.Decision{}, false, nil
	}
	return resource.Decision{Action: resource.ActionReplace, Effect: resource.EffectRoll}, true, nil
}

// Update changes the restart policy and the limits in place.
func (f *Fake) Update(_ context.Context, t container.Target, spec *resourcev1.Container) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[key(t)]
	if !ok {
		return fmt.Errorf("no container %s", t.Name)
	}
	c.Restart, c.RestartRetries = spec.GetRestart(), spec.GetRestartRetries()
	c.Resources = proto.CloneOf(spec.GetResources())
	return nil
}

// Remove deletes the container.
func (f *Fake) Remove(_ context.Context, t container.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Stuck[t.Name] {
		return fmt.Errorf("%s can't be removed", t.Name)
	}
	delete(f.containers, key(t))
	return nil
}

// Healthy fails for broken images.
func (f *Fake) Healthy(_ context.Context, t container.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[key(t)]
	switch {
	case !ok:
		return fmt.Errorf("no container %s", t.Name)
	case f.Broken[c.GetImage()]:
		return fmt.Errorf("%s exited with code 1 on start", c.GetImage())
	}
	return nil
}

// Edit changes a container behind the driver's back, as someone running
// docker by hand would.
func (f *Fake) Edit(name string, edit func(*resourcev1.Container)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		edit(c)
	}
}
