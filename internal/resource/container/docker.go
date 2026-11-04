// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// LabelSet lists what a container's spec sets that its image could set
// too: its command, entrypoint, user, working directory, health check, and
// each environment variable and label by key. Docker fills in from the
// image what a container leaves out, so a value the image has could be
// either. Reading it back, the label says which, so neither a spec that
// repeats its image nor what the image adds shows as drift.
const LabelSet = docker.OwnPrefix + "set"

// Docker is a Runtime on the Docker Engine.
type Docker struct {
	Client *docker.Client
	// Settle is how long a container without a health check must keep
	// running after it starts to count as healthy. Zero means 2 s.
	Settle time.Duration
	// HealthWait bounds waiting for a health check to pass. Zero means 5
	// minutes.
	HealthWait time.Duration
	// Poll is how often Healthy looks. Zero means 250 ms.
	Poll time.Duration
}

var _ Runtime = (*Docker)(nil)

// Labels are what a container at t carries for spec: the spec's own, the
// ones Compose gives a service's container, and Dockwell's own.
func Labels(t Target, spec *resourcev1.Container) map[string]string {
	l := maps.Clone(spec.GetLabels())
	if l == nil {
		l = map[string]string{}
	}
	l[docker.LabelProject] = t.Project
	l[docker.LabelService] = t.Service
	l[docker.LabelNumber] = "1"
	l[docker.LabelOneoff] = "False"
	maps.Copy(l, OwnLabels(spec))
	return l
}

// OwnLabels are Dockwell's own labels for spec, which an exported Compose
// file carries too, so a container docker compose makes from it reads
// back the same. A spec that sets nothing its image could needs none.
func OwnLabels(spec *resourcev1.Container) map[string]string {
	var set []string
	add := func(name string, ok bool) {
		if ok {
			set = append(set, name)
		}
	}
	add("command", len(spec.GetCommand()) > 0)
	add("entrypoint", len(spec.GetEntrypoint()) > 0)
	add("user", spec.GetUser() != "")
	add("working_dir", spec.GetWorkingDir() != "")
	if hc := spec.GetHealthcheck(); hc != nil {
		set = append(set, "healthcheck")
		add("healthcheck.interval", hc.GetInterval() != "")
		add("healthcheck.timeout", hc.GetTimeout() != "")
		add("healthcheck.start_period", hc.GetStartPeriod() != "")
		add("healthcheck.retries", hc.GetRetries() != 0)
	}
	for k := range spec.GetEnv() {
		set = append(set, "env:"+k)
	}
	for k := range spec.GetLabels() {
		set = append(set, "label:"+k)
	}
	if len(set) == 0 {
		return nil
	}
	slices.Sort(set)
	b, _ := json.Marshal(set)
	return map[string]string{LabelSet: string(b)}
}

