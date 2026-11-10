// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// RunState is how a resource that runs something stands on its server, as an
// agent reads it while the control plane is away (ADR 0054).
type RunState struct {
	// Exists says there's something to run, and Running that it runs, or is
	// being restarted by Docker.
	Exists, Running bool
	// ID and Reconcile identify a container and the local action that made it.
	ID, Reconcile string
	// Created says Docker has never started it. An unfinished local creation
	// may finish that start; a container that was stopped is left alone.
	Created bool
	// Failed is why Docker couldn't start it, as Docker leaves in its state. It's
	// empty for one someone stopped, or that exited by itself.
	Failed string
	// Leaving says Docker is removing it, or gave up on removing it: "removing"
	// or "dead". It isn't running, and that isn't a stop: a forced removal kills
	// a container on its way out, so how it stands then is no news.
	Leaving bool
	// Wants says its restart policy has it run: unless-stopped, always or
	// on-failure.
	Wants bool
	// Volumes are the volumes Docker made for the paths its image declares, by
	// path: the data of a database whose spec mounts nothing there.
	Volumes map[string]string
}

// Reviver is a driver whose resource an agent can start again, and check before
// making it again, while the control plane is away (ADR 0054).
type Reviver interface {
	// RunState says how the resource stands.
	RunState(ctx context.Context, ref Ref) (RunState, error)
	// Start starts it, as it is.
	Start(ctx context.Context, ref Ref) error
	// Hold says why the resource can't be made again from spec now, or nothing
	// when it can: what it mounts is gone, and making it again would make it anew,
	// empty, as a volume, or the volume Docker made for a path its image declares
	// (volumes, by path, as last seen); its image keeps data at a path the agent
	// never saw a volume of; or another container mounts one of its volumes, as one
	// that replaced it by hand would.
	Hold(ctx context.Context, ref Ref, spec proto.Message, volumes map[string]string) (string, error)
}

type imageVolumesKey struct{}

// WithImageVolumes has a container made from ctx mount these volumes, by path, at
// the paths its image declares, as a container that was there did: the agent
// making one again while the control plane is away mounts its data again.
func WithImageVolumes(ctx context.Context, byPath map[string]string) context.Context {
	return context.WithValue(ctx, imageVolumesKey{}, byPath)
}

// ImageVolumesFrom returns the volumes WithImageVolumes gave, or nil.
func ImageVolumesFrom(ctx context.Context) map[string]string {
	v, _ := ctx.Value(imageVolumesKey{}).(map[string]string)
	return v
}

// Reviver is ref's kind's driver as a Reviver, or nil when its kind runs nothing.
func (l Local) Reviver(ref Ref) Reviver {
	d, err := l.Drivers.Driver(ref.Kind)
	if err != nil {
		return nil
	}
	r, _ := d.(Reviver)
	return r
}

// Attacher is a driver whose resource containers attach to, as a network, which an
// agent makes again on its own only when none still names the one that's gone
// (ADR 0054).
type Attacher interface {
	// Attached names the containers on the server attached to ref, by their names,
	// whatever Docker has of ref itself now.
	Attached(ctx context.Context, ref Ref) ([]string, error)
}

// Attacher is ref's kind's driver as an Attacher, or nil.
func (l Local) Attacher(ref Ref) Attacher {
	d, err := l.Drivers.Driver(ref.Kind)
	if err != nil {
		return nil
	}
	a, _ := d.(Attacher)
	return a
}
