// SPDX-License-Identifier: AGPL-3.0-only

// Package container is the driver for Docker containers, the "container"
// kind. It talks to Docker through Runtime, which the agent implements
// with the Docker Engine API (Docker) and tests with Fake. Services run
// their app through the same driver.
package container

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the container kind.
const Kind resource.Kind = "container"

// Target is where a container goes in Docker, and the Compose project and
// service its labels name, so docker compose sees it.
type Target struct {
	// Name is the container's name in Docker.
	Name string
	// Project and Service are its Compose project and service.
	Project, Service string
	// Scoped means the spec's volumes and networks are its environment's,
	// which Docker names <project>_<name>, as Compose names a project's
	// own, and that containers on its networks reach it by its service's
	// name. A container on its own names Docker's.
	Scoped bool
}

// HostTarget is where a container run on its own goes: by its name, in
// its server's Compose project.
func HostTarget(ref resource.Ref) Target {
	return Target{Name: ref.Name, Project: docker.Project(ref.Scope), Service: ref.Name}
}

// DockerName is what Docker names a volume or network a spec names.
func (t Target) DockerName(name string) string {
	if t.Scoped {
		return docker.ScopedName(t.Project, name)
	}
	return name
}

// Runtime is the part of Docker the driver needs.
type Runtime interface {
	// Inspect returns the container's spec as Docker has it, or nil if
	// there is no such container.
	Inspect(ctx context.Context, t Target) (*resourcev1.Container, error)
	// Run creates and starts a container, replacing the one with the name.
	// restore says the plan expected it to mount again the data its image
	// kept, which the server keeps for 7 days after a deletion: if that is
	// gone, Run fails rather than make the container with it empty.
	Run(ctx context.Context, t Target, spec *resourcev1.Container, restore bool) error
	// Update changes what Docker can change on a running container: its restart
	// policy, and its limits.
	Update(ctx context.Context, t Target, spec *resourcev1.Container) error
	// Remove stops and removes the container.
	Remove(ctx context.Context, t Target) error
	// Healthy reports whether the container is running and passes its
	// health check.
	Healthy(ctx context.Context, t Target) error
}

// StopTimeout is how long a container has to stop before Docker kills it,
// as docker stop gives it.
const StopTimeout = 10 * time.Second

// DefaultRecreateDowntime is how long a recreated container is down at
// most: the old one has up to StopTimeout to stop, and the new one starts
// in about two seconds. The new one is made, and its image pulled, before
// the old one stops, so neither counts.
const DefaultRecreateDowntime = StopTimeout + 2*time.Second

// Driver manages containers.
type Driver struct {
	Runtime Runtime
	// RecreateDowntime is how long a recreated container is down: stopping
	// the old one and starting the new. Zero means
	// DefaultRecreateDowntime. Rolling deploys with two replicas avoid it;
	// they come with #28.
	RecreateDowntime time.Duration
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "container".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the server: a container run by hand is host-level. Services
// run containers too, as part of their environment.
func (*Driver) Level() resource.Level { return resource.LevelServer }

// New returns an empty Container.
func (*Driver) New() proto.Message { return &resourcev1.Container{} }

// NameRE is the names Docker takes for containers, volumes and networks.
var NameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// serviceForm starts the name of every service's container in Docker,
// <environment ID>-<service>-1 in lowercase (docker.ContainerName), and
// every environment ID starts with env_.
const serviceForm = id.Environment + "_"

// CheckName requires a name Docker takes, and keeps names of the form
// services' containers have for services, so a container run by hand never
// manages one of theirs.
func (*Driver) CheckName(name string) error {
	switch {
	case !NameRE.MatchString(name):
		return fmt.Errorf("%q isn't a valid container name: use letters, digits, dots, dashes and underscores, starting with a letter or digit, such as web", name)
	case strings.HasPrefix(strings.ToLower(name), serviceForm):
		return fmt.Errorf("%q has the form of a service's container, %s<ID>-<service>-1, so a container run by hand can't take it. Choose a name that doesn't start with %q, or run it as a service",
			name, serviceForm, serviceForm)
	}
	return nil
}

// Validate requires an image and valid ports, mounts and networks, and
// fills in defaults, so specs that mean the same compare equal. A
// container on its own that joins Docker's bridge network alone joins it
// as one that names no network does: Docker reads both back the same.
func (*Driver) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.Container)
	if err := Validate(c); err != nil {
		return err
	}
	if slices.Equal(c.Networks, []string{bridge}) {
		c.Networks = nil
	}
	if err := checkOnItsOwn(c); err != nil {
		return err
	}
	return checkOwnNames(c)
}

