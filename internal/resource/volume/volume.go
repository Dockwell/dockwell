// SPDX-License-Identifier: AGPL-3.0-only

// Package volume is the "volume" kind: a Docker volume in an environment,
// which its services mount by name, as a Compose file's volumes. Docker
// names it as Compose names a project's volume, <project>_<name>, with
// Compose's labels, so docker compose finds it.
//
// Deleting one destroys its data, so the plan says so. The volume stays on
// the server for KeepFor first, so undoing the deletion brings the data
// back, and Prune removes it after.
package volume

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Kind is the volume kind.
const Kind resource.Kind = "volume"

// KeepFor is how long a deleted volume stays on its server, with its data,
// as the PRD keeps what destructive actions remove for 7 days.
const KeepFor = 7 * 24 * time.Hour

// Target is a volume in Docker.
type Target struct {
	// Name is the volume's name in Docker.
	Name string
	// Project is its Compose project, and Key its name there.
	Project, Key string
}

// TargetOf is where a volume goes: <project>_<name>, in its environment's
// Compose project.
func TargetOf(ref resource.Ref) Target {
	p := docker.Project(ref.Scope)
	return Target{Name: docker.ScopedName(p, ref.Name), Project: p, Key: ref.Name}
}

// Runtime is the part of Docker the driver needs.
type Runtime interface {
	// Inspect returns the volume as Docker has it, or nil if there is none,
	// or if it was deleted and is only kept.
	Inspect(ctx context.Context, t Target) (*resourcev1.Volume, error)
	// Create makes the volume, or brings back one deleted and kept.
	// restore says the plan expected to bring one back: if it is gone, as
	// a prune removed it, Create fails rather than make it empty.
	Create(ctx context.Context, t Target, spec *resourcev1.Volume, restore bool) error
	// Delete deletes the volume, which stays on the server for KeepFor.
	Delete(ctx context.Context, t Target) error
}

