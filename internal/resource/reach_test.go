// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
)

// What is really as good as root, from what a server trusts, and what isn't: an
// alarm on every folder is an alarm nobody reads.
func TestSystemPaths(t *testing.T) {
	for _, tt := range []struct {
		path     string
		readOnly bool
		want     bool
	}{
		// One write to these runs as root.
		{path: "/var/spool/cron", want: true},
		{path: "/var/spool/cron/crontabs", want: true},
		{path: "/var/spool/cron/crontabs", readOnly: true, want: true},
		{path: "/var/spool/anacron", want: true},
		{path: "/var/spool/at", want: true},
		{path: "/etc/crontab", want: true},
		{path: "/etc/cron.d", want: true},
		{path: "/etc/cron.daily", readOnly: true, want: true},
		{path: "/var/lib/dpkg", want: true},
		{path: "/var/lib/rpm", want: true},
		{path: "/var/lib/systemd", want: true},
		// What holds one of them holds that too.
		{path: "/var/spool", want: true},
		{path: "/var/lib", want: true},
		{path: "/var", want: true},
		// Logging in as someone who may be able to become root.
		{path: "/home", want: true},
		{path: "/home/mira", want: true},
		{path: "/home/mira/.ssh", want: true},
		{path: "/home/mira/.ssh/authorized_keys", readOnly: true, want: true},
		{path: "/home/mira/.bashrc", want: true},
		{path: "/home/mira/.docker/run", want: true},
		{path: "/srv/backup/mira/.ssh", want: true},
		{path: "/root/.ssh", want: true},
		// Every container's files, whichever runtime has them.
		{path: "/var/lib/containers", want: true},
		{path: "/var/lib/containers/storage", want: true},
		{path: "/var/lib/kubelet", want: true},
		{path: "/var/lib/libvirt", want: true},
		{path: "/var/lib/lxc", want: true},
		{path: "/var/lib/lxd", want: true},
		{path: "/var/lib/rancher", want: true},
		{path: "/var/lib/snapd", want: true},
		{path: "/var/snap/docker", want: true},
		{path: "/nix", want: true},
		{path: "/nix/store", want: true},
		{path: "/snap", want: true},
		// Another runtime's socket, wherever it is mounted from.
		{path: "/srv/podman.sock", want: true},
		{path: "/run/podman/podman.sock", want: true},
		{path: "/srv/crio.sock", want: true},
		{path: "/srv/cri-dockerd.sock", want: true},
		// A person's or an app's own.
		{path: "/home/mira/site"},
		{path: "/home/mira/site/.env"},
		{path: "/homework"},
		{path: "/srv"},
		{path: "/srv/site/html", readOnly: true},
		{path: "/srv/.sshfs"},
		{path: "/opt"},
		{path: "/mnt"},
		{path: "/var/www/html"},
		{path: "/var/log", readOnly: true},
		{path: "/var/mail"},
		{path: "/var/lib/mysql"},
		{path: "/var/spool/mail"},
		{path: "/srv/app.sock"},
		{path: "/etc/localtime", readOnly: true},
	} {
		if got := resource.SystemPath(tt.path, tt.readOnly); got != tt.want {
			t.Errorf("%s (read-only %v): as good as root = %v, want %v", tt.path, tt.readOnly, got, tt.want)
		}
	}
}