// Normalize orders a spec Validate refuses, as a newer rule may refuse a
// stored one, as Validate would order it.
func (*Driver) Normalize(spec proto.Message) { Normalize(spec.(*resourcev1.Container)) }

// Normalize fills in c's defaults and orders it, and refuses nothing.
func Normalize(c *resourcev1.Container) {
	if c.Restart == resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED
	}
	for _, p := range c.GetPorts() {
		if p.Protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED {
			p.Protocol = resourcev1.Protocol_PROTOCOL_TCP
		}
	}
	sortPorts(c)
	sortMounts(c)
	slices.Sort(c.Networks)
	slices.Sort(c.ExternalNetworks)
	slices.Sort(c.Aliases)
	slices.Sort(c.CapAdd)
	slices.Sort(c.CapDrop)
	slices.Sort(c.GroupAdd)
}

// checkOwnNames keeps a container on its own out of what isn't its own.
// Its volumes and networks are Docker's, by their names there, so without
// this one could mount an environment's volume, or one Docker made for
// another container's image, or take the name of a volume an environment
// makes later. A service's names are its environment's, so it can't.
func checkOwnNames(c *resourcev1.Container) error {
	for _, m := range c.GetMounts() {
		v := m.GetVolume()
		switch {
		case strings.HasPrefix(strings.ToLower(v), serviceForm):
			return fmt.Errorf("the volume %s has the form of an environment's, %s<ID>_<name>, so a container on its own can't mount it. Mount a volume of its own, or run it as a service", v, serviceForm)
		case anonymous(v):
			return fmt.Errorf("the volume %s has the form of one Docker made for an image, so a container on its own can't mount it. Mount a named volume", v)
		}
	}
	for _, n := range c.GetNetworks() {
		if n == docker.IngressNetwork {
			return fmt.Errorf("the network %s is the ingress's own, and a container on it could take the address of a service that is stopped, and get its traffic. Join a network of its own, or send a domain to a service with a route", n)
		}
		if strings.HasPrefix(strings.ToLower(n), serviceForm) {
			return fmt.Errorf("the network %s has the form of an environment's, %s<ID>_<name>, so a container on its own can't join it. Join a network of its own, or run it as a service", n, serviceForm)
		}
	}
	return nil
}

// bridge is Docker's default network.
const bridge = "bridge"

