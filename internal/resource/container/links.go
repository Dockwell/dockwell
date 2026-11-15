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
		network := sharedNetwork(c, t)
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

// sharedNetwork is a network of a project's own c and t are both on, or "": not
// Docker's default bridge, the host's or none, where names don't reach a container.
func sharedNetwork(c, t *docker.Container) string {
	var shared []string
	for name := range c.NetworkSettings.Networks {
		if name == "bridge" || name == "host" || name == "none" {
			continue
		}
		if _, ok := t.NetworkSettings.Networks[name]; ok {
			shared = append(shared, name)
		}
	}
	slices.Sort(shared)
	if len(shared) == 0 {
		return ""
	}
	return shared[0]
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
			if network := sharedOf(o, c); network != "" && !linkedAs(c, network, l.alias) && !slices.Contains(out, network+"="+l.alias) {
				out = append(out, network+"="+l.alias)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// sharedOf is a network of a project's own that o and c are both on, or "".
func sharedOf(o *docker.Options, c *docker.Container) string {
	network := ""
	for n := range o.NetworkSettings.Networks {
		if _, ok := c.NetworkSettings.Networks[n]; ok && n != "bridge" && n != "host" && n != "none" && (network == "" || n < network) {
			network = n
		}
	}
	return network
}
