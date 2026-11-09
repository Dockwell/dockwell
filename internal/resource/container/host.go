// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a container may reach on its server (ADR 0021). Some server paths
// are never mounted, by a bind or a volume's bind option: the server's own
// system, Dockwell's data and Docker's, and what holds them. Any other
// server path, a mount Docker makes from a volume's options, Docker's
// socket and the server's network are host access, which a person
// approves.

// DefaultDockerRoot is where Docker keeps its data unless it is told
// otherwise.
const DefaultDockerRoot = "/var/lib/docker"

// Host is what a server keeps that no container may mount.
type Host struct {
	// DataDirs are Dockwell's data directories, and DockerRoots Docker's:
	// as configured, and where each leads through symlinks.
	DataDirs, DockerRoots []string
	// Resolve follows a path's symlinks on the server. Nil takes paths as
	// written, as the control plane does, which can't see the server.
	Resolve func(string) (string, error)
	// Walk is Resolve, and the links it passed through on the way. Nil says none.
	Walk func(string) (string, []string, error)
}

// DefaultHost is what the control plane checks, with the system service's
// data directory and Docker's default: it can't see the server. Each agent
// checks again with its own, following symlinks (Paths).
var DefaultHost = Host{DataDirs: []string{datadir.SystemRoot}, DockerRoots: []string{DefaultDockerRoot}}

// system are the server's own, and what each is.
var system = []struct{ path, what string }{
	{"/proc", "the kernel's view of the server's processes"},
	{"/sys", "the kernel's devices and settings"},
	{"/dev", "the server's devices, its disks among them"},
	{"/boot", "what the server boots from"},
}

// within reports whether p is base or under it.
func within(p, base string) bool {
	return p == base || strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/")
}

// instead is what to mount in place of a path no container may mount.
const instead = "Mount only the directory the container needs, such as /srv/app/data, or a volume"

// CheckSpelling refuses a server path that could read otherwise than it
// looks. The kernel follows . and .. through symlinks, so /var/run/.. is /,
// not /var, where /var/run is a symlink: cleaning such a path checks one
// path and mounts another. A // or a trailing / reads the same either way,
// so those Validate cleans. what names the mount.
func CheckSpelling(p, what string) error {
	if strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return fmt.Errorf("the server path %q has a control character in it, which Docker can't take. Write it without one", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("%s has %s in it, which the server reads through symlinks, so Dockwell can't tell where it leads. Write the path without it, such as /srv/app/data", what, part)
		}
	}
	return nil
}

// CheckPath refuses a server path that no container may mount, by a bind
// or a volume's bind option, saying why and what to mount instead. what
// names the mount, such as "the server path / mounted at /host". On a
// server it follows p as Docker will get it, not cleaned.
func (h Host) CheckPath(p, what string) error {
	clean := path.Clean(p)
	if why := h.refuse(clean); why != "" {
		return fmt.Errorf("%s %s, so no container may mount it. %s", what, why, instead)
	}
	if h.Resolve == nil {
		return nil
	}
	to, err := h.Resolve(p)
	if errors.Is(err, resource.ErrPathNotAnswering) {
		return err // it says which, and what to do, and there's no other path to offer
	}
	if err != nil {
		return fmt.Errorf("can't follow %s on this server to check where it leads: %w. Mount a path the agent can read", what, err)
	}
	if why := h.refuse(to); to != clean && why != "" {
		return fmt.Errorf("%s leads to %s through a symlink, and %s %s, so no container may mount it. %s", what, to, to, why, instead)
	}
	return nil
}

// refuse says why no container may mount p, or nothing.
func (h Host) refuse(p string) string {
	if p == "/" {
		return "is the server's whole file system, with Dockwell's keys and every container's data in it"
	}
	for _, s := range system {
		if within(p, s.path) {
			return fmt.Sprintf("is in %s, %s", s.path, s.what)
		}
	}
	for _, kept := range []struct {
		dirs []string
		what string
	}{
		{h.DataDirs, "Dockwell's data directory, %s, which holds the control plane's key and the agent's identity"},
		{h.DockerRoots, "Docker's data directory, %s, which holds every container's files and volumes"},
	} {
		for _, d := range kept.dirs {
			switch {
			case d == "":
			case within(p, d):
				return "is in " + fmt.Sprintf(kept.what, d)
			case within(d, p):
				return "holds " + fmt.Sprintf(kept.what, d)
			}
		}
	}
	return ""
}

// HostAccess lists what a container reaches on its server: each server
// path it mounts, Docker's socket among them, the networks it joins, and what
// it runs with beyond what Docker gives a container. A service's networks are
// its environment's, so its "host" is one of those (ServiceAccess). A container
// on its own has only Docker's, none of which Dockwell made, so each is access
// beyond itself, the server's own network most of all.
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	c := spec.(*resourcev1.Container)
	out := append(mountAccess(c), privilegeAccess(c)...)
	for _, n := range c.GetNetworks() {
		switch n {
		case "host":
			out = append(out, resource.Access{Kind: resource.AccessNetwork})
		case bridge, "none":
			// Docker's default, where a container with no network goes, and no
			// network at all.
		default:
			out = append(out, resource.Access{Kind: resource.AccessExternalNetwork, Path: n})
		}
	}
	return out
}

