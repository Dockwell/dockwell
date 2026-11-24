// SPDX-License-Identifier: AGPL-3.0-only

// Package sshconfig defines a server's managed OpenSSH settings.
package sshconfig

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the server's single SSH settings resource.
const Kind resource.Kind = "ssh.config"

// Backend changes SSH within the server's guarded access transaction.
type Backend interface {
	Ready(context.Context) error
	SSHConfig(context.Context) *resourcev1.SshConfig
	CheckSSH(context.Context, *resourcev1.SshConfig) (*dockwellv1.SshPortMove, error)
	PutSSH(context.Context, *resourcev1.SshConfig, *dockwellv1.SshPortMove) error
	Wait(context.Context) error
}

// Driver manages the server's Dockwell-owned SSH drop-in.
type Driver struct{ Backend Backend }

// Kind names the SSH settings resource.
func (*Driver) Kind() resource.Kind { return Kind }

// Level places SSH settings on the server.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty SSH settings message.
func (*Driver) New() proto.Message { return &resourcev1.SshConfig{} }

// GuardsAccess makes the revision hold the server's access claim.
func (*Driver) GuardsAccess() {}

// Ready checks whether the server can apply guarded changes.
func (d *Driver) Ready(ctx context.Context) error { return d.Backend.Ready(ctx) }

// CheckName accepts the server's single settings resource.
func (*Driver) CheckName(name string) error {
	if name != "default" {
		return errors.New("a server has one SSH configuration, named default")
	}
	return nil
}

// Validate checks values before they can be rendered in the drop-in.
func (*Driver) Validate(spec proto.Message) error {
	s := spec.(*resourcev1.SshConfig)
	switch s.GetRootLogin() {
	case "", "yes", "prohibit-password", "no":
	default:
		return errors.New("choose yes, keys only, or no for root login")
	}
	if s.GetPort() > 65535 {
		return errors.New("choose a port between 1 and 65535")
	}
	if address := s.GetTestAddress(); address != "" {
		addr, err := netip.ParseAddr(address)
		if err != nil || !addr.Unmap().IsGlobalUnicast() || addr.Zone() != "" {
			return errors.New("enter the numeric client IP address the server sees for SSH")
		}
	}
	if len(s.GetListenAddresses()) > 16 {
		return errors.New("choose at most 16 SSH listen addresses")
	}
	var seen []netip.Addr
	for _, input := range s.GetListenAddresses() {
		addr, err := netip.ParseAddr(input)
		if err != nil || addr.Zone() != "" || addr.Is4In6() || addr.IsMulticast() || slices.Contains(seen, addr) {
			return fmt.Errorf("%q isn't a unique, numeric SSH listen address", input)
		}
		seen = append(seen, addr)
	}
	if s.PasswordLogin == nil && s.KeyLogin == nil && s.GetRootLogin() == "" && s.GetPort() == 0 && len(seen) == 0 {
		return errors.New("choose an SSH setting to manage")
	}
	return nil
}

// Observe returns the settings last kept by the agent.
func (d *Driver) Observe(ctx context.Context, _ resource.Ref) (proto.Message, error) {
	s := d.Backend.SSHConfig(ctx)
	if s == nil {
		return nil, nil
	}
	return s, nil
}

// Plan describes the action when there is no plan context.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.Judge(context.Background(), resource.Ref{}, actual, desired, changes)
}

// Judge checks the server and marks the plan as an access change.
func (d *Driver) Judge(ctx context.Context, _ resource.Ref, actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	next, _ := desired.(*resourcev1.SshConfig)
	decision := resource.Decision{Action: resource.ActionUpdate, Warnings: []resource.Warning{resource.WarningMayLockOut}}
	switch {
	case actual == nil:
		decision.Action = resource.ActionCreate
	case desired == nil:
		decision.Action = resource.ActionDelete
	}
	if d.Backend != nil {
		move, err := d.Backend.CheckSSH(ctx, next)
		if err != nil {
			return decision, err
		}
		decision.SSHPortMove = move
	}
	return decision, nil
}

// Apply submits a trial to the server's access manager.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	next, _ := desired.(*resourcev1.SshConfig)
	return d.Backend.PutSSH(ctx, next, step.SSHPortMove)
}

// Verify waits for a resumed trial to finish.
func (d *Driver) Verify(ctx context.Context, _ resource.Ref, _ proto.Message) error {
	return d.Backend.Wait(ctx)
}
