// SPDX-License-Identifier: AGPL-3.0-only

package volume_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// /var/run is a symlink to /run on Debian, Ubuntu, Fedora and Alpine, so
// the kernel reads /var/run/.. as /, and path.Clean as /var. The volume's
// device isn't rewritten, so what is checked isn't what is mounted.
func TestADeviceWithDotDotIsRefused(t *testing.T) {
	d := &volume.Driver{}
	for _, dev := range []string{"/var/run/../var/lib/dockwell", "/var/run/../var/lib/docker", "/var/run/../proc/self/root", "/var/run/../boot"} {
		v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": dev}}
		if err := d.Validate(v); err == nil {
			t.Errorf("control plane: device %s passes Validate; plan words: %v", dev, resource.AccessWords(d.HostAccess(v)[0]))
		}
	}

	// The same on an agent, on a real tree shaped like a server.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "var", "lib", "dockwell")
	for _, dir := range []string{data, filepath.Join(root, "run")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../run", filepath.Join(root, "var", "run")); err != nil {
		t.Fatal(err)
	}
	dev := root + "/var/run/../var/lib/dockwell"
	a, err := os.Stat(dev)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.Stat(data)
	if !os.SameFile(a, b) {
		t.Fatal("the kernel doesn't read the device as the data directory")
	}
	e := dockertest.New(t)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Paths: &container.Paths{Client: e.Client(), DataDir: data}}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	ref := resource.Ref{Kind: volume.Kind, Scope: env, Name: "keys"}
	v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": dev}}
	if err := d.Validate(v); err == nil {
		t.Error("control plane: a device with .. in it passes Validate")
	}
	// Past Validate, the agent's own check reads the path as Docker will
	// get it, not cleaned, as the kernel would.
	if err := rt.CheckHost(t.Context(), v); err == nil || !strings.Contains(err.Error(), "is in Dockwell's data directory") {
		t.Errorf("agent: the check of a device the kernel reads as the data directory = %v", err)
	}
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: v}}); err == nil {
		t.Errorf("agent: plans a volume whose device the kernel reads as the data directory %s", data)
	}
	if err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, v); err == nil {
		got := e.Volume("env_01kat4zr6e3v0y5xbkzb5d9q6m_keys")
		t.Errorf("agent: made a volume with device %q, which is the data directory", got.Options["device"])
	}
}

// The kernel takes mount options in order: rw after ro clears it, as
// moby/sys/mount's parseOptions does.
func TestALaterRwWins(t *testing.T) {
	d := &volume.Driver{}
	v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "ro,bind,rw", "device": "/srv/media"}}
	if err := d.Validate(v); err != nil {
		t.Fatal(err)
	}
	if got := d.HostAccess(v)[0]; got.ReadOnly {
		t.Errorf("o: ro,bind,rw mounts read-write, but the plan says %s", resource.AccessWords(got))
	}
}

// overlay's lowerdir reads a path no container may mount, and the plan
// says only "a overlay mount of overlay".
func TestOverlayIsRefused(t *testing.T) {
	d := &volume.Driver{}
	v := &resourcev1.Volume{Options: map[string]string{
		"type": "overlay", "device": "overlay", "o": "lowerdir=/var/lib/dockwell,upperdir=/srv/u,workdir=/srv/w",
	}}
	if err := d.Validate(v); err == nil || !strings.Contains(err.Error(), `type "overlay"`) {
		t.Errorf("overlay over Dockwell's data directory = %v, want it refused", err)
	}
	// So is a disk, and a bind by another type, and options that mount
	// nothing a plan could name.
	for name, o := range map[string]map[string]string{
		"a disk":         {"type": "ext4", "device": "/dev/sda1"},
		"bind by nfs":    {"type": "nfs", "o": "bind", "device": "/srv"},
		"none, no bind":  {"type": "none", "device": "/srv"},
		"no type at all": {"device": "/srv", "o": "size=1m"},
	} {
		if err := d.Validate(&resourcev1.Volume{Options: o}); err == nil {
			t.Errorf("%s passes", name)
		}
	}
}

