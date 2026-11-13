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
	"net/netip"
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

var (
	_ resource.Driver   = (*Driver)(nil)
	_ resource.Attacher = (*Driver)(nil)
)

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
	if err := container.CheckDriver(n.GetDriver()); err != nil {
		return err
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
	return checkMembers(n)
}

// checkMembers refuses members and a subnet a link network couldn't have (ADR 0084).
func checkMembers(n *resourcev1.Network) error {
	seen := map[string]bool{}
	for _, m := range n.GetMembers() {
		switch {
		case !strings.HasPrefix(m.GetEnvironment(), "env_"):
			return errors.New("a network's member names its environment by its ID, such as env_…")
		case !container.NameRE.MatchString(m.GetService()):
			return fmt.Errorf("a network's member names its service, such as db, not %q", m.GetService())
		case !container.NameRE.MatchString(m.GetAlias()):
			return fmt.Errorf("a network's member needs a name it answers to, such as db, not %q", m.GetAlias())
		case seen[m.GetEnvironment()+"/"+m.GetService()]:
			return fmt.Errorf("a network names %s as a member twice", m.GetService())
		}
		seen[m.GetEnvironment()+"/"+m.GetService()] = true
	}
	if sn := n.GetSubnet(); sn != "" {
		p, err := netip.ParsePrefix(sn)
		if err != nil || !p.Addr().Is4() || p.Masked() != p {
			return fmt.Errorf("a network's subnet is an IPv4 range, such as 10.233.0.8/29, not %q", sn)
		}
	}
	return nil
}

// aliasOn is the name a container answers to on a network, of those Docker lists:
// Docker adds the container's own name, and before 25 its short ID, so the alias is
// what's left. Only the short ID itself is Docker's: an alias such as "db" can begin
// the container's ID, which is hex.
func aliasOn(c *docker.ContainerSummary, network string) string {
	ep := c.NetworkSettings.Networks[network]
	if ep == nil {
		return ""
	}
	for _, a := range ep.Aliases {
		if !slices.Contains(c.Names, "/"+a) && (len(c.ID) < 12 || a != c.ID[:12]) {
			return a
		}
	}
	return ""
}

// MemberTarget is where a network's member runs: its environment's project, and its
// service.
func MemberTarget(m *resourcev1.NetworkMember) (project, svc string) {
	return docker.Project(m.GetEnvironment()), m.GetService()
}