func setOf(labels map[string]string) map[string]bool {
	var list []string
	_ = json.Unmarshal([]byte(labels[LabelSet]), &list)
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

var restartNames = map[resourcev1.RestartPolicy]string{
	resourcev1.RestartPolicy_RESTART_POLICY_NO:             "no",
	resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE:     "on-failure",
	resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED: "unless-stopped",
	resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS:         "always",
}

func restartPolicy(name string) resourcev1.RestartPolicy {
	if name == "" {
		return resourcev1.RestartPolicy_RESTART_POLICY_NO
	}
	for p, n := range restartNames {
		if n == name {
			return p
		}
	}
	return resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED
}

func protocolName(p resourcev1.Protocol) string {
	if p == resourcev1.Protocol_PROTOCOL_UDP {
		return "udp"
	}
	return "tcp"
}

// healthcheck is a spec's health check as Docker takes it.
func healthcheck(hc *resourcev1.Healthcheck) *docker.Healthcheck {
	if hc == nil {
		return nil
	}
	d := func(s string) time.Duration {
		v, _ := time.ParseDuration(s)
		return v
	}
	return &docker.Healthcheck{
		Test: hc.GetTest(), Interval: d(hc.GetInterval()), Timeout: d(hc.GetTimeout()),
		StartPeriod: d(hc.GetStartPeriod()), Retries: int(hc.GetRetries()),
	}
}

// anonymous reports a volume Docker made for a path an image declares,
// which is named by 64 hex digits.
func anonymous(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// createRequest is what Docker is asked to create for spec at t. Volumes
// Docker made for paths the image declares move from old, the container
// it replaces, if any, so their data stays, as Compose keeps them.
func (d *Docker) createRequest(t Target, spec *resourcev1.Container, old *docker.Container) (*docker.CreateRequest, []string) {
	req := &docker.CreateRequest{Config: docker.Config{
		Image:       spec.GetImage(),
		Cmd:         spec.GetCommand(),
		Entrypoint:  spec.GetEntrypoint(),
		Labels:      Labels(t, spec),
		User:        spec.GetUser(),
		WorkingDir:  spec.GetWorkingDir(),
		Healthcheck: healthcheck(spec.GetHealthcheck()),
	}}
	req.Env = environment(spec)
	req.HostConfig.RestartPolicy = docker.RestartPolicy{Name: restartNames[spec.GetRestart()]}
	for _, p := range spec.GetPorts() {
		key := fmt.Sprintf("%d/%s", p.GetContainerPort(), protocolName(p.GetProtocol()))
		if req.HostConfig.PortBindings == nil {
			req.HostConfig.PortBindings = map[string][]docker.PortBinding{}
			req.ExposedPorts = map[string]struct{}{}
		}
		req.ExposedPorts[key] = struct{}{}
		req.HostConfig.PortBindings[key] = append(req.HostConfig.PortBindings[key],
			docker.PortBinding{HostIP: p.GetHostIp(), HostPort: strconv.Itoa(int(p.GetHostPort()))})
	}
	targets := map[string]bool{}
	for _, m := range spec.GetMounts() {
		targets[m.GetTarget()] = true
		switch src := m.GetSource().(type) {
		case *resourcev1.Mount_Volume:
			req.HostConfig.Mounts = append(req.HostConfig.Mounts, docker.Mount{
				Type: "volume", Source: t.DockerName(src.Volume), Target: m.GetTarget(), ReadOnly: m.GetReadOnly(),
			})
		case *resourcev1.Mount_HostPath:
			bind := src.HostPath + ":" + m.GetTarget()
			if m.GetReadOnly() {
				bind += ":ro"
			}
			req.HostConfig.Binds = append(req.HostConfig.Binds, bind)
		}
	}
	if old != nil {
		for _, m := range old.Mounts {
			if m.Type == "volume" && anonymous(m.Name) && !targets[m.Destination] {
				req.HostConfig.Mounts = append(req.HostConfig.Mounts, docker.Mount{Type: "volume", Source: m.Name, Target: m.Destination})
			}
		}
	}
	var aliases []string
	if t.Scoped {
		aliases = []string{t.Service}
	}
	var later []string
	for i, n := range spec.GetNetworks() {
		name := t.DockerName(n)
		if i == 0 {
			req.HostConfig.NetworkMode = name
		} else if !d.Client.AtLeast("1.44") {
			// Older engines take one network at create; the rest join after.
			later = append(later, name)
			continue
		}
		if req.NetworkingConfig.EndpointsConfig == nil {
			req.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{}
		}
		req.NetworkingConfig.EndpointsConfig[name] = &docker.EndpointSettings{Aliases: aliases}
	}
	return req, later
}

// environment is a spec's environment as Docker takes it. Values are
// plain for now; the agent resolves secret references here once
// environments hold them (#29).
func environment(spec *resourcev1.Container) []string {
	keys := slices.Sorted(maps.Keys(spec.GetEnv()))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+spec.GetEnv()[k])
	}
	return out
}

