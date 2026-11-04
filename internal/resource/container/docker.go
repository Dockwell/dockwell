// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/ingress"
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
	// Deleted reports a volume deleted through a plan, which the server
	// keeps for its data: the agent's volume.Kept. A service can't mount
	// one, as it isn't in its environment. Nil means none are kept.
	Deleted Deleted
	// CleanupWait bounds removing a container that a change made but
	// couldn't swap in. Zero means 30 s.
	CleanupWait time.Duration
	// Logger hears what the runtime can't do on its own time, such as
	// cleaning up after a change that failed. Nil means slog's default.
	Logger *slog.Logger
	// Paths finds what no container may mount on this server, and follows
	// symlinks there. Nil checks only what Validate does.
	Paths *Paths
	// Ingress is this server's ingress, which a service with routes joins,
	// and which holds the server's public ports while it has routes. Nil
	// means none.
	Ingress Ingress
	// Logs keeps the logs of the containers a change removes, which Docker
	// removes with them. Nil keeps none.
	Logs LogKeeper
}

// Ingress is what the container driver needs of a server's ingress
// (internal/ingress).
type Ingress interface {
	// Endpoint says how a new container of a service's first instance joins
	// the ingress network, and whether it does.
	Endpoint(ctx context.Context, instance ingress.Instance) (network string, ep docker.EndpointSettings, ok bool, err error)
	// PublicPort reports a port the ingress holds while it has routes.
	PublicPort(ctx context.Context, port uint32, udp bool) (bool, error)
	// RoutedAfter reports a route that still sends to an instance's service once
	// the plan under way applies.
	RoutedAfter(ctx context.Context, instance ingress.Instance, desired []resource.Desired) (bool, error)
}

// Labels of a container that say which deploy made it, which logs are read
// by. They are Dockwell's own, so they aren't drift, and a container without
// them, as one docker compose made, isn't recreated for them.
const (
	LabelRevision = docker.OwnPrefix + "revision"
	LabelChange   = docker.OwnPrefix + "change"
)

// LogKeeper keeps the log of a container about to be removed. A failure to
// keep one never stops the change: it says so in its own log.
type LogKeeper interface {
	// Begin copies what the container's log holds so far, while it still
	// runs, so the step's downtime doesn't wait for that. failed says the
	// container failed, so its log outlives those of containers replaced in
	// the ordinary way.
	Begin(ctx context.Context, c *docker.Container, failed bool) LogKeep
}

// LogKeep is one container's log being kept.
type LogKeep interface {
	// Finish copies what the container wrote since Begin, once it has
	// stopped and before it is removed, for up to LogCatchUp.
	Finish(ctx context.Context, c *docker.Container)
}

// LogCatchUp bounds what a replace waits for a stopped container's last
// lines, which counts as downtime.
const LogCatchUp = time.Second

// Deleted is the list of volumes the server keeps for 7 days once a plan
// takes them out of use: the agent's volume.Kept. It has an environment's
// volumes, which a service can't mount, as they aren't in its environment
// any more; and the volumes Docker made for a container's image, which
// deleting the container, or mounting something else at their paths,
// leaves behind, and which a container of the same name mounts again.
type Deleted interface {
	// Has reports a volume on the list, by its name in Docker.
	Has(name string) (bool, error)
	// KeepImageVolumes lists the volumes a container's image kept data in,
	// by the path each was mounted at, from now.
	KeepImageVolumes(container string, byPath map[string]string) error
	// ImageVolumes returns the volumes listed for a container's image, by
	// path, and when each was listed.
	ImageVolumes(container string) (map[string]string, map[string]time.Time, error)
	// TakeImageVolumes takes the volumes at paths off the list, as the
	// container mounts them again; undo puts them back.
	TakeImageVolumes(container string, paths []string) (undo func(), err error)
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
	l[docker.LabelConfigHash] = ConfigHash(spec)
	maps.Copy(l, OwnLabels(spec))
	return l
}