// ServiceAccess lists what a service's app reaches on its server: each server
// path it mounts, each network it joins that isn't its environment's, the
// server's own network, and what it runs with beyond what Docker gives a
// container.
func ServiceAccess(c *resourcev1.Container) []resource.Access {
	return slices.Concat(mountAccess(c), networkAccess(c), privilegeAccess(c))
}

func mountAccess(c *resourcev1.Container) []resource.Access {
	var out []resource.Access
	for _, m := range c.GetMounts() {
		if p := m.GetHostPath(); p != "" {
			a := PathAccess(p, m.GetReadOnly())
			a.Relabel = relabels[m.GetRelabel()]
			out = append(out, a)
		}
		if m.GetExternal() {
			out = append(out, resource.Access{Kind: resource.AccessExternalVolume, Path: m.GetVolume(), ReadOnly: m.GetReadOnly()})
		}
	}
	return out
}

// PathAccess is what mounting the server path p gives: the path, or
// control of Docker when it is or holds Docker's socket, read-only or not,
// as a socket mounted read-only still takes connections.
func PathAccess(p string, readOnly bool) resource.Access {
	if resource.ControlsDocker(p) {
		return resource.Access{Kind: resource.AccessDocker, Path: p}
	}
	return resource.Access{Kind: resource.AccessPath, Path: p, ReadOnly: readOnly, SameAsRoot: resource.SystemPath(p, readOnly)}
}

// MountOptions is what the local volume driver's o option says.
type MountOptions struct {
	// Bind is true for bind or rbind.
	Bind bool
	// ReadOnly is true when ro comes last of ro and rw, as the kernel takes
	// them in order.
	ReadOnly bool
	// Address is a remote mount's server: addr=, or ip=, which CIFS takes as
	// the same option. A volume names one (validateRemote).
	Address string
}

// ReadMountOptions reads a volume's o option, in order, as Docker does. It
// takes a flag only when an option is exactly its name, and passes anything
// else to the filesystem, which ignores it on a bind. So " ro" and "ro=1"
// don't make a mount read-only, and a plan mustn't say they do.
func ReadMountOptions(o string) MountOptions {
	var out MountOptions
	for _, opt := range strings.Split(o, ",") {
		switch opt {
		case "bind", "rbind":
			out.Bind = true
		case "ro":
			out.ReadOnly = true
		case "rw":
			out.ReadOnly = false
		default:
			name, value, _ := strings.Cut(opt, "=")
			if name == "addr" || name == "ip" {
				out.Address = value
			}
		}
	}
	return out
}

// localMountTypes are what the local driver may mount besides a bind. Anything
// else reads paths a plan can't name: overlay takes them in o, and a disk's
// filesystem takes a device under /dev.
var localMountTypes = []string{"nfs", "nfs4", "cifs", "tmpfs"}

// LocalMountType reports a type the local driver's options may mount besides a
// bind, which reaches nothing on the server: a plan takes these (volume's
// validateMount), and an agent's root check counts any other as root.
func LocalMountType(kind string) bool { return slices.Contains(localMountTypes, kind) }

// BindDevice is the server path a volume binds, as the local driver's
// options type: none, o: bind and device: /srv/data have it.
func BindDevice(driver string, options map[string]string) (string, bool) {
	if driver != "" && driver != "local" {
		return "", false
	}
	if !ReadMountOptions(options["o"]).Bind {
		return "", false
	}
	return options["device"], true
}

