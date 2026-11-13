// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// Stopping the panel an import came from (ADR 0089): its own containers and Swarm
// services, never what it ran, as a step of a change that its undo puts back. Its
// proxy goes on running, as it serves the domains of what was taken on until those
// come as routes, a step of its own. Nothing of the panel is deleted: its database
// and folders stay, so starting it again brings it back as it was.

// What a stop stops of each panel, in the order it stops them: the panel first, so
// it doesn't see its database go, and what it keeps its state in last. A start
// goes the other way.
var stops = map[string][]string{
	resource.PanelCoolify: {"coolify", "coolify-realtime", "coolify-sentinel", "coolify-redis", "coolify-db"},
	resource.PanelDokploy: {"dokploy", "dokploy-redis", "dokploy-postgres"},
}

// proxies are each panel's proxy, which stays.
var proxies = map[string]string{resource.PanelCoolify: "coolify-proxy", resource.PanelDokploy: "dokploy-traefik"}

// stopWait is how long a container has to stop before Docker kills it.
const stopWait = 30 * time.Second

// mostReplicas is the most a start scales a panel's service to: what a panel runs
// of its own is one each, and a start asks no more than a stop read.
const mostReplicas = 16

// Stopper stops and starts a panel's own containers and services, with Docker.
type Stopper struct {
	Docker *docker.Client
}

// PlanPanel reads how the panel's own containers and services stand, and plans
// the stop or the start: false where they stand as asked already. A stop reads
// them; a start, and a stop that resumes, take what its plan kept of how each stood,
// so that's what an undo puts back. Only the panel's own names are acted on, and
// never its proxy.
func (s Stopper) PlanPanel(ctx context.Context, ask resource.PanelAsked) (*resource.Panel, bool, error) {
	if s.Docker == nil {
		return nil, false, errors.New("this agent reads no Docker, so it can't stop or start a panel")
	}
	order, ok := stops[ask.Kind]
	if !ok {
		return nil, false, fmt.Errorf("there's no panel %q to stop: name coolify or dokploy", ask.Kind)
	}
	for _, p := range ask.Parts {
		if !slices.Contains(order, p.Name) {
			return nil, false, fmt.Errorf("%s isn't one of %s's own containers, so Dockwell won't act on it", p.Name, resource.PanelName(ask.Kind))
		}
		if !restartWord(p.WasRestart) || p.WasReplicas > mostReplicas {
			return nil, false, fmt.Errorf("what the plan kept of how %s stood isn't what a stop reads, so Dockwell won't act on it", p.Name)
		}
	}
	plan := &resource.Panel{Kind: ask.Kind, Start: ask.Start, Parts: ask.Parts}
	if proxy := proxies[ask.Kind]; s.present(ctx, proxy) {
		plan.Stays = proxy
	}
	if len(plan.Parts) == 0 && !ask.Start {
		found, err := s.read(ctx, order)
		if err != nil {
			return nil, false, err
		}
		if len(found) == 0 && !s.anyThere(ctx, order) {
			return nil, false, fmt.Errorf("%s doesn't run here: Docker has none of its own containers or services", resource.PanelName(ask.Kind))
		}
		if len(found) == 0 {
			return plan, false, nil
		}
		plan.Parts = found
	}
	if ask.Start {
		// A start goes the other way to a stop: what the panel keeps its state in first.
		plan.Parts = slices.Clone(plan.Parts)
		slices.SortStableFunc(plan.Parts, func(a, b resource.PanelPart) int {
			return slices.Index(order, b.Name) - slices.Index(order, a.Name)
		})
	}
	left := false
	for _, p := range plan.Parts {
		as, err := s.stands(ctx, p)
		if err != nil {
			return nil, false, err
		}
		left = left || !as.like(p, ask.Start)
	}
	return plan, left, nil
}

// ApplyPanel stops or starts each part as planned, saying each, then reads each
// again, and fails where one doesn't stand as asked. Each act is one that's safe
// again: a resume repeats what was done before it stopped.
func (s Stopper) ApplyPanel(ctx context.Context, p resource.Panel, say func(string)) error {
	if s.Docker == nil {
		return errors.New("this agent reads no Docker, so it can't stop or start a panel")
	}
	for _, part := range p.Parts {
		var err error
		switch {
		case part.Swarm && p.Start:
			say(fmt.Sprintf("Scaling the service %s to %d", part.Name, part.WasReplicas))
			err = s.Docker.ScaleService(ctx, part.Name, part.WasReplicas)
		case part.Swarm:
			say(fmt.Sprintf("Scaling the service %s to none", part.Name))
			err = s.Docker.ScaleService(ctx, part.Name, 0)
		case p.Start:
			say("Starting " + part.Name)
			if part.WasRestart != "" {
				err = s.Docker.UpdateRestartPolicy(ctx, part.Name, docker.RestartPolicy{Name: part.WasRestart})
			}
			if err == nil && part.WasRunning {
				err = s.start(ctx, part.Name)
			}
		default:
			// Its restart policy first: one that's always would start it again with
			// Docker or the server.
			say("Stopping " + part.Name)
			err = s.Docker.UpdateRestartPolicy(ctx, part.Name, docker.RestartPolicy{Name: "no"})
			if err == nil {
				err = s.Docker.StopContainer(ctx, part.Name, stopWait)
			}
		}
		if err != nil {
			return fmt.Errorf("can't %s %s: %w", verb(p.Start), part.Name, err)
		}
	}
	for _, part := range p.Parts {
		as, err := s.stands(ctx, part)
		if err != nil {
			return err
		}
		if !as.like(part, p.Start) {
			return fmt.Errorf("%s doesn't stand as the step left it: %s", part.Name, as.words(part.Swarm))
		}
	}
	return nil
}

