// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	kernelmounts "github.com/dockwell/dockwell/internal/mounts"
	"github.com/dockwell/dockwell/internal/resource"
	containerdriver "github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const (
	// listBytes is the most of a list of containers that goes down the agent's
	// stream in one message, far under the 8 MiB a message can be, as stats wait
	// behind it, and a message over that would end the stream. Past it, a list says
	// how many it left out. A container comes to a few hundred bytes, so this is a
	// few thousand of them.
	listBytes = 1 << 20
	// listEach is the most ports and the most mounts one container lists, as a port
	// range publishes a port for each number.
	listEach = 32
)

// containers answers a containers call, from Docker's list and nothing else: that
// reads the server without touching it, and it is one request however many
// containers there are, none of which has its environment in it.
func containers(ctx context.Context, client *docker.Client, call *agentv1.ContainersCall) (*agentv1.ContainersResult, error) {
	if client == nil {
		return nil, errors.New("this agent can't list containers, as it has no Docker driver")
	}
	out := &agentv1.ContainersResult{}
	size := 0
	err := client.EachContainer(ctx, containerFilter(call.GetContainer()), func(s *docker.ContainerSummary) error {
		if _, ok := s.Labels[docker.LabelIngress]; ok {
			out.Ingress++
			return nil
		}
		// A helper lives for one backup or restore, and a drill's database for one
		// check of a backup: nobody's to look at.
		if _, ok := s.Labels[docker.LabelVolumeTool]; ok {
			return nil
		}
		if _, ok := s.Labels[docker.LabelDrill]; ok {
			return nil
		}
		c := serverContainer(s)
		if size += proto.Size(c); size > listBytes {
			out.Unlisted++
			return nil
		}
		out.Containers = append(out.Containers, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// adoption reads a container as the service a call names, with its driver. What it
// sends holds the container's environment, as what an agent reports of any service
// does: over the agent's own stream, and into a spec's sensitive field, which the
// control plane seals, and no plan or log shows.
func (c *calls) adoption(ctx context.Context, call *agentv1.AdoptionCall) (*agentv1.AdoptionResult, error) {
	ref := resourcepb.FromRef(call.GetResource())
	d, err := c.cfg.Drivers.Driver(ref.Kind)
	if err != nil {
		return nil, err
	}
	a, ok := d.(resource.Adopter)
	if !ok {
		return nil, fmt.Errorf("this agent can't take on what a %s runs", ref.Kind)
	}
	read, err := a.ReadAdoption(ctx, ref, call.GetContainer())
	if err != nil {
		return nil, err
	}
	b, err := resource.Encode(read.Spec)
	if err != nil {
		return nil, err
	}
	return &agentv1.AdoptionResult{Spec: b, Refused: read.Refused, Shows: read.Shows, Manager: read.Manager, Folder: read.Folder, LeanedOn: read.LeanedOn, Invalid: read.Invalid}, nil
}

// ContainerAsGoodAsRoot says whether a container, by its full ID, reaches as far as
// root on its server, by the rule a plan goes by (resource.Access.AsGoodAsRoot), and
// on what it mounts. A shell in such a container takes a passkey (ADR 0037). It
// counts as root:
//   - what it runs as a service reaches, by its spec, and what no service's spec
//     can hold, as what that is can't be told;
//   - each server path it binds, by where that leads through symlinks now, as a
//     plan's do (Access.Leading);
//   - each volume that binds a server path, by that path, followed the same way;
//     each on a driver other than local; and each local one that mounts what a plan
//     refuses, such as a disk, an overlay or proc (container.LocalMountType);
//   - what each of its mounts is in its own mount namespace, which is what the
//     kernel mounted when it started, whatever a link says since: the server path,
//     or a filesystem of the kernel's own, such as proc or sysfs (mountedNow).
//
// leads says where server paths lead, for those that lead elsewhere (FollowPaths),
// and table reads a process's mounts (ProcMounts). Anything it can't read counts as
// root.
func ContainerAsGoodAsRoot(drivers *resource.Registry, client *docker.Client, leads func(ctx context.Context, paths ...string) (map[string]string, error), table MountTable) func(ctx context.Context, container string) (bool, error) {
	return func(ctx context.Context, container string) (bool, error) {
		d, err := drivers.Driver(service.Kind)
		if err != nil {
			return true, err
		}
		a, ok := d.(resource.Adopter)
		if !ok {
			return true, nil
		}
		read, err := a.ReadAdoption(ctx, resource.Ref{Kind: service.Kind, Name: "shell"}, container)
		if err != nil {
			return true, err
		}
		if len(read.Refused) > 0 || read.Invalid != "" {
			return true, nil
		}
		var reaches []resource.Access
		if h, ok := d.(resource.HostAccesser); ok {
			reaches = h.HostAccess(read.Spec)
		}
		if client == nil {
			return true, errors.New("this agent can't reach Docker to read what the container mounts")
		}
		c, err := client.InspectContainer(ctx, container)
		if err != nil {
			return true, err
		}
		mounted, root, err := mountedAccess(ctx, client, c)
		if err != nil || root {
			return true, err
		}
		reaches = append(reaches, mounted...)
		now, root, err := mountedNow(table, c)
		if err != nil || root {
			return true, err
		}
		reaches = append(reaches, now...)
		var paths []string
		for i := range reaches {
			if reaches[i].Kind == resource.AccessPath || reaches[i].Kind == resource.AccessDocker {
				paths = append(paths, reaches[i].Path)
			}
		}
		led, err := leads(ctx, paths...)
		if err != nil {
			return true, err
		}
		for i := range reaches {
			if reaches[i].Leading(led[path.Clean(reaches[i].Path)]).AsGoodAsRoot() {
				return true, nil
			}
		}
		return false, nil
	}
}

// mountedAccess is what a container's mounts reach on its server, as Docker has
// them: each server path it binds, and the one each volume binds. root is true for
// a volume on a driver other than local, one that mounts what a plan refuses, or
// one that binds a path that isn't absolute, as where those mount from can't be told.
func mountedAccess(ctx context.Context, client *docker.Client, c *docker.Container) (reaches []resource.Access, root bool, err error) {
	for _, m := range c.Mounts {
		switch m.Type {
		case "bind":
			reaches = append(reaches, containerdriver.PathAccess(m.Source, !m.RW))
		case "volume":
			v, err := client.InspectVolume(ctx, m.Name)
			if err != nil {
				return nil, true, fmt.Errorf("can't read the volume %s it mounts: %w", m.Name, err)
			}
			if v.Driver != "" && v.Driver != "local" {
				return nil, true, nil
			}
			dev, bind := containerdriver.BindDevice(v.Driver, v.Options)
			switch {
			case bind && !path.IsAbs(dev):
				return nil, true, nil
			case bind:
				reaches = append(reaches, containerdriver.PathAccess(dev, !m.RW))
			case len(v.Options) > 0 && !containerdriver.LocalMountType(v.Options["type"]):
				return nil, true, nil
			}
		}
	}
	return reaches, false, nil
}

// MountTable reads a process's mounts, as its /proc/<pid>/mountinfo has them, and
// the agent's own for pid 0.
type MountTable func(pid int) (string, error)

// ProcMounts reads them from /proc.
func ProcMounts(pid int) (string, error) {
	name := "/proc/self/mountinfo"
	if pid > 0 {
		name = fmt.Sprintf("/proc/%d/mountinfo", pid)
	}
	b, err := os.ReadFile(name)
	return string(b), err
}

// mountedNow is what each of a container's mounts is in its own mount namespace,
// which is what the kernel mounted when it started: a link followed then stays
// followed, whatever it says since, and a volume shows what it really is. Each
// counts as the server path it is, found through the agent's own list, or as root
// where it's a filesystem of the kernel's own, such as proc or sysfs, or one the
// server has nowhere that a plan doesn't take, such as a disk of its own. A volume
// that's the folder Docker keeps it in is that volume, and reaches nothing more.
// What can't be read counts as root.
func mountedNow(table MountTable, c *docker.Container) (reaches []resource.Access, root bool, err error) {
	if len(c.Mounts) == 0 {
		return nil, false, nil
	}
	if table == nil || c.State.Pid <= 0 {
		return nil, true, errors.New("can't read what the container has mounted, as Docker gives it no process")
	}
	inside, err := table(c.State.Pid)
	if err != nil {
		return nil, true, fmt.Errorf("can't read what the container has mounted: %w", err)
	}
	host, err := table(0)
	if err != nil {
		return nil, true, fmt.Errorf("can't read what this server has mounted: %w", err)
	}
	theirs, ours := kernelmounts.Parse(inside), kernelmounts.Parse(host)
	for _, m := range c.Mounts {
		if m.Type != "bind" && m.Type != "volume" {
			continue
		}
		at, ok := kernelmounts.At(theirs, m.Destination)
		if !ok {
			return nil, true, fmt.Errorf("the container has nothing mounted at %s, where Docker says it has", m.Destination)
		}
		if kernelmounts.Pseudo(at.FSType) {
			return nil, true, nil
		}
		where := kernelmounts.ServerPaths(at, ours)
		if len(where) == 0 {
			if m.Type == "volume" && containerdriver.LocalMountType(at.FSType) {
				continue // a tmpfs, or a remote share
			}
			return nil, true, nil
		}
		for _, p := range where {
			if m.Type == "volume" && volumeFolder(p, m) {
				continue
			}
			reaches = append(reaches, containerdriver.PathAccess(p, !m.RW))
		}
	}
	return reaches, false, nil
}

// volumeFolder says p is where Docker keeps the volume m mounts: the source it
// gives, or, where it gives none, a _data folder under volumes named for it.
func volumeFolder(p string, m docker.MountPoint) bool {
	if m.Source != "" {
		return path.Clean(p) == path.Clean(m.Source)
	}
	return strings.HasSuffix(path.Clean(p), "/volumes/"+m.Name+"/_data")
}

// FollowPaths says where server paths lead on this server, for those that lead
// elsewhere, with the resolver a plan's Leads uses, and needs no Docker. The agent
// itself follows them with container.Paths.Follow, which is the same, with where
// Docker keeps its data.
func FollowPaths(_ context.Context, paths ...string) (map[string]string, error) {
	return containerdriver.Host{Resolve: containerdriver.Resolve}.Leads(paths...)
}

// containerFilter asks Docker for one container, exactly (docker.Exactly).
func containerFilter(ref string) map[string][]string { return docker.Exactly(ref) }

// serverContainer is what the API says of a container: a list's fields, and of its
// labels only those that say which Compose project it is in, as anything else in
// them can hold what a person meant to keep to themselves.
func serverContainer(s *docker.ContainerSummary) *dockwellv1.ServerContainer {
	c := &dockwellv1.ServerContainer{
		Id:             s.ID,
		Name:           plainName(s.Names),
		Image:          s.Image,
		State:          containerState(s.State),
		Status:         s.Status,
		ComposeProject: s.Labels[docker.LabelProject],
		ComposeService: s.Labels[docker.LabelService],
		OneOff:         s.Labels[docker.LabelOneoff] == "True",
	}
	if s.Created > 0 {
		c.CreateTime = timestamppb.New(time.Unix(s.Created, 0))
	}
	ports := publishedPorts(s.Ports)
	c.MorePorts = int32(max(len(ports)-listEach, 0)) //nolint:gosec // a container has far fewer than that
	c.Ports = ports[:min(len(ports), listEach)]
	mounts := mountsOf(s.Mounts)
	c.MoreMounts = int32(max(len(mounts)-listEach, 0)) //nolint:gosec // a container has far fewer than that
	c.Mounts = mounts[:min(len(mounts), listEach)]
	return c
}

// plainName is a container's own name, without Docker's leading slash. A list also
// names a container by each link to it, as /other/alias.
func plainName(names []string) string {
	for _, n := range names {
		if name := strings.TrimPrefix(n, "/"); !strings.Contains(name, "/") {
			return name
		}
	}
	return ""
}

func containerState(state string) dockwellv1.ContainerState {
	switch state {
	case "created":
		return dockwellv1.ContainerState_CONTAINER_STATE_CREATED
	case "running":
		return dockwellv1.ContainerState_CONTAINER_STATE_RUNNING
	case "paused":
		return dockwellv1.ContainerState_CONTAINER_STATE_PAUSED
	case "restarting":
		return dockwellv1.ContainerState_CONTAINER_STATE_RESTARTING
	case "removing":
		return dockwellv1.ContainerState_CONTAINER_STATE_REMOVING
	case "exited":
		return dockwellv1.ContainerState_CONTAINER_STATE_EXITED
	case "dead":
		return dockwellv1.ContainerState_CONTAINER_STATE_DEAD
	}
	return dockwellv1.ContainerState_CONTAINER_STATE_UNSPECIFIED
}

// publishedPorts are the ports a container publishes, by port. Docker lists a port
// on every address once for IPv4 and once for IPv6, which is one port to a person.
func publishedPorts(ports []docker.SummaryPort) []*dockwellv1.ContainerPort {
	type key struct {
		server, inner uint16
		protocol      string
	}
	every := map[key]bool{}
	addresses := map[key][]string{}
	for _, p := range ports {
		if p.PublicPort == 0 {
			continue
		}
		k := key{p.PublicPort, p.PrivatePort, p.Type}
		if p.IP == "" || p.IP == "0.0.0.0" || p.IP == "::" {
			every[k] = true
			continue
		}
		addresses[k] = append(addresses[k], p.IP)
	}
	out := make([]*dockwellv1.ContainerPort, 0, len(every)+len(addresses))
	add := func(k key, address string) {
		out = append(out, &dockwellv1.ContainerPort{ServerPort: uint32(k.server), ContainerPort: uint32(k.inner), Address: address, Protocol: k.protocol})
	}
	for k := range every {
		add(k, "")
	}
	for k, list := range addresses {
		if every[k] {
			continue
		}
		for _, a := range list {
			add(k, a)
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.ContainerPort) int {
		return cmp.Or(cmp.Compare(a.GetServerPort(), b.GetServerPort()), cmp.Compare(a.GetProtocol(), b.GetProtocol()), cmp.Compare(a.GetAddress(), b.GetAddress()))
	})
	return out
}

// mountsOf are a container's volumes and server paths, by where it mounts them. A
// tmpfs holds nothing past a restart, so it isn't one of them.
func mountsOf(mounts []docker.MountPoint) []*dockwellv1.ContainerMount {
	out := make([]*dockwellv1.ContainerMount, 0, len(mounts))
	for _, m := range mounts {
		c := &dockwellv1.ContainerMount{Target: m.Destination, ReadOnly: !m.RW}
		switch m.Type {
		case "volume":
			c.Volume, c.Anonymous = m.Name, docker.IsFullID(m.Name)
		case "bind":
			c.HostPath = m.Source
		default:
			continue
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *dockwellv1.ContainerMount) int { return cmp.Compare(a.GetTarget(), b.GetTarget()) })
	return out
}