// Where an agent says a path leads can only make a plan louder.
func TestLeadingOnlyMakesAPlanLouder(t *testing.T) {
	path := func(p string, readOnly bool) resource.Access {
		return resource.Access{Kind: resource.AccessPath, Path: p, ReadOnly: readOnly, SameAsRoot: resource.SystemPath(p, readOnly)}
	}
	for _, tt := range []struct {
		name    string
		access  resource.Access
		to      string
		root    bool
		kind    resource.AccessKind
		leadsTo string
	}{
		{"to /etc", path("/srv/link", false), "/etc", true, resource.AccessPath, "/etc"},
		{"to /etc, read-only", path("/srv/link", true), "/etc", true, resource.AccessPath, "/etc"},
		{"to a home's keys", path("/srv/link", true), "/home/mira/.ssh", true, resource.AccessPath, "/home/mira/.ssh"},
		{"to Docker's socket", path("/srv/link", true), "/var/run/docker.sock", true, resource.AccessDocker, "/var/run/docker.sock"},
		{"to another runtime's socket", path("/srv/link", false), "/run/containerd/containerd.sock", true, resource.AccessDocker, "/run/containerd/containerd.sock"},
		{"to a folder", path("/srv/link", false), "/srv/site", false, resource.AccessPath, "/srv/site"},
		{"to a time zone, read-only", path("/srv/tz", true), "/etc/localtime", false, resource.AccessPath, "/etc/localtime"},
		{"to a time zone, written to", path("/srv/tz", false), "/etc/localtime", true, resource.AccessPath, "/etc/localtime"},
		// An agent that says less, or lies, leaves what the spec says.
		{"nowhere", path("/srv/site", false), "", false, resource.AccessPath, ""},
		{"nowhere, from /etc", path("/etc", false), "", true, resource.AccessPath, ""},
		{"from /etc to a folder", path("/etc", false), "/srv/site", true, resource.AccessPath, "/srv/site"},
		{"from the socket to a folder", resource.Access{Kind: resource.AccessDocker, Path: "/var/run/docker.sock"}, "/srv/site", true, resource.AccessDocker, "/srv/site"},
		{"to where it is", path("/srv/site", false), "/srv/site", false, resource.AccessPath, ""},
		// What an agent says of a network Dockwell didn't make goes on it, as it can
		// only make the plan louder.
		{"a network's driver", resource.Access{Kind: resource.AccessExternalNetwork, Path: "lan"}, "macvlan", false, resource.AccessExternalNetwork, "macvlan"},
		{"a network it says nothing of", resource.Access{Kind: resource.AccessExternalNetwork, Path: "lan"}, "", false, resource.AccessExternalNetwork, ""},
		// Only a path or a network leads anywhere.
		{"a share", resource.Access{Kind: resource.AccessMount, Path: "/etc", Type: "nfs"}, "/etc", false, resource.AccessMount, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.access.Leading(tt.to)
			if got.AsGoodAsRoot() != tt.root || got.Kind != tt.kind || got.LeadsTo != tt.leadsTo {
				t.Errorf("Leading(%q) = %+v, same as root %v; want root %v, kind %v, leads to %q", tt.to, got, got.AsGoodAsRoot(), tt.root, tt.kind, tt.leadsTo)
			}
		})
	}
}

// A network on the host driver is the server's own network, under another name.
func TestAHostNetworkIsAsGoodAsRootByAnyName(t *testing.T) {
	if !(resource.Access{Kind: resource.AccessNetworkDriver, Driver: "host"}).AsGoodAsRoot() {
		t.Error("a network on the host driver doesn't count as root")
	}
	if (resource.Access{Kind: resource.AccessNetworkDriver, Driver: "macvlan", Path: "eth0"}).AsGoodAsRoot() {
		t.Error("macvlan counts as root")
	}
}

// What a person reads says what they would approve, wherever a path leads.
func TestAccessWordsSayWhereAPathLeads(t *testing.T) {
	for want, a := range map[string]resource.Access{
		"the server path /etc (read-only), which is as good as root":                                                                                       {Kind: resource.AccessPath, Path: "/etc", ReadOnly: true, SameAsRoot: true},
		"the server path /srv/link, which leads to /etc (read-write), which is as good as root":                                                            {Kind: resource.AccessPath, Path: "/srv/link", LeadsTo: "/etc", SameAsRoot: true},
		"the server path /srv/site (read-only)":                                                                                                            {Kind: resource.AccessPath, Path: "/srv/site", ReadOnly: true},
		"the server path /srv/link, which leads to /etc (read-only)":                                                                                       {Kind: resource.AccessPath, Path: "/srv/link", LeadsTo: "/etc", ReadOnly: true},
		"the server path /srv/link, which leads to Docker's socket at /var/run/docker.sock, which gives control of Docker on the server, the same as root": {Kind: resource.AccessDocker, Path: "/srv/link", LeadsTo: "/var/run/docker.sock"},
		"the volume driver local-persist at /etc, which reaches what the plugin does (read-write)":                                                         {Kind: resource.AccessMount, Driver: "local-persist", Path: "/etc"},
	} {
		if got := resource.AccessWords(a); got != want {
			t.Errorf("%+v reads %q, want %q", a, got, want)
		}
	}
}

// A path to Docker's socket controls Docker however it's written, as the server
// resolves it (the review of #589, M1).
func TestADottedPathToDockerControlsIt(t *testing.T) {
	for _, p := range []string{"/var/run/docker.sock/.", "/srv/../var/run/docker.sock/.", "/run/user/1000/x/..", "/srv/../run/user/1000"} {
		if !resource.ControlsDocker(p) {
			t.Errorf("%s doesn't control Docker", p)
		}
	}
	if resource.ControlsDocker("/srv/media/..") {
		t.Error("/srv controls Docker")
	}
}