// Validate checks a container's spec and normalizes it in place. A
// service's app is checked with it, as a container whose networks are its
// environment's.
func Validate(c *resourcev1.Container) error {
	if err := CheckImage(c.GetImage()); err != nil {
		return err
	}
	// An entrypoint of one empty string is how docker run --entrypoint ""
	// says to run none.
	if slices.Equal(c.Entrypoint, []string{""}) {
		c.Entrypoint, c.NoEntrypoint = nil, true
	}
	if c.GetNoEntrypoint() && len(c.GetEntrypoint()) > 0 {
		return errors.New("a container with no_entrypoint runs no entrypoint, so it takes none in entrypoint too. Leave out one of them")
	}
	if c.Restart == resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED
	}
	for k := range c.GetEnv() {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("%q isn't a valid environment variable name: use letters, digits and underscores, such as API_URL", k)
		}
	}
	for k := range c.GetLabels() {
		if docker.IsOwnLabel(k) {
			return fmt.Errorf("the label %s is Dockwell's own, and Docker Compose's; choose a key that doesn't start with %s or %s", k, docker.ComposePrefix, docker.OwnPrefix)
		}
	}
	if err := validatePorts(c); err != nil {
		return err
	}
	if err := validateMounts(c); err != nil {
		return err
	}
	if n := len(c.GetNetworks()); n > maxNames {
		return fmt.Errorf("the container joins %d networks, and Dockwell takes up to %d. Join fewer", n, maxNames)
	}
	seen := map[string]bool{}
	for _, n := range c.GetNetworks() {
		switch {
		case !NameRE.MatchString(n):
			return fmt.Errorf("%q isn't a valid network name: use letters, digits, dots, dashes and underscores, such as backend", n)
		case seen[n]:
			return fmt.Errorf("the network %s is listed twice; list each network once", n)
		}
		seen[n] = true
	}
	slices.Sort(c.Networks)
	if wd := c.GetWorkingDir(); wd != "" {
		if !path.IsAbs(wd) {
			return fmt.Errorf("the working directory %s isn't an absolute path; start it with /, such as /app", wd)
		}
		c.WorkingDir = path.Clean(wd)
	}
	if err := validateRestart(c); err != nil {
		return err
	}
	if err := validateNetworking(c); err != nil {
		return err
	}
	if err := validateResources(c); err != nil {
		return err
	}
	if err := validatePrivileges(c); err != nil {
		return err
	}
	return validateHealthcheck(c)
}

// validateRestart refuses a count of restarts on a policy with none, which
// Docker refuses as it makes the container, so only once the change is applied.
func validateRestart(c *resourcev1.Container) error {
	if c.GetRestartRetries() > 0 && c.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE {
		return fmt.Errorf("only on-failure restarts a set number of times, and this container's policy is %s. Use on-failure, or leave restart_retries out", restartNames[c.GetRestart()])
	}
	return nil
}

// What one container may ask for, far past what anything needs. Checks and
// plans compare these with each other, so a request of 8 MiB that repeats
// them shouldn't cost minutes.
const (
	MaxMounts = 256
	MaxPorts  = 4096
)

func validatePorts(c *resourcev1.Container) error {
	if n := len(c.GetPorts()); n > MaxPorts {
		return fmt.Errorf("the container publishes %d ports, and Dockwell takes up to %d. Put a proxy in front of them, or publish fewer", n, MaxPorts)
	}
	type port struct {
		number   uint32
		protocol resourcev1.Protocol
	}
	seen := map[port][]string{} // the addresses each is published on so far
	for _, p := range c.GetPorts() {
		if p.Protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED {
			p.Protocol = resourcev1.Protocol_PROTOCOL_TCP
		}
		if p.GetHostPort() == 0 || p.GetHostPort() > 65535 || p.GetContainerPort() == 0 || p.GetContainerPort() > 65535 {
			return fmt.Errorf("ports run from 1 to 65535, not %d:%d", p.GetHostPort(), p.GetContainerPort())
		}
		if ip := p.GetHostIp(); ip != "" {
			addr, err := netip.ParseAddr(ip)
			if err != nil {
				return fmt.Errorf("%q isn't an IP address to publish port %d on; give one such as 127.0.0.1, or leave it out for every address", ip, p.GetHostPort())
			}
			p.HostIp = addr.String()
		}
		key := port{p.GetHostPort(), p.GetProtocol()}
		for _, ip := range seen[key] {
			if !overlap(ip, p.GetHostIp()) {
				continue
			}
			name := strconv.Itoa(int(p.GetHostPort()))
			if p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
				name += "/udp"
			}
			if ip == p.GetHostIp() {
				return fmt.Errorf("server port %s is published twice on %s; give each mapping its own port", name, addressWords(ip))
			}
			return fmt.Errorf("server port %s is published on %s and on %s, which Docker can't bind together. Publish it on one of them, or give each mapping its own port",
				name, addressWords(ip), addressWords(p.GetHostIp()))
		}
		seen[key] = append(seen[key], p.GetHostIp())
	}
	sortPorts(c)
	return nil
}

