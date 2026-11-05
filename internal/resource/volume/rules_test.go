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
		"the nfs mount 10.0.0.2:/export (read-only)":               {"type": "nfs", "o": "addr=10.0.0.2,ro", "device": ":/export"},
		"the nfs4 mount 10.0.0.2:/export (read-write)":             {"type": "nfs4", "o": "addr=10.0.0.2,ro,rw", "device": ":/export"},
		"the cifs mount //nas/share (read-write)":                  {"type": "cifs", "o": "username=svc,password=hunter2", "device": "//nas/share"},
		"the cifs mount //nas/share at 10.0.0.2 (read-write)":      {"type": "cifs", "o": "ip=10.0.0.2,guest", "device": "//nas/share"},
		"the cifs mount //nas/share at nas.example (read-only)":    {"type": "cifs", "o": "addr=nas.example,ro", "device": "//nas/share"},
		"the nfs mount nas:/export at 10.0.0.2 (read-write)":       {"type": "nfs", "o": "addr=10.0.0.2", "device": "nas:/export"},
		"the nfs mount nas3:/export at 10.0.0.2 (read-write)":      {"type": "nfs", "o": "addr=10.0.0.2", "device": "nas3:/export"},
		`the cifs mount "//6.6.6.6/x at 10.0.0.2" (read-write)`:    {"type": "cifs", "o": "guest", "device": "//6.6.6.6/x at 10.0.0.2"},
		`the cifs mount "//nas/My Share" at 10.0.0.2 (read-write)`: {"type": "cifs", "o": "addr=10.0.0.2", "device": "//nas/My Share"},
		`the nfs mount ":/x at 10.0.0.2" at 6.6.6.6 (read-write)`:  {"type": "nfs", "o": "addr=6.6.6.6", "device": ":/x at 10.0.0.2"},
		`the cifs mount "//nas1/文档" at 10.0.0.2 (read-write)`:      {"type": "cifs", "o": "addr=10.0.0.2", "device": "//nas1/文档"},
		"a tmpfs mount (read-write)":                               {"type": "tmpfs", "device": "tmpfs"},
		"the server path /srv/media (read-only)":                   {"type": "none", "o": "rw,bind,ro", "device": "/srv/media"},
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