// ConfigHash is the config hash a container carries for spec, without
// which docker compose doesn't take it as its project's: compose ps leaves
// it out, and compose up fails on its name. It is a SHA-256 of the spec
// with its environment's values left out, as they may be secrets, so it
// changes with the spec. It never equals the hash compose computes from
// its own types, so docker compose up from an export recreates each
// container once, and compose's own hash is on it from then on.
func ConfigHash(spec *resourcev1.Container) string {
	c := proto.CloneOf(spec)
	for k := range c.GetEnv() {
		c.Env[k] = ""
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	add("no_entrypoint", spec.GetNoEntrypoint())
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

// restartOf is a spec's restart policy as Docker takes it.
func restartOf(spec *resourcev1.Container) docker.RestartPolicy {
	return docker.RestartPolicy{Name: restartNames[spec.GetRestart()], MaximumRetryCount: int(spec.GetRestartRetries())}
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
// it replaces, if any, so their data stays, as Compose keeps them. Where
// old has none, one kept for the container's image, as a deletion left it,
// is mounted again: those are returned, by path.
func (d *Docker) createRequest(ctx context.Context, t Target, spec *resourcev1.Container, old *docker.Container, kept map[string]string) (*docker.CreateRequest, []string, map[string]string) {
	labels := Labels(t, spec)
	if deploy := resource.DeployOf(ctx); deploy.Revision > 0 {
		labels[LabelRevision] = strconv.Itoa(deploy.Revision)
		labels[LabelChange] = deploy.Change
	}
	req := &docker.CreateRequest{Config: docker.Config{
		Image:       spec.GetImage(),
		Cmd:         spec.GetCommand(),
		Entrypoint:  spec.GetEntrypoint(),
		Labels:      labels,
		User:        spec.GetUser(),
		WorkingDir:  spec.GetWorkingDir(),
		Healthcheck: healthcheck(spec.GetHealthcheck()),
	}}
	if spec.GetNoEntrypoint() {
		// Empty, not left out, as Compose sends entrypoint: [].
		req.Entrypoint = []string{}
	}
	req.Env = environment(spec)
	req.HostConfig.RestartPolicy = restartOf(spec)
	req.HostConfig.Privileged, req.HostConfig.CapAdd, req.HostConfig.CapDrop, req.HostConfig.GroupAdd =
		spec.GetPrivileged(), spec.GetCapAdd(), spec.GetCapDrop(), spec.GetGroupAdd()
	req.HostConfig.NanoCPUs = cpuNanos(spec.GetResources().GetCpus())
	req.HostConfig.Memory = memoryBytes(spec.GetResources().GetMemory())
	req.HostConfig.MemoryReservation = memoryBytes(spec.GetResources().GetMemoryReservation())
	req.HostConfig.LogConfig = d.logConfig(ctx)
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
			req.HostConfig.Binds = append(req.HostConfig.Binds, bind(src.HostPath, m))
		}
	}
	from := imageVolumes(old)
	restored := map[string]string{}
	for _, p := range slices.Sorted(maps.Keys(kept)) {
		if _, ok := from[p]; !ok && !targets[p] {
			from[p], restored[p] = kept[p], kept[p]
		}
	}
	for _, p := range slices.Sorted(maps.Keys(from)) {
		if !targets[p] {
			req.HostConfig.Mounts = append(req.HostConfig.Mounts, docker.Mount{Type: "volume", Source: from[p], Target: p})
		}
	}
	if spec.GetHostNetwork() {
		req.HostConfig.NetworkMode = "host"
		return req, nil, restored
	}
	aliases := endpointAliases(t, spec)
	var later []string
	for i, name := range dockerNetworks(t, spec) {
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
	return req, later, restored
}

// logConfig is how Docker keeps a new container's log. Docker's default,
// json-file, keeps it without a limit, which can fill a server's disk, so it
// gets rotation. Any other default is the operator's choice, which stands:
// local rotates by itself, and journald and the like keep logs elsewhere. A
// container's log config isn't part of its spec, so it can't read as drift,
// and it comes in with the next replace, never by itself.
func (d *Docker) logConfig(ctx context.Context) *docker.LogConfig {
	info, err := d.Client.Info(ctx)
	if err != nil || info.LoggingDriver == "" || info.LoggingDriver == docker.LogDriverJSONFile {
		return docker.Rotation()
	}
	return nil
}

// failedBefore reports a container that wasn't well when a step came to
// replace it, so its log is worth keeping longer than an ordinary one's.
func failedBefore(c *docker.Container) bool {
	s := c.State
	switch {
	case s.Restarting, s.Health != nil && s.Health.Status == "unhealthy":
		return true
	default:
		return !s.Running && s.ExitCode != 0
	}
}

// beginKeepingLogs starts keeping the log of a container a step is about to
// remove. Dockwell's rollback removes what failed, whatever its state says.
func (d *Docker) beginKeepingLogs(ctx context.Context, c *docker.Container) LogKeep {
	if d.Logs == nil {
		return nil
	}
	return d.Logs.Begin(ctx, c, resource.DeployOf(ctx).Rollback || failedBefore(c))
}

// finishKeepingLogs takes in a stopped container's last lines, and how it
// ended, before it is removed.
func (d *Docker) finishKeepingLogs(ctx context.Context, keep LogKeep, c *docker.Container) {
	if keep == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), LogCatchUp)
	defer cancel()
	if stopped, err := d.Client.InspectContainer(ctx, c.ID); err == nil {
		c = stopped
	}
	keep.Finish(ctx, c)
}

// imageVolumes are the volumes Docker made for c's image, by the path
// each is mounted at. They hold data, such as a database's, that no spec
// names.
func imageVolumes(c *docker.Container) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	for _, m := range c.Mounts {
		if m.Type == "volume" && anonymous(m.Name) {
			out[m.Destination] = m.Name
		}
	}
	return out
}

