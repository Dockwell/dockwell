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
	// Runs counts containers started, to see what was recreated.
	Runs int
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{containers: map[string]*resourcev1.Container{}, Broken: map[string]bool{}}
}

var _ container.Runtime = (*Fake)(nil)

// Inspect returns a copy, as Docker would.
func (f *Fake) Inspect(_ context.Context, t container.Target) (*resourcev1.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[t.Name]
	if !ok {
		return nil, nil
	}
	return proto.Clone(c).(*resourcev1.Container), nil
}

// Run replaces any container with the name.
func (f *Fake) Run(_ context.Context, t container.Target, spec *resourcev1.Container, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[t.Name] = proto.Clone(spec).(*resourcev1.Container)
	f.Runs++
	return nil
}

// SetRestart updates the policy in place.
func (f *Fake) SetRestart(_ context.Context, t container.Target, policy resourcev1.RestartPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[t.Name]
	if !ok {
		return fmt.Errorf("no container %s", t.Name)
	}
	c.Restart = policy
	return nil
}

// Remove deletes the container.
func (f *Fake) Remove(_ context.Context, t container.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, t.Name)
	return nil
}

// Healthy fails for broken images.
func (f *Fake) Healthy(_ context.Context, t container.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[t.Name]
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
