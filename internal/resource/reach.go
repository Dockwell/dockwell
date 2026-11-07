// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"path"
	"slices"
	"strings"
)

// sockets are where Docker and containerd take connections, as the system
// service and rootless Docker have them.
var sockets = []string{"/var/run/docker.sock", "/run/docker.sock", "/var/run/containerd/containerd.sock", "/run/containerd/containerd.sock"}

// ControlsDocker reports a server path that is, or holds, Docker's or
// containerd's socket: whoever reaches it controls Docker, which is the same as
// root.
func ControlsDocker(p string) bool {
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

// systemPaths are what the system runs on and trusts. Each is as good as root to
// reach, read-only or not, and so is anything that holds one. The list can't be
// complete, and ADR 0028 says what it leaves out.
var systemPaths = []string{
	// The system itself.
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr",
	"/nix", "/snap", "/var/run",
	// What root runs on a schedule, or from what it installs.
	"/var/spool/anacron", "/var/spool/at", "/var/spool/cron", "/var/lib/dpkg", "/var/lib/rpm", "/var/lib/snapd", "/var/lib/systemd",
	// Other runtimes' sockets and every container's or VM's files, as Docker's own are.
	"/var/lib/containerd", "/var/lib/containers", "/var/lib/docker", "/var/lib/kubelet", "/var/lib/libvirt",
	"/var/lib/lxc", "/var/lib/lxd", "/var/lib/rancher", "/var/snap/docker", "/var/snap/lxd",
}

// runtimeSockets take connections from whoever can reach them, and run containers
// or machines as root, wherever they are mounted from.
var runtimeSockets = []string{"cri-dockerd.sock", "crio.sock", "libvirt-sock", "podman.sock", "snapd.socket"}

// harmlessToRead are in those, and say nothing worth having. Apps mount them
// read-only to keep the server's time zone, every day.
var harmlessToRead = []string{"/etc/localtime", "/etc/timezone", "/usr/share/zoneinfo"}

// SystemPlaces are the paths SystemPath goes by, for what has to find them by what
// they are rather than what they're called.
func SystemPlaces() []string { return slices.Clone(systemPaths) }

// SystemPath reports a server path as good as root to reach, as it is written or
// as it resolves on a server.
func SystemPath(p string, readOnly bool) bool {
	p = path.Clean(p)
	if readOnly && slices.ContainsFunc(harmlessToRead, func(h string) bool { return within(p, h) }) {
		return false
	}
	if slices.Contains(runtimeSockets, path.Base(p)) || inLogins(p) {
		return true
	}
	return slices.ContainsFunc(systemPaths, func(s string) bool { return within(p, s) || within(s, p) })
}

// inLogins reports what lets someone in as a user who may be able to become root:
// any .ssh, wherever it is, a person's home as a whole, as that holds theirs, and
// the dotfiles in it, which their shell runs. A folder in a home is that person's
// own business, such as /home/mira/site.
func inLogins(p string) bool {
	if slices.Contains(strings.Split(p, "/"), ".ssh") {
		return true
	}
	if p == "/home" {
		return true
	}
	rest, ok := strings.CutPrefix(p, "/home/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) == 1 || strings.HasPrefix(parts[1], ".")
}

// within reports whether p is base or under it.
func within(p, base string) bool {
	return p == base || strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/")
}

// Within reports whether the server path p is base or under it.
func Within(p, base string) bool { return within(p, base) }
