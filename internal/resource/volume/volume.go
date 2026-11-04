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
// a mount the local driver isn't allowed to make, and a bind of a server
// path no container may mount.
func (*Driver) Validate(spec proto.Message) error {
	v := spec.(*resourcev1.Volume)
	if v.Driver == "" {
		v.Driver = "local"
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
	return validateMount(v)
}

// mountTypes are what the local driver may mount besides a bind. Anything
// else reads paths the plan can't name: overlay takes them in o, and a
// disk's filesystem takes a device under /dev.
var mountTypes = []string{"nfs", "nfs4", "cifs", "tmpfs"}

// validateMount checks what the local driver's options mount, by an
// allowlist. A bind's device is checked as a container's bind is.
func validateMount(v *resourcev1.Volume) error {
	options := v.GetOptions()
	if v.GetDriver() != "local" || len(options) == 0 {
		return nil
	}
	kind := options["type"]
	dev, bind := container.BindDevice(v.GetDriver(), options)
	switch {
	case bind && kind != "none" && kind != "":
		return fmt.Errorf("the volume binds %s with type %s, and a bind takes type none. Use type: none, or a mount without bind", dev, kind)
	case bind:
		if !path.IsAbs(dev) {
			return fmt.Errorf("the volume binds the server path %q, which isn't absolute; start its device with /, such as /srv/data", dev)
		}
		if err := container.CheckSpelling(dev, "the server path "+dev+" that the volume binds"); err != nil {
			return err
		}
		dev = path.Clean(dev)
		options["device"] = dev
		return container.DefaultHost.CheckPath(dev, "the server path "+dev+" that the volume binds")
	case kind == "none":
		return errors.New("a volume of type none binds a server path, and this one has no bind. Add bind to o, such as o: bind, with the path in device")
	case kind == "":
		return errors.New("a volume's driver options need a type. Use type none with bind, nfs, nfs4, cifs or tmpfs")
	case !slices.Contains(mountTypes, kind):
		return fmt.Errorf("the volume mounts type %q, which Dockwell can't run: a plan couldn't show what it reaches. Use type none with bind, nfs, nfs4, cifs or tmpfs", kind)
	}
	return nil
}

// HostAccess is what a volume reaches on its server, beyond what Docker
// keeps for it: the server path it binds, the mount its options make, such
// as an NFS share, or what a plugin driver does. It names the type, where
// it reaches and how, and never the credentials in its options.
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	v := spec.(*resourcev1.Volume)
	options := v.GetOptions()
	o := container.ReadMountOptions(options["o"])
	if v.GetDriver() != "local" && v.GetDriver() != "" {
		return []resource.Access{{Kind: resource.AccessMount, Driver: v.GetDriver(), ReadOnly: o.ReadOnly}}
	}
	if dev, ok := container.BindDevice(v.GetDriver(), options); ok {
		return []resource.Access{container.PathAccess(path.Clean(dev), o.ReadOnly)}
	}
	if len(options) == 0 {
		return nil
	}
	a := resource.Access{Kind: resource.AccessMount, Type: options["type"], Address: o.Address, ReadOnly: o.ReadOnly}
	if a.Type != "tmpfs" {
		// A device may carry a user and password before an @.
		dev := options["device"]
		a.Path = dev[strings.LastIndex(dev, "@")+1:]
	}
	return []resource.Access{a}
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
