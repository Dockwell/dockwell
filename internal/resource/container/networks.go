// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
)

// A service can join a network the server already has (ADR 0021). It holds the
// network's name in Docker in its own spec, with no resource of its own to go
// with it: so Dockwell has nothing it could make, change or delete, and what a
// service reaches is in the spec a person approved, not one another change can
// swap beneath it.

const (
	// maxNames is how many other names, or networks the server has, one container
	// may ask for. Each network costs the agent a look at what is on it each time
	// it plans.
	maxNames = 64
	// maxNameLength is the longest name a DNS lookup can ask for.
	maxNameLength = 253
)

// dockerOwnNetworks are the networks that mean something to Docker by name, so
// joining one is not joining a network the server has.
var dockerOwnNetworks = map[string]string{
	"host":    "It is the server's own network. Use host_network, as network_mode: host does in a Compose file",
	"none":    "It means no network, which Dockwell can't run yet",
	bridge:    "It is Docker's default network, where a service with no networks goes. Leave it out",
	"default": "It means Docker's default network, where a service with no networks goes. Leave it out",
	// A container on it could take the address of a service that is stopped, and
	// get its traffic (ADR 0024).
	docker.IngressNetwork: "It is the ingress's own, which a route puts a service on. Add a route instead",
}

// validateNetworking checks what a container asks of the server's networks, and
// that each name is one a plan can show as it is.
func validateNetworking(c *resourcev1.Container) error {
	if n := len(c.GetExternalNetworks()); n > maxNames {
		return fmt.Errorf("the container joins %d networks the server has, and Dockwell takes up to %d. Join fewer", n, maxNames)
	}
	if n := len(c.GetAliases()); n > maxNames {
		return fmt.Errorf("the container has %d other names, and Dockwell takes up to %d. Give it fewer", n, maxNames)
	}
	seen := map[string]bool{}
	for _, n := range c.GetExternalNetworks() {
		switch why, reserved := dockerOwnNetworks[n]; {
		case !NameRE.MatchString(n) || len(n) > maxNameLength:
			return fmt.Errorf("%q isn't a valid network name: use letters, digits, dots, dashes and underscores, such as backend", n)
		case reserved:
			return fmt.Errorf("a service can't join %s as a network the server has. %s", n, why)
		case strings.HasPrefix(strings.ToLower(n), serviceForm):
			return fmt.Errorf("the network %s has the form of an environment's, %s<ID>_<name>, so a service can't join it as a network the server has. "+
				"Put both services in one environment, or on a network of the server's own", n, serviceForm)
		case seen[n]:
			return fmt.Errorf("the network %s is listed twice; list each network once", n)
		case slices.Contains(c.GetNetworks(), n):
			return fmt.Errorf("the network %s is listed both as one of the environment's and as one the server has, and a Compose file can't hold both. Rename the environment's", n)
		}
		seen[n] = true
	}
	slices.Sort(c.ExternalNetworks)
	seen = map[string]bool{}
	for _, a := range c.GetAliases() {
		if err := CheckAlias(a); err != nil {
			return fmt.Errorf("%q %w", a, err)
		}
		if seen[strings.ToLower(a)] {
			return fmt.Errorf("the name %s is listed twice; list each name once", a)
		}
		seen[strings.ToLower(a)] = true
	}
	slices.Sort(c.Aliases)
	switch {
	case c.GetHostNetwork() && len(c.GetNetworks())+len(c.GetExternalNetworks()) > 0:
		return errors.New("a service on the server's own network joins no others. Leave out host_network, or its networks")
	case c.GetHostNetwork() && len(c.GetPorts()) > 0:
		return errors.New("a service on the server's own network listens on the server's ports itself, and Docker publishes none for it. Leave out its ports")
	case len(c.GetAliases()) > 0 && len(c.GetNetworks())+len(c.GetExternalNetworks()) == 0:
		return errors.New("a service answers to other names on its networks, and this one has none, so nothing could reach it by them. Put it on a network, or leave out its aliases")
	}
	return nil
}

