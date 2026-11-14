// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/lazyre"
)

// What a container reaches of its server past what Docker gives it, a spec's own
// (ADR 0097): devices, GPUs, security options past no_new_privileges, the
// server's processes, and kernel settings of its own namespaces. Each but the
// last is host access, which a person chooses and approves; the last reaches
// nothing of the server, so it's a field like any other.

// allPermissions are a device's when a spec says none.
const allPermissions = "rwm"

// WritesHostAccess reports a spec with any of these, sysctls included, which an
// agent that doesn't say it runs them would drop without a word
// (Hello.runs_host_access).
func WritesHostAccess(c *resourcev1.Container) bool {
	return len(c.GetDevices()) > 0 || c.GetGpus() != nil || len(c.GetSecurityOpt()) > 0 || c.GetHostPid() || len(c.GetSysctls()) > 0
}

// securityOptForms are the security options a spec holds past no_new_privileges.
var securityOptForms = lazyre.Regexp(`^(seccomp=unconfined|apparmor=unconfined|label=disable|label=(user|role|type|level):[A-Za-z0-9_.:,-]+|systempaths=unconfined)$`)

// namespacedSysctl is a kernel setting of a container's own namespaces, as Docker
// takes one: of its network, or of its inter-process communication.
var namespacedSysctl = lazyre.Regexp(`^(net\.[a-z0-9_./-]+|kernel\.shm[a-z_]+|kernel\.msg[a-z_]+|kernel\.sem|fs\.mqueue\.[a-z_]+)$`)

// validateHostAccess checks what a container reaches of its server, and puts it
// in one form, as Docker reports it back.
func validateHostAccess(c *resourcev1.Container) error {
	seen := map[string]bool{}
	for _, d := range c.GetDevices() {
		host := d.GetHostPath()
		if !strings.HasPrefix(host, "/dev/") || path.Clean(host) != host {
			return fmt.Errorf("%q isn't a device of the server: give its path under /dev, such as /dev/dri/renderD128", host)
		}
		if seen[host] {
			return fmt.Errorf("the device %s is listed twice. List it once", host)
		}
		seen[host] = true
		d.ContainerPath = cmp.Or(d.GetContainerPath(), host)
		if !path.IsAbs(d.GetContainerPath()) || path.Clean(d.GetContainerPath()) != d.GetContainerPath() {
			return fmt.Errorf("%q isn't a path in the container for the device %s: give an absolute one", d.GetContainerPath(), host)
		}
		p := d.GetPermissions()
		if p == allPermissions {
			p = ""
		}
		for _, ch := range p {
			if !strings.ContainsRune(allPermissions, ch) || strings.Count(p, string(ch)) > 1 {
				return fmt.Errorf("%q isn't what a container may do with the device %s: give r, w and m, in that order, or leave it empty for all three", d.GetPermissions(), host)
			}
		}
		if p != "" && p != orderedPermissions(p) {
			return fmt.Errorf("%q isn't what a container may do with the device %s: give r, w and m, in that order", d.GetPermissions(), host)
		}
		d.Permissions = p
	}
	slices.SortFunc(c.Devices, func(a, b *resourcev1.Device) int { return cmp.Compare(a.GetHostPath(), b.GetHostPath()) })
	if g := c.GetGpus(); g != nil {
		switch {
		case g.GetCount() < -1:
			return fmt.Errorf("%d isn't a count of GPUs: give how many, or -1 for all", g.GetCount())
		case g.GetCount() != 0 && len(g.GetDeviceIds()) > 0:
			return fmt.Errorf("gpus gives a count and device_ids. Give one of them")
		case g.GetCount() == 0 && len(g.GetDeviceIds()) == 0:
			return fmt.Errorf("gpus gives no count and no device_ids. Give how many, -1 for all, or which")
		}
		g.Capabilities = sortedSet(g.GetCapabilities())
		if slices.Equal(g.GetCapabilities(), []string{"gpu"}) {
			g.Capabilities = nil
		}
	}
	for _, o := range c.GetSecurityOpt() {
		if _, ok := NoNewPrivileges(o); ok {
			return fmt.Errorf("security_opt has %q, which is no_new_privileges' to say. Set no_new_privileges instead", o)
		}
		if !securityOptForms().MatchString(o) {
			return fmt.Errorf("security_opt has %q, which Dockwell doesn't run. Use seccomp=unconfined, apparmor=unconfined, label=disable, label=type:<type> or systempaths=unconfined", o)
		}
	}
	c.SecurityOpt = sortedSet(c.GetSecurityOpt())
	for _, k := range slices.Sorted(maps.Keys(c.GetSysctls())) {
		v := c.GetSysctls()[k]
		switch {
		case !namespacedSysctl().MatchString(k):
			return fmt.Errorf("sysctls has %s, which isn't a container's own: only net.*, kernel.shm*, kernel.msg*, kernel.sem and fs.mqueue.* are, and the rest would change the server. Take it out", k)
		case strings.HasPrefix(k, "net.") && c.GetHostNetwork():
			return fmt.Errorf("sysctls has %s, and the container runs on the server's own network, where it would change the server's. Take it out, or give the container a network of its own", k)
		case v == "" || strings.ContainsAny(v, "\n\r\x00"):
			return fmt.Errorf("sysctls gives %s a value Docker can't set. Give it one, such as 1", k)
		}
	}
	return nil
}

