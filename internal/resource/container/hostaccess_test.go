// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"strings"
	"testing"
	"time"

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
