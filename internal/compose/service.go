// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// takeBuilds takes build out of each service that has image beside it, as nothing
// here reads it: Dockwell builds nothing. Its variables need no value, and the
// transforms that would read it never see it. It lists those services, for the
// plan to say. A service with build alone is refused, as there is nothing to run.
func takeBuilds(raw map[string]any) ([]string, error) {
	services, _ := raw["services"].(map[string]any)
	var built []string
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc, _ := services[name].(map[string]any)
		if _, ok := svc["build"]; !ok {
			continue
		}
		if _, ok := svc["image"]; !ok {
			return nil, invalidAt(place{"the service " + name, "build"}, "the service %s %s", name, why["build"])
		}
		delete(svc, "build")
		built = append(built, name)
	}
	return built, nil
}

// takeResources takes each service's limits out of a read file, in both the forms
// Compose takes them, and leaves compose-go's types out of it: its amounts read
// any number they don't expect as nothing, which would drop a limit without a word.
// Docker has no CPU reservation outside Swarm, and docker compose drops it, so it
// is dropped here too, and the plan says so.
func takeResources(dict map[string]any) (map[string]*resourcev1.Resources, []Notice, error) {
	services, _ := dict["services"].(map[string]any)
	out := map[string]*resourcev1.Resources{}
	var notices []Notice
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc, _ := services[name].(map[string]any)
		what := "the service " + name
		resources, _ := mapAt(svc["deploy"], "resources")
		limits, _ := mapAt(resources, "limits")
		reservations, _ := mapAt(resources, "reservations")
		cpus, err := either(what, "cpus", svc["cpus"], "deploy.resources.limits.cpus", limits["cpus"], container.CanonicalCPUs)
		if err != nil {
			return nil, nil, err
		}
		memory, err := either(what, "mem_limit", svc["mem_limit"], "deploy.resources.limits.memory", limits["memory"], container.CanonicalMemory)
		if err != nil {
			return nil, nil, err
		}
		reserve, err := either(what, "mem_reservation", svc["mem_reservation"], "deploy.resources.reservations.memory", reservations["memory"], container.CanonicalMemory)
		if err != nil {
			return nil, nil, err
		}
		if r, err := container.CanonicalCPUs(amount(reservations["cpus"])); err != nil {
			return nil, nil, invalid("%s's deploy.resources.reservations.cpus: %v", what, err)
		} else if r != "" {
			notices = append(notices, Notice{
				Reason: "cpu_reservation", Service: name, Names: []string{r},
				Message: fmt.Sprintf("%s reserves %s CPUs, which only Swarm can keep, so nothing holds them for it, as with docker compose. A limit is kept", name, r),
			})
		}
		if cpus != "" || memory != "" || reserve != "" {
			out[name] = &resourcev1.Resources{Cpus: cpus, Memory: memory, MemoryReservation: reserve}
		}
		for _, k := range []string{"cpus", "mem_limit", "mem_reservation"} {
			delete(svc, k)
		}
		// Only the limits come out: how a service is replaced stays for compose-go.
		deploy, _ := svc["deploy"].(map[string]any)
		delete(deploy, "resources")
		if len(deploy) == 0 {
			delete(svc, "deploy")
		}
	}
	return out, notices, nil
}

// mapAt is the mapping at key in a value that may be a mapping, or nothing.
func mapAt(v any, key string) (map[string]any, bool) {
	m, _ := v.(map[string]any)
	inner, ok := m[key].(map[string]any)
	return inner, ok
}

