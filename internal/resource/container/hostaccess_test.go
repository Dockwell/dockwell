// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a container reaches of its server reads back as its spec asked for it, in
// one form (ADR 0097): devices with rwm as none, a GPU request with gpu as none,
// security options in order and key=value, the server's processes, and sysctls.
func TestHostAccessReadsBack(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("media:1", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}}})
	d := &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	spec := &resourcev1.Container{
		Image: "media:1",
		Devices: []*resourcev1.Device{
			{HostPath: "/dev/net/tun"},
			{HostPath: "/dev/dri/renderD128", ContainerPath: "/dev/dri/renderD128", Permissions: "rwm"},
			{HostPath: "/dev/ttyUSB0", ContainerPath: "/dev/zigbee", Permissions: "rw"},
		},
		Gpus:        &resourcev1.Gpus{Count: -1, Capabilities: []string{"gpu"}},
		SecurityOpt: []string{"seccomp=unconfined", "apparmor=unconfined"},
		HostPid:     true,
		Sysctls:     map[string]string{"net.ipv4.ip_forward": "1", "net.ipv4.conf.all.src_valid_mark": "1"},
	}
	if err := Validate(spec); err != nil {
		t.Fatal(err)
	}
	if err := d.Run(t.Context(), taken, spec, resource.Decision{Action: resource.ActionCreate}); err != nil {
		t.Fatal(err)
	}
	c, err := d.Client.ContainerNamed(t.Context(), taken.Name)
	if err != nil {
		t.Fatal(err)
	}
	if c.HostConfig.PidMode != "host" || len(c.HostConfig.Devices) != 3 || c.HostConfig.Devices[0].CgroupPermissions != "rwm" || len(c.HostConfig.DeviceRequests) != 1 {
		t.Errorf("Docker was asked for %+v", c.HostConfig)
	}
	got, err := d.projected(t.Context(), taken, c)
	if err != nil {
		t.Fatal(err)
	}
	Normalize(got)
	if diff := resource.Diff(spec, got); len(diff) != 0 {
		t.Errorf("it reads back as different: %+v", diff)
	}
}

// A spec says what it reaches in one form, and refuses what Docker would fail on,
// or what would reach the server past what's chosen: a sysctl that isn't a
// container's own, or a network one on the server's own network.
func TestWhatHostAccessASpecTakes(t *testing.T) {
	for name, c := range map[string]struct {
		spec *resourcev1.Container
		says string
	}{
		"a device outside /dev":       {&resourcev1.Container{Devices: []*resourcev1.Device{{HostPath: "/srv/disk"}}}, "isn't a device of the server"},
		"a device twice":              {&resourcev1.Container{Devices: []*resourcev1.Device{{HostPath: "/dev/fuse"}, {HostPath: "/dev/fuse"}}}, "listed twice"},
		"permissions out of order":    {&resourcev1.Container{Devices: []*resourcev1.Device{{HostPath: "/dev/fuse", Permissions: "wr"}}}, "in that order"},
		"a count and ids":             {&resourcev1.Container{Gpus: &resourcev1.Gpus{Count: 1, DeviceIds: []string{"0"}}}, "Give one of them"},
		"no GPUs":                     {&resourcev1.Container{Gpus: &resourcev1.Gpus{}}, "gives no count"},
		"no-new-privileges":           {&resourcev1.Container{SecurityOpt: []string{"no-new-privileges:true"}}, "Set no_new_privileges instead"},
		"a seccomp profile":           {&resourcev1.Container{SecurityOpt: []string{"seccomp=/srv/p.json"}}, "which Dockwell doesn't run"},
		"the server's sysctl":         {&resourcev1.Container{Sysctls: map[string]string{"vm.swappiness": "10"}}, "isn't a container's own"},
		"a net sysctl on the network": {&resourcev1.Container{HostNetwork: true, Sysctls: map[string]string{"net.ipv4.ip_forward": "1"}}, "the server's own network"},
		"an empty sysctl":             {&resourcev1.Container{Sysctls: map[string]string{"kernel.sem": ""}}, "a value Docker can't set"},
	} {
		c.spec.Image = "x:1"
		if err := Validate(c.spec); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.says)
		}
	}
}

