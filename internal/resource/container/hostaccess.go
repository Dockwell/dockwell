// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/lazyre"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a container reaches of its server past what Docker gives it, a spec's own
// (ADR 0097): devices, GPUs, security options past no_new_privileges, the
// server's processes, and kernel settings of its own namespaces. Each but the
// last is host access, which a person chooses and approves; the last reaches
// nothing of the server, so it's a field like any other.

// allPermissions are a device's when a spec says none.
const allPermissions = "rwm"

// WritesHostAccess reports a spec with any of these, sysctls included, or a field
// that came with them, a health check's start_interval and names on one network
// alone, which an agent that doesn't say it runs them would drop without a word
// (Hello.runs_host_access; the review of ADR 0097, L7 and L8). The control plane's
// gate asks this one.
func WritesHostAccess(c *resourcev1.Container) bool {
	return len(c.GetDevices()) > 0 || c.GetGpus() != nil || len(c.GetSecurityOpt()) > 0 || c.GetHostPid() || len(c.GetSysctls()) > 0 ||
		c.GetHealthcheck().GetStartInterval() != "" || len(c.GetNetworkAliases()) > 0
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
			return errors.New("gpus gives a count and device_ids. Give one of them")
		case g.GetCount() == 0 && len(g.GetDeviceIds()) == 0:
			return errors.New("gpus gives no count and no device_ids. Give how many, -1 for all, or which")
		}
		if !slices.Contains(gpuDrivers, g.GetDriver()) {
			return fmt.Errorf("gpus asks the driver %q, which Dockwell doesn't run. Use nvidia or cdi, or leave it empty for Docker's", g.GetDriver())
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
	// Docker adds label=disable by itself to what's privileged or shares the
	// server's processes, so a spec that asks for it alone there asks for nothing.
	c.SecurityOpt = sortedSet(withoutDaemonsLabel(c.GetSecurityOpt(), c.GetPrivileged() || c.GetHostPid()))
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

// gpuDrivers are the drivers a GPU request may ask: Docker's default, NVIDIA's,
// and the Container Device Interface, whose devices a plan names one by one.
var gpuDrivers = []string{"", "nvidia", "cdi"}

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
	shared := c.HostConfig.Privileged || c.HostConfig.PidMode == "host" || c.HostConfig.IpcMode == "host"
	out.SecurityOpt = sortedSet(withoutDaemonsLabel(out.SecurityOpt, shared))
	out.HostPid = c.HostConfig.PidMode == "host"
	if len(c.HostConfig.Sysctls) > 0 {
		out.Sysctls = maps.Clone(c.HostConfig.Sysctls)
	}
}

// labelDisable is the security option Docker's daemon adds by itself to a container
// that's privileged, or shares the server's processes or shared memory, and asks
// for no SELinux label of its own (moby's generateSecurityOpt, with go-selinux's
// DisableSecOpt), whether SELinux is on or not.
const labelDisable = "label=disable"

// ConfinedAgain says, in words that follow the app's name, that SELinux confines
// it again once it's neither privileged nor sees the server's processes: Docker
// turned its labels off only for those, and a spec doesn't hold that label beside
// them (withoutDaemonsLabel). It's "" for any other change (the narrow check of
// ADR 0097, L-a).
func ConfinedAgain(current, desired *resourcev1.Container) string {
	shared := func(c *resourcev1.Container) bool { return c.GetPrivileged() || c.GetHostPid() }
	labelled := func(c *resourcev1.Container) bool {
		return slices.ContainsFunc(c.GetSecurityOpt(), func(o string) bool { return strings.HasPrefix(o, "label=") })
	}
	if current == nil || desired == nil || !shared(current) || labelled(current) || shared(desired) || labelled(desired) {
		return ""
	}
	return "runs under SELinux's labels again, on a server that has them, as Docker turned them off only while it was privileged or saw the server's processes. To keep them off, add label=disable to its security_opt"
}

