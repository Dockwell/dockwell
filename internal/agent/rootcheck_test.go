// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// rootCheck is the agent's root check on a fake engine, whose Mountinfo stands in
// for the kernel's lists of what each container mounted as it started.
func rootCheck(t *testing.T) (*dockertest.Engine, func(id string) (bool, error)) {
	t.Helper()
	e := dockertest.New(t)
	e.AddLocalImage("acme/app:1", dockertest.Image{Shell: "/bin/sh"})
	client := e.Client()
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Paths: &container.Paths{Client: client, DataDir: t.TempDir()}}}
	drivers := resource.NewRegistry(&containers, &service.Driver{Containers: containers})
	check := ContainerAsGoodAsRoot(drivers, client, FollowPaths, e.Mountinfo)
	return e, func(id string) (bool, error) { return check(t.Context(), id) }
}

func bindFrom(name, source string, inMounts, inBinds bool) docker.Container {
	c := docker.Container{Name: "/" + name, Config: docker.Config{Image: "acme/app:1"}}
	if inBinds {
		c.HostConfig.Binds = []string{source + ":/host"}
	}
	if inMounts {
		c.Mounts = []docker.MountPoint{{Type: "bind", Source: source, Destination: "/host", RW: true}}
	}
	return c
}

// withVolume is a container that mounts the volume v, which has options, and isn't
// there for nil options.
func withVolume(e *dockertest.Engine, name, v string, options map[string]string) docker.Container {
	if options != nil {
		e.AddVolume(docker.Volume{Name: v, Driver: "local", Options: options})
	}
	return docker.Container{
		Name: "/" + name, Config: docker.Config{Image: "acme/app:1"},
		HostConfig: docker.HostConfig{Binds: []string{v + ":/data"}},
		Mounts:     []docker.MountPoint{{Type: "volume", Name: v, Destination: "/data", RW: true}},
	}
}