// What a person approves is all there in the plan's words, and credentials
// never are.
func TestAccessWordsNameItAllAndNoCredentials(t *testing.T) {
	d := &volume.Driver{}
	for want, o := range map[string]map[string]string{
		"the nfs mount 10.0.0.2:/export (read-only)":      {"type": "nfs", "o": "addr=10.0.0.2,ro", "device": ":/export"},
		"the nfs4 mount 10.0.0.2:/export (read-write)":    {"type": "nfs4", "o": "addr=10.0.0.2,ro,rw", "device": ":/export"},
		"the cifs mount //nas/share (read-write)":         {"type": "cifs", "o": "username=svc,password=hunter2", "device": "//nas/share"},
		"the cifs mount 10.0.0.2//nas/share (read-write)": {"type": "cifs", "o": "ip=10.0.0.2,guest", "device": "//nas/share"},
		"the cifs mount 10.0.0.3//nas/share (read-write)": {"type": "cifs", "o": "ip=10.0.0.2,addr=10.0.0.3", "device": "//nas/share"},
		"the cifs mount 10.0.0.2//nas/share (read-only)":  {"type": "cifs", "o": "addr=10.0.0.3,ip=10.0.0.2,ro", "device": "//nas/share"},
		"the nfs mount 10.0.0.2:/snap@1 (read-write)":     {"type": "nfs", "o": "addr=10.0.0.2", "device": ":/snap@1"},
		"a tmpfs mount (read-write)":                      {"type": "tmpfs", "device": "tmpfs"},
		"the server path /srv/media (read-only)":          {"type": "none", "o": "rw,bind,ro", "device": "/srv/media"},
	} {
		v := &resourcev1.Volume{Options: o}
		if err := d.Validate(v); err != nil {
			t.Fatal(err)
		}
		got := resource.AccessWords(d.HostAccess(v)[0])
		if got != want {
			t.Errorf("%v reads %q, want %q", o, got, want)
		}
	}
}

// Host access compares a mount by its type and device alone, so options
// such as an NFS server's address change unseen.
func TestMountOptionsAreCompared(t *testing.T) {
	d := &volume.Driver{}
	nfs := func(addr string) []resource.Access {
		return d.HostAccess(&resourcev1.Volume{Driver: "local", Options: map[string]string{"type": "nfs", "device": ":/export", "o": "addr=" + addr}})
	}
	if got := resource.NewAccess(nfs("10.0.0.2"), nfs("203.0.113.9")); len(got) == 0 {
		t.Error("moving a volume to another NFS server counts as no new access")
	}
}

// A plugin driver with no options reaches whatever the plugin does.
func TestAPluginDriverIsHostAccess(t *testing.T) {
	d := &volume.Driver{}
	v := &resourcev1.Volume{Driver: "acme/hostpath"}
	if err := d.Validate(v); err != nil {
		t.Fatal(err)
	}
	if len(d.HostAccess(v)) == 0 {
		t.Error("a volume on a plugin driver with no options counts as no host access, so a token applies it")
	}
}

// Docker takes a flag only when an option is exactly its name, and hands
// anything else to the filesystem, which ignores it on a bind. A plan that
// said read-only here would have a person approve write access.
func TestABindIsReadOnlyOnlyWhenDockerWouldMakeItSo(t *testing.T) {
	d := &volume.Driver{}
	for o, want := range map[string]bool{
		"bind,ro":     true,
		"ro,bind":     true,
		"bind,rw,ro":  true,
		"bind,ro,rw":  false,
		"bind, ro":    false,
		"bind,ro ":    false,
		"bind,ro=1":   false,
		"bind,RO":     false,
		"bind,\"ro\"": false,
	} {
		v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": o, "device": "/srv/x"}}
		if err := d.Validate(v); err != nil {
			t.Fatalf("o: %q: %v", o, err)
		}
		if got := d.HostAccess(v)[0].ReadOnly; got != want {
			t.Errorf("o: %q reads as read-only: %v, want %v", o, got, want)
		}
	}
	// A bind Docker wouldn't make isn't a bind.
	for _, o := range []string{" bind", "bind=1", "BIND"} {
		v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": o, "device": "/srv/x"}}
		if err := d.Validate(v); err == nil {
			t.Errorf("o: %q passes as a bind", o)
		}
	}
}