// withoutDaemonsLabel is opts without the label=disable Docker's daemon adds by
// itself to a container that runs as shared asks, so nothing reads it as asked for:
// opts in the form a spec keeps them.
func withoutDaemonsLabel(opts []string, shared bool) []string {
	if !shared {
		return opts
	}
	labels := 0
	for _, o := range opts {
		if strings.HasPrefix(o, "label=") {
			labels++
		}
	}
	if labels != 1 || !slices.Contains(opts, labelDisable) {
		return opts
	}
	return slices.DeleteFunc(slices.Clone(opts), func(o string) bool { return o == labelDisable })
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

// rootDevices are devices of the server that are the same as root by what they
// are wherever they are: its memory, its kernel's memory and its I/O ports. Any
// under /dev/mapper is a disk. The agent finds a block device at any path, and a
// link to one of these (HostDevices, Decision.DeviceRoot).
var rootDevices = []string{"/dev/mem", "/dev/kmem", "/dev/port"}

// rootDevicePrefixes are names of devices that write a disk or firmware directly:
// block SCSI generic devices pass a write command straight to a disk, and MTD
// devices are flash.
var rootDevicePrefixes = []string{"/dev/mapper/", "/dev/bsg/", "/dev/mtd", "/dev/raw/"}

// genericDevice is a SCSI or NVMe generic device's name, as /dev/sg0 or /dev/ng0n1,
// which passes a write command straight to a disk; a digit follows, so Intel SGX's
// /dev/sgx_enclave isn't one (the narrow check of ADR 0097, L-c).
var genericDevice = lazyre.Regexp(`^/dev/(sg|ng)[0-9]`)

// RootDevice reports a device path that is the same as root by its name.
func RootDevice(p string) bool {
	return slices.Contains(rootDevices, p) || genericDevice().MatchString(p) ||
		slices.ContainsFunc(rootDevicePrefixes, func(prefix string) bool { return strings.HasPrefix(p, prefix) })
}

// rootSubsystems are the kernel subsystems of character devices that are the same
// as root: what passes commands straight to a disk, flash, and raw disk access. Of
// mem's, memory, the kernel's memory and ports are (rootMem), and null, zero and the
// random ones aren't.
var rootSubsystems = []string{"scsi_generic", "bsg", "nvme-generic", "mtd", "raw"}

// rootMem are the minor numbers of mem's devices that are the same as root:
// /dev/mem, /dev/kmem and /dev/port.
var rootMem = []uint32{1, 2, 4}

// confinedTypes are the SELinux types of a confined container, which keep it as
// SELinux keeps a container: any other type, as spc_t or one of the server's own
// domains, may not, so it's the same as root.
var confinedTypes = []string{"container_t", "container_init_t", "container_kvm_t", "svirt_lxc_net_t"}

// rootSecurityOption reports a security option that is the same as root: the
// kernel's system paths writable, an SELinux type that isn't a confined
// container's, another SELinux user or role, which can reach what the type can't,
// or SELinux's labels off, which runs it as spc_t, unconfined, on a server that
// has SELinux (the narrow check of ADR 0097, L-d).
func rootSecurityOption(o string) bool {
	if t, ok := strings.CutPrefix(o, "label=type:"); ok {
		return !slices.Contains(confinedTypes, t)
	}
	return o == "systempaths=unconfined" || o == labelDisable || strings.HasPrefix(o, "label=user:") || strings.HasPrefix(o, "label=role:")
}

// hostAccess is what a container's devices, GPUs, security options and the
// server's processes give it, which a person approves (ADR 0097). Its sysctls
// are its own namespaces', which reach nothing of the server.
func hostAccess(c *resourcev1.Container) []resource.Access {
	var out []resource.Access
	for _, d := range c.GetDevices() {
		p := cmp.Or(d.GetPermissions(), allPermissions)
		out = append(out, resource.Access{
			Kind: resource.AccessDevice, Path: d.GetHostPath(), ReadOnly: !strings.ContainsAny(p, "wm"), SameAsRoot: RootDevice(d.GetHostPath()),
		})
	}
	if g := c.GetGpus(); g != nil {
		which := strings.Join(g.GetDeviceIds(), ",")
		switch {
		case g.GetCount() == -1:
			which = "all"
		case g.GetCount() > 0:
			which = strconv.Itoa(int(g.GetCount()))
		}
		// The driver and what it's asked for are part of what's approved: another of
		// either reaches something else.
		out = append(out, resource.Access{Kind: resource.AccessGPUs, Path: which, Type: g.GetDriver(), Detail: strings.Join(g.GetCapabilities(), ",")})
		// A CDI device can bring any mounts and devices its spec on the server lists, so
		// each is named as a device of its own.
		if g.GetDriver() == "cdi" {
			for _, id := range g.GetDeviceIds() {
				out = append(out, resource.Access{Kind: resource.AccessDevice, Path: id})
			}
		}
	}
	for _, o := range c.GetSecurityOpt() {
		out = append(out, resource.Access{Kind: resource.AccessSecurityOption, Path: o, SameAsRoot: rootSecurityOption(o)})
	}
	if c.GetHostPid() {
		out = append(out, resource.Access{Kind: resource.AccessHostProcesses, SameAsRoot: true})
	}
	return out
}

// rootTogether are the capabilities that make whatever else a container reaches
// of its server the same as root: with the server's processes, SYS_PTRACE reads
// and writes any of them, and the rest reach the kernel or a disk directly.
var rootTogether = []string{"ALL", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_RAWIO"}

// together puts first, once for the container, that what it reaches of its server
// is the same as root with a capability of rootTogether, or privileged mode: the
// two together are more than either is alone.
func together(c *resourcev1.Container, access []resource.Access) []resource.Access {
	var with []string
	if c.GetPrivileged() {
		with = append(with, "privileged mode")
	}
	for _, name := range c.GetCapAdd() {
		if slices.Contains(rootTogether, name) {
			with = append(with, name)
		}
	}
	if len(with) == 0 || !slices.ContainsFunc(access, func(a resource.Access) bool {
		return a.Kind != resource.AccessCapability && a.Kind != resource.AccessPrivileged && a.Kind != resource.AccessGroup
	}) {
		return access
	}
	first := resource.Access{Kind: resource.AccessRootTogether, Path: strings.Join(with, ", "), SameAsRoot: true}
	return append([]resource.Access{first}, access...)
}

// HostDevices is Docker.Devices on the server the agent runs on: a device there,
// followed through links, is the same as root where it's a block device, or one
// RootDevice names.
func HostDevices(p string) (bool, error) {
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false, fmt.Errorf("the device %s isn't on this server, so the container couldn't start. Take it out, or plug the device in and plan again", p)
	}
	fi, err := os.Stat(target)
	if err != nil {
		return false, fmt.Errorf("can't read the device %s on this server: %w", p, err)
	}
	if fi.Mode()&os.ModeDevice == 0 {
		return false, fmt.Errorf("%s isn't a device on this server. Give a device's path, or mount it as a path instead", p)
	}
	if fi.Mode()&os.ModeCharDevice == 0 || RootDevice(target) {
		return true, nil
	}
	// A character device is what its kernel subsystem says it is, whatever it's
	// called, which /sys says by its numbers.
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("can't read the numbers of the device %s on this server, so Dockwell can't tell whether it's the same as root. Plan again, and if it goes on, take the device out", p)
	}
	return rootSubsystem(sysDevChar, p, uint64(st.Rdev)) //nolint:unconvert // Rdev is uint32 on some platforms
}

// sysDevChar is where Linux names each character device by its numbers.
const sysDevChar = "/sys/dev/char"

// rootSubsystem reports a character device p, by its numbers, whose kernel
// subsystem under root is one of rootSubsystems. Only a device /sys has no entry
// for is taken as it is; a /sys it can't read fails, as calling the device plain
// would be the quiet way to be wrong (the narrow check of ADR 0097, L-b).
func rootSubsystem(root, p string, rdev uint64) (bool, error) {
	cantRead := func(err error) error {
		return fmt.Errorf("can't read what kind of device %s is from %s on this server, so Dockwell can't tell whether it's the same as root: %w. Let the agent read %s, then plan again", p, root, err, root)
	}
	if _, err := os.Stat(root); err != nil {
		return false, cantRead(err)
	}
	entry := filepath.Join(root, fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev)))
	switch _, err := os.Lstat(entry); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, cantRead(err)
	}
	link, err := os.Readlink(filepath.Join(entry, "subsystem"))
	if err != nil {
		return false, cantRead(err)
	}
	sub := filepath.Base(link)
	return slices.Contains(rootSubsystems, sub) || sub == "mem" && slices.Contains(rootMem, unix.Minor(rdev)), nil
}
