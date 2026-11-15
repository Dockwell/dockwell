// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// A container's links, as Compose's links: and docker run --link make them
// (ADR 0089, "What the take-on reads of links"). On a network of a project's own, a
// link only gives the container another name for the one it names. Docker
// Compose links each by the name the file gives, the linked service's
// <service>-<n>, and its container's name. So a link by a name the linked container
// answers to already, on a network both are on, asks nothing a service can't say,
// and needs nothing. A link by any other name is a name the linked container needs
// beside its own (LinkAlias), which a Compose project taken on whole can give it. A
// link on Docker's default bridge, where names don't resolve otherwise, is refused.

// link is one of a container's links: the container it names, and the name the
// linking container reaches it by.
type link struct{ target, alias string }

// linksOf reads a container's links, from its HostConfig ("/target:/self/alias")
// and each network's endpoint ("target:alias"), each once.
func linksOf(o *docker.Options) []link {
	var out []link
	add := func(target, alias string) {
		l := link{target: strings.TrimPrefix(target, "/"), alias: alias}
		if l.target != "" && l.alias != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	var host []string
	_ = json.Unmarshal(o.HostConfig["Links"], &host)
	for _, s := range host {
		target, rest, _ := strings.Cut(s, ":")
		add(target, rest[strings.LastIndex(rest, "/")+1:])
	}
	for _, ep := range o.NetworkSettings.Networks {
		var list []string
		_ = json.Unmarshal(ep["Links"], &list)
		for _, s := range list {
			target, alias, ok := strings.Cut(s, ":")
			if !ok {
				alias = target
			}
			add(target, alias)
		}
	}
	return out
}

// linkedAs reports a name t answers to on network already, and goes on answering to
// once a replace names it otherwise, as a link by it needs nothing: its own, its
// aliases there, and what others linked to it by there. A numbered name Compose
// links by, as db-1, isn't one: nothing answers to it after a replace, so it's kept
// as one the container takes on.
func linkedAs(t *docker.Container, network, name string) bool {
	return slices.Contains(answersTo(t, network), name) || slices.Contains(strings.Split(t.Config.Labels[docker.LabelLinkedAs], ","), network+"="+name)
}

// othersAnswer reports another container on network than those in except that
// answers to name, so a link by it, which went to one of them, can't be left to the
// network's names to reach (the review of ADR 0097, L4).
func (d *Docker) othersAnswer(ctx context.Context, network, name string, except ...string) (bool, error) {
	list, err := d.Client.ListContainers(ctx, map[string][]string{"network": {network}})
	if err != nil {
		return false, fmt.Errorf("can't list what's on the network %s: %w", network, err)
	}
	for i := range list {
		if slices.Contains(except, list[i].ID) {
			continue
		}
		o, err := d.Client.InspectContainer(ctx, list[i].ID)
		switch {
		case docker.IsNotFound(err):
			continue
		case err != nil:
			return false, err
		}
		if slices.ContainsFunc(answersTo(o, network), func(a string) bool { return strings.EqualFold(a, name) }) {
			return true, nil
		}
	}
	return false, nil
}

// readLinks says what c's links ask: a refusal for each a service can't say, and
// for each by a name its linked container doesn't answer to, the alias it would
// need. One by a name it answers to needs nothing.
func (d *Docker) readLinks(ctx context.Context, c *docker.Container, o *docker.Options) (refused []string, aliases []resource.LinkAlias, err error) {
	for _, l := range linksOf(o) {
		t, err := d.Client.InspectContainer(ctx, l.target)
		switch {
		case docker.IsNotFound(err):
			refused = append(refused, fmt.Sprintf("a link to %s, which isn't on this server", l.target))
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("can't read %s, which it links to: %w", l.target, err)
		}
		network, err := d.linkNetwork(ctx, sharedNetworks(c.NetworkSettings.Networks, t), t.Config.Labels[docker.LabelProject], c.HostConfig.NetworkMode)
		if err != nil {
			return nil, nil, err
		}
		if network == "" {
			refused = append(refused, fmt.Sprintf("a link to %s on no network of a project's own, where a name of its doesn't reach it otherwise", l.target))
			continue
		}
		others, err := d.othersAnswer(ctx, network, l.alias, c.ID, t.ID)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case others:
			refused = append(refused, fmt.Sprintf("a link to %s as %s, which another container on %s answers to as well", l.target, l.alias, network))
		case linkedAs(t, network, l.alias):
		default:
			aliases = append(aliases, resource.LinkAlias{Container: strings.TrimPrefix(t.Name, "/"), Service: t.Config.Labels[docker.LabelService], Alias: l.alias, Network: network})
		}
	}
	return refused, aliases, nil
}

// sharedNetworks are the networks of a project's own that a container on networks
// and t are both on, by name: not Docker's default bridge, the host's or none,
// where names don't reach a container.
func sharedNetworks[V any](networks map[string]V, t *docker.Container) []string {
	var shared []string
	for name := range networks {
		if name == "bridge" || name == "host" || name == "none" {
			continue
		}
		if _, ok := t.NetworkSettings.Networks[name]; ok {
			shared = append(shared, name)
		}
	}
	slices.Sort(shared)
	return shared
}

// linkNetwork is the one of shared a link's name goes on: one the linked
// container's own Compose project has, then the linking container's main
// network, mode, and only then the first by name, as a panel's network shared by
// every app, such as dokploy-network, sorts early, and a name there could reach
// another project's container (the narrow check of ADR 0097, N1). It's "" where
// none is shared.
func (d *Docker) linkNetwork(ctx context.Context, shared []string, project, mode string) (string, error) {
	if len(shared) == 0 {
		return "", nil
	}
	if project != "" {
		for _, name := range shared {
			n, err := d.Client.NetworkNamed(ctx, name)
			switch {
			case docker.IsNotFound(err):
				continue
			case err != nil:
				return "", fmt.Errorf("can't read the network %s, which a link is on: %w", name, err)
			}
			if n.Labels[docker.LabelProject] == project {
				return name, nil
			}
		}
	}
	if slices.Contains(shared, mode) {
		return mode, nil
	}
	return shared[0], nil
}

// linkedNames are the names other containers link to c by, on a network both are
// on, that it doesn't answer to already: taken on, it answers to them as aliases
// of its own, so the links go on reaching it once a replace names it otherwise
// (ADR 0089). It reads each container on the server, as leaningOn does.
func (d *Docker) linkedNames(ctx context.Context, c *docker.Container) ([]string, error) {
	list, err := d.Client.ListContainers(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("can't list what links to this container: %w", err)
	}
	self := strings.TrimPrefix(c.Name, "/")
	var out []string
	for i := range list {
		if list[i].ID == c.ID {
			continue
		}
		o, err := d.Client.InspectOptions(ctx, list[i].ID)
		switch {
		case docker.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("can't read what links to this container: %w", err)
		}
		for _, l := range linksOf(o) {
			if l.target != self && (len(l.target) < 12 || !strings.HasPrefix(c.ID, l.target)) {
				continue
			}
			var mode string
			_ = json.Unmarshal(o.HostConfig["NetworkMode"], &mode)
			network, err := d.linkNetwork(ctx, sharedNetworks(o.NetworkSettings.Networks, c), c.Config.Labels[docker.LabelProject], mode)
			if err != nil {
				return nil, err
			}
			if network != "" && !linkedAs(c, network, l.alias) && !slices.Contains(out, network+"="+l.alias) {
				out = append(out, network+"="+l.alias)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}
