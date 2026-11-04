// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// sockets are where Docker and containerd take connections, as the
// system service and rootless Docker have them.
var sockets = []string{"/var/run/docker.sock", "/run/docker.sock", "/var/run/containerd/containerd.sock", "/run/containerd/containerd.sock"}

// controlsDocker reports a server path that is, or holds, Docker's or
// containerd's socket: whoever reaches it controls Docker, which is the
// same as root.
func controlsDocker(p string) bool {
	if base := path.Base(p); base == "docker.sock" || base == "containerd.sock" {
		return true
	}
	if slices.ContainsFunc(sockets, func(s string) bool { return within(s, p) }) {
		return true
	}
	// Rootless Docker listens in its user's runtime directory, such as
	// /run/user/1000/docker.sock.
	user, ok := strings.CutPrefix(p, "/run/user")
	return ok && (user == "" || strings.HasPrefix(user, "/") && !strings.Contains(user[1:], "/"))
}

// HostAccess lists what a container reaches on its server: each server
// path it mounts, Docker's socket among them, and the server's network.
// A service's networks are its environment's, so its "host" is one of
// those (ServiceAccess).
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	c := spec.(*resourcev1.Container)
	out := mountAccess(c)
	if slices.Contains(c.GetNetworks(), "host") {
		out = append(out, resource.Access{Kind: resource.AccessNetwork})
	}
	return out
}

// ServiceAccess lists what a service's app reaches on its server: each
// server path it mounts. Its networks are its environment's.
func ServiceAccess(c *resourcev1.Container) []resource.Access { return mountAccess(c) }

func mountAccess(c *resourcev1.Container) []resource.Access {
	var out []resource.Access
	for _, m := range c.GetMounts() {
		if p := m.GetHostPath(); p != "" {
			a := PathAccess(p, m.GetReadOnly())
			a.Relabel = relabels[m.GetRelabel()]
			out = append(out, a)
		}
	}
	return out
}

// PathAccess is what mounting the server path p gives: the path, or
// control of Docker when it is or holds Docker's socket, read-only or not,
// as a socket mounted read-only still takes connections.
func PathAccess(p string, readOnly bool) resource.Access {
	if controlsDocker(p) {
		return resource.Access{Kind: resource.AccessDocker, Path: p}
	}
	return resource.Access{Kind: resource.AccessPath, Path: p, ReadOnly: readOnly}
}

// MountOptions is what the local volume driver's o option says.
type MountOptions struct {
	// Bind is true for bind or rbind.
	Bind bool
	// ReadOnly is true when ro comes last of ro and rw, as the kernel takes
	// them in order.
	ReadOnly bool
	// Address is a remote mount's server: addr=, or ip=, which CIFS takes as
	// the same option. The last of them counts, as it does for the kernel.
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
		v, err := d.Client.InspectVolume(ctx, t.DockerName(m.GetVolume()))
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

// Host is what the server keeps that no container may mount, with the
// defaults the control plane checks too.
func (p *Paths) Host(ctx context.Context) (Host, error) {
	info, err := p.Client.Info(ctx)
	if err != nil {
		return Host{}, fmt.Errorf("can't ask Docker where it keeps its data: %w", err)
	}
	h := Host{Resolve: resolve}
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

// resolve follows an absolute path on this server part by part, as the
// kernel does: .. goes up from where a symlink led, not from how the path
// is spelled. Docker makes a server path that isn't there, so what doesn't
// exist yet is taken as it is: it holds no symlinks.
func resolve(p string) (string, error) {
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
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			at = next
			continue
		case err != nil:
			return "", err
		case fi.Mode()&fs.ModeSymlink == 0:
			at = next
			continue
		}
		links++
		if links > maxLinks {
			return "", fmt.Errorf("%s leads through more than %d symlinks", p, maxLinks)
		}
		to, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(to) {
			at = "/"
		}
		rest = append(strings.Split(to, "/"), rest...)
	}
	return at, nil
}
