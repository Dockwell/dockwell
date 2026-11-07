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
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
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
	// Number is the container's number among its service's, as a rollout gives
	// it to the one it makes. Zero means the first.
	Number int
	// Scoped means the spec's volumes and networks are its environment's,
	// which Docker names <project>_<name>, as Compose names a project's
	// own, and that containers on its networks reach it by its service's
	// name. A container on its own names Docker's.
	Scoped bool
	// Prefix is what the names of a service's containers start with in place
	// of Project, as people read them: "<project>.<environment>", such as
	// default.production, for default.production.web-1. Empty is Project, as
	// Compose names them. Only names take it: labels, volumes and networks go
	// by Project (ADR 0035).
	Prefix string
}

// ScopeNameRE is the form of what people call an environment in its
// containers' names, "<project>.<environment>": two names of the kind a
// project and an environment take (resource.CheckLabel), which have no dots,
// so the parts never run together, and what follows the last dot of a name is
// the service and its number.
var ScopeNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?\.[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// readableForm is the form of a service's container named after what people
// call its environment, default.production.web-1: three names of the kind a
// project, an environment and a service take, and a number.
var readableForm = regexp.MustCompile(`^` + labelPart + `\.` + labelPart + `\.` + labelPart + `-[0-9]+$`)

// labelPart is a name of the kind resource.CheckLabel takes.
const labelPart = `[a-z]([a-z0-9-]{0,61}[a-z0-9])?`

// HostTarget is where a container run on its own goes: by its name, in
// its server's Compose project.
func HostTarget(ref resource.Ref) Target {
	return Target{Name: ref.Name, Project: docker.Project(ref.Scope), Service: ref.Name}
}

// keptKey is what the volumes a container's image kept are listed under, after a
// change takes it out of use: for a service, the name Compose gives its first
// container, which is what it was named when the list began, and never changes
// with what people call its environment (ADR 0035), so an undo across an upgrade
// or a rename finds its data. A container on its own is its name.
func (t Target) keptKey() string {
	if t.Scoped {
		return docker.ContainerName(t.Project, t.Service)
	}
	return t.Name
}

// DockerName is what Docker names a volume or network a spec names.
func (t Target) DockerName(name string) string {
	if t.Scoped {
		return docker.ScopedName(t.Project, name)
	}
	return name
}

// VolumeName is what Docker names the volume a mount names.
func (t Target) VolumeName(m *resourcev1.Mount) string {
	if m.GetExternal() {
		return m.GetVolume()
	}
	return t.DockerName(m.GetVolume())
}

// Runtime is the part of Docker the driver needs.
type Runtime interface {
	// Inspect returns the container's spec as Docker has it, or nil if
	// there is no such container.
	Inspect(ctx context.Context, t Target) (*resourcev1.Container, error)
	// Run makes what runs at t match spec, as dec says: replacing a service
	// with no gap where it can, and otherwise stopping it first. dec.Restores
	// says the plan expected it to mount again the data its image kept, which
	// the server keeps for 7 days after a deletion: if that is gone, Run fails
	// rather than make the container with it empty.
	Run(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision) error
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

// recreateStart is how long a recreated container takes to start.
const recreateStart = 2 * time.Second

// DefaultRecreateDowntime is how long a recreated container is down at
// most: the old one has up to StopTimeout to stop, up to LogCatchUp goes on
// keeping its last lines, and the new one starts in about two seconds. The new
// one is made, and its image pulled, before the old one stops, so neither counts.
const DefaultRecreateDowntime = StopTimeout + LogCatchUp + recreateStart

// Driver manages containers.
type Driver struct {
	Runtime Runtime
	// RecreateDowntime is how long a recreated container is down: stopping
	// the old one and starting the new. Zero takes the old one's own time to
	// stop, as its spec says, or DefaultRecreateDowntime.
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

// CheckNewName keeps the form of a service's container named after its
// environment for services, from a container run by hand made anew: one that has
// a name of that form already goes on being edited, undone and adopted, as it was
// there before the form was a service's (ADR 0035).
func (*Driver) CheckNewName(name string) error {
	if readableForm.MatchString(name) {
		return fmt.Errorf("%q has the form of a service's container, <project>.<environment>.<service>-1, so a container run by hand can't take it. Choose a name that doesn't end in -<number> after two dots, or run it as a service", name)
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
	// Docker puts a container on host or none on that alone.
	for alone, what := range map[string]string{"host": "the server's own network, host,", "none": "none, which is no network,"} {
		if slices.Contains(c.Networks, alone) && len(c.Networks) > 1 {
			return fmt.Errorf("a container on %s joins no other network, as Docker has it. List %s alone, or leave it out", what, alone)
		}
	}
	if err := checkOnItsOwn(c); err != nil {
		return err
	}
	if c.GetRollout() != nil {
		return errors.New("a container on its own has one name, so it is stopped and made again, and rollout settings mean nothing for it. Leave rollout out, or run it as a service")
	}
	if err := binding.CheckWords(c, func(name string) bool { _, ok := c.GetEnv()[name]; return ok }); err != nil {
		return err
	}
	return checkOwnNames(c)
}

// NamedAsText says what the container writes as ${NAME} as text, for what its
// environment has.
func (*Driver) NamedAsText(spec proto.Message) map[string][]string {
	c := spec.(*resourcev1.Container)
	return binding.NamedAsText(c, func(name string) bool { _, ok := c.GetEnv()[name]; return ok })
}

// Normalize orders a spec Validate refuses, as a newer rule may refuse a
// stored one, as Validate would order it.
func (*Driver) Normalize(spec proto.Message) { Normalize(spec.(*resourcev1.Container)) }

// Normalize fills in c's defaults and orders it, and refuses nothing.
func Normalize(c *resourcev1.Container) {
	slices.Sort(c.References)
	if c.Restart == resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED
	}
	for _, p := range c.GetPorts() {
		if p.Protocol == resourcev1.Protocol_PROTOCOL_UNSPECIFIED {
			p.Protocol = resourcev1.Protocol_PROTOCOL_TCP
		}
		if addr, err := netip.ParseAddr(p.GetHostIp()); err == nil {
			p.HostIp = addr.String()
		}
	}
	// In the form Validate leaves them, as what Docker reports can be written otherwise:
	// its CLI sends CAP_NET_ADMIN, and a Compose file can say /data/.
	for _, m := range c.GetMounts() {
		if path.IsAbs(m.GetTarget()) {
			m.Target = path.Clean(m.GetTarget())
		}
		if src, ok := m.GetSource().(*resourcev1.Mount_HostPath); ok && path.IsAbs(src.HostPath) {
			src.HostPath = path.Clean(src.HostPath)
		}
	}
	c.CapAdd, c.CapDrop = capabilityNames(c.GetCapAdd()), capabilityNames(c.GetCapDrop())
	sortPorts(c)
	sortMounts(c)
	sortTmpfs(c)
	slices.Sort(c.Networks)
	slices.Sort(c.ExternalNetworks)
	slices.Sort(c.Aliases)
	slices.Sort(c.CapAdd)
	slices.Sort(c.CapDrop)
	slices.Sort(c.GroupAdd)
	slices.Sort(c.Expose)
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
		case m.GetExternal():
			return fmt.Errorf("a container on its own names Docker's volumes already, so the volume %s takes no external. Leave it out", v)
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
	if err := validateWords(c); err != nil {
		return err
	}
	if err := validatePorts(c); err != nil {
		return err
	}
	for _, p := range c.GetExpose() {
		if !exposedPort.MatchString(p) {
			return fmt.Errorf("expose takes a port and its protocol, such as 3000/tcp, and got %q. Write it that way", p)
		}
	}
	if err := validateMounts(c); err != nil {
		return err
	}
	if err := validateMemoryMounts(c); err != nil {
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
	if err := validateStopGrace(c); err != nil {
		return err
	}
	if err := validateRollout(c); err != nil {
		return err
	}
	return validateHealthcheck(c)
}

// validateWords checks the fields that name values, as far as a container alone can
// say: what they name, a service's Validate checks against what it uses. Their label
// is Dockwell's own, so a spec can't set it (IsOwnLabel), and what its fields come
// to goes by this field alone.
func validateWords(c *resourcev1.Container) error {
	return binding.CheckWords(c, func(string) bool { return true })
}

// validateRestart refuses a count of restarts on a policy with none, which
// Docker refuses as it makes the container, so only once the change is applied.
func validateRestart(c *resourcev1.Container) error {
	if c.GetRestartRetries() > 0 && c.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE {
		return fmt.Errorf("only on-failure restarts a set number of times, and this container's policy is %s. Use on-failure, or leave restart_retries out", restartNames[c.GetRestart()])
	}
	return nil
}

// maxStopGrace is the longest a container may ask to be given to stop. A step
// waits that long, and Stop holds a change up for no more than a minute anyway.
const maxStopGrace = time.Hour

// validateStopGrace takes a container's time to stop in whole seconds, as Docker
// keeps it, rounded up.
func validateStopGrace(c *resourcev1.Container) error {
	s := c.GetStopGracePeriod()
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > maxStopGrace {
		return fmt.Errorf("%q isn't a time for a container to stop in: give one from 1s to 1h, such as 30s", s)
	}
	if rem := d % time.Second; rem != 0 {
		d += time.Second - rem
	}
	c.StopGracePeriod = d.String()
	return nil
}

// maxDrain is the longest a deploy may wait on what is in flight.
const maxDrain = time.Hour

func validateRollout(c *resourcev1.Container) error {
	r := c.GetRollout()
	if r == nil {
		return nil
	}
	if s := r.GetDrain(); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 || d > maxDrain {
			return fmt.Errorf("%q isn't a time to let what is in flight finish: give one from 1s to 1h, such as 1m", s)
		}
		r.Drain = d.String()
	}
	if r.GetOrder() == resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST && c.GetHostNetwork() {
		return errors.New("a service on the server's own network can't start a new container before the old one stops, as both would listen on its ports. Leave the order out, or set it to stop-first")
	}
	if r.GetOrder() == resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST && len(c.GetPorts()) > 0 {
		return fmt.Errorf("a service that publishes port %d can't start a new container before the old one stops, as two can't publish one port. Send a domain to it with a route instead, or leave order out",
			c.GetPorts()[0].GetHostPort())
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
			if m.GetExternal() && strings.HasPrefix(strings.ToLower(src.Volume), serviceForm) {
				return fmt.Errorf("the volume %s has the form of an environment's, %s<ID>_<name>, so it can't be mounted as one Dockwell didn't make. Mount it by its name in its own environment", src.Volume, serviceForm)
			}
			if m.GetExternal() && anonymous(src.Volume) {
				return fmt.Errorf("the volume %s has the form of one Docker made for an image, so it can't be mounted by name. Mount a named volume", src.Volume)
			}
		case *resourcev1.Mount_HostPath:
			if m.GetExternal() {
				return fmt.Errorf("the mount at %s is a server path, which Dockwell didn't make either way, so it takes no external. Leave it out", m.Target)
			}
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

// sortTmpfs orders them by where they go, as mounts are.
func sortTmpfs(c *resourcev1.Container) {
	slices.SortFunc(c.Tmpfs, func(a, b *resourcev1.Tmpfs) int { return strings.Compare(a.GetTarget(), b.GetTarget()) })
}

// validateMemoryMounts checks /dev/shm and each tmpfs, and puts their sizes in their
// one form. Neither reaches the server: what they hold counts against the container's
// memory, so neither is host access. They run after the mounts, which are checked.
func validateMemoryMounts(c *resourcev1.Container) error {
	var err error
	if c.ShmSize, err = CanonicalMemory(c.GetShmSize()); err != nil {
		return fmt.Errorf("the size of /dev/shm: %w", err)
	}
	if n := len(c.GetMounts()) + len(c.GetTmpfs()); n > MaxMounts {
		return fmt.Errorf("the container has %d mounts, and Dockwell takes up to %d. Mount a parent path, or fewer volumes", n, MaxMounts)
	}
	seen := map[string]bool{}
	for _, m := range c.GetMounts() {
		seen[m.GetTarget()] = true
	}
	for _, t := range c.GetTmpfs() {
		if !path.IsAbs(t.GetTarget()) {
			return fmt.Errorf("a tmpfs's target %q isn't an absolute path in the container; start it with /, such as /run/cache", t.GetTarget())
		}
		if err := checkMountString(t.GetTarget(), "a tmpfs's target"); err != nil {
			return err
		}
		t.Target = path.Clean(t.GetTarget())
		if t.Target == "/" {
			return errors.New("a tmpfs can't be the container's root, as the image is there. Give it a path, such as /run/cache")
		}
		if t.Size, err = CanonicalMemory(t.GetSize()); err != nil {
			return fmt.Errorf("the tmpfs at %s: %w", t.Target, err)
		}
		if seen[t.Target] {
			return fmt.Errorf("two mounts share the target %s; give each its own", t.Target)
		}
		seen[t.Target] = true
		if t.Target == shmPath && c.GetShmSize() != "" {
			return fmt.Errorf("the container sizes %s twice, as shm_size and as a tmpfs there, and Docker would take the tmpfs. Keep one", shmPath)
		}
	}
	sortTmpfs(c)
	return nil
}

func validateHealthcheck(c *resourcev1.Container) error {
	hc := c.GetHealthcheck()
	if hc == nil {
		return nil
	}
	test := hc.GetTest()
	switch {
	case hc.GetHttp() != nil && hc.GetTcp() != nil, hc.GetHttp() != nil && len(test) > 0, hc.GetTcp() != nil && len(test) > 0:
		return errors.New("a health check is one of test, http and tcp. Keep the one you want")
	case hc.GetHttp() != nil:
		if err := validateHTTPCheck(hc.GetHttp()); err != nil {
			return err
		}
	case hc.GetTcp() != nil:
		if err := validateCheckPort(hc.GetTcp().GetPort()); err != nil {
			return err
		}
	case len(test) == 0:
		return errors.New("a health check needs a test, such as [\"CMD-SHELL\", \"curl -f http://localhost/ || exit 1\"], http, tcp, or [\"NONE\"] to turn off the image's")
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
	if AgentCheck(hc) == nil {
		return nil
	}
	// The agent makes these, with few workers, so they have limits of their own.
	if d, _ := time.ParseDuration(hc.GetInterval()); hc.GetInterval() != "" && d < health.MinInterval {
		return fmt.Errorf("a health check's interval is %s, and the agent checks at most once every %s. Give a longer one", d, health.MinInterval)
	}
	if d, _ := time.ParseDuration(hc.GetTimeout()); d > health.MaxTimeout {
		return fmt.Errorf("a health check's timeout is %s, and the agent waits at most %s. Give a shorter one", d, health.MaxTimeout)
	}
	if hc.GetRetries() > maxRetries {
		return fmt.Errorf("a health check allows %d failures in a row, and Dockwell takes up to %d. Give fewer, and a start period for an app that takes a while to start", hc.GetRetries(), maxRetries)
	}
	return nil
}

const maxRetries = 100

func validateCheckPort(p uint32) error {
	if p == 0 || p > 65535 {
		return fmt.Errorf("a health check needs the port the app listens on inside its container, from 1 to 65535, such as 8080, not %d", p)
	}
	return nil
}

// validateHTTPCheck keeps what goes on the wire to what it says: a path and a
// host that can't start another line of the request, or name anywhere else.
func validateHTTPCheck(h *resourcev1.HttpCheck) error {
	if err := validateCheckPort(h.GetPort()); err != nil {
		return err
	}
	if p := h.GetPath(); p != "" {
		switch {
		case !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//"):
			return fmt.Errorf("the health check's path %q is a path in the container's app, so it starts with one /, such as /healthz", p)
		case len(p) > maxCheckPath, strings.ContainsAny(p, " #"), strings.IndexFunc(p, unicode.IsControl) >= 0:
			return fmt.Errorf("the health check's path %q can't go in a request: use up to %d characters, with no spaces, control characters or #", p, maxCheckPath)
		}
	}
	if host := h.GetHost(); host != "" && !checkHostRE.MatchString(host) {
		return fmt.Errorf("the health check's host %q isn't one to send: give a name and, if you need one, a port, such as app.example.com", host)
	}
	return nil
}

const maxCheckPath = 1024

var checkHostRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?(:[0-9]{1,5})?$`)

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
		// It is the old one that takes this long to stop.
		if g, err := time.ParseDuration(actual.(*resourcev1.Container).GetStopGracePeriod()); err == nil && g > 0 {
			downtime = g + recreateStart
		}
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
// timeout. It is no longer than MaxHealthWait, after which Verify stops
// waiting. What the spec's check leaves out takes the image's, then
// Docker's defaults. Zero for a container without a check.
func HealthBound(spec *resourcev1.Healthcheck, img *docker.Healthcheck) time.Duration {
	if c := AgentCheck(spec); c != nil {
		return min(c.Bound(), MaxHealthWait)
	}
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
	bound := hc.StartPeriod + time.Duration(retries)*(interval+timeout)
	return min(bound, MaxHealthWait)
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

// settler is a Runtime that knows what a rollout left, as Docker does.
type settler interface {
	Left(ctx context.Context, t Target, desired *resourcev1.Container, restart string) (resource.Decision, bool, error)
}

// SettleAt says what is left of the container at t that reads back as desired:
// what a rollout that stopped partway left, or a restart it hasn't had. A runtime
// that can't tell has only a restart to do.
func (d *Driver) SettleAt(ctx context.Context, t Target, desired *resourcev1.Container, restart string) (resource.Decision, bool, error) {
	if s, ok := d.Runtime.(settler); ok {
		return s.Left(ctx, t, desired, restart)
	}
	if restart == "" {
		return resource.Decision{}, false, nil
	}
	dec, err := d.Plan(desired, desired, nil)
	return dec, err == nil, err
}

// ApplyAt carries out step on the container at t.
func (d *Driver) ApplyAt(ctx context.Context, t Target, step resource.Step, desired proto.Message) error {
	switch step.Action {
	case resource.ActionCreate, resource.ActionReplace:
		spec := desired.(*resourcev1.Container)
		if step.Adopts != "" {
			a, ok := d.Runtime.(adopter)
			if !ok {
				return errors.New("this runtime can't take on a container")
			}
			return a.Adopt(ctx, t, step.Adopts, spec, step.Decision)
		}
		if p, ok := d.Runtime.(puller); ok && step.Pull {
			same, err := p.PullAgain(ctx, t, spec, step.Pulled)
			if err != nil {
				return err
			}
			// With nothing newer, a step changes what it has to, which may be
			// nothing, and only it can say so.
			switch {
			case !same || step.Action != resource.ActionReplace:
			case len(step.Changes) == 0:
				// One that resumes what it had pulled and replaced says so as any
				// replace does.
				if !step.Pulled {
					resource.Report(ctx, resource.Outcome{
						Did:  resource.Did(resource.ActionNone),
						Note: fmt.Sprintf("%s is already the newest image, so %s stays as it is", spec.GetImage(), t.Service),
					})
				}
				return resource.ErrAlreadySo
			case inPlace(step.Changes):
				resource.Report(ctx, resource.Outcome{
					Did:  resource.Did(resource.ActionUpdate),
					Note: fmt.Sprintf("%s is already the newest image, so only %s's restart policy or limits changed", spec.GetImage(), t.Service),
				})
				return d.Runtime.Update(ctx, t, spec)
			}
		}
		return d.Runtime.Run(ctx, t, spec, step.Decision)
	case resource.ActionUpdate:
		return d.Runtime.Update(ctx, t, desired.(*resourcev1.Container))
	case resource.ActionDelete:
		return d.Runtime.Remove(ctx, t)
	default:
		return nil
	}
}

// puller is a Runtime that can ask for an image again, and tell whether what it
// gets is what runs, and is running. With pulled, it has that already.
type puller interface {
	PullAgain(ctx context.Context, t Target, spec *resourcev1.Container, pulled bool) (same bool, err error)
}

// runner is a Runtime that can say how a container that's there isn't running.
type runner interface {
	NotRunning(ctx context.Context, t Target) (string, error)
}

// NotRunning says how a container that's there isn't running, in a word.
func (d *Driver) NotRunning(ctx context.Context, ref resource.Ref) (string, error) {
	return d.NotRunningAt(ctx, HostTarget(ref))
}

// NotRunningAt says how the container at t isn't running, in a word.
func (d *Driver) NotRunningAt(ctx context.Context, t Target) (string, error) {
	if r, ok := d.Runtime.(runner); ok {
		return r.NotRunning(ctx, t)
	}
	return "", nil
}

// exposedPort is a port as Docker keeps one it exposes.
var exposedPort = regexp.MustCompile(`^\d{1,5}/(tcp|udp|sctp)$`)

// WhatRuns is a copy of a container's spec without its limits.
func (*Driver) WhatRuns(spec proto.Message) proto.Message {
	c := proto.Clone(spec).(*resourcev1.Container)
	c.Resources = nil
	return c
}

// ImageOf is the image a container runs, as it is written.
func (*Driver) ImageOf(spec proto.Message) string { return spec.(*resourcev1.Container).GetImage() }

// SetImage changes the image a container runs.
func (*Driver) SetImage(spec proto.Message, image string) { spec.(*resourcev1.Container).Image = image }

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