// What a container's devices, GPUs, security options and the server's processes
// give it is host access, each as loud as it is: a device by its name, an SELinux
// type that's unconfined, writable system paths and the server's processes as
// root; and with SYS_ADMIN, SYS_MODULE, SYS_PTRACE, SYS_RAWIO, ALL or privileged
// mode, all of it as root, said once and first. Sysctls give it nothing of the
// server.
func TestWhatHostAccessGives(t *testing.T) {
	c := &resourcev1.Container{
		Image: "x:1",
		Devices: []*resourcev1.Device{
			{HostPath: "/dev/dri/renderD128"}, {HostPath: "/dev/ttyUSB0", Permissions: "r"}, {HostPath: "/dev/mem"}, {HostPath: "/dev/mapper/vg-root"},
		},
		Gpus:        &resourcev1.Gpus{Count: 2},
		SecurityOpt: []string{"apparmor=unconfined", "label=disable", "label=type:spc_t", "label=type:container_unconfined_t", "systempaths=unconfined"},
		HostPid:     true,
		Sysctls:     map[string]string{"net.ipv4.ip_forward": "1"},
	}
	got := ServiceAccess(c)
	want := []resource.Access{
		{Kind: resource.AccessDevice, Path: "/dev/dri/renderD128"},
		{Kind: resource.AccessDevice, Path: "/dev/ttyUSB0", ReadOnly: true},
		{Kind: resource.AccessDevice, Path: "/dev/mem", SameAsRoot: true},
		{Kind: resource.AccessDevice, Path: "/dev/mapper/vg-root", SameAsRoot: true},
		{Kind: resource.AccessGPUs, Path: "2"},
		{Kind: resource.AccessSecurityOption, Path: "apparmor=unconfined"},
		{Kind: resource.AccessSecurityOption, Path: "label=disable"},
		{Kind: resource.AccessSecurityOption, Path: "label=type:spc_t", SameAsRoot: true},
		{Kind: resource.AccessSecurityOption, Path: "label=type:container_unconfined_t", SameAsRoot: true},
		{Kind: resource.AccessSecurityOption, Path: "systempaths=unconfined", SameAsRoot: true},
		{Kind: resource.AccessHostProcesses, SameAsRoot: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ServiceAccess = %+v\nwant %+v", got, want)
	}
	for _, with := range []*resourcev1.Container{
		{Image: "x:1", Devices: []*resourcev1.Device{{HostPath: "/dev/fuse"}}, CapAdd: []string{"SYS_ADMIN"}},
		{Image: "x:1", HostPid: true, CapAdd: []string{"SYS_PTRACE"}},
		{Image: "x:1", Gpus: &resourcev1.Gpus{Count: -1}, Privileged: true},
	} {
		got := ServiceAccess(with)
		if len(got) == 0 || got[0].Kind != resource.AccessRootTogether || !got[0].SameAsRoot ||
			slices.ContainsFunc(got[1:], func(a resource.Access) bool { return a.Kind == resource.AccessRootTogether }) {
			t.Errorf("with %v, ServiceAccess = %+v", with, got)
		}
	}
	// A capability alone is what it was.
	if got := ServiceAccess(&resourcev1.Container{Image: "x:1", CapAdd: []string{"SYS_PTRACE"}}); len(got) != 1 || got[0].Kind != resource.AccessCapability {
		t.Errorf("SYS_PTRACE alone = %+v", got)
	}
}

// The agent checks each device on its server as a plan is made: one that isn't
// there fails the plan, by name, and one that's the same as root there says so.
func TestAPlanFindsEachDevice(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("x:1", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}}})
	on := map[string]bool{"/dev/dri/renderD128": false, "/dev/disk/by-id/usb-stick": true}
	d := &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Devices: func(p string) (bool, error) {
		root, ok := on[p]
		if !ok {
			return false, fmt.Errorf("the device %s isn't on this server", p)
		}
		return root, nil
	}}
	spec := &resourcev1.Container{Image: "x:1", Devices: []*resourcev1.Device{{HostPath: "/dev/dri/renderD128"}, {HostPath: "/dev/disk/by-id/usb-stick"}}}
	dec := &resource.Decision{Action: resource.ActionCreate}
	if err := d.Judge(t.Context(), taken, spec, dec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dec.DeviceRoot, []string{"/dev/disk/by-id/usb-stick"}) {
		t.Errorf("the plan finds %v the same as root", dec.DeviceRoot)
	}
	spec.Devices = append(spec.Devices, &resourcev1.Device{HostPath: "/dev/ttyACM0"})
	if err := d.Judge(t.Context(), taken, spec, &resource.Decision{Action: resource.ActionCreate}); err == nil || !strings.Contains(err.Error(), "/dev/ttyACM0 isn't on this server") {
		t.Errorf("planning a device that isn't there = %v", err)
	}
	// On this machine, /dev/null is a device and not the same as root, and /etc/hostname
	// is no device.
	if root, err := HostDevices("/dev/null"); err != nil || root {
		t.Errorf("/dev/null reads %v, %v", root, err)
	}
	if _, err := HostDevices("/etc/hostname"); err == nil || !strings.Contains(err.Error(), "isn't a device") {
		t.Errorf("a file reads %v", err)
	}
}