// covered are the volumes of c's image at paths spec mounts something
// else at, which a replace leaves behind.
func covered(c *docker.Container, spec *resourcev1.Container) map[string]string {
	out := map[string]string{}
	for p, name := range imageVolumes(c) {
		if slices.ContainsFunc(spec.GetMounts(), func(m *resourcev1.Mount) bool { return m.GetTarget() == p }) {
			out[p] = name
		}
	}
	return out
}

// keptVolumes are the volumes kept for the image of the container at t,
// by path, that are still on the server.
func (d *Docker) keptVolumes(ctx context.Context, t Target) (map[string]string, error) {
	if d.Deleted == nil {
		return nil, nil
	}
	byPath, _, err := d.Deleted.ImageVolumes(t.Name)
	if err != nil {
		return nil, err
	}
	for p, name := range byPath {
		_, err := d.Client.InspectVolume(ctx, name)
		switch {
		case docker.IsNotFound(err):
			delete(byPath, p)
		case err != nil:
			return nil, err
		}
	}
	return byPath, nil
}

// keep lists the volumes of the container at t's image as kept, which a
// change took out of use. They stay on the server whatever happens: one
// the list can't take is logged, and stays until a docker volume prune.
func (d *Docker) keep(t Target, byPath map[string]string) {
	if d.Deleted == nil || len(byPath) == 0 {
		return
	}
	if err := d.Deleted.KeepImageVolumes(t.Name, byPath); err != nil {
		d.logger().Warn("can't list the volumes a container's image kept data in, so they stay on the server until docker volume prune removes them",
			slog.String("container", t.Name), slog.Any("volumes", byPath), slog.String("error", err.Error()))
	}
}

// relabels are Docker's options for relabelling a server path.
var relabels = map[resourcev1.Relabel]string{resourcev1.Relabel_RELABEL_SHARED: "z", resourcev1.Relabel_RELABEL_PRIVATE: "Z"}

// bind is a server path's mount as Docker's binds have it, such as
// /srv/data:/data:ro,z.
func bind(source string, m *resourcev1.Mount) string {
	var options []string
	if m.GetReadOnly() {
		options = append(options, "ro")
	}
	if o, ok := relabels[m.GetRelabel()]; ok {
		options = append(options, o)
	}
	b := source + ":" + m.GetTarget()
	if len(options) > 0 {
		b += ":" + strings.Join(options, ",")
	}
	return b
}

