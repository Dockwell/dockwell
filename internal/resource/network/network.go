// SPDX-License-Identifier: AGPL-3.0-only

// Package network is the "network" kind: a Docker network in an
// environment, which its services join by name, as a Compose file's
// networks. Docker names it as Compose names a project's network,
// <project>_<name>, with Compose's labels, so docker compose finds it.
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Kind is the network kind.
const Kind resource.Kind = "network"

// Target is a network in Docker.
type Target struct {
	// Name is the network's name in Docker.
	Name string
	// Project is its Compose project, and Key its name there.
	Project, Key string
}

// TargetOf is where a network goes: <project>_<name>, in its environment's
// Compose project.
func TargetOf(ref resource.Ref) Target {
	p := docker.Project(ref.Scope)
	return Target{Name: docker.ScopedName(p, ref.Name), Project: p, Key: ref.Name}
}

// Driver manages networks on the Docker Engine.
type Driver struct {
	Client *docker.Client
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "network".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the environment: a network joins its services.
func (*Driver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "net".
func (*Driver) IDPrefix() string { return "net" }

// New returns an empty Network.
func (*Driver) New() proto.Message { return &resourcev1.Network{} }

// CheckName requires a name Docker takes, up to 63 characters, as Compose
// files name networks, such as backend.
func (*Driver) CheckName(name string) error {
	if !container.NameRE.MatchString(name) || len(name) > 63 {
		return fmt.Errorf("%q isn't a valid network name: use up to 63 letters, digits, dots, dashes and underscores, starting with a letter or digit, such as backend", name)
	}
	return nil
}

// Validate fills in the bridge driver, and refuses Dockwell's own labels.
func (*Driver) Validate(spec proto.Message) error {
	n := spec.(*resourcev1.Network)
	if n.Driver == "" {
		n.Driver = "bridge"
	}
	for k := range n.GetLabels() {
		if docker.IsOwnLabel(k) {
			return fmt.Errorf("the label %s is Dockwell's own, and Docker Compose's; choose a key that doesn't start with %s or %s", k, docker.ComposePrefix, docker.OwnPrefix)
		}
	}
	for k := range n.GetOptions() {
		if strings.TrimSpace(k) == "" {
			return errors.New("a network's driver options need names, such as com.docker.network.bridge.name")
		}
	}
	return nil
}

// plainOptions are the options that keep a bridge network what it is: a
// network of its own, behind the server's firewall. Any other may change
// how it routes, such as gateway_mode_ipv4: nat-unprotected, or what it
// attaches to, such as bridge.name.
var plainOptions = []string{
	"com.docker.network.driver.mtu",
	"com.docker.network.bridge.enable_icc",
	"com.docker.network.container_iface_prefix",
}

// HostAccess is what a network reaches past the server's own bridge. A
// driver but bridge, such as macvlan, puts containers on the server's LAN,
// past its firewall, and so may an option outside plainOptions.
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	n := spec.(*resourcev1.Network)
	var other []string
	for _, k := range slices.Sorted(maps.Keys(n.GetOptions())) {
		if k != "parent" && !slices.Contains(plainOptions, k) {
			other = append(other, k+"="+n.GetOptions()[k])
		}
	}
	driver := n.GetDriver()
	if (driver == "bridge" || driver == "") && len(other) == 0 {
		return nil
	}
	if driver == "" {
		driver = "bridge"
	}
	return []resource.Access{{Kind: resource.AccessNetworkDriver, Driver: driver, Path: n.GetOptions()["parent"], Detail: strings.Join(other, ",")}}
}

// labelSet lists the driver options a network's spec sets, so options
// Docker adds of its own, under com.docker.network., don't read as drift.
const labelSet = docker.OwnPrefix + "set"

// Labels are what a network at t carries for spec, an exported Compose
// file's own label among them.
func Labels(t Target, spec *resourcev1.Network) map[string]string {
	l := maps.Clone(spec.GetLabels())
	if l == nil {
		l = map[string]string{}
	}
	l[docker.LabelProject] = t.Project
	l[docker.LabelNetwork] = t.Key
	maps.Copy(l, OwnLabels(spec))
	return l
}

// OwnLabels are Dockwell's own labels for spec: none for a network with
// no driver options.
func OwnLabels(spec *resourcev1.Network) map[string]string {
	var set []string
	for _, k := range slices.Sorted(maps.Keys(spec.GetOptions())) {
		set = append(set, "option:"+k)
	}
	if len(set) == 0 {
		return nil
	}
	b, _ := json.Marshal(set)
	return map[string]string{labelSet: string(b)}
}

// Project reads a network as Docker reports it back into a spec, without
// Dockwell's and Compose's labels, or options Docker adds.
func Project(n *docker.Network) *resourcev1.Network {
	var list []string
	_ = json.Unmarshal([]byte(n.Labels[labelSet]), &list)
	set := map[string]bool{}
	for _, s := range list {
		set[s] = true
	}
	out := &resourcev1.Network{Driver: n.Driver, Internal: n.Internal}
	for k, v := range n.Options {
		if strings.HasPrefix(k, "com.docker.network.") && !set["option:"+k] {
			continue
		}
		if out.Options == nil {
			out.Options = map[string]string{}
		}
		out.Options[k] = v
	}
	for k, v := range n.Labels {
		if docker.IsOwnLabel(k) {
			continue
		}
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[k] = v
	}
	return out
}

// Observe asks Docker.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	n, err := d.Client.NetworkNamed(ctx, TargetOf(ref).Name)
	switch {
	case docker.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return Project(n), nil
}

// Plan creates and deletes networks. Docker can't change a network, and
// making it again would cut off every container on it, so a change to one
// is refused.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete}, nil
	}
	what := "its spec"
	if len(changes) > 0 {
		what = changes[0].Path
	}
	return resource.Decision{}, fmt.Errorf("docker can't change a network once it is made, so its %s can't change. "+
		"Make a new network as you want it, move the services to it, then delete this one", what)
}

// Apply creates or deletes the network.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	t := TargetOf(step.Ref)
	switch step.Action {
	case resource.ActionCreate:
		spec := desired.(*resourcev1.Network)
		_, err := d.Client.CreateNetwork(ctx, &docker.NetworkCreate{
			Name: t.Name, Driver: spec.GetDriver(), Internal: spec.GetInternal(), Options: spec.GetOptions(), Labels: Labels(t, spec),
		})
		if err != nil {
			return fmt.Errorf("docker can't create the network %s: %w", t.Name, err)
		}
	case resource.ActionDelete:
		n, err := d.Client.NetworkNamed(ctx, t.Name)
		switch {
		case docker.IsNotFound(err):
			return nil
		case err != nil:
			return err
		}
		err = d.Client.RemoveNetwork(ctx, n.ID)
		if docker.IsInUse(err) {
			var names []string
			for _, c := range n.Containers {
				names = append(names, c.Name)
			}
			slices.Sort(names)
			return fmt.Errorf("containers are still on the network %s (%s), so it can't go. Delete their services first, in this change or an earlier one",
				t.Name, strings.Join(names, ", "))
		}
		if err != nil && !docker.IsNotFound(err) {
			return err
		}
	case resource.ActionUpdate, resource.ActionReplace:
		return fmt.Errorf("docker can't change the network %s once it is made", t.Name)
	case resource.ActionNone:
	}
	return nil
}

// Verify has nothing to check: the network exists as asked for.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// Ready checks Docker answers.
func (d *Driver) Ready(ctx context.Context) error { return d.Client.Ping(ctx) }

// ReadyKey is the client: whether its Docker answers.
func (d *Driver) ReadyKey() any { return d.Client }