// sortPorts orders ports: order doesn't change what they mean, so it
// mustn't show as a change.
func sortPorts(c *resourcev1.Container) {
	slices.SortFunc(c.Ports, func(a, b *resourcev1.PortMapping) int {
		switch {
		case a.GetHostPort() != b.GetHostPort():
			return int(a.GetHostPort()) - int(b.GetHostPort())
		case a.GetProtocol() != b.GetProtocol():
			return int(a.GetProtocol()) - int(b.GetProtocol())
		}
		return strings.Compare(a.GetHostIp(), b.GetHostIp())
	})
}

// sortMounts orders mounts by where they go, for the same reason.
func sortMounts(c *resourcev1.Container) {
	slices.SortFunc(c.Mounts, func(a, b *resourcev1.Mount) int { return strings.Compare(a.GetTarget(), b.GetTarget()) })
}

// checkMountString refuses what can't go in Docker's source:target:options
// form of a bind, where a colon would start the next part, so a target of
// /app:rshared would set an option the spec refuses.
func checkMountString(p, what string) error {
	switch {
	case strings.Contains(p, ":"):
		return fmt.Errorf("%s %q has a colon in it, which Docker would read as the start of what comes next. Use a path without one", what, p)
	case strings.IndexFunc(p, unicode.IsControl) >= 0:
		return fmt.Errorf("%s %q has a control character in it, which Docker can't take. Write it without one", what, p)
	}
	return nil
}

// overlap reports whether a port can't be published on two server
// addresses together, as Docker binds them: the same address, or every
// address and any other one. No address means every IPv4 and IPv6 one,
// 0.0.0.0 every IPv4 one, and :: every IPv6 one, so 0.0.0.0 and :: don't
// overlap. Addresses are valid and normalized.
func overlap(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	x, y := netip.MustParseAddr(a), netip.MustParseAddr(b)
	return x.Is4() == y.Is4() && (x.IsUnspecified() || y.IsUnspecified())
}

// addressWords names a server address a port is published on.
func addressWords(ip string) string {
	switch ip {
	case "":
		return "every address"
	case "0.0.0.0":
		return "0.0.0.0 (every IPv4 address)"
	case "::":
		return ":: (every IPv6 address)"
	}
	return ip
}