// Project reads a container as Docker reports it back into a spec, at t,
// leaving out what Docker took from its image: an image's environment,
// labels, command and the rest, unless the spec set them too, as LabelSet
// says; Dockwell's and Compose's own labels; and anything Docker adds,
// such as IDs, start times and the volumes it made for the image.
func Project(c *docker.Container, img *docker.Config, t Target) *resourcev1.Container {
	if img == nil {
		img = &docker.Config{}
	}
	set := setOf(c.Config.Labels)
	out := &resourcev1.Container{
		Image:   c.Config.Image,
		Restart: restartPolicy(c.HostConfig.RestartPolicy.Name),
	}
	if set["command"] || !slices.Equal(c.Config.Cmd, img.Cmd) {
		out.Command = c.Config.Cmd
	}
	if set["entrypoint"] || !slices.Equal(c.Config.Entrypoint, img.Entrypoint) {
		out.Entrypoint = c.Config.Entrypoint
	}
	if set["user"] || c.Config.User != img.User {
		out.User = c.Config.User
	}
	if set["working_dir"] || c.Config.WorkingDir != img.WorkingDir {
		out.WorkingDir = c.Config.WorkingDir
	}
	imageEnv := map[string]string{}
	for _, kv := range img.Env {
		k, v, _ := strings.Cut(kv, "=")
		imageEnv[k] = v
	}
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		if iv, ok := imageEnv[k]; set["env:"+k] || !ok || iv != v {
			if out.Env == nil {
				out.Env = map[string]string{}
			}
			out.Env[k] = v
		}
	}
	for k, v := range c.Config.Labels {
		if docker.IsOwnLabel(k) {
			continue
		}
		if iv, ok := img.Labels[k]; set["label:"+k] || !ok || iv != v {
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			out.Labels[k] = v
		}
	}
	out.Healthcheck = projectHealthcheck(c.Config.Healthcheck, img.Healthcheck, set)
	for key, bindings := range c.HostConfig.PortBindings {
		port, proto, _ := strings.Cut(key, "/")
		cp, _ := strconv.ParseUint(port, 10, 16)
		protocol := resourcev1.Protocol_PROTOCOL_TCP
		if proto == "udp" {
			protocol = resourcev1.Protocol_PROTOCOL_UDP
		}
		for _, b := range bindings {
			hp, _ := strconv.ParseUint(b.HostPort, 10, 16)
			out.Ports = append(out.Ports, &resourcev1.PortMapping{
				HostPort: uint32(hp), ContainerPort: uint32(cp), Protocol: protocol, HostIp: b.HostIP,
			})
		}
	}
	unscope := func(name string) string {
		if prefix := t.Project + "_"; t.Scoped && strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
		return name
	}
	for _, m := range c.HostConfig.Mounts {
		switch {
		case m.Type == "volume" && m.Source != "" && !anonymous(m.Source):
			out.Mounts = append(out.Mounts, &resourcev1.Mount{
				Source: &resourcev1.Mount_Volume{Volume: unscope(m.Source)}, Target: m.Target, ReadOnly: m.ReadOnly,
			})
		case m.Type == "bind":
			out.Mounts = append(out.Mounts, &resourcev1.Mount{
				Source: &resourcev1.Mount_HostPath{HostPath: m.Source}, Target: m.Target, ReadOnly: m.ReadOnly,
			})
		}
	}
	for _, b := range c.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		m := &resourcev1.Mount{Target: parts[1], ReadOnly: len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro")}
		switch {
		case strings.HasPrefix(parts[0], "/"):
			m.Source = &resourcev1.Mount_HostPath{HostPath: parts[0]}
		case anonymous(parts[0]):
			continue
		default:
			m.Source = &resourcev1.Mount_Volume{Volume: unscope(parts[0])}
		}
		out.Mounts = append(out.Mounts, m)
	}
	networks := slices.Sorted(maps.Keys(c.NetworkSettings.Networks))
	if !slices.Equal(networks, []string{"bridge"}) {
		for _, n := range networks {
			out.Networks = append(out.Networks, unscope(n))
		}
	}
	return out
}

// projectHealthcheck reads a container's health check, leaving out what
// Docker took from the image's. A container whose spec sets no check has
// the image's whole; one whose spec sets one has the image's values only
// in the fields the spec left out, as LabelSet says.
func projectHealthcheck(c, img *docker.Healthcheck, set map[string]bool) *resourcev1.Healthcheck {
	if c == nil {
		return nil
	}
	if img == nil {
		img = &docker.Healthcheck{}
	}
	if !set["healthcheck"] && slices.Equal(c.Test, img.Test) &&
		c.Interval == img.Interval && c.Timeout == img.Timeout && c.StartPeriod == img.StartPeriod && c.Retries == img.Retries {
		return nil
	}
	// fromImage reports a value Docker filled in from the image.
	fromImage := func(field string, same bool) bool { return set["healthcheck"] && !set["healthcheck."+field] && same }
	dur := func(field string, v, iv time.Duration) string {
		if v == 0 || fromImage(field, v == iv) {
			return ""
		}
		return v.String()
	}
	out := &resourcev1.Healthcheck{
		Test:        c.Test,
		Interval:    dur("interval", c.Interval, img.Interval),
		Timeout:     dur("timeout", c.Timeout, img.Timeout),
		StartPeriod: dur("start_period", c.StartPeriod, img.StartPeriod),
	}
	if c.Retries > 0 && !fromImage("retries", c.Retries == img.Retries) {
		out.Retries = uint32(c.Retries) //nolint:gosec // retries are few
	}
	return out
}

// find returns the container named t.Name exactly, or nil. Docker takes a
// name as a reference that can match another container's ID, so every
// lookup goes through here, and what follows acts on the ID it returns.
func (d *Docker) find(ctx context.Context, name string) (*docker.Container, error) {
	c, err := d.Client.ContainerNamed(ctx, name)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	return c, err
}

