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
	"sync"

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

// CheckPath refuses a server path that no container may mount, by a bind
// or a volume's bind option, saying why and what to mount instead. what
// names the mount, such as "the server path / mounted at /host".
func (h Host) CheckPath(p, what string) error {
	p = path.Clean(p)
	if why := h.refuse(p); why != "" {
		return fmt.Errorf("%s %s, so no container may mount it. %s", what, why, instead)
	}
	if h.Resolve == nil {
		return nil
	}
	to, err := h.Resolve(p)
	if err != nil {
		return fmt.Errorf("can't follow %s on this server to check where it leads: %w. Mount a path the agent can read", what, err)
	}
	if why := h.refuse(to); to != p && why != "" {
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
	return slices.ContainsFunc(sockets, func(s string) bool { return within(s, p) }) || within(p, "/run/user") && !strings.Contains(strings.TrimPrefix(p, "/run/user/"), "/")
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
			out = append(out, PathAccess(p, m.GetReadOnly()))
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

// CheckHost refuses a spec that mounts a server path no container may,
// as the server has it: Dockwell's and Docker's data directories where
// they are, and where each path leads through symlinks.
func (d *Docker) CheckHost(ctx context.Context, spec *resourcev1.Container) error {
	if d.Paths == nil {
		return nil
	}
	var h Host
	for _, m := range spec.GetMounts() {
		p := m.GetHostPath()
		if p == "" {
			continue
		}
		if h.Resolve == nil {
			var err error
			if h, err = d.Paths.Host(ctx); err != nil {
				return err
			}
		}
		if err := h.CheckPath(p, fmt.Sprintf("the server path %s mounted at %s", p, m.GetTarget())); err != nil {
			return err
		}
	}
	return nil
}

// hostChecker is a Runtime that checks a spec on its server, as Docker
// does.
type hostChecker interface {
	CheckHost(ctx context.Context, spec *resourcev1.Container) error
}

// Check refuses a spec that mounts a server path no container may, as
// the agent's server has it.
func (d *Driver) Check(ctx context.Context, _ resource.Ref, desired proto.Message) error {
	return d.CheckApp(ctx, desired.(*resourcev1.Container))
}

// CheckApp checks a container's spec on the server, for a service's app
// too.
func (d *Driver) CheckApp(ctx context.Context, c *resourcev1.Container) error {
	if hc, ok := d.Runtime.(hostChecker); ok {
		return hc.CheckHost(ctx, c)
	}
	return nil
}

// Paths finds, on the agent's own server, what no container may mount:
// Dockwell's data directory, Docker's, as its engine reports it, and where
// each leads through symlinks. It follows a path's symlinks on the server
// too.
type Paths struct {
	Client *docker.Client
	// DataDir is the agent's data directory.
	DataDir string

	mu   sync.Mutex
	host *Host // once Docker has said where its data is
}

// Host is what the server keeps that no container may mount, with the
// defaults the control plane checks too.
func (p *Paths) Host(ctx context.Context) (Host, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host != nil {
		return *p.host, nil
	}
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
	p.host = &h
	return h, nil
}

// resolve follows a path's symlinks on this server. Docker makes a server
// path that isn't there, so the part that is there is followed, a symlink
// to what isn't there yet included, and the rest added as it is.
func resolve(p string) (string, error) {
	return resolveDepth(p, 0)
}

// maxLinks bounds the symlinks resolve follows, as the kernel does.
const maxLinks = 40

func resolveDepth(p string, links int) (string, error) {
	to, err := filepath.EvalSymlinks(p)
	switch {
	case err == nil:
		return to, nil
	case !errors.Is(err, fs.ErrNotExist) || p == "/":
		return "", err
	}
	parent, err := resolveDepth(filepath.Dir(p), links)
	if err != nil {
		return "", err
	}
	at := filepath.Join(parent, filepath.Base(p))
	if fi, err := os.Lstat(at); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if links >= maxLinks {
			return "", fmt.Errorf("%s leads through more than %d symlinks", p, maxLinks)
		}
		to, err := os.Readlink(at)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(to) {
			to = filepath.Join(parent, to)
		}
		return resolveDepth(filepath.Clean(to), links+1)
	}
	return at, nil
}