func validateMounts(c *resourcev1.Container) error {
	if n := len(c.GetMounts()); n > MaxMounts {
		return fmt.Errorf("the container has %d mounts, and Dockwell takes up to %d. Mount a parent path, or fewer volumes", n, MaxMounts)
	}
	seen := map[string]bool{}
	for _, m := range c.GetMounts() {
		target := m.GetTarget()
		if !path.IsAbs(target) {
			return fmt.Errorf("a mount's target %q isn't an absolute path in the container; start it with /, such as /data", target)
		}
		if err := checkMountString(target, "a mount's target"); err != nil {
			return err
		}
		m.Target = path.Clean(target)
		switch src := m.GetSource().(type) {
		case *resourcev1.Mount_Volume:
			if !NameRE.MatchString(src.Volume) {
				return fmt.Errorf("%q isn't a valid volume name: use letters, digits, dots, dashes and underscores, such as data", src.Volume)
			}
			if m.GetRelabel() != resourcev1.Relabel_RELABEL_UNSPECIFIED {
				return fmt.Errorf("the volume %s mounted at %s is relabelled for SELinux, which Dockwell can do for a server path alone. Leave relabel out, or mount a server path", src.Volume, m.Target)
			}
		case *resourcev1.Mount_HostPath:
			if !path.IsAbs(src.HostPath) {
				return fmt.Errorf("the server path %q mounted at %s isn't absolute; start it with /, such as /srv/data", src.HostPath, m.Target)
			}
			if err := checkMountString(src.HostPath, "the server path"); err != nil {
				return err
			}
			if err := CheckSpelling(src.HostPath, fmt.Sprintf("the server path %s mounted at %s", src.HostPath, m.Target)); err != nil {
				return err
			}
			src.HostPath = path.Clean(src.HostPath)
			if err := DefaultHost.CheckPath(src.HostPath, fmt.Sprintf("the server path %s mounted at %s", src.HostPath, m.Target)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the mount at %s needs a volume or a server path to mount", m.Target)
		}
		if seen[m.Target] {
			return fmt.Errorf("two mounts share the target %s; give each its own", m.Target)
		}
		seen[m.Target] = true
	}
	sortMounts(c)
	return nil
}

func validateHealthcheck(c *resourcev1.Container) error {
	hc := c.GetHealthcheck()
	if hc == nil {
		return nil
	}
	test := hc.GetTest()
	switch {
	case len(test) == 0:
		return errors.New("a health check needs a test, such as [\"CMD-SHELL\", \"curl -f http://localhost/ || exit 1\"], or [\"NONE\"] to turn off the image's")
	case test[0] == "NONE":
		if len(test) > 1 || hc.GetInterval() != "" || hc.GetTimeout() != "" || hc.GetStartPeriod() != "" || hc.GetRetries() != 0 {
			return errors.New("a health check of [\"NONE\"] turns the image's off, so it takes nothing else")
		}
		return nil
	case test[0] != "CMD" && test[0] != "CMD-SHELL":
		return fmt.Errorf("a health check's test starts with CMD, CMD-SHELL or NONE, not %q", test[0])
	case len(test) < 2:
		return fmt.Errorf("a health check's test needs a command after %s", test[0])
	}
	for _, f := range []*string{&hc.Interval, &hc.Timeout, &hc.StartPeriod} {
		if *f == "" {
			continue
		}
		d, err := time.ParseDuration(*f)
		if err != nil || d <= 0 {
			return fmt.Errorf("%q isn't a health check duration; give one such as 30s or 1m", *f)
		}
		*f = d.String()
	}
	return nil
}

// Observe asks Docker.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	return d.ObserveAt(ctx, HostTarget(ref))
}

// ObserveAt reads the container at t.
func (d *Driver) ObserveAt(ctx context.Context, t Target) (proto.Message, error) {
	c, err := d.Runtime.Inspect(ctx, t)
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

// Plan recreates the container for any change Docker can't make in place,
// which is all of them except the restart policy and its limits (inPlace). A
// recreated container with a health check is down for the recreate, and may
// take until its check passes to be healthy again (UntilHealthy).
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete}, nil
	}
	if inPlace(changes) {
		return resource.Decision{Action: resource.ActionUpdate}, nil
	}
	downtime := d.RecreateDowntime
	if downtime == 0 {
		downtime = DefaultRecreateDowntime
	}
	dec := resource.Decision{Action: resource.ActionReplace, Effect: resource.EffectRecreate, Downtime: downtime}
	if b := HealthBound(desired.(*resourcev1.Container).GetHealthcheck(), nil); b > 0 {
		dec.UntilHealthy = downtime + b
	}
	return dec, nil
}

// inPlace reports a change that is nothing but what Docker changes on a running
// container: the restart policy, and its limits. It reads a limit of zero as
// leaving it as it is, so lifting one takes a new container.
func inPlace(changes []resource.Change) bool {
	for _, c := range changes {
		switch c.Path {
		case "restart", "restart_retries":
		case "resources.cpus", "resources.memory", "resources.memory_reservation":
			if c.Op == resource.OpRemoved {
				return false
			}
		default:
			return false
		}
	}
	return len(changes) > 0
}

// Docker's defaults for a health check's interval, timeout and retries.
const (
	defaultInterval = 30 * time.Second
	defaultTimeout  = 30 * time.Second
	defaultRetries  = 3
)

// DefaultHealthWait is how long Verify waits for a container's health
// check to pass, at most.
const DefaultHealthWait = 5 * time.Minute