// orderedPermissions puts a device's permissions in rwm's order.
func orderedPermissions(p string) string {
	var b strings.Builder
	for _, ch := range allPermissions {
		if strings.ContainsRune(p, ch) {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func sortedSet(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := slices.Clone(list)
	slices.Sort(out)
	return slices.Compact(out)
}

// writeHostAccess asks Docker for what a spec reaches of its server.
func writeHostAccess(hc *docker.HostConfig, spec *resourcev1.Container) {
	for _, d := range spec.GetDevices() {
		hc.Devices = append(hc.Devices, docker.DeviceMapping{
			PathOnHost: d.GetHostPath(), PathInContainer: cmp.Or(d.GetContainerPath(), d.GetHostPath()), CgroupPermissions: cmp.Or(d.GetPermissions(), allPermissions),
		})
	}
	if g := spec.GetGpus(); g != nil {
		caps := g.GetCapabilities()
		if len(caps) == 0 {
			caps = []string{"gpu"}
		}
		hc.DeviceRequests = []docker.DeviceRequest{{Driver: g.GetDriver(), Count: int(g.GetCount()), DeviceIDs: g.GetDeviceIds(), Capabilities: [][]string{caps}}}
	}
	hc.SecurityOpt = append(hc.SecurityOpt, spec.GetSecurityOpt()...)
	if spec.GetHostPid() {
		hc.PidMode = "host"
	}
	if len(spec.GetSysctls()) > 0 {
		hc.Sysctls = maps.Clone(spec.GetSysctls())
	}
}

// projectHostAccess reads back what a container reaches of its server, in the form
// validateHostAccess puts a spec in.
func projectHostAccess(c *docker.Container, out *resourcev1.Container) {
	for _, d := range c.HostConfig.Devices {
		p := orderedPermissions(d.CgroupPermissions)
		if p == allPermissions {
			p = ""
		}
		out.Devices = append(out.Devices, &resourcev1.Device{HostPath: d.PathOnHost, ContainerPath: cmp.Or(d.PathInContainer, d.PathOnHost), Permissions: p})
	}
	slices.SortFunc(out.Devices, func(a, b *resourcev1.Device) int { return cmp.Compare(a.GetHostPath(), b.GetHostPath()) })
	if len(c.HostConfig.DeviceRequests) == 1 {
		r := c.HostConfig.DeviceRequests[0]
		g := &resourcev1.Gpus{Count: int32(max(r.Count, -1)), DeviceIds: r.DeviceIDs, Driver: r.Driver} //nolint:gosec // a count of GPUs is small
		if len(r.Capabilities) == 1 {
			g.Capabilities = sortedSet(r.Capabilities[0])
		}
		if slices.Equal(g.GetCapabilities(), []string{"gpu"}) {
			g.Capabilities = nil
		}
		out.Gpus = g
	}
	for _, o := range c.HostConfig.SecurityOpt {
		if _, ok := NoNewPrivileges(o); !ok {
			out.SecurityOpt = append(out.SecurityOpt, securityOptForm(o))
		}
	}
	out.SecurityOpt = sortedSet(out.SecurityOpt)
	out.HostPid = c.HostConfig.PidMode == "host"
	if len(c.HostConfig.Sysctls) > 0 {
		out.Sysctls = maps.Clone(c.HostConfig.Sysctls)
	}
}

// securityOptForm is a security option as a spec keeps it: Docker takes the old
// key:value, as label:disable, as well as key=value.
func securityOptForm(o string) string {
	if !strings.Contains(o, "=") {
		if k, v, ok := strings.Cut(o, ":"); ok {
			return k + "=" + v
		}
	}
	return o
}