// A plan shows the share a person approves, so nothing else may name
// another, and a device never shows a password.
func TestARemoteMountCanOnlyNameWhatThePlanShows(t *testing.T) {
	d := &volume.Driver{}
	for name, o := range map[string]map[string]string{
		"unc":                    {"type": "cifs", "o": "unc=//evil/x", "device": "//nas/share"},
		"target":                 {"type": "cifs", "o": "guest,target=//evil/x", "device": "//nas/share"},
		"path":                   {"type": "cifs", "o": "path=//evil/x", "device": "//nas/share"},
		"prefixpath":             {"type": "cifs", "o": "prefixpath=other", "device": "//nas/share"},
		"another case":           {"type": "cifs", "o": "UNC=//evil/x", "device": "//nas/share"},
		"a user and password":    {"type": "cifs", "o": "guest", "device": "//svc:hunter2@nas/share"},
		"a password with a /":    {"type": "cifs", "o": "guest", "device": "//svc:hunter/2@nas/share"},
		"an @ past the server":   {"type": "cifs", "o": "guest", "device": "//evil.example/x@nas/share"},
		"backslashes":            {"type": "cifs", "o": "guest", "device": `\\\\svc:hunter2@nas\\share`},
		"no share":               {"type": "cifs", "o": "guest", "device": "//nas"},
		"no slashes":             {"type": "cifs", "o": "guest", "device": "nas/share"},
		"no device":              {"type": "cifs", "o": "guest"},
		"three slashes":          {"type": "cifs", "o": "guest", "device": "///nas/share"},
		"a user on an NFS mount": {"type": "nfs", "o": "addr=10.0.0.2", "device": "svc:hunter2@nas:/export"},
	} {
		err := d.Validate(&resourcev1.Volume{Options: o})
		if err == nil {
			t.Errorf("%s passes", name)
			continue
		}
		if strings.Contains(err.Error(), "hunter") {
			t.Errorf("%s: the refusal repeats the password: %v", name, err)
		}
	}

	// One made by hand wasn't checked, and a plan shows it as it adopts it.
	for name, o := range map[string]map[string]string{
		"cifs": {"type": "cifs", "o": "guest", "device": "//svc:hunter/2@nas/share"},
		"nfs":  {"type": "nfs", "o": "addr=10.0.0.2", "device": "svc:hunter2@nas:/export"},
	} {
		got := resource.AccessWords(d.HostAccess(&resourcev1.Volume{Options: o})[0])
		if strings.Contains(got, "hunter") {
			t.Errorf("%s made by hand reads %q", name, got)
		}
	}
}

// The server changes what the plan shows, so a token can't change it alone.
func TestAnotherRemoteIsNewAccess(t *testing.T) {
	d := &volume.Driver{}
	access := func(o string) []resource.Access {
		return d.HostAccess(&resourcev1.Volume{Options: map[string]string{"type": "cifs", "o": o, "device": "//nas/share"}})
	}
	if got := resource.NewAccess(access("addr=10.0.0.2"), access("addr=10.0.0.2,ip=6.6.6.6")); len(got) != 1 {
		t.Errorf("another ip= reads as no new access: %v", got)
	}
	if got := resource.NewAccess(access("ip=10.0.0.2"), access("addr=10.0.0.2")); len(got) != 0 {
		t.Errorf("ip= and addr= of one server read as new access: %v", got)
	}
}