// A plan shows the server and share a person approves, and masks the rest
// of o, so only what can't change them passes.
func TestARemoteMountCanOnlyNameWhatThePlanShows(t *testing.T) {
	d := &volume.Driver{}
	const share, export = "//nas/share", ":/export"
	for name, o := range map[string]map[string]string{
		// Options that name another server or share, or change how the rest reads.
		"source":                {"type": "cifs", "o": "source=//evil/x", "device": share},
		"unc":                   {"type": "cifs", "o": "unc=//evil/x", "device": share},
		"target":                {"type": "cifs", "o": "guest,target=//evil/x", "device": share},
		"path":                  {"type": "cifs", "o": "path=//evil/x", "device": share},
		"prefixpath":            {"type": "cifs", "o": "prefixpath=other", "device": share},
		"sep":                   {"type": "cifs", "o": "sep=;;ip=6.6.6.6", "device": share},
		"a double comma":        {"type": "cifs", "o": "addr=6.6.6.6,password=hunter2,,ip=10.0.0.2", "device": share},
		"a comma at the end":    {"type": "cifs", "o": "guest,", "device": share},
		"sloppy on CIFS":        {"type": "cifs", "o": "addr=6.6.6.6,sloppy,ip=", "device": share},
		"sloppy on NFS":         {"type": "nfs", "o": "addr=6.6.6.6,sloppy,ip=10.0.0.2", "device": export},
		"ip on NFS":             {"type": "nfs", "o": "addr=10.0.0.2,ip=6.6.6.6", "device": export},
		"mountaddr":             {"type": "nfs", "o": "addr=10.0.0.2,mountaddr=6.6.6.6", "device": export},
		"mounthost":             {"type": "nfs4", "o": "addr=10.0.0.2,mounthost=evil", "device": export},
		"source on NFS":         {"type": "nfs", "o": "addr=10.0.0.2,source=evil:/x", "device": export},
		"anything unknown":      {"type": "cifs", "o": "whatever=1", "device": share},
		"another case":          {"type": "cifs", "o": "ADDR=6.6.6.6", "device": share},
		"a space":               {"type": "cifs", "o": "guest, ip=6.6.6.6", "device": share},
		"a comma in a password": {"type": "cifs", "o": "password=a,hunter2", "device": share},

		// Which server, once.
		"two servers":    {"type": "cifs", "o": "addr=10.0.0.2,addr=6.6.6.6", "device": share},
		"addr and ip":    {"type": "cifs", "o": "addr=10.0.0.2,ip=6.6.6.6", "device": share},
		"no NFS server":  {"type": "nfs", "o": "rw", "device": export},
		"an empty addr":  {"type": "nfs", "o": "addr=", "device": export},
		"a name for ip":  {"type": "cifs", "o": "ip=nas.example", "device": share},
		"words for addr": {"type": "nfs", "o": "addr=10.0.0.2 and more", "device": export},

		// A value where none goes, and the other way round.
		"ro=1":           {"type": "nfs", "o": "addr=10.0.0.2,ro=1", "device": export},
		"username alone": {"type": "cifs", "o": "username", "device": share},
		"addr alone":     {"type": "nfs", "o": "addr", "device": export},
		"guest=1":        {"type": "cifs", "o": "guest=1", "device": share},

		// Devices.
		"a user and password":       {"type": "cifs", "o": "guest", "device": "//svc:hunter2@nas/share"},
		"a password with a /":       {"type": "cifs", "o": "guest", "device": "//svc:hunter/2@nas/share"},
		"an @ past the server":      {"type": "cifs", "o": "guest", "device": "//evil.example/x@nas/share"},
		"backslashes":               {"type": "cifs", "o": "guest", "device": `\\\\nas\\share`},
		"an escape":                 {"type": "cifs", "o": "guest", "device": "//nas/a%2Fb"},
		"blank braille":             {"type": "cifs", "o": "guest", "device": "//6.6.6.6/x\u2800at\u280010.0.0.2"},
		"a Hangul filler":           {"type": "cifs", "o": "guest", "device": "//nas/share\u3164"},
		"another filler":            {"type": "nfs", "o": "addr=10.0.0.2", "device": ":/x\u115fy"},
		"a halfwidth filler":        {"type": "cifs", "o": "guest", "device": "//nas/share\uffa0"},
		"a variation selector":      {"type": "cifs", "o": "guest", "device": "//nas/share\ufe0f"},
		"a plus":                    {"type": "cifs", "o": "guest", "device": "//nas/a+b"},
		"a hash":                    {"type": "cifs", "o": "guest", "device": "//nas/a#"},
		"an invisible space":        {"type": "cifs", "o": "guest", "device": "//nas/share\u200b"},
		"text run backwards":        {"type": "cifs", "o": "guest", "device": "//nas/\u202eerahs"},
		"a space at the end":        {"type": "cifs", "o": "guest", "device": "//nas/share "},
		"a number as an NFS server": {"type": "nfs", "o": "addr=10.0.0.2", "device": "3:/export"},
		"a number as a server":      {"type": "cifs", "o": "guest", "device": "//010.0.0.2/share"},
		"hex as a server":           {"type": "cifs", "o": "guest", "device": "//0x7f.1/share"},
		"a number as addr":          {"type": "cifs", "o": "addr=010.0.0.2", "device": "//nas/share"},
		"a long number as addr":     {"type": "nfs", "o": "addr=2130706433", "device": ":/export"},
		"hex as addr":               {"type": "nfs", "o": "addr=0x7f.0.0.1", "device": ":/export"},
		"words in a device":         {"type": "cifs", "o": "guest", "device": "//nas/x (read-only), and more"},
		"a line in a device":        {"type": "cifs", "o": "guest", "device": "//nas/x\nthe cifs mount //other/y"},
		"no share":                  {"type": "cifs", "o": "guest", "device": "//nas"},
		"no slashes":                {"type": "cifs", "o": "guest", "device": "nas/share"},
		"no device":                 {"type": "cifs", "o": "guest"},
		"three slashes":             {"type": "cifs", "o": "guest", "device": "///nas/share"},
		"a colon in the server":     {"type": "cifs", "o": "guest", "device": "//svc:hunter2/share"},
		"a user on NFS":             {"type": "nfs", "o": "addr=10.0.0.2", "device": "svc:hunter2@nas:/export"},
		"a password in an NFS path": {"type": "nfs", "o": "addr=10.0.0.2", "device": "svc:/hunter2@nas:/export"},
		"no NFS path":               {"type": "nfs", "o": "addr=10.0.0.2", "device": "svc:hunter2@nas:export"},
		"a CIFS share on NFS":       {"type": "nfs", "o": "addr=10.0.0.2", "device": "//svc:hunter2@nas/share"},
		"a colon in an NFS server":  {"type": "nfs", "o": "addr=10.0.0.2", "device": "svc:hunter2:/export"},
		"too long":                  {"type": "cifs", "o": "password=" + strings.Repeat("a", 4096), "device": share},
		"a device too long":         {"type": "nfs", "o": "addr=10.0.0.2", "device": ":/" + strings.Repeat("a", 4096)},
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

	// What people really write still passes.
	for _, o := range []map[string]string{
		{"type": "cifs", "o": "username=svc,password=p@ss=w0rd,vers=3.0,uid=1000,gid=1000,file_mode=0644,dir_mode=0755", "device": share},
		{"type": "cifs", "o": "addr=10.0.0.2,username=svc,password=x,domain=corp,sec=ntlmssp,iocharset=utf8,nounix,noperm", "device": "//10.0.0.2/My Share/sub"},
		{"type": "cifs", "o": "guest", "device": "//nas_1.example/share$"},
		{"type": "cifs", "device": share},
		{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/srv/nfs/data"},
		{"type": "nfs", "o": "addr=10.0.0.2,nfsvers=4.1,rw,noatime,hard,timeo=600,retrans=2,noresvport", "device": "nas.example:/export"},
		{"type": "nfs4", "o": "addr=nas.example,nolock,soft,ro", "device": ":/"},
		{"type": "nfs", "o": "addr=fe80::1", "device": "[fe80::1]:/export"},
		{"type": "nfs", "o": "addr=10.0.0.2,rw,hard,intr", "device": ":/export"},
		{"type": "nfs", "o": "addr=10.0.0.2,rw,bg,hard,tcp,vers=3,timeo=600,retry=2,mountvers=3,mountport=2049,mountproto=tcp", "device": "10.0.0.2:/export"},
		{"type": "cifs", "o": "addr=10.0.0.2,username=a,password=b,_netdev,nostrictsync,nosetuids,nouser_xattr,dynperm,intr,dirmode=0755", "device": "//10.0.0.2/share"},
		{"type": "cifs", "o": "guest", "device": "//nas1/文档"},
		{"type": "nfs", "o": "addr=10.0.0.2", "device": ":/srv/c++/a#b%c"},
	} {
		if err := d.Validate(&resourcev1.Volume{Options: o}); err != nil {
			t.Errorf("%v: %v", o, err)
		}
	}
}

// The server changes what the plan shows, so a token can't change it alone.
func TestAnotherRemoteIsNewAccess(t *testing.T) {
	d := &volume.Driver{}
	access := func(o string) []resource.Access {
		return d.HostAccess(&resourcev1.Volume{Options: map[string]string{"type": "cifs", "o": o, "device": "//nas/share"}})
	}
	if got := resource.NewAccess(access("addr=10.0.0.2"), access("ip=6.6.6.6")); len(got) != 1 {
		t.Errorf("another ip= reads as no new access: %v", got)
	}
	if got := resource.NewAccess(access("ip=10.0.0.2"), access("addr=10.0.0.2")); len(got) != 0 {
		t.Errorf("ip= and addr= of one server read as new access: %v", got)
	}
}

// A plugin's options can say where it mounts, and that is what it reaches. Any
// other value stays out of what a plan says, as it may be a secret.
func TestAPluginThatMountsTheSystemIsAsGoodAsRoot(t *testing.T) {
	access := func(options map[string]string) resource.Access {
		t.Helper()
		got := (&volume.Driver{}).HostAccess(&resourcev1.Volume{Driver: "local-persist", Options: options})
		if len(got) != 1 {
			t.Fatalf("%v reaches %v", options, got)
		}
		return got[0]
	}
	for _, at := range []string{"/etc", "/etc/", "/root/.ssh", "/home", "/var/run/docker.sock"} {
		if a := access(map[string]string{"mountpoint": at}); !a.AsGoodAsRoot() || a.Path == "" {
			t.Errorf("a plugin with mountpoint %s reads %+v", at, a)
		}
	}
	for _, options := range []map[string]string{{"mountpoint": "/srv/data"}, {"token": "s3cret"}, {"mountpoint": "relative/etc"}, nil} {
		if a := access(options); a.AsGoodAsRoot() || a.Path != "" {
			t.Errorf("a plugin with %v reads %+v", options, a)
		}
	}
}