func verb(start bool) string {
	if start {
		return "start"
	}
	return "stop"
}

// start starts a container, where it isn't running already.
func (s Stopper) start(ctx context.Context, name string) error {
	c, err := s.Docker.InspectContainer(ctx, name)
	if err != nil {
		return err
	}
	if c.State.Running {
		return nil
	}
	return s.Docker.StartContainer(ctx, name)
}

// anyThere reports any of names on the server.
func (s Stopper) anyThere(ctx context.Context, names []string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return s.present(ctx, n) })
}

// present reports a container or service of that name on the server.
func (s Stopper) present(ctx context.Context, name string) bool {
	if _, err := s.Docker.InspectContainer(ctx, name); err == nil {
		return true
	}
	_, err := s.Docker.InspectService(ctx, name)
	return err == nil
}

// read finds each of the panel's own that a stop has to stop, in order, as it
// stands now: a Swarm service where the swarm has one of that name, or a container.
func (s Stopper) read(ctx context.Context, order []string) ([]resource.PanelPart, error) {
	var out []resource.PanelPart
	for _, name := range order {
		svc, err := s.Docker.InspectService(ctx, name)
		if err == nil {
			n, replicated := svc.Spec.Mode.Replicas()
			if !replicated {
				return nil, fmt.Errorf("the service %s runs one task on each node, so Dockwell can't scale it to none", name)
			}
			if n > 0 {
				out = append(out, resource.PanelPart{Name: name, Swarm: true, WasRunning: true, WasReplicas: n})
			}
			continue
		}
		c, err := s.Docker.InspectContainer(ctx, name)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("can't read the container %s: %w", name, err)
		}
		// One that's stopped, and stays so with Docker and the server, has nothing
		// to stop.
		if restart := c.HostConfig.RestartPolicy.Name; c.State.Running || restart != "" && restart != "no" {
			out = append(out, resource.PanelPart{Name: name, WasRunning: c.State.Running, WasRestart: restart})
		}
	}
	return out, nil
}

// standing is how a part stands now.
type standing struct {
	gone     bool
	running  bool
	restart  string
	replicas uint64
}

func (s Stopper) stands(ctx context.Context, p resource.PanelPart) (standing, error) {
	if p.Swarm {
		svc, err := s.Docker.InspectService(ctx, p.Name)
		if docker.IsNotFound(err) {
			return standing{gone: true}, nil
		}
		if err != nil {
			return standing{}, fmt.Errorf("can't read the service %s: %w", p.Name, err)
		}
		n, _ := svc.Spec.Mode.Replicas()
		return standing{running: n > 0, replicas: n}, nil
	}
	c, err := s.Docker.InspectContainer(ctx, p.Name)
	if docker.IsNotFound(err) {
		return standing{gone: true}, nil
	}
	if err != nil {
		return standing{}, fmt.Errorf("can't read the container %s: %w", p.Name, err)
	}
	return standing{running: c.State.Running, restart: c.HostConfig.RestartPolicy.Name}, nil
}

// like reports a part standing as a stop or a start leaves it. One that's gone has
// nothing left to stop, and nothing a start can bring back: a person removed it.
func (st standing) like(p resource.PanelPart, start bool) bool {
	switch {
	case st.gone:
		return !start
	case p.Swarm && start:
		return st.replicas == p.WasReplicas
	case p.Swarm:
		return st.replicas == 0
	case start:
		return (p.WasRestart == "" || st.restart == p.WasRestart) && (st.running || !p.WasRunning)
	default:
		return !st.running && (st.restart == "no" || st.restart == "")
	}
}

// words says how a part stands, for a step that left it otherwise.
func (st standing) words(swarm bool) string {
	switch {
	case st.gone:
		return "it isn't there any more"
	case swarm:
		return fmt.Sprintf("it runs %d replicas", st.replicas)
	case st.running:
		return fmt.Sprintf("it's running, with the restart policy %q", st.restart)
	default:
		return fmt.Sprintf("it's stopped, with the restart policy %q", st.restart)
	}
}

// restartWord reports a restart policy Docker has, or none.
func restartWord(w string) bool {
	switch w {
	case "", "no", "always", "unless-stopped", "on-failure":
		return true
	}
	return false
}
