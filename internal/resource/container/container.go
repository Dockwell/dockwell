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
	// Run creates and starts a container, replacing any with the name.
	Run(ctx context.Context, t Target, spec *resourcev1.Container) error
	// SetRestart changes the restart policy of a running container.
	SetRestart(ctx context.Context, t Target, policy resourcev1.RestartPolicy) error
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
	return validateHealthcheck(c)
}

func validatePorts(c *resourcev1.Container) error {
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
	// Order doesn't change what ports mean, so it mustn't show as a change.
	slices.SortFunc(c.Ports, func(a, b *resourcev1.PortMapping) int {
		switch {
		case a.GetHostPort() != b.GetHostPort():
			return int(a.GetHostPort()) - int(b.GetHostPort())
		case a.GetProtocol() != b.GetProtocol():
			return int(a.GetProtocol()) - int(b.GetProtocol())
		}
		return strings.Compare(a.GetHostIp(), b.GetHostIp())
	})
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
	seen := map[string]bool{}
	for _, m := range c.GetMounts() {
		target := m.GetTarget()
		if !path.IsAbs(target) {
			return fmt.Errorf("a mount's target %q isn't an absolute path in the container; start it with /, such as /data", target)
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
			src.HostPath = path.Clean(src.HostPath)
		default:
			return fmt.Errorf("the mount at %s needs a volume or a server path to mount", m.Target)
		}
		if seen[m.Target] {
			return fmt.Errorf("two mounts share the target %s; give each its own", m.Target)
		}
		seen[m.Target] = true
	}
	slices.SortFunc(c.Mounts, func(a, b *resourcev1.Mount) int { return strings.Compare(a.GetTarget(), b.GetTarget()) })
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
// which is all of them except the restart policy.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete}, nil
	}
	// Only a change that is nothing but the restart policy happens in place.
	inPlace := len(changes) > 0
	for _, c := range changes {
		if c.Path != "restart" {
			inPlace = false
			break
		}
	}
	if inPlace {
		return resource.Decision{Action: resource.ActionUpdate}, nil
	}
	downtime := d.RecreateDowntime
	if downtime == 0 {
		downtime = DefaultRecreateDowntime
	}
	return resource.Decision{Action: resource.ActionReplace, Effect: resource.EffectRecreate, Downtime: downtime}, nil
}

// Apply runs, updates or removes the container.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	return d.ApplyAt(ctx, HostTarget(step.Ref), step.Action, desired)
}

// ApplyAt carries out action on the container at t.
func (d *Driver) ApplyAt(ctx context.Context, t Target, action resource.Action, desired proto.Message) error {
	switch action {
	case resource.ActionCreate, resource.ActionReplace:
		return d.Runtime.Run(ctx, t, desired.(*resourcev1.Container))
	case resource.ActionUpdate:
		return d.Runtime.SetRestart(ctx, t, desired.(*resourcev1.Container).GetRestart())
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