// CheckHost refuses a spec that mounts a server path no container may, as
// the server has it now: Dockwell's and Docker's data directories where
// they are, and where each path leads through symlinks. That covers a
// volume that binds a path too, as a symlink may have changed since the
// volume was made. A bind resolves each time a container starts, so this
// narrows the window, and can't close it (docs/security/sign-in.md).
func (d *Docker) CheckHost(ctx context.Context, t Target, spec *resourcev1.Container) error {
	if err := d.checkPublicPorts(ctx, spec); err != nil {
		return err
	}
	if err := d.checkNetworks(ctx, t, spec); err != nil {
		return err
	}
	if err := d.checkRouted(ctx, t, spec); err != nil {
		return err
	}
	if err := d.checkReach(ctx, t, spec); err != nil {
		return err
	}
	if err := d.checkCPUs(ctx, t, spec); err != nil {
		return err
	}
	if d.Paths == nil || len(spec.GetMounts()) == 0 {
		return nil
	}
	h, err := d.Paths.Host(ctx)
	if err != nil {
		return err
	}
	for _, m := range spec.GetMounts() {
		if p := m.GetHostPath(); p != "" {
			if err := h.CheckPath(p, fmt.Sprintf("the server path %s mounted at %s", p, m.GetTarget())); err != nil {
				return err
			}
			continue
		}
		v, err := d.Client.InspectVolume(ctx, t.VolumeName(m))
		switch {
		case docker.IsNotFound(err):
			continue // the change makes it, or checkRefs says what is wrong
		case err != nil:
			return err
		}
		if dev, ok := BindDevice(v.Driver, v.Options); ok {
			what := fmt.Sprintf("the server path %s that the volume %s binds", dev, m.GetVolume())
			if err := h.CheckPath(dev, what); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPublicPorts refuses a port the server's ingress holds: the new
// container would fail to start only once the old one was gone.
func (d *Docker) checkPublicPorts(ctx context.Context, spec *resourcev1.Container) error {
	if d.Ingress == nil {
		return nil
	}
	for _, p := range spec.GetPorts() {
		udp := p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP
		held, err := d.Ingress.PublicPort(ctx, p.GetHostPort(), udp)
		if err != nil {
			return err
		}
		if held {
			return fmt.Errorf("this server's ingress holds port %d for its routes, so a container can't publish it. Publish another port, or send the domain here with a route", p.GetHostPort())
		}
	}
	return nil
}

// Leads says where the server paths a spec mounts lead through symlinks on this
// server, those it binds through a volume too, for the ones that lead elsewhere.
// A plan says so, as a symlink to /etc reaches /etc whatever it is called.
func (d *Docker) Leads(ctx context.Context, t Target, spec *resourcev1.Container) (map[string]string, error) {
	paths, err := d.pathLeads(ctx, t, spec)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		paths = nil
	}
	drivers, err := d.networkDrivers(ctx, t, spec)
	if err != nil {
		return nil, err
	}
	if len(drivers) == 0 {
		return paths, nil
	}
	if paths == nil {
		return drivers, nil
	}
	maps.Copy(paths, drivers)
	return paths, nil
}

// networkDrivers says which driver each network the container joins uses, of those
// Dockwell didn't make, by name, where that isn't Docker's own bridge. Only the
// server knows, and a driver such as macvlan puts what joins on the server's LAN. A
// network that isn't there yet says nothing, as checkExternalNetworks refuses it.
func (d *Docker) networkDrivers(ctx context.Context, t Target, spec *resourcev1.Container) (map[string]string, error) {
	names := spec.GetExternalNetworks()
	if !t.Scoped {
		names = slices.Concat(names, spec.GetNetworks()) // a container on its own has only Docker's
	}
	var out map[string]string
	for _, name := range names {
		n, err := d.Client.NetworkNamed(ctx, name)
		switch {
		case docker.IsNotFound(err):
			continue
		case err != nil:
			return nil, err
		}
		if !slices.Contains([]string{"", bridge, "host", "null"}, n.Driver) {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = n.Driver
		}
	}
	return out, nil
}

// LeadsMoved says which of the server paths a spec mounts don't lead where they did
// when what runs now was made, as its label says. One with no label is older than
// labels, so all that can be said is whether a path gives more now than it reads as,
// or what was taken on or made before would ask on every plan.
func (d *Docker) LeadsMoved(ctx context.Context, t Target, spec *resourcev1.Container) ([]string, error) {
	leads, err := d.pathLeads(ctx, t, spec)
	if err != nil || len(leads) == 0 {
		return nil, err
	}
	all, err := d.instances(ctx, t)
	if err != nil || len(all) == 0 {
		return nil, err // nothing runs, so a plan counts all it reaches anyway
	}
	label, known := all[0].Config.Labels[LabelLeads]
	var was map[string]string
	if known {
		if err := json.Unmarshal([]byte(label), &was); err != nil {
			return nil, fmt.Errorf("can't read where %s's paths led: %w", all[0].name(), err)
		}
	}
	var moved []string
	for _, p := range slices.Sorted(maps.Keys(leads)) {
		if known && was[p] != leads[p] || !known && givesMore(p, leads[p], false) {
			moved = append(moved, p)
		}
	}
	return moved, nil
}

// leadsAsPlanned checks the server paths a container is about to mount lead where
// its plan said, as a link can change between the two and Docker follows what it
// finds, and says what to keep on the container: nothing where it has no server
// paths, and a record that none leads elsewhere where that's so, as no record at all
// says only that it's older than records. stopped says what came of a refusal.
func (d *Docker) leadsAsPlanned(ctx context.Context, t Target, spec *resourcev1.Container, dec resource.Decision, stopped string) (string, error) {
	leads, err := d.pathLeads(ctx, t, spec)
	if err != nil || leads == nil {
		return "", err
	}
	for _, p := range slices.Sorted(maps.Keys(leads)) {
		if dec.PlannedLeads[p] != leads[p] {
			return "", &pathNotPlanned{path: p, to: leads[p], stopped: stopped}
		}
	}
	label, err := json.Marshal(leads)
	if err != nil {
		return "", fmt.Errorf("can't keep where %s's paths lead: %w", t.Name, err)
	}
	return string(label), nil
}

// pathNotPlanned is a server path that leads somewhere its plan wasn't approved for.
type pathNotPlanned struct{ path, to, stopped string }

func (e *pathNotPlanned) Error() string {
	return fmt.Sprintf("the server path %s leads to %s now, which isn't what this plan was approved for, so %s. Plan the change again to see what it reaches", e.path, e.to, e.stopped)
}

// pathLeads says where the server paths a container mounts lead on this server, for
// those that lead elsewhere: empty where none does, and nil where it mounts none.
func (d *Docker) pathLeads(ctx context.Context, t Target, spec *resourcev1.Container) (map[string]string, error) {
	if d.Paths == nil || len(spec.GetMounts()) == 0 {
		return nil, nil
	}
	h, err := d.Paths.Host(ctx)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, m := range spec.GetMounts() {
		if p := m.GetHostPath(); p != "" {
			paths = append(paths, p)
			continue
		}
		v, err := d.Client.InspectVolume(ctx, t.VolumeName(m))
		switch {
		case docker.IsNotFound(err):
			continue // the change makes it, and says where its own path leads
		case err != nil:
			return nil, err
		}
		if dev, ok := BindDevice(v.Driver, v.Options); ok {
			paths = append(paths, dev)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return h.Leads(paths...)
}

// Leads says where each path leads on the server, for those that lead elsewhere.
func (h Host) Leads(paths ...string) (map[string]string, error) {
	if h.Resolve == nil {
		return nil, nil
	}
	out := map[string]string{}
	for _, p := range paths {
		to, err := h.Resolve(p)
		if err != nil {
			return nil, fmt.Errorf("can't follow the server path %s to see where it leads: %w. Mount a path the agent can read", p, err)
		}
		if clean := path.Clean(p); to != clean {
			out[clean] = to
		}
	}
	return out, nil
}

// pathLeader is a Runtime that can say where a spec's server paths lead, as
// Docker can.
type pathLeader interface {
	Leads(ctx context.Context, t Target, spec *resourcev1.Container) (map[string]string, error)
}

// Leads says where the server paths a container mounts lead on this server.
func (d *Driver) Leads(ctx context.Context, ref resource.Ref, desired proto.Message) (map[string]string, error) {
	return d.LeadsAt(ctx, HostTarget(ref), desired.(*resourcev1.Container))
}

// LeadsAt says where the server paths the container at t mounts lead, for a
// service's app too.
func (d *Driver) LeadsAt(ctx context.Context, t Target, c *resourcev1.Container) (map[string]string, error) {
	if l, ok := d.Runtime.(pathLeader); ok {
		return l.Leads(ctx, t, c)
	}
	return nil, nil
}

// pathMover is a Runtime that can say which server paths have moved (PathMover).
type pathMover interface {
	LeadsMoved(ctx context.Context, t Target, spec *resourcev1.Container) ([]string, error)
}

// LeadsMoved says which of the server paths a container mounts don't lead where
// they did when what runs now was made.
func (d *Driver) LeadsMoved(ctx context.Context, ref resource.Ref, desired proto.Message) ([]string, error) {
	return d.LeadsMovedAt(ctx, HostTarget(ref), desired.(*resourcev1.Container))
}

// LeadsMovedAt is LeadsMoved for the container at t, for a service's app too.
func (d *Driver) LeadsMovedAt(ctx context.Context, t Target, c *resourcev1.Container) ([]string, error) {
	if m, ok := d.Runtime.(pathMover); ok {
		return m.LeadsMoved(ctx, t, c)
	}
	return nil, nil
}

// checkReach refuses a check the agent can't make, at plan time, rather than as
// a deploy that fails and rolls back. The agent reaches a container at the address
// Docker reports, which the server itself can do on a bridge network and not
// otherwise: rootless Docker keeps its networks in a namespace of its own, and a
// server can't reach a macvlan or ipvlan container of its own, or an overlay's.
// A container on several networks may be checked on any of them, so one such
// network is enough to refuse. A command check runs inside the container, and has
// none of this. A network the same change is still to make isn't there to ask,
// and there the check's own failure says what is wrong.
func (d *Docker) checkReach(ctx context.Context, t Target, spec *resourcev1.Container) error {
	check := spec.GetHealthcheck()
	var kind string
	switch {
	case check.GetHttp() != nil:
		kind = "An HTTP check"
	case check.GetTcp() != nil:
		kind = "A TCP check"
	default:
		return nil
	}
	info, err := d.Client.Info(ctx)
	if err != nil {
		return err
	}
	if info.Rootless() {
		return fmt.Errorf("this server's Docker runs rootless, so the agent can't reach a container's address. %s can't run here. Use a command check, which Docker runs inside the container", kind)
	}
	// A network the server has is its own name in Docker, and checkNetworks refuses
	// one that isn't there.
	names := map[string]string{}
	for _, name := range spec.GetNetworks() {
		names[name] = t.DockerName(name)
	}
	for _, name := range spec.GetExternalNetworks() {
		names[name] = name
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		n, err := d.Client.NetworkNamed(ctx, names[name])
		switch {
		case docker.IsNotFound(err):
			continue
		case err != nil:
			return err
		}
		if n.Driver != "bridge" && n.Driver != "" {
			return fmt.Errorf("the network %s uses the %s driver, so the agent can't reach a container's address on it. %s can't run here. Use a command check, which Docker runs inside the container", name, n.Driver, kind)
		}
	}
	return nil
}

// hostChecker is a Runtime that checks a spec on its server, as Docker
// does.
type hostChecker interface {
	CheckHost(ctx context.Context, t Target, spec *resourcev1.Container) error
}

// Check refuses a spec that mounts a server path no container may, as
// the agent's server has it.
func (d *Driver) Check(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	return d.CheckAt(ctx, HostTarget(ref), desired.(*resourcev1.Container))
}

// CheckAt checks the container at t's spec on the server, for a service's
// app too.
func (d *Driver) CheckAt(ctx context.Context, t Target, c *resourcev1.Container) error {
	if hc, ok := d.Runtime.(hostChecker); ok {
		return hc.CheckHost(ctx, t, c)
	}
	return nil
}

// Paths finds, on the agent's own server, what no container may mount:
// Dockwell's data directory, Docker's, as its engine reports it, and where
// each leads through symlinks. It follows a path's symlinks on the server
// too. It asks Docker each time: Docker's data root can change with a
// restart of Docker, which the agent reconnects to without a word.
type Paths struct {
	Client *docker.Client
	// DataDir is the agent's data directory.
	DataDir string
}

// LeadsAsMade checks where the server paths c mounts lead, before something starts
// it: a rollback's way back, or what stopped it for a backup. Docker follows what it
// finds, so a folder that became a link since would hand c what the link leads to,
// and a token can have a backup stop and start it at will. It goes by c's record, and
// by planned, where a person approved paths for the step at hand.
//
// Where one has moved, c is kept from starting by itself too, as a daemon restart or
// a reboot would follow the link with nobody asked. A plan that remakes it puts its
// restart policy back.
func (p *Paths) LeadsAsMade(ctx context.Context, c *docker.Container, planned map[string]string) error {
	moved, err := p.movedSince(ctx, c, planned)
	if err != nil || moved == nil {
		return err
	}
	return p.holdDown(ctx, c.ID, moved)
}

// holdDown keeps a container that a moved path stopped from starting by itself, and
// returns why, which is what the caller says.
func (p *Paths) holdDown(ctx context.Context, containerID string, why error) error {
	if err := p.Client.UpdateContainer(ctx, containerID, docker.ContainerUpdate{RestartPolicy: docker.RestartPolicy{Name: "no"}}); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("%w. Docker wouldn't stop it starting by itself, after a restart of the server: %w", why, err)
	}
	return why
}

// movedSince is a server path c mounts that leads somewhere nobody approved for it:
// not where its record says, nor where planned says. With no record c is older than
// records, so all that can be said is whether a path gives more now than it reads
// as: refusing any link would keep /var/run/docker.sock down wherever /var/run is one.
func (p *Paths) movedSince(ctx context.Context, c *docker.Container, planned map[string]string) (*PathMovedError, error) {
	if p == nil {
		return nil, nil
	}
	var paths []string
	readOnly := map[string]bool{}
	for _, m := range c.Mounts {
		source := m.Source
		switch m.Type {
		case "bind":
		case "volume":
			v, err := p.Client.InspectVolume(ctx, m.Name)
			switch {
			case docker.IsNotFound(err):
				continue
			case err != nil:
				return nil, err
			}
			dev, ok := BindDevice(v.Driver, v.Options)
			if !ok {
				continue
			}
			source = dev
		default:
			continue
		}
		paths = append(paths, source)
		readOnly[path.Clean(source)] = !m.RW
	}
	now, err := p.Follow(ctx, paths...)
	if err != nil {
		return nil, err
	}
	label, known := c.Config.Labels[LabelLeads]
	var was map[string]string
	if known {
		if err := json.Unmarshal([]byte(label), &was); err != nil {
			return nil, fmt.Errorf("can't read where %s's paths led: %w", strings.TrimPrefix(c.Name, "/"), err)
		}
	}
	for _, at := range slices.Sorted(maps.Keys(now)) {
		switch {
		case planned[at] == now[at]:
		case known && was[at] == now[at]:
		case !known && !givesMore(at, now[at], readOnly[at]):
		default:
			return &PathMovedError{Path: at, To: now[at], Name: cmp.Or(c.Config.Labels[docker.LabelService], strings.TrimPrefix(c.Name, "/"))}, nil
		}
	}
	return nil, nil
}

// givesMore says a path with no record of where it led gives more now than it reads
// as, which is all that can be said of it.
func givesMore(at, to string, readOnly bool) bool {
	a := PathAccess(at, readOnly)
	return !a.AsGoodAsRoot() && a.Leading(to).AsGoodAsRoot()
}

// Survey says where the server paths on this server lead, for those that lead
// elsewhere: what every container here mounts, what the volumes they mount bind, and
// paths, each with the folder it's in. A plan works out what's inside what, and one
// folder can be written two ways, so it needs every path here and not only its own.
// One that can't be followed counts as it's written, as a plan for something else
// shouldn't fail on it. It names the containers Dockwell didn't make, with the paths
// they mount, as something a plan makes can write around those too.
func (d *Docker) Survey(ctx context.Context, paths []string) (resource.Surveyed, error) {
	if d.Paths == nil {
		return resource.Surveyed{}, nil
	}
	list, err := d.Client.ListContainers(ctx, nil)
	if err != nil {
		return resource.Surveyed{}, err
	}
	devices := map[string]string{}
	type mounted struct {
		paths    []string
		readOnly map[string]bool
	}
	unmanaged := map[string]*mounted{}
	for i := range list {
		c := &list[i]
		var mine *mounted
		if !madeByDockwell(c.Labels) {
			mine = &mounted{readOnly: map[string]bool{}}
			unmanaged[listedName(c.Names)] = mine
		}
		for _, m := range c.Mounts {
			source := ""
			switch {
			case m.Type == "bind":
				source = m.Source
			case m.Type == "volume" && m.Name != "":
				dev, seen := devices[m.Name]
				if !seen {
					v, err := d.Client.InspectVolume(ctx, m.Name)
					switch {
					case docker.IsNotFound(err):
					case err != nil:
						return resource.Surveyed{}, err
					default:
						dev, _ = BindDevice(v.Driver, v.Options)
					}
					devices[m.Name] = dev
				}
				source = dev
			}
			if source == "" {
				continue
			}
			paths = append(paths, source)
			if mine != nil {
				mine.paths = append(mine.paths, path.Clean(source))
				mine.readOnly[path.Clean(source)] = !m.RW
			}
		}
	}
	h, err := d.Paths.Host(ctx)
	if err != nil || h.Resolve == nil {
		return resource.Surveyed{}, err
	}
	out := resource.Surveyed{Leads: map[string]string{}, Via: map[string][]string{}}
	seen := map[string]bool{}
	for _, p := range paths {
		for i, at := range []string{path.Clean(p), path.Dir(path.Clean(p))} {
			if seen[at] || !path.IsAbs(at) {
				continue
			}
			seen[at] = true
			to, err := h.Resolve(at)
			// For a path itself, every link on the way: rewriting any of them moves it.
			if i == 0 && h.Walk != nil {
				var via []string
				if to, via, err = h.Walk(at); err == nil && len(via) > 0 {
					out.Via[at] = via
				}
			}
			if err == nil && to != at {
				out.Leads[at] = to
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(unmanaged)) {
		m := unmanaged[name]
		if len(m.paths) == 0 {
			continue
		}
		u := resource.Unmanaged{Name: name, Paths: m.paths}
		for _, p := range m.paths {
			u.Root = u.Root || PathAccess(p, m.readOnly[p]).Leading(out.Leads[p]).AsGoodAsRoot()
		}
		out.Unmanaged = append(out.Unmanaged, u)
	}
	return out, nil
}

// listedName is a container's name as a list has it, without a link's.
func listedName(names []string) string {
	for _, n := range names {
		if n = strings.TrimPrefix(n, "/"); !strings.Contains(n, "/") {
			return n
		}
	}
	return ""
}

// madeByDockwell says a container's labels are Dockwell's: what it made or took on,
// and its own, such as the proxy.
func madeByDockwell(labels map[string]string) bool {
	if labels[docker.LabelProject] != "" {
		return true
	}
	for k := range labels {
		if strings.HasPrefix(k, docker.OwnPrefix) {
			return true
		}
	}
	return false
}

// Survey says where every server path on the server leads (Docker.Survey).
func (d *Driver) Survey(ctx context.Context, paths []string) (resource.Surveyed, error) {
	if s, ok := d.Runtime.(interface {
		Survey(ctx context.Context, paths []string) (resource.Surveyed, error)
	}); ok {
		return s.Survey(ctx, paths)
	}
	return resource.Surveyed{}, nil
}

// Follow says where server paths lead on this server, for those that lead elsewhere.
func (p *Paths) Follow(ctx context.Context, paths ...string) (map[string]string, error) {
	if p == nil || len(paths) == 0 {
		return nil, nil
	}
	h, err := p.Host(ctx)
	if err != nil {
		return nil, err
	}
	return h.Leads(paths...)
}

// VolumeAsPlanned checks the server path a volume binds leads where a plan said, as
// a helper mounts the volume and Docker follows what it finds.
// stopped says what came of a refusal.
func (p *Paths) VolumeAsPlanned(ctx context.Context, v *docker.Volume, planned map[string]string, stopped string) error {
	dev, ok := BindDevice(v.Driver, v.Options)
	if !ok {
		return nil
	}
	now, err := p.Follow(ctx, dev)
	if err != nil {
		return err
	}
	for _, at := range slices.Sorted(maps.Keys(now)) {
		if planned[at] != now[at] {
			return fmt.Errorf("the server path %s leads to %s now, which isn't what this plan was approved for, so %s. Plan it again to see what it reaches", at, now[at], stopped)
		}
	}
	return nil
}

// PathMovedError is a container that wasn't started, as a server path it mounts leads
// somewhere nobody approved. docker start would follow it, so its fix is its own.
type PathMovedError struct {
	Path, To string
	// Name is the service, or the container where it's no service's.
	Name string
}

func (e *PathMovedError) Error() string {
	return fmt.Sprintf("the server path %s leads to %s now, which isn't where a person approved for %s, so it wasn't started, and won't start by itself, as Docker would follow it. Put the path back and plan a start of %s, or plan a restart of it, which asks a person to approve where the path leads now",
		e.Path, e.To, e.Name, e.Name)
}

// Host is what the server keeps that no container may mount, with the
// defaults the control plane checks too.
func (p *Paths) Host(ctx context.Context) (Host, error) {
	info, err := p.Client.Info(ctx)
	if err != nil {
		return Host{}, fmt.Errorf("can't ask Docker where it keeps its data: %w", err)
	}
	h := Host{Resolve: resolve, Walk: resolveVia}
	add := func(list *[]string, dirs ...string) {
		for _, d := range dirs {
			if d == "" {
				continue
			}
			d = filepath.Clean(d)
			*list = append(*list, d)
			if to, err := resolve(d); err == nil && to != d {
				*list = append(*list, to)
			}
		}
	}
	add(&h.DataDirs, datadir.SystemRoot, p.DataDir)
	add(&h.DockerRoots, DefaultDockerRoot, info.DockerRootDir)
	return h, nil
}

// maxLinks bounds the symlinks resolve follows, as the kernel's limit does.
const maxLinks = 40

// Resolve follows an absolute path on this server, as resolve does.
func Resolve(p string) (string, error) { return resolve(p) }

// resolve is walkLinks with a bound, as a path can lead into a mount that has
// stopped answering (followWithin).
func resolve(p string) (string, error) { return followWithin(p, walkLinks) }

// resolveVia is resolve, and the links the walk passed through on the way, each by
// its own path in the folder it's in, resolved: what moves the path where it leads
// if something rewrites it. They're read only once the walk is back, which the
// walk's own answer says.
func resolveVia(p string) (string, []string, error) {
	var via []string
	to, err := followWithin(p, func(p string, touch func(string) error) (string, error) {
		to, links, err := walkLinksVia(p, touch)
		via = links
		return to, err
	})
	if err != nil {
		return "", nil, err
	}
	return to, via, nil
}

// walkLinks follows an absolute path on this server part by part, as the
// kernel does: .. goes up from where a symlink led, not from how the path
// is spelled. Docker makes a server path that isn't there, so what doesn't
// exist yet is taken as it is: it holds no symlinks.
func walkLinks(p string, touch func(string) error) (string, error) {
	to, _, err := walkLinksVia(p, touch)
	return to, err
}

// walkLinksVia is walkLinks, and the links it passed through, by their own paths.
func walkLinksVia(p string, touch func(string) error) (string, []string, error) {
	var via []string
	at := "/"
	rest := strings.Split(p, "/")
	for links := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			at = filepath.Dir(at)
			continue
		}
		next := filepath.Join(at, part)
		if err := touch(next); err != nil {
			return "", nil, err
		}
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			at = next
			continue
		case err != nil:
			return "", nil, err
		case fi.Mode()&fs.ModeSymlink == 0:
			at = next
			continue
		}
		links++
		if links > maxLinks {
			return "", nil, fmt.Errorf("%s leads through more than %d symlinks", p, maxLinks)
		}
		to, err := os.Readlink(next)
		if err != nil {
			return "", nil, err
		}
		via = append(via, next)
		if filepath.IsAbs(to) {
			at = "/"
		}
		rest = append(strings.Split(to, "/"), rest...)
	}
	return at, via, nil
}

// Planned is what a plan wants of a container on this server, for checking what
// runs beside it. A nil Spec takes it away.
type Planned struct {
	// Name is its container's name, and Project and Service its Compose labels,
	// which find it whatever it's named (ADR 0035).
	Name             string
	Project, Service string
	// Words name it for people, such as "web".
	Words string
	Spec  *resourcev1.Container
}

// portChecker is a Runtime that can see what its server has published.
type portChecker interface {
	CheckPorts(ctx context.Context, t Target, spec *resourcev1.Container, planned []Planned) error
}

// CheckPortsAt refuses a port another container on this server holds, or another
// in the plan wants. Applying would otherwise stop what runs, fail to start it,
// and roll back, for a plan that could have said so.
func (d *Driver) CheckPortsAt(ctx context.Context, t Target, c *resourcev1.Container, planned []Planned) error {
	for _, other := range planned {
		if other.Name == t.Name || other.Spec == nil {
			continue
		}
		for _, p := range c.GetPorts() {
			if publishes(other.Spec, p) {
				return fmt.Errorf("%s also publishes %s in this same change, and only one can. Choose another port on the server for one of them", other.Words, portWords(p))
			}
		}
	}
	if pc, ok := d.Runtime.(portChecker); ok {
		return pc.CheckPorts(ctx, t, c, planned)
	}
	return nil
}

// CheckPlan refuses a port something else on this server holds, as a container
// run by hand is about to start with it.
func (d *Driver) CheckPlan(ctx context.Context, want resource.Desired, all []resource.Desired) error {
	var planned []Planned
	for _, o := range all {
		if o.Ref.Kind != Kind {
			continue
		}
		c, _ := o.Spec.(*resourcev1.Container)
		h := HostTarget(o.Ref)
		planned = append(planned, Planned{Name: h.Name, Project: h.Project, Service: h.Service, Words: o.Ref.Name, Spec: c})
	}
	return d.CheckPortsAt(ctx, HostTarget(want.Ref), want.Spec.(*resourcev1.Container), planned)
}

// publishes reports whether a container publishes something that can't share a
// server with p.
func publishes(c *resourcev1.Container, p *resourcev1.PortMapping) bool {
	return slices.ContainsFunc(c.GetPorts(), func(o *resourcev1.PortMapping) bool {
		return o.GetHostPort() == p.GetHostPort() && protocolName(o.GetProtocol()) == protocolName(p.GetProtocol()) && overlap(o.GetHostIp(), p.GetHostIp())
	})
}

// portWords names a port on the server, with its address when it has only one.
func portWords(p *resourcev1.PortMapping) string {
	words := fmt.Sprintf("port %d", p.GetHostPort())
	if p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
		words += "/udp"
	}
	if p.GetHostIp() != "" {
		words += " on " + addressWords(p.GetHostIp())
	}
	return words
}

// CheckPorts refuses a port that something this server runs has bound, by what
// it is called there: what Docker reports, so a container Dockwell didn't make
// counts, and a stopped one, which holds nothing, doesn't. What the plan itself
// takes away or moves off the port doesn't count either. A process on the server
// that isn't a container isn't seen: starting then fails, as it always did.
func (d *Docker) CheckPorts(ctx context.Context, t Target, spec *resourcev1.Container, planned []Planned) error {
	if len(spec.GetPorts()) == 0 {
		return nil
	}
	list, err := d.Client.ListContainers(ctx, nil)
	if err != nil {
		return fmt.Errorf("can't list what this server runs, to check its ports are free: %w", err)
	}
	for i := range list {
		c := &list[i]
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if name == t.Name || c.Labels[docker.LabelProject] == t.Project && c.Labels[docker.LabelService] == t.Service {
			continue // what this is about to replace
		}
		for _, p := range spec.GetPorts() {
			if !holds(c, p) || freedBy(planned, c, name, p) {
				continue
			}
			return fmt.Errorf("%s publishes %s, so %s can't. Choose another port on the server, or free that one first",
				holderWords(t, c, name), portWords(p), t.Service)
		}
	}
	return nil
}

// holds reports whether a listed container has bound what p publishes.
func holds(c *docker.ContainerSummary, p *resourcev1.PortMapping) bool {
	return slices.ContainsFunc(c.Ports, func(held docker.SummaryPort) bool {
		if uint32(held.PublicPort) != p.GetHostPort() || held.Type != protocolName(p.GetProtocol()) {
			return false
		}
		// The engine's word for an address isn't checked as a spec's is.
		if _, err := netip.ParseAddr(held.IP); err != nil {
			return true
		}
		return overlap(p.GetHostIp(), held.IP)
	})
}

// freedBy reports a holder the plan takes away, or moves off the port: by its
// Compose labels, as a service's container may be named the readable way, the way
// Compose names one, or under another number than its first, and by its name for
// one with none.
func freedBy(planned []Planned, c *docker.ContainerSummary, name string, p *resourcev1.PortMapping) bool {
	project, service := c.Labels[docker.LabelProject], c.Labels[docker.LabelService]
	i := slices.IndexFunc(planned, func(o Planned) bool {
		if project != "" && service != "" && o.Project != "" {
			return o.Project == project && o.Service == service
		}
		return o.Name == name
	})
	return i >= 0 && (planned[i].Spec == nil || !publishes(planned[i].Spec, p))
}

// holderWords names what holds a port, as far as this server can tell: a Compose
// project here is an environment's or a server's ID, which says nothing to people.
func holderWords(t Target, c *docker.ContainerSummary, name string) string {
	project, service := c.Labels[docker.LabelProject], c.Labels[docker.LabelService]
	switch {
	case service != "" && project == t.Project:
		return service
	case service != "" && (strings.HasPrefix(project, id.Environment+"_") || strings.HasPrefix(project, id.Server+"_")):
		return service + ", in another environment,"
	}
	return "The container " + name + ", which Dockwell didn't make,"
}