// CheckAlias says why name can't be another name for a container, in words that go
// after the name. They don't hold it, as a caller may have it from a variable.
func CheckAlias(name string) error {
	switch {
	case !NameRE.MatchString(name) || len(name) > maxNameLength:
		return errors.New("isn't a name a container can answer to: use letters, digits, dots, dashes and underscores, starting with a letter or digit, such as db")
	case strings.HasPrefix(strings.ToLower(name), serviceForm):
		return fmt.Errorf("has the form of a service's container, %s<ID>-<service>-1, so it can't be another name for one. Choose a name that doesn't start with %q", serviceForm, serviceForm)
	}
	return nil
}

// CheckDriver says why name isn't one Docker takes for a driver of a network or a
// volume, such as macvlan or vieux/sshfs:latest. A plan says what a driver reaches by
// its name. It doesn't hold the name, as it may come from a variable.
func CheckDriver(name string) error {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && strings.ContainsRune("_.:/-", r):
		default:
			return errors.New("the driver isn't a name Docker takes: use letters, digits, dots, dashes, underscores, colons and slashes, such as macvlan")
		}
	}
	return nil
}

// checkOnItsOwn refuses what only means something for a service. A container on
// its own names Docker's networks, in networks, and is reached by its name.
func checkOnItsOwn(c *resourcev1.Container) error {
	switch {
	case len(c.GetExternalNetworks()) > 0:
		return errors.New("a container on its own names Docker's networks in networks, so it takes no external_networks. List them there")
	case c.GetHostNetwork():
		return errors.New(`a container on its own names the server's own network in networks, so it takes no host_network. Use networks: ["host"]`)
	case len(c.GetAliases()) > 0:
		return errors.New("a container on its own is reached by its name, and takes no aliases. Name it what others reach it by, or run it as a service")
	}
	return nil
}

// networkAccess is what a service reaches through its networks: each network the
// server has, with the other names it takes there, as a person approves it
// answering to those for whatever else is on the network; and the server's own.
func networkAccess(c *resourcev1.Container) []resource.Access {
	var out []resource.Access
	if c.GetHostNetwork() {
		out = append(out, resource.Access{Kind: resource.AccessNetwork})
	}
	names := strings.Join(c.GetAliases(), ",")
	for _, n := range c.GetExternalNetworks() {
		out = append(out, resource.Access{Kind: resource.AccessExternalNetwork, Path: n, Detail: names})
	}
	return out
}

// Answers are the names a service answers to on each of its networks, so that
// nothing else on one can answer to them too. Its Docker network names those the
// server has, and its environment's are given as they are in the spec.
func Answers(name string, c *resourcev1.Container) []resource.Answer {
	if c.GetHostNetwork() {
		return nil
	}
	names := append([]string{name}, c.GetAliases()...)
	out := make([]resource.Answer, 0, len(names)*(len(c.GetNetworks())+len(c.GetExternalNetworks())))
	for _, a := range names {
		for _, n := range c.GetNetworks() {
			out = append(out, resource.Answer{Network: n, Name: a})
		}
		for _, n := range c.GetExternalNetworks() {
			out = append(out, resource.Answer{Network: n, External: true, Name: a})
		}
	}
	return out
}

// dockerNetworks are the networks a container at t joins, by their names in
// Docker: its environment's first, for a service, or Docker's own for a
// container on its own, then those the server has.
func dockerNetworks(t Target, spec *resourcev1.Container) []string {
	out := make([]string, 0, len(spec.GetNetworks())+len(spec.GetExternalNetworks()))
	for _, n := range spec.GetNetworks() {
		out = append(out, t.DockerName(n))
	}
	return append(out, spec.GetExternalNetworks()...)
}

// endpointAliases are the names a container at t answers to on each network it
// joins: for a service, its service's name and any others. A container on its
// own is reached by its own name.
func endpointAliases(t Target, spec *resourcev1.Container) []string {
	if !t.Scoped {
		return nil
	}
	return append([]string{t.Service}, spec.GetAliases()...)
}