// HealthBound is how long a container may take to turn healthy once it
// starts, at most, by its health check: its start period, then as many
// failed checks as its retries, which is when Docker calls it unhealthy.
// Docker waits an interval after each check ends, and a check may take its
// timeout. It is no longer than DefaultHealthWait, after which Verify stops
// waiting. What the spec's check leaves out takes the image's, then
// Docker's defaults. Zero for a container without a check.
func HealthBound(spec *resourcev1.Healthcheck, img *docker.Healthcheck) time.Duration {
	var hc docker.Healthcheck
	if img != nil {
		hc = *img
	}
	if s := healthcheck(spec); s != nil {
		if len(s.Test) > 0 {
			hc.Test = s.Test
		}
		if s.Interval > 0 {
			hc.Interval = s.Interval
		}
		if s.Timeout > 0 {
			hc.Timeout = s.Timeout
		}
		if s.StartPeriod > 0 {
			hc.StartPeriod = s.StartPeriod
		}
		if s.Retries > 0 {
			hc.Retries = s.Retries
		}
	}
	if len(hc.Test) == 0 || hc.Test[0] == "NONE" {
		return 0
	}
	interval, timeout, retries := or(hc.Interval, defaultInterval), or(hc.Timeout, defaultTimeout), hc.Retries
	if retries <= 0 {
		retries = defaultRetries
	}
	return min(hc.StartPeriod+time.Duration(retries)*(interval+timeout), DefaultHealthWait)
}

// judge is a Runtime that knows its server, as Docker does: whose
// container has a name, what its image keeps, and how long its health
// check may take.
type judge interface {
	Judge(ctx context.Context, t Target, desired *resourcev1.Container, dec *resource.Decision) error
}

// Judge plans a step as Plan does, then with what the server has.
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.JudgeAt(ctx, HostTarget(ref), actual, desired, changes)
}

// JudgeAt judges the step for the container at t.
func (d *Driver) JudgeAt(ctx context.Context, t Target, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	dec, err := d.Plan(actual, desired, changes)
	if err != nil {
		return dec, err
	}
	if j, ok := d.Runtime.(judge); ok {
		var spec *resourcev1.Container
		if desired != nil {
			spec = desired.(*resourcev1.Container)
		}
		if err := j.Judge(ctx, t, spec, &dec); err != nil {
			return resource.Decision{}, err
		}
	}
	return dec, nil
}

// Apply runs, updates or removes the container.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	return d.ApplyAt(ctx, HostTarget(step.Ref), step, desired)
}

// ApplyAt carries out step on the container at t.
func (d *Driver) ApplyAt(ctx context.Context, t Target, step resource.Step, desired proto.Message) error {
	switch step.Action {
	case resource.ActionCreate, resource.ActionReplace:
		return d.Runtime.Run(ctx, t, desired.(*resourcev1.Container), step.Restores)
	case resource.ActionUpdate:
		return d.Runtime.Update(ctx, t, desired.(*resourcev1.Container))
	case resource.ActionDelete:
		return d.Runtime.Remove(ctx, t)
	default:
		return nil
	}
}

// Verify checks the container is running and passes its health check.
func (d *Driver) Verify(ctx context.Context, ref resource.Ref, _ proto.Message) error {
	return d.Runtime.Healthy(ctx, HostTarget(ref))
}

// VerifyAt checks the container at t is running and healthy.
func (d *Driver) VerifyAt(ctx context.Context, t Target) error {
	return d.Runtime.Healthy(ctx, t)
}

// Ready says why the runtime can't run containers now, such as Docker not
// running. A runtime that can always run is always ready.
func (d *Driver) Ready(ctx context.Context) error {
	if r, ok := d.Runtime.(resource.Readier); ok {
		return r.Ready(ctx)
	}
	return nil
}

// ReadyKey is the runtime's, such as its Docker client, so the drivers on
// one Docker share one answer.
func (d *Driver) ReadyKey() any {
	if r, ok := d.Runtime.(resource.SharedReadier); ok {
		return r.ReadyKey()
	}
	return d
}
