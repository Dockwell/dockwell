// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// stopWait is how long a container has to stop before Docker kills it.
const stopWait = 30 * time.Second

// mostReplicas is the most a start scales a panel's service to: what a panel runs
// of its own is one each, and a start asks no more than a stop read.
const mostReplicas = 16

// defaultGrace is how long Docker gives a task to stop where its service doesn't
// say, and taskMargin how much longer a scale to none waits for it to be over: for
// Swarm to act on the new spec, and to kill what didn't stop.
const (
	defaultGrace = 10 * time.Second
	taskMargin   = time.Minute
)

// Stopper stops and starts a panel's own containers and services, with Docker.
type Stopper struct {
	Docker *docker.Client
	// TaskWait is how long a scale waits for the service's tasks to be over, its
	// grace period and taskMargin where it's zero; a test sets less.
	TaskWait time.Duration
	// BeforeProxy readies what takes the proxy's ports, Dockwell's ingress, by
	// pulling its image, so the time its domains are down never holds a pull. A
	// failure leaves the proxy as it is.
	BeforeProxy func(context.Context) error
	// Check checks a handover's chosen domains again, just before the proxy stops:
	// PanelEvidence of what it found, or why one stays behind now (Reader.Check).
	// Without it, a stop of the proxy is refused.
	Check func(ctx context.Context, kind string, domains []string) (string, error)
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
	if ask.Apps {
		return s.planApps(ctx, ask)
	}
	order, ok := stops[ask.Kind]
	if !ok {
		return nil, false, fmt.Errorf("there's no panel %q to stop: name coolify or dokploy", ask.Kind)
	}
	proxy := resource.PanelProxy(ask.Kind)
	if ask.Proxy {
		// Its proxy alone, as its domains come over to Dockwell's ingress.
		order = []string{proxy}
	}
	for _, p := range ask.Parts {
		if !slices.Contains(order, p.Name) {
			return nil, false, fmt.Errorf("%s isn't one of %s's own containers, so Dockwell won't act on it", p.Name, resource.PanelName(ask.Kind))
		}
		if !restartWord(p.WasRestart) || p.WasReplicas > mostReplicas {
			return nil, false, fmt.Errorf("what the plan kept of how %s stood isn't what a stop reads, so Dockwell won't act on it", p.Name)
		}
	}
	plan := &resource.Panel{Kind: ask.Kind, Start: ask.Start, Proxy: ask.Proxy, Parts: ask.Parts}
	if ask.Proxy && !ask.Start {
		plan.Domains, plan.Evidence = ask.Domains, ask.Evidence
	}
	if !ask.Proxy && s.present(ctx, proxy) {
		plan.Stays = proxy
	}
	if len(plan.Parts) == 0 && !ask.Start {
		found, err := s.read(ctx, order)
		if err != nil {
			return nil, false, err
		}
		if len(found) == 0 && !s.anyThere(ctx, order) {
			if ask.Proxy {
				return nil, false, fmt.Errorf("%s's proxy, %s, isn't on this server, so there's nothing holding ports 80 and 443 for it", resource.PanelName(ask.Kind), proxy)
			}
			return nil, false, fmt.Errorf("%s doesn't run here: Docker has none of its own containers or services", resource.PanelName(ask.Kind))
		}
		if len(found) == 0 {
			return plan, false, nil
		}
		plan.Parts = found
	}
	if len(plan.Parts) == 0 && ask.Start {
		// Started with no stop's plan to put back, each that's stopped starts, with
		// the restart policy the panel's installer gives its own.
		found, err := s.readStopped(ctx, order)
		if err != nil {
			return nil, false, err
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

// planApps plans scaling Swarm services of what Dokploy runs to none, as each is
// made again in Dockwell, or back to what each ran (ADR 0089). The control plane
// names them, so each must be a service Dokploy labelled as an app's, never one of
// its own or another's; a stop reads how each stands, and a stop that resumes or a
// start takes what its plan kept.
func (s Stopper) planApps(ctx context.Context, ask resource.PanelAsked) (*resource.Panel, bool, error) {
	if ask.Kind != resource.PanelDokploy || len(ask.Parts) == 0 {
		return nil, false, errors.New("only Dokploy runs apps as Swarm services, so name its services to scale")
	}
	plan := &resource.Panel{Kind: ask.Kind, Start: ask.Start, Apps: true}
	left := false
	for _, p := range ask.Parts {
		svc, err := s.Docker.InspectService(ctx, p.Name)
		if err != nil {
			return nil, false, fmt.Errorf("can't read the Swarm service %s: %w", p.Name, err)
		}
		l := svc.Spec.Labels
		if slices.Contains(stops[resource.PanelDokploy], p.Name) || p.Name == resource.PanelProxy(resource.PanelDokploy) ||
			!dokployOwns(l) {
			return nil, false, fmt.Errorf("%s isn't a service Dokploy runs for an app, so Dockwell won't scale it", p.Name)
		}
		n, replicated := svc.Spec.Mode.Replicas()
		if !replicated {
			return nil, false, fmt.Errorf("the service %s runs one task on each node, so Dockwell can't scale it to none", p.Name)
		}
		part := resource.PanelPart{Name: p.Name, Swarm: true, WasRunning: n > 0, WasReplicas: n, WasPorts: portsOf(svc.Spec.EndpointSpec.Ports), WasNetworks: networksOf(&svc.Spec)}
		if p.WasReplicas > 0 || ask.Start {
			if p.WasReplicas > mostReplicas {
				return nil, false, fmt.Errorf("what the plan kept of how %s stood isn't what a stop reads, so Dockwell won't act on it", p.Name)
			}
			part.WasRunning, part.WasReplicas, part.WasPorts, part.WasNetworks = p.WasRunning, p.WasReplicas, p.WasPorts, p.WasNetworks
		}
		live, err := s.tasksLive(ctx, p.Name)
		if err != nil {
			return nil, false, err
		}
		if ask.Start {
			left = left || n != part.WasReplicas
		} else {
			// Asking for none isn't done while a task isn't over: a stop that
			// resumes after a restart waits again, as its copy would start beside it.
			left = left || n != 0 || live > 0
		}
		plan.Parts = append(plan.Parts, part)
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
	if p.Proxy && !p.Start {
		// What the person chose from must still read as it did: the domains' checks
		// run again, and a digest that differs from the plan's leaves the proxy be.
		if err := s.checkAgain(ctx, p); err != nil {
			return err
		}
	}
	if p.Proxy && !p.Start && s.BeforeProxy != nil {
		say("Pulling the image of Dockwell's ingress, before the proxy stops")
		if err := s.BeforeProxy(ctx); err != nil {
			return fmt.Errorf("can't ready Dockwell's ingress, so %s goes on running: %w", p.Parts[0].Name, err)
		}
	}
	for _, part := range p.Parts {
		var err error
		switch {
		case part.Swarm && p.Start && p.Apps:
			// What took its place stops first, and nothing of its own may still be
			// stopping: Swarm's scale up starts a task at once.
			err = s.untilNoTask(ctx, part.Name, say, "after %s, so Dockwell doesn't scale it back up beside it")
			if err == nil {
				say(fmt.Sprintf("Scaling the service %s to %d, with the ports it published and the networks it joined", part.Name, part.WasReplicas))
				err = s.Docker.PlaceService(ctx, part.Name, part.WasReplicas, servicePorts(part.WasPorts), json.RawMessage(part.WasNetworks))
			}
		case part.Swarm && p.Start:
			say(fmt.Sprintf("Scaling the service %s to %d", part.Name, part.WasReplicas))
			err = s.Docker.ScaleService(ctx, part.Name, part.WasReplicas)
		case part.Swarm && p.Apps:
			// Its ports and networks come off too: Swarm's routing mesh holds a
			// published port for as long as the service publishes it, so its copy
			// couldn't, and its name would answer on a network beside its copy's.
			say(fmt.Sprintf("Scaling the service %s to none, and taking it off its ports and networks", part.Name))
			err = s.Docker.PlaceService(ctx, part.Name, 0, nil, nil)
			if err == nil {
				err = s.untilNoTask(ctx, part.Name, say, "%s after it was scaled to none, so nothing takes its place")
			}
		case part.Swarm:
			// A scale changes the spec at once, and Swarm stops the task after: what
			// takes its place, such as a copy on the same data, waits for that.
			say(fmt.Sprintf("Scaling the service %s to none", part.Name))
			err = s.Docker.ScaleService(ctx, part.Name, 0)
			if err == nil {
				err = s.untilNoTask(ctx, part.Name, say, "%s after it was scaled to none, so nothing takes its place")
			}
		case p.Start:
			// Started first, and its policy put back after: one that doesn't start
			// stays at no, so Docker or a reboot doesn't keep trying it.
			say("Starting " + part.Name)
			if part.WasRunning {
				err = s.start(ctx, part.Name)
			}
			if err == nil && part.WasRestart != "" {
				err = s.Docker.UpdateRestartPolicy(ctx, part.Name, docker.RestartPolicy{Name: part.WasRestart})
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

// checkAgain checks a handover's chosen domains again, before its proxy stops, and
// refuses where one stays behind now or what was found differs from the plan's.
func (s Stopper) checkAgain(ctx context.Context, p resource.Panel) error {
	proxy := resource.PanelProxy(p.Kind)
	switch {
	case len(p.Domains) == 0 || p.Evidence == "":
		return fmt.Errorf("this plan names no domains a person chose to hand over, so %s goes on running. Plan the handover again", proxy)
	case s.Check == nil:
		return fmt.Errorf("this agent can't check the domains again, so %s goes on running. Update the agent, then plan the handover again", proxy)
	}
	got, err := s.Check(ctx, p.Kind, p.Domains)
	if err != nil {
		return fmt.Errorf("%s goes on running, as a domain no longer reads as the plan found it: %w. Plan the handover again", proxy, err)
	}
	if got != p.Evidence {
		return fmt.Errorf("%s goes on running, as what Dockwell checked of %s changed since the plan. Plan the handover again to see what changed", proxy, strings.Join(p.Domains, ", "))
	}
	return nil
}

// portsOf are the ports a Swarm service publishes, as a plan keeps them.
func portsOf(ports []docker.ServicePort) []resource.PanelPort {
	var out []resource.PanelPort
	for _, p := range ports {
		out = append(out, resource.PanelPort{Protocol: p.Protocol, Target: p.TargetPort, Published: p.PublishedPort, Mode: p.PublishMode})
	}
	return out
}

// networksOf are the networks a Swarm service joins, as Docker has them: in its
// task template, or where an old client named them, beside it. A start puts them
// back in its task template, which Docker reads the same.
func networksOf(s *docker.ServiceSpec) string {
	if len(s.TaskTemplate.Networks) > 0 {
		return string(s.TaskTemplate.Networks)
	}
	return string(s.Networks)
}

// servicePorts are the ports a plan kept, as Docker takes them.
func servicePorts(ports []resource.PanelPort) []docker.ServicePort {
	out := make([]docker.ServicePort, 0, len(ports))
	for _, p := range ports {
		out = append(out, docker.ServicePort{Protocol: p.Protocol, TargetPort: p.Target, PublishedPort: p.Published, PublishMode: p.Mode})
	}
	return out
}

// tasksLive counts what of a Swarm service may still run: each task Swarm's tasks
// API doesn't call over, on every node, starting ones too, and each container of
// its tasks on this node that hasn't exited. Both count: on a real Swarm, an
// update that asks for none and changes the task template, as taking the service
// off its ports and networks does, drops its task from the tasks API at once,
// while its container still shuts down, such as a database for two seconds, and a
// copy that started then would run beside it on the same data.
func (s Stopper) tasksLive(ctx context.Context, service string) (int, error) {
	tasks, err := s.Docker.ListTasks(ctx, service)
	if err != nil {
		return 0, fmt.Errorf("can't list the tasks of the service %s: %w", service, err)
	}
	n := 0
	for i := range tasks {
		if !tasks[i].Over() {
			n++
		}
	}
	list, err := s.Docker.ListContainers(ctx, map[string][]string{"label": {"com.docker.swarm.service.name=" + service}})
	if err != nil {
		return 0, fmt.Errorf("can't list the containers of the service %s: %w", service, err)
	}
	for i := range list {
		if containerLive(list[i].State) {
			n++
		}
	}
	return n, nil
}

// containerLive says a task's container, in a state Docker names, may run what a
// copy would run beside: running, restarting, paused or being removed. One made
// whose start failed, which Swarm leaves behind and a scale to none doesn't take
// away, isn't, nor one that exited or died: a task really about to start is one
// the tasks API doesn't call over.
func containerLive(state string) bool {
	switch state {
	case "running", "restarting", "paused", "removing":
		return true
	}
	return false
}

// untilNoTask waits for a Swarm service to have no task that isn't over, and fails
// where one still is once its stop's grace period and taskMargin more have passed,
// or TaskWait where a test sets it, saying why as when holds, with the wait in it.
func (s Stopper) untilNoTask(ctx context.Context, service string, say func(string), when string) error {
	wait := s.TaskWait
	if wait == 0 {
		grace := defaultGrace
		if svc, err := s.Docker.InspectService(ctx, service); err == nil && svc.Spec.TaskTemplate.ContainerSpec.StopGracePeriod != nil {
			grace = time.Duration(*svc.Spec.TaskTemplate.ContainerSpec.StopGracePeriod)
		}
		wait = grace + taskMargin
	}
	deadline := time.Now().Add(wait)
	said := false
	for {
		n, err := s.tasksLive(ctx, service)
		if err != nil || n == 0 {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service %s still has a task that isn't over "+when, service, wait.Round(time.Second))
		}
		if !said {
			say(fmt.Sprintf("Waiting for the task of %s to stop", service))
			said = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
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

// ownPolicies are the restart policies each panel's installer gives its own, where
// it isn't always: Coolify's proxy and sentinel restart unless stopped (Coolify's
// RESTART_MODE, and its sentinel's docker run). Dokploy's Traefik is always.
var ownPolicies = map[string]string{"coolify-proxy": "unless-stopped", "coolify-sentinel": "unless-stopped"}

// readStopped finds each of the panel's own, in order, that's stopped: a container
// not running, which starts with the policy it had, or where a stop left it at no,
// the one the panel's installer gives it (ownPolicies); a Swarm service at none,
// which scales to one, as each runs.
func (s Stopper) readStopped(ctx context.Context, order []string) ([]resource.PanelPart, error) {
	var out []resource.PanelPart
	for _, name := range order {
		if svc, err := s.Docker.InspectService(ctx, name); err == nil {
			if n, replicated := svc.Spec.Mode.Replicas(); replicated && n == 0 {
				out = append(out, resource.PanelPart{Name: name, Swarm: true, WasRunning: true, WasReplicas: 1})
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
		if !c.State.Running {
			restart := c.HostConfig.RestartPolicy.Name
			if restart == "" || restart == "no" {
				restart = cmp.Or(ownPolicies[name], "always")
			}
			out = append(out, resource.PanelPart{Name: name, WasRunning: true, WasRestart: restart})
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
	// tasks are how many containers of a Swarm service's tasks run on this node.
	tasks int
	// ports are how many ports a Swarm service publishes.
	ports int
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
		tasks, err := s.tasksLive(ctx, p.Name)
		if err != nil {
			return standing{}, err
		}
		return standing{running: n > 0 || tasks > 0, replicas: n, tasks: tasks, ports: len(svc.Spec.EndpointSpec.Ports)}, nil
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
		return st.replicas == p.WasReplicas && (len(p.WasPorts) == 0 || st.ports == len(p.WasPorts))
	case p.Swarm:
		return st.replicas == 0 && st.tasks == 0 && (len(p.WasPorts) == 0 || st.ports == 0)
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
	case swarm && st.ports > 0 && st.replicas == 0 && st.tasks == 0:
		if st.ports == 1 {
			return "it runs none, and still publishes a port"
		}
		return fmt.Sprintf("it runs none, and still publishes %d ports", st.ports)
	case swarm && st.replicas == 0 && st.tasks > 0:
		return fmt.Sprintf("it asks for none, and %d of its tasks still run", st.tasks)
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