// environment is a spec's environment as Docker takes it. For a service
// it holds the values of what the service uses, which the service driver
// resolved in memory for this one call (binding.Environment), so nothing
// here may log it or keep it. The config hash leaves its values out too.
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
		//nolint:gosec // a count of restarts is small, and Docker keeps it as an int
		RestartRetries: uint32(max(c.HostConfig.RestartPolicy.MaximumRetryCount, 0)),
		Privileged:     c.HostConfig.Privileged,
		CapAdd:         c.HostConfig.CapAdd,
		CapDrop:        c.HostConfig.CapDrop,
		GroupAdd:       c.HostConfig.GroupAdd,
		Resources:      projectResources(&c.HostConfig),
	}
	if set["command"] || !slices.Equal(c.Config.Cmd, img.Cmd) {
		out.Command = c.Config.Cmd
	}
	// Docker gives a container the image's entrypoint unless it was asked
	// for none: one with none, of an image that has one, runs none. The
	// label tells for an image without one.
	switch {
	case len(c.Config.Entrypoint) == 0 && (set["no_entrypoint"] || len(img.Entrypoint) > 0):
		out.NoEntrypoint = true
	case set["entrypoint"] || !slices.Equal(c.Config.Entrypoint, img.Entrypoint):
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
		port, transport, _ := strings.Cut(key, "/")
		cp, _ := strconv.ParseUint(port, 10, 16)
		protocol := resourcev1.Protocol_PROTOCOL_TCP
		if transport == "udp" {
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
		m := &resourcev1.Mount{Target: parts[1]}
		if len(parts) > 2 {
			for _, o := range strings.Split(parts[2], ",") {
				switch o {
				case "ro":
					m.ReadOnly = true
				case "z", "Z":
					// Dockwell relabels a server path alone.
					if strings.HasPrefix(parts[0], "/") {
						m.Relabel = resourcev1.Relabel_RELABEL_SHARED
						if o == "Z" {
							m.Relabel = resourcev1.Relabel_RELABEL_PRIVATE
						}
					}
				}
			}
		}
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
	projectNetworks(c, t, out)
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
	resource.Progress(ctx, "Pulling "+ref)
	err = d.Client.PullImage(ctx, ref, func(p string) { resource.Progress(ctx, "Pulling "+ref+": "+p) })
	if err != nil && !docker.IsUnavailable(err) && ctx.Err() == nil {
		return pullFailure(ref, err)
	}
	return err
}

// PullAgain asks for a container's image again, as a deploy of the tag it already
// names does, and says whether what it got is what runs, and is running. What it
// finds goes on record, before anything is replaced. With pulled, a run stopped
// partway has done the pull, so it needs no registry. See "What pull means" in ADR
// 0021.
func (d *Docker) PullAgain(ctx context.Context, t Target, spec *resourcev1.Container, pulled bool) (same bool, err error) {
	ref := spec.GetImage()
	old, err := d.current(ctx, t)
	if err != nil {
		return false, err
	}
	// Only a container of this tag can say what it pointed at. Another image is
	// there after a failure, and what replaces that isn't a tag that moved.
	moving := old != nil && resource.BareImage(old.Config.Image) == resource.BareImage(ref)
	previous := ""
	if moving {
		if img, err := d.Client.InspectImage(ctx, old.Image); err == nil {
			previous = img.Digest(ref)
		}
	}
	if !pulled {
		if err := d.pullAgain(ctx, t, old, ref); err != nil {
			return false, err
		}
	}
	img, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	if err != nil {
		return false, fmt.Errorf("pulled %s, but can't read it back: %w", ref, err)
	}
	digest := img.Digest(ref)
	if old != nil && old.Image == img.ID && notRunning(old) == "" {
		resource.Report(ctx, resource.Outcome{Digest: digest})
		return true, nil
	}
	resource.Report(ctx, resource.Outcome{Digest: digest, Previous: previous, Replacing: moving && old.Image != img.ID})
	return false, nil
}

// pullAgain pulls ref. One that fails leaves what runs running, and fails the
// step. Where nothing is running there's nothing to keep safe, so the image the
// server has will do, which is how someone gets a service back while its registry
// is down.
func (d *Docker) pullAgain(ctx context.Context, t Target, old *docker.Container, ref string) error {
	resource.Progress(ctx, "pulling "+ref+" again")
	err := d.Client.PullImage(ctx, ref, func(p string) { resource.Progress(ctx, "pulling "+ref+" again, "+p) })
	if err == nil || docker.IsUnavailable(err) || ctx.Err() != nil {
		return err
	}
	if old != nil && notRunning(old) == "" {
		resource.Report(ctx, resource.Outcome{Untouched: true})
		return fmt.Errorf("can't pull %s again: %w, so nothing was replaced, and what runs goes on running. Check that its registry answers, and the image's name and tag, then deploy again. Dockwell can't pull private images yet", ref, err)
	}
	switch _, have := d.Client.InspectImage(ctx, docker.WithTag(ref)); {
	case have == nil:
		resource.Report(ctx, resource.Outcome{Note: fmt.Sprintf("Can't pull %s again: %s, so %s runs the image this server has. Deploy again once its registry answers, to get anything newer", ref, err, t.Service)})
		return nil
	case docker.IsNotFound(have):
		return fmt.Errorf("can't pull %s: %w. %s isn't running, and this server doesn't have that image, so it can't start. Check that its registry answers, and the image's name and tag, then deploy again. Dockwell can't pull private images yet", ref, err, t.Service)
	default:
		return have
	}
}

// NotRunning says how the container at t isn't running, in a word.
func (d *Docker) NotRunning(ctx context.Context, t Target) (string, error) {
	c, err := d.find(ctx, t.Name)
	if err != nil || c == nil {
		return "", err
	}
	return notRunning(c), nil
}

// notRunning says how a container isn't running, in a word, or nothing if it is.
// Docker counts one that's restarting or paused as running, and neither serves.
func notRunning(c *docker.Container) string {
	if c.State.Running && !c.State.Restarting && !c.State.Paused {
		return ""
	}
	return cmp.Or(c.State.Status, "stopped")
}

// reportRan puts the image a container now runs on record, by digest. Not
// knowing it costs a moving tag its rollback, not the deploy, so a failure to
// read it doesn't fail anything.
func (d *Docker) reportRan(ctx context.Context, ref string) {
	if img, err := d.Client.InspectImage(ctx, docker.WithTag(ref)); err == nil {
		resource.Report(ctx, resource.Outcome{Digest: img.Digest(ref)})
	}
}

// pullableWait bounds asking a registry for an image while planning. A plan
// shouldn't wait on a registry, so one that doesn't answer in time says nothing,
// and the pull finds out.
const pullableWait = 10 * time.Second

// notPullable says an image's registry won't give it to a pull. One that doesn't
// answer in time, is down or is limiting how often it's asked says nothing of it.
func (d *Docker) notPullable(ctx context.Context, ref string) bool {
	ctx, cancel := context.WithTimeout(ctx, pullableWait)
	defer cancel()
	return docker.NotPullable(d.Client.CheckPullable(ctx, ref))
}

// checkImage refuses a plan for an image the server hasn't got and its registry
// won't give it, which would otherwise fail only once the change was applied. A
// file with build beside image often names one only the server it was built on
// has. One the server has runs as it is, wherever it came from.
func (d *Docker) checkImage(ctx context.Context, t Target, ref string) error {
	_, err := d.Client.InspectImage(ctx, docker.WithTag(ref))
	switch {
	case err == nil:
		return nil
	case !docker.IsNotFound(err):
		return err
	}
	if d.notPullable(ctx, ref) {
		return fmt.Errorf("the image %s isn't on this server, and its registry won't give it to a pull, so %s can't run: it has no such image, or wants credentials, which Dockwell can't send yet. "+
			"Build or load the image on this server, or push it where a pull can get it, then plan again", ref, t.Service)
	}
	return nil
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
		for _, m := range spec.GetMounts() {
			v := m.GetVolume()
			if v == "" || d.Deleted == nil {
				continue
			}
			if deleted, err := d.Deleted.Has(v); err != nil {
				return err
			} else if deleted {
				return fmt.Errorf("the volume %s was deleted, and the server keeps its data for 7 days, so %s can't mount it. Mount a volume of another name", v, t.Name)
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
		if d.Deleted == nil {
			continue
		}
		if deleted, err := d.Deleted.Has(t.DockerName(v)); err != nil {
			return err
		} else if deleted {
			return fmt.Errorf("the volume %s was deleted, so %s can't mount it, though the server keeps its data for 7 days. "+
				"Add it to the environment again, in this change or an earlier one, which brings its data back", v, t.Service)
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

// Run creates and starts the container, replacing the one of the name,
// which must be Dockwell's. The new one is made, under a name of its own,
// before the old one stops, so only stopping one and starting the other is
// downtime. The volumes Docker made for the old one's image move to the
// new one, and those a mount now covers are kept for 7 days, as a deleted
// container's are; a container made again mounts those kept for it. What it
// fails with is in plain words.
func (d *Docker) Run(ctx context.Context, t Target, spec *resourcev1.Container, restore bool) error {
	return explained(t, d.run(ctx, t, spec, restore))
}

func (d *Docker) run(ctx context.Context, t Target, spec *resourcev1.Container, restore bool) error {
	if err := d.ensureImage(ctx, spec.GetImage()); err != nil {
		return err
	}
	if err := d.CheckHost(ctx, t, spec); err != nil {
		return err
	}
	if err := d.checkRefs(ctx, t, spec); err != nil {
		return err
	}
	old, err := d.find(ctx, t.Name)
	if err != nil {
		return err
	}
	if old != nil {
		if err := owned(t, old); err != nil {
			return err
		}
	} else if old, err = d.unfinished(ctx, t); err != nil {
		return err
	}
	kept, err := d.keptVolumes(ctx, t)
	if err != nil {
		return err
	}
	req, later, restored := d.createRequest(ctx, t, spec, old, kept)
	if restore && len(restored) == 0 {
		return fmt.Errorf("the data %s's image kept, which this change was to mount again, is gone: the server keeps it for 7 days after a change takes it out of use, then removes it. "+
			"Plan the change again: it makes %s with its image's volumes empty", t.Name, t.Name)
	}
	undo := func() {}
	if len(restored) > 0 {
		if undo, err = d.Deleted.TakeImageVolumes(t.Name, slices.Collect(maps.Keys(restored))); err != nil {
			return err
		}
	}
	name := t.Name
	if old != nil {
		// As Compose names a container it is about to swap in.
		name = old.ID[:12] + "_" + t.Name
		left, err := d.find(ctx, name)
		if err == nil && left != nil {
			err = owned(Target{Name: name, Project: t.Project, Service: t.Service}, left)
		}
		if err != nil {
			undo()
			return err
		}
		if left != nil {
			_ = d.Client.RemoveContainer(ctx, left.ID, true) // a try that failed before
		}
	}
	id, err := d.Client.CreateContainer(ctx, name, req)
	if err != nil {
		undo()
		return fmt.Errorf("docker can't create %s: %w", t.Name, err)
	}
	discard := func(err error) error {
		d.discard(ctx, id, name)
		undo()
		return err
	}
	for _, n := range later {
		if err := d.Client.ConnectNetwork(ctx, n, id, endpointAliases(t, spec)); err != nil {
			return discard(fmt.Errorf("can't put %s on the network %s: %w", t.Name, n, err))
		}
	}
	if err := d.joinIngress(ctx, t, spec, id); err != nil {
		return discard(err)
	}
	if old != nil {
		keep := d.beginKeepingLogs(ctx, old)
		resource.Progress(ctx, "Stopping the old container")
		if err := d.Client.StopContainer(ctx, old.ID, StopTimeout); err != nil && !docker.IsNotFound(err) {
			return discard(fmt.Errorf("can't stop the old %s: %w", t.Name, err))
		}
		d.finishKeepingLogs(ctx, keep, old)
		if err := d.Client.RemoveContainer(ctx, old.ID, true); err != nil && !docker.IsNotFound(err) {
			return discard(fmt.Errorf("can't remove the old %s: %w", t.Name, err))
		}
		d.keep(t, covered(old, spec))
		// The old one is gone: finish the swap even if the caller stops
		// waiting, as a service left with no container is worse than a
		// step that ends late. If the new one can't take the name, the
		// next Run finishes it (unfinished).
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), finishWait)
		defer cancel()
		if err := d.Client.RenameContainer(ctx, id, t.Name); err != nil {
			return fmt.Errorf("removed the old %s, but can't give the new one its name: %w. The next change to it finishes the swap, as the rollback that follows does", t.Name, err)
		}
	}
	resource.Progress(ctx, "Starting "+t.Service)
	if err := d.Client.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("docker can't start %s: %w", t.Name, err)
	}
	d.reportRan(ctx, spec.GetImage())
	return nil
}

// joinIngress puts a service's new container on the ingress network, at its
// instance's address, before it starts, so a replaced container serves its
// routes as the old one did. A service with no routes doesn't join, and nor does
// one on the server's own network, which Docker can't put on another. A plan
// refuses that with a route left on it (checkRouted), so here its route is moving
// to a port on the server in the same plan, in a later step.
func (d *Docker) joinIngress(ctx context.Context, t Target, spec *resourcev1.Container, id string) error {
	if d.Ingress == nil || !t.Scoped || spec.GetHostNetwork() {
		return nil
	}
	network, ep, ok, err := d.Ingress.Endpoint(ctx, ingress.Instance{Project: t.Project, Service: t.Service, Number: 1})
	if err != nil || !ok {
		return err
	}
	if err := d.Client.ConnectEndpoint(ctx, network, id, ep); err != nil {
		return fmt.Errorf("can't put %s on the network %s: %w", t.Name, network, err)
	}
	return nil
}

// owned refuses a container at t's name that Dockwell didn't make: one
// without the Compose project and service Dockwell labels t's with, such
// as one someone started by hand. Dockwell never stops, replaces or
// removes it.
func owned(t Target, c *docker.Container) error {
	l := c.Config.Labels
	if l[docker.LabelProject] == t.Project && l[docker.LabelService] == t.Service {
		return nil
	}
	return fmt.Errorf("a container named %s is on this server, but Dockwell didn't make it: it doesn't have the labels %s=%s and %s=%s that Dockwell's has, "+
		"so Dockwell leaves it alone. Rename it with docker rename %s <another name>, or remove it with docker rm -f %s, then plan again",
		t.Name, docker.LabelProject, t.Project, docker.LabelService, t.Service, t.Name, t.Name)
}

// Judge says what a step does on this server beyond what the specs say:
// it refuses a container at the name that Dockwell didn't make; a step
// that takes data the image keeps out of use destroys it, as the server
// keeps it for 7 days; a step that mounts such data again restores it;
// and a recreated container's health check, its image's included, says
// how long it may take to be healthy.
func (d *Docker) Judge(ctx context.Context, t Target, desired *resourcev1.Container, dec *resource.Decision) error {
	c, err := d.current(ctx, t)
	if err != nil {
		return err
	}
	if c != nil {
		if err := owned(t, c); err != nil {
			return err
		}
		if dec.Action == resource.ActionReplace {
			// A replace makes the new one under this name, and a try that
			// failed before leaves one there, but it may be someone's.
			swap := c.ID[:12] + "_" + t.Name
			left, err := d.find(ctx, swap)
			if err == nil && left != nil {
				err = owned(Target{Name: swap, Project: t.Project, Service: t.Service}, left)
			}
			if err != nil {
				return err
			}
		}
	}
	var out map[string]string
	switch dec.Action {
	case resource.ActionDelete:
		out = imageVolumes(c)
	case resource.ActionCreate, resource.ActionReplace:
		// A pinned image that is gone says so below, as a rollback has somewhere
		// else to go.
		if !strings.Contains(desired.GetImage(), "@") {
			if err := d.checkImage(ctx, t, desired.GetImage()); err != nil {
				return err
			}
		}
		out = covered(c, desired)
		kept, err := d.keptVolumes(ctx, t)
		if err != nil {
			return err
		}
		have := imageVolumes(c)
		for p := range kept {
			_, moves := have[p]
			dec.Restores = dec.Restores || !moves && !slices.ContainsFunc(desired.GetMounts(), func(m *resourcev1.Mount) bool { return m.GetTarget() == p })
		}
	case resource.ActionNone, resource.ActionUpdate:
	}
	if len(out) > 0 {
		dec.Destroys, dec.Kept = true, slices.Sorted(maps.Keys(out))
	}
	// A pinned image that is gone can't run: where a rollback pins one, it says so
	// and goes back to its tag.
	if ref := desired.GetImage(); strings.Contains(ref, "@") && (dec.Action == resource.ActionCreate || dec.Action == resource.ActionReplace) {
		if _, err := d.Client.InspectImage(ctx, ref); docker.IsNotFound(err) && d.notPullable(ctx, ref) {
			dec.ImageGone = true
		}
	}
	if hc := desired.GetHealthcheck(); dec.Action == resource.ActionReplace && !complete(hc) {
		img, err := d.Client.InspectImage(ctx, docker.WithTag(desired.GetImage()))
		switch {
		case err == nil:
			if b := HealthBound(hc, img.Config.Healthcheck); b > 0 {
				dec.UntilHealthy = dec.Downtime + b
			}
		case docker.IsNotFound(err):
			// It is pulled as the step applies: its own check isn't known.
			dec.HealthUnknown = true
		default:
			return err
		}
	}
	return nil
}

// complete reports a health check that says all HealthBound reads, so
// the image's can't change it.
func complete(hc *resourcev1.Healthcheck) bool {
	t := hc.GetTest()
	return len(t) > 0 && (t[0] == "NONE" || hc.GetInterval() != "" && hc.GetTimeout() != "" && hc.GetStartPeriod() != "" && hc.GetRetries() > 0)
}

// current is the container at t: the one of its name, or the newest a
// replace left under another, which the next Run takes as the old one.
func (d *Docker) current(ctx context.Context, t Target) (*docker.Container, error) {
	c, err := d.find(ctx, t.Name)
	if err != nil || c != nil {
		return c, err
	}
	left, err := d.leftovers(ctx, t)
	if err != nil || len(left) == 0 {
		return nil, err
	}
	c, err = d.Client.InspectContainer(ctx, left[0].ID)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	return c, err
}

// finishWait bounds finishing a replace once the old container is gone.
const finishWait = 30 * time.Second

// discard removes a container Run made but couldn't swap in, even if the
// caller stopped waiting, for up to CleanupWait. One that is left, as
// when Docker doesn't answer, is logged with how to remove it; the next
// replace removes it too, as it has the name a replace gives the new one.
func (d *Docker) discard(ctx context.Context, id, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), or(d.CleanupWait, cleanupWait))
	defer cancel()
	if err := d.Client.RemoveContainer(ctx, id, true); err != nil && !docker.IsNotFound(err) {
		d.logger().Warn("can't remove a container made for a change that failed, so it is left on the server. Remove it with docker rm -f",
			slog.String("container", name), slog.String("id", id[:min(12, len(id))]), slog.String("error", err.Error()))
	}
}