// environmentOf is the environment an environment's Docker project is of: the project
// is its ID in lowercase (docker.Project), and an ID is its lowercase prefix and an
// uppercase ULID, so the ID comes back whole. Any other project is as it is.
func environmentOf(project string) string {
	if rest, ok := strings.CutPrefix(project, "env_"); ok {
		return "env_" + strings.ToUpper(rest)
	}
	return project
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
	// A subnet it was given reads back; one Docker chose doesn't.
	if spec.GetSubnet() != "" {
		set = append(set, "subnet")
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
	if set["subnet"] && len(n.IPAM.Config) > 0 {
		out.Subnet = n.IPAM.Config[0].Subnet
	}
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

// Attached names the containers on the server attached to the network, stopped
// ones too, by name. One attached to a network that's gone still names it.
func (d *Driver) Attached(ctx context.Context, ref resource.Ref) ([]string, error) {
	name := TargetOf(ref).Name
	list, err := d.Client.ListContainers(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list {
		c := &list[i]
		if _, ok := c.NetworkSettings.Networks[name]; ok && len(c.Names) > 0 {
			out = append(out, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	return out, nil
}

// Observe asks Docker, and reads as members the containers of other environments'
// services on it.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	t := TargetOf(ref)
	n, err := d.Client.NetworkNamed(ctx, t.Name)
	switch {
	case docker.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	out := Project(n)
	members, err := d.members(ctx, t)
	if err != nil {
		return nil, err
	}
	out.Members = members
	return out, nil
}

// members are the services of other environments with a container on the network,
// each once, with the name it answers to there.
func (d *Driver) members(ctx context.Context, t Target) ([]*resourcev1.NetworkMember, error) {
	list, err := d.Client.ListContainers(ctx, map[string][]string{"network": {t.Name}})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []*resourcev1.NetworkMember
	for i := range list {
		c := &list[i]
		project, svc := c.Labels[docker.LabelProject], c.Labels[docker.LabelService]
		if project == "" || svc == "" || project == t.Project || seen[project+"/"+svc] {
			continue
		}
		seen[project+"/"+svc] = true
		out = append(out, &resourcev1.NetworkMember{Environment: environmentOf(project), Service: svc, Alias: aliasOn(c, t.Name)})
	}
	slices.SortFunc(out, func(a, b *resourcev1.NetworkMember) int {
		return strings.Compare(a.GetEnvironment()+"/"+a.GetService(), b.GetEnvironment()+"/"+b.GetService())
	})
	return out, nil
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
	// Who joins it changes in place: containers join and leave a running network.
	if len(changes) > 0 && !slices.ContainsFunc(changes, func(c resource.Change) bool { return !strings.HasPrefix(c.Path, "members") }) {
		return resource.Decision{Action: resource.ActionUpdate}, nil
	}
	what := "its spec"
	for _, c := range changes {
		if !strings.HasPrefix(c.Path, "members") {
			what = c.Path
			break
		}
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
		create := &docker.NetworkCreate{
			Name: t.Name, Driver: spec.GetDriver(), Internal: spec.GetInternal(), Options: spec.GetOptions(), Labels: Labels(t, spec),
		}
		if sn := spec.GetSubnet(); sn != "" {
			create.IPAM = &docker.IPAM{Config: []docker.IPAMConfig{{Subnet: sn}}}
		}
		if _, err := d.Client.CreateNetwork(ctx, create); err != nil {
			return fmt.Errorf("docker can't create the network %s: %w", t.Name, err)
		}
		return d.join(ctx, t, nil, spec.GetMembers())
	case resource.ActionDelete:
		n, err := d.Client.NetworkNamed(ctx, t.Name)
		switch {
		case docker.IsNotFound(err):
			return nil
		case err != nil:
			return err
		}
		// Members of other environments leave first, as they aren't the network's to go with.
		if members, err := d.members(ctx, t); err == nil {
			if err := d.join(ctx, t, members, nil); err != nil {
				return err
			}
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
	case resource.ActionUpdate:
		have, err := d.members(ctx, t)
		if err != nil {
			return err
		}
		return d.join(ctx, t, have, desired.(*resourcev1.Network).GetMembers())
	case resource.ActionReplace:
		return fmt.Errorf("docker can't change the network %s once it is made", t.Name)
	case resource.ActionNone:
	}
	return nil
}

// join has each member's containers join the network, answering to its alias, and
// each it had and doesn't want leave, in place: a running container joins and leaves
// a network without a restart. A member with no container yet joins as one is made
// (container.Driver.Joins).
func (d *Driver) join(ctx context.Context, t Target, have, want []*resourcev1.NetworkMember) error {
	key := func(m *resourcev1.NetworkMember) string {
		return docker.Project(m.GetEnvironment()) + "/" + m.GetService()
	}
	wanted := map[string]bool{}
	for _, m := range want {
		wanted[key(m)] = true
	}
	had := map[string]*resourcev1.NetworkMember{}
	for _, m := range have {
		had[key(m)] = m
	}
	for _, m := range have {
		if !wanted[key(m)] {
			if err := d.each(ctx, m, func(id string) error { return d.Client.DisconnectNetwork(ctx, t.Name, id) }); err != nil {
				return fmt.Errorf("can't take %s off the network %s: %w", m.GetService(), t.Name, err)
			}
		}
	}
	for _, m := range want {
		if old, ok := had[key(m)]; ok && old.GetAlias() == m.GetAlias() {
			continue
		} else if ok {
			if err := d.each(ctx, m, func(id string) error { return d.Client.DisconnectNetwork(ctx, t.Name, id) }); err != nil {
				return fmt.Errorf("can't take %s off the network %s: %w", m.GetService(), t.Name, err)
			}
		}
		if err := d.each(ctx, m, func(id string) error { return d.Client.ConnectNetwork(ctx, t.Name, id, []string{m.GetAlias()}) }); err != nil {
			return fmt.Errorf("can't put %s on the network %s: %w", m.GetService(), t.Name, err)
		}
	}
	return nil
}

// each calls f with every container of a member's service on the server.
func (d *Driver) each(ctx context.Context, m *resourcev1.NetworkMember, f func(id string) error) error {
	project, svc := MemberTarget(m)
	list, err := d.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + project, docker.LabelService + "=" + svc}})
	if err != nil {
		return err
	}
	for i := range list {
		if err := f(list[i].ID); err != nil {
			return err
		}
	}
	return nil
}

// Verify has nothing to check: the network exists as asked for.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// Ready checks Docker answers.
func (d *Driver) Ready(ctx context.Context) error { return d.Client.Ping(ctx) }

// ReadyKey is the client: whether its Docker answers.
func (d *Driver) ReadyKey() any { return d.Client }
