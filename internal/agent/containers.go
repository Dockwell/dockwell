// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
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
	return &agentv1.AdoptionResult{Spec: b, Refused: read.Refused, Shows: read.Shows, Manager: read.Manager, Folder: read.Folder}, nil
}

// containerFilter asks Docker for one container, exactly: a name goes through its
// filter as a whole, so one that a prefix of an ID would also match can't find
// another container (ADR 0021).
func containerFilter(ref string) map[string][]string {
	switch {
	case ref == "":
		return nil
	case isFullID(ref):
		return map[string][]string{"id": {ref}}
	}
	return map[string][]string{"name": {"^/" + regexp.QuoteMeta(ref) + "$"}}
}

func isFullID(ref string) bool {
	if len(ref) != 64 {
		return false
	}
	_, err := hex.DecodeString(ref)
	return err == nil
}

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
			c.Volume, c.Anonymous = m.Name, isFullID(m.Name)
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