// cleanupWait bounds removing a container a failed change made.
const cleanupWait = 30 * time.Second

func (d *Docker) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// leftovers lists the containers made for t that never got its name: a
// replace makes the new one as <the old one's ID, 12 digits>_<name>, as
// Compose does, and renames it once the old one is gone. Newest first.
func (d *Docker) leftovers(ctx context.Context, t Target) ([]docker.ContainerSummary, error) {
	list, err := d.Client.ListContainers(ctx, map[string][]string{
		"name":  {"^/[0-9a-f]{12}_" + regexp.QuoteMeta(t.Name) + "$"},
		"label": {docker.LabelProject + "=" + t.Project, docker.LabelService + "=" + t.Service},
	})
	slices.SortStableFunc(list, func(a, b docker.ContainerSummary) int { return cmp.Compare(b.Created, a.Created) })
	return list, err
}

// unfinished finds a replace that removed the old container at t, but
// couldn't give the new one its name: the new one is taken as the old one,
// so the volumes Docker made for its image, which moved to it with their
// data, move on again. Any others left over are removed.
func (d *Docker) unfinished(ctx context.Context, t Target) (*docker.Container, error) {
	left, err := d.leftovers(ctx, t)
	if err != nil || len(left) == 0 {
		return nil, err
	}
	for _, l := range left[1:] {
		if err := d.Client.RemoveContainer(ctx, l.ID, true); err != nil && !docker.IsNotFound(err) {
			return nil, err
		}
	}
	c, err := d.Client.InspectContainer(ctx, left[0].ID)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	return c, err
}