// Inspect reads the container at t, and its image's defaults.
func (d *Docker) Inspect(ctx context.Context, t Target) (*resourcev1.Container, error) {
	c, err := d.find(ctx, t.Name)
	if err != nil || c == nil {
		return nil, err
	}
	var defaults *docker.Config
	img, err := d.Client.InspectImage(ctx, c.Image)
	switch {
	case err == nil:
		defaults = img.Config
	case !docker.IsNotFound(err):
		return nil, err
	}
	return Project(c, defaults, t), nil
}

// ensureImage pulls an image that isn't on the server yet. One that is
// stays as it is, as docker compose up leaves it. It asks for the image
// with its tag: Docker takes a name without one, made of hex digits such
// as cafe, as a prefix of any image's ID too.
func (d *Docker) ensureImage(ctx context.Context, ref string) error {
	_, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	if !docker.IsNotFound(err) {
		return err
	}
	resource.Progress(ctx, "pulling "+ref)
	err = d.Client.PullImage(ctx, ref, func(p string) { resource.Progress(ctx, "pulling "+ref+", "+p) })
	if err != nil && !docker.IsUnavailable(err) && ctx.Err() == nil {
		return fmt.Errorf("can't pull %s: %w. Check the image's name and tag; Dockwell can't pull private images yet", ref, err)
	}
	return err
}

// checkRefs refuses a container whose networks aren't on the server, as
// Docker would take a missing network's name as a prefix of another's ID;
// and a service whose volumes aren't, before Docker would make an
// unmanaged volume of the name.
func (d *Docker) checkRefs(ctx context.Context, t Target, spec *resourcev1.Container) error {
	if !t.Scoped {
		for _, n := range spec.GetNetworks() {
			if _, err := d.Client.NetworkNamed(ctx, n); docker.IsNotFound(err) {
				return fmt.Errorf("the network %s isn't on this server, so %s can't join it. Make it with docker network create %s, or leave it out", n, t.Name, n)
			} else if err != nil {
				return err
			}
		}
		return nil
	}
	for _, m := range spec.GetMounts() {
		v := m.GetVolume()
		if v == "" {
			continue
		}
		if _, err := d.Client.InspectVolume(ctx, t.DockerName(v)); docker.IsNotFound(err) {
			return fmt.Errorf("the volume %s isn't on this server, so %s can't mount it. Add it to the environment, in this change or an earlier one", v, t.Service)
		} else if err != nil {
			return err
		}
	}
	for _, n := range spec.GetNetworks() {
		if _, err := d.Client.NetworkNamed(ctx, t.DockerName(n)); docker.IsNotFound(err) {
			return fmt.Errorf("the network %s isn't on this server, so %s can't join it. Add it to the environment, in this change or an earlier one", n, t.Service)
		} else if err != nil {
			return err
		}
	}
	return nil
}

// Run creates and starts the container, replacing any of the name. The new
// one is made, under a name of its own, before the old one stops, so only
// stopping one and starting the other is downtime.
func (d *Docker) Run(ctx context.Context, t Target, spec *resourcev1.Container) error {
	if err := d.ensureImage(ctx, spec.GetImage()); err != nil {
		return err
	}
	if err := d.checkRefs(ctx, t, spec); err != nil {
		return err
	}
	old, err := d.find(ctx, t.Name)
	if err != nil {
		return err
	}
	req, later := d.createRequest(t, spec, old)
	name := t.Name
	if old != nil {
		// As Compose names a container it is about to swap in.
		name = old.ID[:12] + "_" + t.Name
		if left, err := d.find(ctx, name); err != nil {
			return err
		} else if left != nil {
			_ = d.Client.RemoveContainer(ctx, left.ID, true) // a try that failed before
		}
	}
	id, err := d.Client.CreateContainer(ctx, name, req)
	if err != nil {
		return fmt.Errorf("docker can't create %s: %w", t.Name, err)
	}
	discard := func(err error) error {
		_ = d.Client.RemoveContainer(context.WithoutCancel(ctx), id, true)
		return err
	}
	var aliases []string
	if t.Scoped {
		aliases = []string{t.Service}
	}
	for _, n := range later {
		if err := d.Client.ConnectNetwork(ctx, n, id, aliases); err != nil {
			return discard(fmt.Errorf("can't put %s on the network %s: %w", t.Name, n, err))
		}
	}
	if old != nil {
		resource.Progress(ctx, "stopping the old container")
		if err := d.Client.StopContainer(ctx, old.ID, StopTimeout); err != nil && !docker.IsNotFound(err) {
			return discard(fmt.Errorf("can't stop the old %s: %w", t.Name, err))
		}
		if err := d.Client.RemoveContainer(ctx, old.ID, true); err != nil && !docker.IsNotFound(err) {
			return discard(fmt.Errorf("can't remove the old %s: %w", t.Name, err))
		}
		if err := d.Client.RenameContainer(ctx, id, t.Name); err != nil {
			return fmt.Errorf("removed the old %s, but can't give the new one its name: %w", t.Name, err)
		}
	}
	resource.Progress(ctx, "starting")
	if err := d.Client.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("docker can't start %s: %w", t.Name, err)
	}
	return nil
}