// Driver manages volumes.
type Driver struct {
	Runtime Runtime
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "volume".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the environment: a volume belongs to its services.
func (*Driver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "vol".
func (*Driver) IDPrefix() string { return "vol" }

// New returns an empty Volume.
func (*Driver) New() proto.Message { return &resourcev1.Volume{} }

// CheckName requires a name Docker takes, up to 63 characters, as Compose
// files name volumes, such as db-data.
func (*Driver) CheckName(name string) error {
	if !container.NameRE.MatchString(name) || len(name) > 63 {
		return fmt.Errorf("%q isn't a valid volume name: use up to 63 letters, digits, dots, dashes and underscores, starting with a letter or digit, such as db-data", name)
	}
	return nil
}

// Validate fills in the local driver, and refuses Dockwell's own labels,
// and a bind of a server path no container may mount.
func (*Driver) Validate(spec proto.Message) error {
	v := spec.(*resourcev1.Volume)
	if v.Driver == "" {
		v.Driver = "local"
	}
	if dev, ok := bindDevice(v); ok {
		if !path.IsAbs(dev) {
			return fmt.Errorf("the volume binds the server path %q, which isn't absolute; start its device with /, such as /srv/data", dev)
		}
		if err := container.DefaultHost.CheckPath(dev, "the server path "+path.Clean(dev)+" that the volume binds"); err != nil {
			return err
		}
	}
	for k := range v.GetLabels() {
		if docker.IsOwnLabel(k) {
			return fmt.Errorf("the label %s is Dockwell's own, and Docker Compose's; choose a key that doesn't start with %s or %s", k, docker.ComposePrefix, docker.OwnPrefix)
		}
	}
	for k := range v.GetOptions() {
		if strings.TrimSpace(k) == "" {
			return errors.New("a volume's driver options need names, such as type")
		}
	}
	return nil
}

// bindDevice is the server path a volume binds, as the local driver's
// options type: none, o: bind and device: /srv/data have it.
func bindDevice(v *resourcev1.Volume) (string, bool) {
	o := v.GetOptions()
	if v.GetDriver() != "local" && v.GetDriver() != "" {
		return "", false
	}
	for _, opt := range strings.Split(o["o"], ",") {
		if opt == "bind" || opt == "rbind" {
			return o["device"], true
		}
	}
	return "", false
}

// HostAccess is what a volume reaches on its server, beyond what Docker
// keeps for it: the server path it binds, or the mount its options make,
// such as an NFS share. A service of its environment that mounts it
// reaches that too, as a person approved when the volume was made.
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	v := spec.(*resourcev1.Volume)
	if dev, ok := bindDevice(v); ok {
		readOnly := slices.Contains(strings.Split(v.GetOptions()["o"], ","), "ro")
		return []resource.Access{container.PathAccess(path.Clean(dev), readOnly)}
	}
	if len(v.GetOptions()) == 0 {
		return nil
	}
	return []resource.Access{{Kind: resource.AccessMount, Type: v.GetOptions()["type"], Path: v.GetOptions()["device"]}}
}

// checker is a Runtime that checks a spec on its server, as Docker does.
type checker interface {
	CheckHost(ctx context.Context, spec *resourcev1.Volume) error
}

// Check refuses a volume that binds a server path no container may mount,
// as the agent's server has it.
func (d *Driver) Check(ctx context.Context, _ resource.Ref, desired proto.Message) error {
	if c, ok := d.Runtime.(checker); ok {
		return c.CheckHost(ctx, desired.(*resourcev1.Volume))
	}
	return nil
}

// restorer is a Runtime that keeps deleted volumes, as Docker does.
type restorer interface {
	// Restores reports whether creating the volume at t brings back one
	// deleted and kept, with its data.
	Restores(ctx context.Context, t Target) (bool, error)
}

// Judge plans a step as Plan does, and says whether a create brings back
// a volume the server kept, as undoing its deletion does: the plan says
// so, and its fingerprint differs from one that makes the volume empty.
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	dec, err := d.Plan(actual, desired, changes)
	if err != nil || dec.Action != resource.ActionCreate {
		return dec, err
	}
	if r, ok := d.Runtime.(restorer); ok {
		if dec.Restores, err = r.Restores(ctx, TargetOf(ref)); err != nil {
			return resource.Decision{}, err
		}
	}
	return dec, nil
}

// Observe asks Docker.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	v, err := d.Runtime.Inspect(ctx, TargetOf(ref))
	if err != nil || v == nil {
		return nil, err
	}
	return v, nil
}

// Plan creates and deletes volumes. Deleting one destroys its data, though
// it is kept for KeepFor first. Docker can't change a volume, and making
// it again would empty it, so a change to one is refused.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete, Destroys: true}, nil
	}
	what := "its spec"
	if len(changes) > 0 {
		what = changes[0].Path
	}
	return resource.Decision{}, fmt.Errorf("docker can't change a volume once it is made, and making it again would empty it, so its %s can't change. "+
		"Make a new volume as you want it, move the data across, then delete this one", what)
}

// Apply creates or deletes the volume.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	t := TargetOf(step.Ref)
	switch step.Action {
	case resource.ActionCreate:
		return d.Runtime.Create(ctx, t, desired.(*resourcev1.Volume), step.Restores)
	case resource.ActionDelete:
		return d.Runtime.Delete(ctx, t)
	case resource.ActionUpdate, resource.ActionReplace:
		return fmt.Errorf("docker can't change the volume %s once it is made", t.Name)
	case resource.ActionNone:
	}
	return nil
}

// Verify has nothing to check: the volume exists as asked for.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// Ready says why volumes can't be made now, such as Docker not running.
func (d *Driver) Ready(ctx context.Context) error {
	if r, ok := d.Runtime.(resource.Readier); ok {
		return r.Ready(ctx)
	}
	return nil
}

// ReadyKey is the runtime's, such as its Docker client.
func (d *Driver) ReadyKey() any {
	if r, ok := d.Runtime.(resource.SharedReadier); ok {
		return r.ReadyKey()
	}
	return d
}