// Update changes the restart policy and the limits in place.
func (d *Docker) Update(ctx context.Context, t Target, spec *resourcev1.Container) error {
	c, err := d.find(ctx, t.Name)
	switch {
	case err != nil:
		return err
	case c == nil:
		return fmt.Errorf("there's no container %s on this server to change. Plan the change again, which makes it", t.Name)
	}
	if err := owned(t, c); err != nil {
		return err
	}
	if err := d.checkCPUs(ctx, t, spec); err != nil {
		return err
	}
	return d.Client.UpdateContainer(ctx, c.ID, docker.ContainerUpdate{
		RestartPolicy:     restartOf(spec),
		NanoCPUs:          cpuNanos(spec.GetResources().GetCpus()),
		Memory:            memoryBytes(spec.GetResources().GetMemory()),
		MemoryReservation: memoryBytes(spec.GetResources().GetMemoryReservation()),
	})
}

// Remove stops the container, giving it StopTimeout, and removes it, if
// it is Dockwell's. Its volumes stay: an environment's are resources of
// their own, and those Docker made for its image are kept for 7 days, so
// a container of the name made again in that time, as undoing the
// deletion does, mounts them again.
func (d *Docker) Remove(ctx context.Context, t Target) error {
	c, err := d.find(ctx, t.Name)
	if err != nil {
		return err
	}
	if c != nil {
		if err := owned(t, c); err != nil {
			return err
		}
		left, err := d.leftovers(ctx, t)
		if err != nil {
			return err
		}
		for _, l := range left {
			if err := d.Client.RemoveContainer(ctx, l.ID, true); err != nil && !docker.IsNotFound(err) {
				return err
			}
		}
	} else if c, err = d.unfinished(ctx, t); err != nil || c == nil {
		return err
	}
	keep := d.beginKeepingLogs(ctx, c)
	if err := d.Client.StopContainer(ctx, c.ID, StopTimeout); err != nil && !docker.IsNotFound(err) {
		return err
	}
	d.finishKeepingLogs(ctx, keep, c)
	if err := d.Client.RemoveContainer(ctx, c.ID, true); err != nil && !docker.IsNotFound(err) {
		return err
	}
	d.keep(t, imageVolumes(c))
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
// see why, never what it printed, which may hold secrets; the plain words
// name what it runs as, and what to look at.
func (d *Docker) Healthy(ctx context.Context, t Target) error {
	wait := or(d.HealthWait, DefaultHealthWait)
	ctx, cancel := context.WithTimeoutCause(ctx, wait, errWaited)
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
		return resource.Plainly(t.Service+" isn't on this server, though it was just started. See if someone removed it with docker ps -a, then try again.",
			fmt.Errorf("%s isn't on this server, though it was just started. See if someone removed it with docker ps -a", t.Name))
	}
	for found != nil {
		c, err := d.Client.InspectContainer(ctx, found.ID)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if docker.IsNotFound(err) {
				return resource.Plainly(t.Service+" was removed while Dockwell waited for it to start. See who removed it with docker events, then try again.",
					fmt.Errorf("%s was removed while Dockwell waited for it to start. See who removed it with docker events", t.Name))
			}
			return err
		}
		s := c.State
		if restarts < 0 {
			restarts = c.RestartCount
			if s.Health != nil {
				resource.Progress(ctx, "Waiting for "+t.Service+" to pass its health check")
			} else {
				resource.Progress(ctx, "Checking that "+t.Service+" stays up")
			}
		}
		switch {
		case s.Restarting || c.RestartCount > restarts:
			// No word of docker logs: a rollback takes this container away.
			return resource.PlainlyBecause(resource.ReasonExits, fmt.Sprintf("%s keeps restarting: it exited with code %d. Check its command and variables, then try again.", t.Service, s.ExitCode),
				fmt.Errorf("%s keeps restarting: it exited with code %d", t.Name, s.ExitCode))
		case !s.Running:
			return resource.PlainlyBecause(resource.ReasonExits, fmt.Sprintf("%s stopped right after it started, with exit code %d. Check its command and variables, then try again.", t.Service, s.ExitCode),
				fmt.Errorf("%s stopped right after it started, with exit code %d", t.Name, s.ExitCode))
		case s.Health != nil && s.Health.Status == "healthy":
			return nil
		case s.Health != nil && s.Health.Status == "unhealthy":
			return resource.PlainlyBecause(resource.ReasonUnhealthy, t.Service+" runs, but fails its health check. Check the command its health check runs, then try again.",
				fmt.Errorf("%s runs, but fails its health check", t.Name))
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
	// The caller's deadline may come first, such as the step's own time
	// running out after a long pull: that isn't the health check's doing.
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errWaited):
		return resource.PlainlyBecause(resource.ReasonUnhealthy, fmt.Sprintf("%s didn't pass its health check within %s, so Dockwell stopped waiting. Check the command its health check runs and how long the app takes to start, then try again.", t.Service, human.Duration(wait)),
			fmt.Errorf("%s didn't pass its health check within %s", t.Name, human.Duration(wait)))
	case errors.Is(cause, context.DeadlineExceeded):
		return resource.Plainly(fmt.Sprintf("The step's time ran out before %s passed its health check, so Dockwell stopped waiting. A long pull may have used it up, so try again.", t.Service),
			fmt.Errorf("the step's time ran out before %s passed its health check, so Dockwell stopped waiting: %w. "+
				"See how it is with docker inspect --format '{{json .State.Health}}' %s", t.Name, cause, t.Name))
	default:
		return cause
	}
}

// errWaited ends waiting for a health check that took HealthWait.
var errWaited = errors.New("waited for the health check for long enough")

// Ready checks Docker answers, and agrees an API version with it.
func (d *Docker) Ready(ctx context.Context) error { return d.Client.Ping(ctx) }

// ReadyKey is the client: whether its Docker answers.
func (d *Docker) ReadyKey() any { return d.Client }