// SetRestart changes the restart policy in place.
func (d *Docker) SetRestart(ctx context.Context, t Target, policy resourcev1.RestartPolicy) error {
	c, err := d.find(ctx, t.Name)
	switch {
	case err != nil:
		return err
	case c == nil:
		return fmt.Errorf("there's no container %s on this server to change. Plan the change again, which makes it", t.Name)
	}
	return d.Client.UpdateRestartPolicy(ctx, c.ID, docker.RestartPolicy{Name: restartNames[policy]})
}

// Remove stops the container, giving it StopTimeout, and removes it. Its
// volumes stay: an environment's are resources of their own, and those
// Docker made for its image stay until a prune.
func (d *Docker) Remove(ctx context.Context, t Target) error {
	c, err := d.find(ctx, t.Name)
	if err != nil || c == nil {
		return err
	}
	err = d.Client.StopContainer(ctx, c.ID, StopTimeout)
	switch {
	case docker.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if err := d.Client.RemoveContainer(ctx, c.ID, true); err != nil && !docker.IsNotFound(err) {
		return err
	}
	return nil
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Healthy waits for the container to pass its health check, or, without
// one, to keep running for Settle. One that exits, keeps restarting or
// fails its check isn't healthy. Messages name the container and how to
// see why, never what it printed, which may hold secrets.
func (d *Docker) Healthy(ctx context.Context, t Target) error {
	ctx, cancel := context.WithTimeout(ctx, or(d.HealthWait, 5*time.Minute))
	defer cancel()
	settle := or(d.Settle, 2*time.Second)
	poll := time.NewTicker(or(d.Poll, 250*time.Millisecond))
	defer poll.Stop()
	start := time.Now()
	restarts := -1
	// Watch the container of the name, by its ID.
	found, err := d.find(ctx, t.Name)
	switch {
	case err != nil && ctx.Err() == nil:
		return err
	case err == nil && found == nil:
		return fmt.Errorf("%s isn't on this server, though it was just started. See if someone removed it with docker ps -a", t.Name)
	}
	for found != nil {
		c, err := d.Client.InspectContainer(ctx, found.ID)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if docker.IsNotFound(err) {
				return fmt.Errorf("%s was removed while Dockwell waited for it to start. See who removed it with docker events", t.Name)
			}
			return err
		}
		if restarts < 0 {
			restarts = c.RestartCount
		}
		s := c.State
		switch {
		case s.Restarting || c.RestartCount > restarts:
			return fmt.Errorf("%s keeps restarting: it exited with code %d. See why with docker logs %s", t.Name, s.ExitCode, t.Name)
		case !s.Running:
			return fmt.Errorf("%s stopped right after it started, with exit code %d. See why with docker logs %s", t.Name, s.ExitCode, t.Name)
		case s.Health != nil && s.Health.Status == "healthy":
			return nil
		case s.Health != nil && s.Health.Status == "unhealthy":
			return fmt.Errorf("%s runs, but fails its health check. See why with docker inspect --format '{{json .State.Health}}' %s", t.Name, t.Name)
		case s.Health == nil && time.Since(start) >= settle:
			return nil
		}
		select {
		case <-ctx.Done():
		case <-poll.C:
			continue
		}
		break
	}
	if err := ctx.Err(); err != nil && !isDeadline(err) {
		return err
	}
	return fmt.Errorf("%s didn't pass its health check within %s. See why with docker inspect --format '{{json .State.Health}}' %s",
		t.Name, human.Duration(or(d.HealthWait, 5*time.Minute)), t.Name)
}

func isDeadline(err error) bool { return err == context.DeadlineExceeded } //nolint:errorlint // ctx.Err() isn't wrapped

// Ready checks Docker answers, and agrees an API version with it.
func (d *Docker) Ready(ctx context.Context) error { return d.Client.Ping(ctx) }