// Docker adds label=disable by itself to a container that's privileged or shares the
// server's processes, with no label of its own: a take-on reads none, so nothing is
// asked of a person for it, and a spec that asks for it alone there asks for
// nothing. One with a label of its own keeps it.
func TestTheDaemonsOwnLabelIsNoneOfTheSpecs(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("x:1", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}}})
	c := e.Client()
	c.Adopted = &docker.Adopted{}
	d := &Docker{Client: c, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	for name, hc := range map[string]docker.HostConfig{
		"privileged": {Privileged: true},
		"host-pid":   {PidMode: "host"},
	} {
		id, err := c.CreateContainer(t.Context(), name, &docker.CreateRequest{Config: docker.Config{Image: "x:1"}, HostConfig: hc})
		if err != nil {
			t.Fatal(err)
		}
		read, err := d.ReadAdoption(t.Context(), Target{Name: "env_x-" + name + "-1", Project: "env_x", Service: name, Scoped: true}, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := read.Spec.(*resourcev1.Container).GetSecurityOpt(); len(got) != 0 || slices.Contains(read.Refused, "security_opt label=disable") {
			t.Errorf("%s reads security_opt %v, refused %v", name, got, read.Refused)
		}
	}
	spec := &resourcev1.Container{Image: "x:1", Privileged: true, SecurityOpt: []string{"label=disable"}}
	if err := Validate(spec); err != nil || len(spec.GetSecurityOpt()) != 0 {
		t.Errorf("a privileged spec with label=disable alone keeps %v, %v", spec.GetSecurityOpt(), err)
	}
	spec = &resourcev1.Container{Image: "x:1", SecurityOpt: []string{"label=disable"}}
	if err := Validate(spec); err != nil || !slices.Equal(spec.GetSecurityOpt(), []string{"label=disable"}) {
		t.Errorf("an ordinary spec with label=disable keeps %v, %v", spec.GetSecurityOpt(), err)
	}
}

// A character device is as root as its kernel subsystem says, whatever it's called:
// a SCSI generic, NVMe generic or block SCSI generic device, flash, raw disk access,
// and memory and ports of mem's, but not null and the like. Their usual names are
// root by name too.
func TestACharacterDeviceIsWhatItsSubsystemSays(t *testing.T) {
	root := t.TempDir()
	for at, sub := range map[string]string{"21:0": "scsi_generic", "240:1": "nvme-generic", "90:0": "mtd", "1:1": "mem", "1:3": "mem", "226:128": "drm"} {
		if err := os.MkdirAll(filepath.Join(root, at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../class/"+sub, filepath.Join(root, at, "subsystem")); err != nil {
			t.Fatal(err)
		}
	}
	for at, want := range map[[2]uint32]bool{{21, 0}: true, {240, 1}: true, {90, 0}: true, {1, 1}: true, {1, 3}: false, {226, 128}: false, {4, 64}: false} {
		if got := rootSubsystem(root, unix.Mkdev(at[0], at[1])); got != want {
			t.Errorf("%d:%d reads root %v", at[0], at[1], got)
		}
	}
	for p, want := range map[string]bool{"/dev/sg0": true, "/dev/ng0n1": true, "/dev/bsg/0:0:0:0": true, "/dev/mtd0": true, "/dev/mapper/vg-root": true, "/dev/dri/renderD128": false, "/dev/net/tun": false} {
		if RootDevice(p) != want {
			t.Errorf("%s reads root %v", p, !want)
		}
	}
}

// SELinux types are judged by what's known to confine a container: those are
// amber, and any other, as container_runtime_t or a server's own domain, is the same
// as root, as another SELinux user or role is.
func TestAnSELinuxTypeIsRootUnlessItConfines(t *testing.T) {
	for o, root := range map[string]bool{
		"label=type:container_t": false, "label=type:container_init_t": false, "label=type:container_kvm_t": false, "label=type:svirt_lxc_net_t": false,
		"label=type:spc_t": true, "label=type:container_runtime_t": true, "label=type:kernel_t": true, "label=type:init_t": true, "label=type:sysadm_t": true,
		"label=user:system_u": true, "label=role:system_r": true, "label=level:s0:c100,c200": false, "label=disable": false,
		"apparmor=unconfined": false, "seccomp=unconfined": false, "systempaths=unconfined": true,
	} {
		if got := rootSecurityOption(o); got != root {
			t.Errorf("%s reads root %v", o, got)
		}
	}
}

// A GPU request's driver and what it asks the GPUs for are part of what a person
// approves, so another of either is new access; the driver is Docker's, NVIDIA's or
// CDI's, and each CDI device is named as a device (the review's L3).
func TestAGPUsDriverIsApproved(t *testing.T) {
	if err := Validate(&resourcev1.Container{Image: "x:1", Gpus: &resourcev1.Gpus{Count: 1, Driver: "amd"}}); err == nil || !strings.Contains(err.Error(), "doesn't run") {
		t.Errorf("an unknown driver = %v", err)
	}
	nvidia := ServiceAccess(&resourcev1.Container{Image: "x:1", Gpus: &resourcev1.Gpus{DeviceIds: []string{"0"}, Driver: "nvidia"}})
	cdi := ServiceAccess(&resourcev1.Container{Image: "x:1", Gpus: &resourcev1.Gpus{DeviceIds: []string{"nvidia.com/gpu=0"}, Driver: "cdi"}})
	more := ServiceAccess(&resourcev1.Container{Image: "x:1", Gpus: &resourcev1.Gpus{DeviceIds: []string{"0"}, Driver: "nvidia", Capabilities: []string{"display", "gpu"}}})
	if len(resource.NewAccess(nvidia, more)) == 0 {
		t.Error("asking the GPUs for more isn't new access")
	}
	if !slices.Contains(cdi, resource.Access{Kind: resource.AccessDevice, Path: "nvidia.com/gpu=0"}) || len(resource.NewAccess(nvidia, cdi)) == 0 {
		t.Errorf("a CDI request reads %+v", cdi)
	}
}