// projectNetworks reads a container's networks back into out. A service's that
// don't carry its project's prefix are ones the server has, so nothing but the
// name tells them apart; and Docker's reports of who it answers to on them hold
// more than the spec asked for, by version, which it leaves out.
func projectNetworks(c *docker.Container, t Target, out *resourcev1.Container) {
	if t.Scoped && c.HostConfig.NetworkMode == "host" {
		out.HostNetwork = true
		return
	}
	// The ingress network is the ingress's to give and take, and no spec names
	// it, so being on it isn't drift (ADR 0024).
	networks := slices.DeleteFunc(slices.Sorted(maps.Keys(c.NetworkSettings.Networks)), func(n string) bool { return n == docker.IngressNetwork })
	if slices.Equal(networks, []string{bridge}) {
		return
	}
	prefix := t.Project + "_"
	names := map[string]bool{}
	for _, n := range networks {
		switch {
		case !t.Scoped:
			out.Networks = append(out.Networks, n)
		case strings.HasPrefix(n, prefix):
			out.Networks = append(out.Networks, strings.TrimPrefix(n, prefix))
		default:
			out.ExternalNetworks = append(out.ExternalNetworks, n)
		}
		if ep := c.NetworkSettings.Networks[n]; t.Scoped && ep != nil {
			for _, a := range ep.Aliases {
				if a != t.Service && a != t.Name && a != strings.TrimPrefix(c.Name, "/") && (len(a) != 12 || !strings.HasPrefix(c.ID, a)) {
					names[a] = true
				}
			}
		}
	}
	out.Aliases = slices.Sorted(maps.Keys(names))
}

// checkRouted refuses taking a service with a route onto the server's own network,
// as Docker can't put it on the ingress's as well, so the route would stop reaching
// it. It looks only as a plan is made, as only a plan knows its other steps: one
// that moves the route too, to a port on the server, can go in either order.
func (d *Docker) checkRouted(ctx context.Context, t Target, spec *resourcev1.Container) error {
	desired := resource.DesiredIn(ctx)
	if !spec.GetHostNetwork() || d.Ingress == nil || !t.Scoped || desired == nil {
		return nil
	}
	routed, err := d.Ingress.RoutedAfter(ctx, ingress.Instance{Project: t.Project, Service: t.Service, Number: 1}, desired)
	if err != nil || !routed {
		return err
	}
	return fmt.Errorf("the service %[1]s has a route, and Docker can't put a container on the server's own network on the ingress's as well, so the route would stop reaching it. "+
		"Send the route to the port %[1]s will listen on with host_port in this change too, or leave out host_network", t.Service)
}

// checkExternalNetworks refuses a service whose networks the server hasn't got,
// by name, as it plans and again as it applies. It looks at what is on each too:
// two containers answering to one name on a network take each other's traffic,
// and only the server sees what other projects run there, or what is on it from
// outside Dockwell. A name is looked up without regard to case, as DNS names are.
func (d *Docker) checkExternalNetworks(ctx context.Context, t Target, spec *resourcev1.Container) error {
	if len(spec.GetExternalNetworks()) == 0 {
		return nil
	}
	ours := map[string]bool{}
	for _, a := range endpointAliases(t, spec) {
		ours[strings.ToLower(a)] = true
	}
	for _, name := range spec.GetExternalNetworks() {
		n, err := d.Client.NetworkNamed(ctx, name)
		switch {
		case docker.IsNotFound(err):
			return fmt.Errorf("the network %s isn't on this server, so %s can't join it, and Dockwell never makes a network it doesn't own. "+
				"Make it with docker network create %s, or take it out of the service", name, t.Service, name)
		case err != nil:
			return err
		}
		list, err := d.Client.ListContainers(ctx, map[string][]string{"network": {n.ID}})
		if err != nil {
			return err
		}
		for _, other := range list {
			if other.Labels[docker.LabelProject] == t.Project && other.Labels[docker.LabelService] == t.Service {
				continue // itself, or what a replace left of it
			}
			c, err := d.Client.InspectContainer(ctx, other.ID)
			switch {
			case docker.IsNotFound(err):
				continue
			case err != nil:
				return err
			}
			theirs := []string{strings.TrimPrefix(c.Name, "/")}
			if ep := c.NetworkSettings.Networks[name]; ep != nil {
				theirs = append(theirs, ep.Aliases...)
			}
			for _, a := range theirs {
				if ours[strings.ToLower(a)] {
					return fmt.Errorf("%s would answer to %s on the network %s, and the container %s there already does, so each could get the other's traffic. "+
						"Rename the service, or change its container_name, or stop that container, then plan again", t.Service, a, name, strings.TrimPrefix(c.Name, "/"))
				}
			}
		}
	}
	return nil
}