// amount is a value in a file as text, in the form YAML wrote it.
func amount(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// either reads a limit Compose takes in two places, which must agree, as compose-go's
// loader has them.
func either(what, shortName string, short any, longName string, long any, read func(string) (string, error)) (string, error) {
	a, err := read(amount(short))
	if err != nil {
		return "", invalid("%s's %s: %v", what, shortName, err)
	}
	b, err := read(amount(long))
	if err != nil {
		return "", invalid("%s's %s: %v", what, longName, err)
	}
	if a != "" && b != "" && a != b {
		return "", invalid("%s sets %s to %s and %s to %s, which say different things. Keep one of them", what, shortName, a, longName, b)
	}
	return cmp.Or(a, b), nil
}

// readNetworkMode reads what a service's network_mode means to Dockwell: host,
// bridge, or a network's name, which reads as that network. One the file doesn't
// list is one on the server, as docker compose takes it, which a person approves
// by name. Those that share what another container has stay refused, by name.
func (m *model) readNetworkMode(what string, s *types.ServiceConfig) error {
	mode := s.NetworkMode
	if mode != "" && len(s.Networks) > 0 {
		return invalidAt(place{what, "network_mode"}, "%s has both network_mode and networks, which Compose doesn't take together. Keep one of them", what)
	}
	kind, _, shared := strings.Cut(mode, ":")
	switch {
	case mode == "", mode == "bridge", mode == "host":
		return nil
	case mode == "none":
		return invalidAt(place{what, "network_mode"}, "%s has network_mode none, no network at all, which Dockwell can't run yet. Remove it, or put the service on a network", what)
	case shared && (kind == "service" || kind == "container"):
		return invalidAt(place{what, "network_mode"}, "%s has network_mode %s, sharing another container's network, which Dockwell can't run yet. Put both on one network, where each reaches the other by its name", what, mode)
	case shared:
		return invalidAt(place{what, "network_mode"}, "%s has network_mode %s, which Dockwell can't run yet. Use host, bridge or a network's name", what, mode)
	}
	if _, listed := m.Networks[mode]; !listed {
		if m.Networks == nil {
			m.Networks = types.Networks{}
		}
		m.Networks[mode] = types.NetworkConfig{External: true}
	}
	s.Networks = map[string]*types.ServiceNetworkConfig{mode: nil}
	s.NetworkMode = ""
	return nil
}

// joinNetworks puts a service on what its file says: networks of its environment,
// networks the server has, or the server's own, and gives it the other names it
// answers to there, which its container_name is one of.
func joinNetworks(name string, s *types.ServiceConfig, c *resourcev1.Container, p *model) error {
	what := "the service " + name
	if s.NetworkMode == "host" {
		c.HostNetwork = true
		if len(c.Ports) > 0 {
			var published []string
			for _, port := range c.Ports {
				published = append(published, strconv.FormatUint(uint64(port.GetHostPort()), 10))
			}
			c.Ports = nil
			p.notices = append(p.notices, Notice{
				Reason: "host_network_ports", Service: name, Names: published,
				Message: name + " is on the server's own network, where Docker publishes no ports, as with docker compose, so it listens on the server's ports itself, whatever the file lists",
			})
		}
	}
	var aliases []string
	for i, key := range slices.Sorted(maps.Keys(s.Networks)) {
		n := p.Networks[key]
		if bool(n.External) {
			c.ExternalNetworks = append(c.ExternalNetworks, cmp.Or(n.Name, key))
		} else {
			c.Networks = append(c.Networks, key)
		}
		var here []string
		if cfg := s.Networks[key]; cfg != nil {
			here = slices.Sorted(slices.Values(cfg.Aliases))
		}
		if i > 0 && !slices.Equal(here, aliases) {
			return invalid("%s has other aliases on the network %s than on its others, and Dockwell gives a service one set of names on all its networks. Give each the same aliases", what, key)
		}
		aliases = here
	}
	c.Aliases = aliases
	joined := len(c.Networks)+len(c.ExternalNetworks) > 0
	own := s.ContainerName
	if own != "" && own != name {
		// On no network it is never an alias, so nothing else checks it, and a plan
		// says it either way. It doesn't repeat it, as it may come from a variable.
		if err := container.CheckAlias(own); err != nil {
			return invalid("%s's container_name %v", what, err)
		}
	}
	switch {
	case own == "" || own == name:
	case joined:
		if !slices.Contains(c.Aliases, own) {
			c.Aliases = append(c.Aliases, own)
		}
		p.notices = append(p.notices, Notice{
			Reason: "container_name", Service: name, Names: []string{own},
			Message: fmt.Sprintf("%s runs under the name Dockwell gives it, not %s, as Dockwell names what it runs as Compose does. Other containers on its networks still reach it by %s", name, own, own),
		})
	default:
		p.notices = append(p.notices, Notice{
			Reason: "container_name", Service: name, Names: []string{own},
			Message: fmt.Sprintf("%s runs under the name Dockwell gives it, not %s, and has no network of its own to answer to that name on, so nothing reaches it by %s", name, own, own),
		})
	}
	return nil
}