// A container counts as root on its server by what it mounts, however that's
// written: through links, relative ones and links to links, a parent that's a link,
// the kernel's own trees, only in Mounts or only in Binds, and volumes that bind a
// server path or mount what a plan refuses. A plain folder, a tmpfs and a remote
// share don't.
func TestARootCheckReadsWhatAContainerMounts(t *testing.T) {
	e, check := rootCheck(t)
	links := t.TempDir()
	safe := filepath.Join(links, "safe")
	if err := os.Mkdir(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	link := func(name, to string) {
		if err := os.Symlink(to, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("parent", "/etc")
	relEtc, err := filepath.Rel(links, "/etc")
	if err != nil {
		t.Fatal(err)
	}
	link("rel", relEtc)
	link("hop2", "/etc")
	link("hop1", "hop2")
	link("devlink", "/etc")
	link("relsock", "../../../../../../../../var/run/docker.sock")
	for _, c := range []struct {
		c    docker.Container
		root bool
	}{
		{bindFrom("parent-link", filepath.Join(links, "parent", "ssh"), true, true), true},
		{bindFrom("relative-link", filepath.Join(links, "rel"), true, true), true},
		{bindFrom("link-to-link", filepath.Join(links, "hop1"), true, true), true},
		{bindFrom("relative-sock", filepath.Join(links, "relsock"), true, true), true},
		{bindFrom("proc-root", "/proc/1/root", true, true), true},
		{bindFrom("sys-cgroup", "/sys/fs/cgroup", true, true), true},
		{bindFrom("mounts-only", "/etc", true, false), true},
		{bindFrom("binds-only", "/etc", false, true), true},
		{bindFrom("safe-dir", safe, true, true), false},
		{withVolume(e, "vol-devlink", "v-devlink", map[string]string{"type": "none", "o": "bind", "device": filepath.Join(links, "devlink")}), true},
		{withVolume(e, "vol-rbind", "v-rbind", map[string]string{"type": "none", "o": "rbind", "device": "/etc"}), true},
		{withVolume(e, "vol-relative-dev", "v-reldev", map[string]string{"type": "none", "o": "bind", "device": "etc"}), true},
		{withVolume(e, "vol-tmpfs", "v-tmpfs", map[string]string{"type": "tmpfs", "device": "tmpfs", "o": "size=1m"}), false},
		{withVolume(e, "vol-nfs", "v-nfs", map[string]string{"type": "nfs", "device": ":/export", "o": "addr=192.0.2.10"}), false},
		{withVolume(e, "vol-plain", "v-plain", map[string]string{}), false},
		// What a plan refuses (volume.validateMount) isn't a bind, and reaches what
		// the plan couldn't show.
		{withVolume(e, "vol-ext4", "v-ext4", map[string]string{"type": "ext4", "device": "/dev/sda1"}), true},
		{withVolume(e, "vol-ext4-other", "v-ext4-other", map[string]string{"type": "ext4", "device": "/dev/sdb1"}), true},
		{withVolume(e, "vol-overlay", "v-overlay", map[string]string{"type": "overlay", "device": "overlay", "o": "lowerdir=/etc,upperdir=/srv/u,workdir=/srv/w"}), true},
		{withVolume(e, "vol-proc", "v-proc", map[string]string{"type": "proc", "device": "proc"}), true},
		{withVolume(e, "vol-sysfs", "v-sysfs", map[string]string{"type": "sysfs", "device": "sysfs"}), true},
	} {
		added := e.AddContainer(c.c)
		root, err := check(added.ID)
		if root != c.root {
			t.Errorf("%s: root %v, want %v (%v)", c.c.Name, root, c.root, err)
		}
	}
}

// What's mounted is what the kernel mounted as the container started, which a link
// moved since, or replaced with a folder, doesn't change: /etc is still mounted.
func TestARootCheckGoesByWhatWasMounted(t *testing.T) {
	e, check := rootCheck(t)
	links := t.TempDir()
	safe := filepath.Join(links, "safe")
	moving := filepath.Join(links, "moving")
	if err := os.Mkdir(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", moving); err != nil {
		t.Fatal(err)
	}
	c := e.AddContainer(bindFrom("moving-link", moving, true, true))
	if root, err := check(c.ID); !root {
		t.Fatalf("a link to /etc isn't root: %v", err)
	}
	if err := os.Remove(moving); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(safe, moving); err != nil {
		t.Fatal(err)
	}
	if root, err := check(c.ID); !root {
		t.Errorf("the link moved after it started, and /etc, still mounted, isn't root: %v", err)
	}
	if err := os.Remove(moving); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(moving, 0o755); err != nil {
		t.Fatal(err)
	}
	if root, err := check(c.ID); !root {
		t.Errorf("the link became a folder after it started, and /etc, still mounted, isn't root: %v", err)
	}
	// One that starts after the link moved mounts where it leads then.
	fresh := e.AddContainer(bindFrom("fresh", moving, true, true))
	if root, err := check(fresh.ID); root {
		t.Errorf("a container that mounted a plain folder is root: %v", err)
	}
}

// What can't be read counts as root: a volume Docker can't inspect, and mounts the
// kernel's list doesn't have, or that can't be read.
func TestARootCheckFailsClosed(t *testing.T) {
	e, check := rootCheck(t)
	missing := e.AddContainer(withVolume(e, "vol-missing", "v-not-there", nil))
	if root, err := check(missing.ID); !root || err == nil {
		t.Errorf("a volume Docker can't inspect: root %v, %v", root, err)
	}

	client := e.Client()
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Paths: &container.Paths{Client: client, DataDir: t.TempDir()}}}
	drivers := resource.NewRegistry(&containers, &service.Driver{Containers: containers})
	plain := e.AddContainer(withVolume(e, "plain-1", "v-plain-1", map[string]string{}))
	unreadable := ContainerAsGoodAsRoot(drivers, client, FollowPaths, func(int) (string, error) { return "", errors.New("permission denied") })
	if root, err := unreadable(t.Context(), plain.ID); !root || err == nil {
		t.Errorf("mounts that can't be read: root %v, %v", root, err)
	}
	elsewhere := ContainerAsGoodAsRoot(drivers, client, FollowPaths, func(pid int) (string, error) {
		if pid == 0 {
			return e.Mountinfo(0)
		}
		return "600 500 0:90 / / rw - overlay overlay rw\n", nil
	})
	if root, err := elsewhere(t.Context(), plain.ID); !root || err == nil {
		t.Errorf("a mount the kernel's list doesn't have: root %v, %v", root, err)
	}
	// One with nothing mounted needs no list at all.
	bare := e.AddContainer(docker.Container{Name: "/bare-1", Config: docker.Config{Image: "acme/app:1"}})
	if root, err := unreadable(t.Context(), bare.ID); root || err != nil {
		t.Errorf("a container that mounts nothing: root %v, %v", root, err)
	}
}

// What Docker says of a volume is enough to count it as root where it mounts what a
// plan refuses, before the kernel's list is read, so the two can't drift apart: the
// plan's own list of what the local driver may mount (container.LocalMountType).
func TestAVolumeAPlanRefusesIsRoot(t *testing.T) {
	e, _ := rootCheck(t)
	client := e.Client()
	for _, c := range []struct {
		name    string
		options map[string]string
		root    bool
	}{
		{"ext4", map[string]string{"type": "ext4", "device": "/dev/sda1"}, true},
		{"overlay", map[string]string{"type": "overlay", "device": "overlay", "o": "lowerdir=/etc,upperdir=/srv/u,workdir=/srv/w"}, true},
		{"proc", map[string]string{"type": "proc", "device": "proc"}, true},
		{"sysfs", map[string]string{"type": "sysfs", "device": "sysfs"}, true},
		{"no-type", map[string]string{"device": "/dev/sdb1"}, true},
		{"relative-bind", map[string]string{"type": "none", "o": "bind", "device": "etc"}, true},
		{"tmpfs", map[string]string{"type": "tmpfs", "device": "tmpfs"}, false},
		{"nfs4", map[string]string{"type": "nfs4", "device": ":/export", "o": "addr=192.0.2.10"}, false},
		{"cifs", map[string]string{"type": "cifs", "device": "//192.0.2.10/share"}, false},
		{"plain", map[string]string{}, false},
	} {
		added := e.AddContainer(withVolume(e, "with-"+c.name, "v-"+c.name, c.options))
		inspected, err := client.InspectContainer(t.Context(), added.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, root, err := mountedAccess(t.Context(), client, inspected); root != c.root || err != nil {
			t.Errorf("a volume of %s: root %v, want %v (%v)", c.name, root, c.root, err)
		}
	}
}
