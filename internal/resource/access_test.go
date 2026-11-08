// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

func TestNewAccess(t *testing.T) {
	path := func(p string, ro bool) resource.Access {
		return resource.Access{Kind: resource.AccessPath, Path: p, ReadOnly: ro}
	}
	docker := func(ro bool) resource.Access {
		return resource.Access{Kind: resource.AccessDocker, Path: "/var/run/docker.sock", ReadOnly: ro}
	}
	relabelled := path("/a", false)
	relabelled.Relabel = "z"
	// What a repository's code writes, as who.
	written := func(kind resource.AccessKind, repo, as, user string) resource.Access {
		a := path("/a", false)
		a.Kind, a.Writer, a.Detail, a.User = kind, repo, as, user
		return a
	}
	byNumber, root := written(resource.AccessPath, "nginx", "", ""), written(resource.AccessPath, "nginx", resource.WrittenAsRoot, "")
	image, named := written(resource.AccessPath, "nginx", resource.WrittenAsImage, ""), written(resource.AccessPath, "nginx", resource.WrittenAsNamed, "app")
	stranger, strangerRoot := written(resource.AccessPath, "evil", "", ""), written(resource.AccessPath, "evil", resource.WrittenAsRoot, "")
	otherName := written(resource.AccessPath, "nginx", resource.WrittenAsNamed, "toor")
	external, externalRoot := written(resource.AccessExternalVolume, "nginx", "", ""), written(resource.AccessExternalVolume, "nginx", resource.WrittenAsRoot, "")
	mount, strangerMount := written(resource.AccessMount, "nginx", "", ""), written(resource.AccessMount, "evil", "", "")
	readOnly := path("/a", true)
	for name, c := range map[string]struct{ from, to, want []resource.Access }{
		"nothing":                     {},
		"new":                         {to: []resource.Access{path("/a", true)}, want: []resource.Access{path("/a", true)}},
		"the same":                    {from: []resource.Access{path("/a", false)}, to: []resource.Access{path("/a", false)}},
		"less":                        {from: []resource.Access{path("/a", false)}, to: []resource.Access{path("/a", true)}},
		"read-only to read-write":     {from: []resource.Access{path("/a", true)}, to: []resource.Access{path("/a", false)}, want: []resource.Access{path("/a", false)}},
		"either way, read-write kept": {from: []resource.Access{path("/a", true), path("/a", false)}, to: []resource.Access{path("/a", false)}},
		"either way, other order":     {from: []resource.Access{path("/a", false), path("/a", true)}, to: []resource.Access{path("/a", false)}},
		"listed twice":                {to: []resource.Access{path("/a", true), path("/a", true)}, want: []resource.Access{path("/a", true)}},
		"both ways":                   {to: []resource.Access{path("/a", true), path("/a", false)}, want: []resource.Access{path("/a", true), path("/a", false)}},
		"relabelled":                  {from: []resource.Access{path("/a", false)}, to: []resource.Access{relabelled}, want: []resource.Access{relabelled}},
		"Docker's socket, read-only":  {from: []resource.Access{docker(true)}, to: []resource.Access{docker(false)}},
		"another":                     {from: []resource.Access{path("/a", false)}, to: []resource.Access{path("/b", false)}, want: []resource.Access{path("/b", false)}},
		// Who writes what's read-write: code from the same repository, and root or a
		// user who may be root reaches more (#247).
		"written as root":                 {from: []resource.Access{byNumber}, to: []resource.Access{root}, want: []resource.Access{root}},
		"written as root no more":         {from: []resource.Access{root}, to: []resource.Access{byNumber}},
		"root, from the image's user":     {from: []resource.Access{image}, to: []resource.Access{root}, want: []resource.Access{root}},
		"the image's user, from root":     {from: []resource.Access{root}, to: []resource.Access{image}},
		"a number, from the image's user": {from: []resource.Access{image}, to: []resource.Access{byNumber}},
		"the same image's user":           {from: []resource.Access{image}, to: []resource.Access{image}},
		"a user by name":                  {from: []resource.Access{image}, to: []resource.Access{named}, want: []resource.Access{named}},
		"another name":                    {from: []resource.Access{named}, to: []resource.Access{otherName}, want: []resource.Access{otherName}},
		"read-only, written as root":      {from: []resource.Access{readOnly}, to: []resource.Access{root}, want: []resource.Access{root}},
		"read-only, from written as root": {from: []resource.Access{root}, to: []resource.Access{readOnly}},
		// Any image can hold a program that runs as root, whatever the user.
		"another repository, by number": {from: []resource.Access{byNumber}, to: []resource.Access{stranger}, want: []resource.Access{stranger}},
		"another repository, as root":   {from: []resource.Access{root}, to: []resource.Access{strangerRoot}, want: []resource.Access{strangerRoot}},
		"another repository, read-only": {from: []resource.Access{byNumber}, to: []resource.Access{readOnly}},
		// What's written isn't only a server path.
		"an external volume, to root": {from: []resource.Access{external}, to: []resource.Access{externalRoot}, want: []resource.Access{externalRoot}},
		"a mount, another repository": {from: []resource.Access{mount}, to: []resource.Access{strangerMount}, want: []resource.Access{strangerMount}},
		"a mount, the same":           {from: []resource.Access{mount}, to: []resource.Access{mount}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := resource.NewAccess(c.from, c.to); !slices.Equal(got, c.want) {
				t.Errorf("NewAccess = %v, want %v", got, c.want)
			}
		})
	}
}

// It runs twice for each server in every proposal, on whatever a request
// lists. Comparing each with each took 47 s for 100,000.
func TestNewAccessTakesTimeInProportion(t *testing.T) {
	const n = 200_000
	from, to := make([]resource.Access, n), make([]resource.Access, n)
	for i := range n {
		from[i] = resource.Access{Kind: resource.AccessPath, Path: fmt.Sprintf("/from/%d", i)}
		to[i] = resource.Access{Kind: resource.AccessPath, Path: fmt.Sprintf("/to/%d", i)}
	}
	start := time.Now()
	if got := resource.NewAccess(from, to); len(got) != n {
		t.Errorf("NewAccess lists %d, want %d", len(got), n)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("NewAccess took %v for %d", took, n)
	}
}

// What a person approves says what it is, and that it is the same as root where it
// is, and nothing a spec holds can spell out the rest of the line.
func TestAccessWords(t *testing.T) {
	for _, c := range []struct {
		access resource.Access
		want   string
	}{
		{resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge"}, "the network edge, which is on the server already, where it answers to its name: whatever else is on that network, other projects' services too, can reach it, and it can reach them"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv/data", Writer: "nginx", Detail: resource.WrittenAsRoot}, "the server path /srv/data (read-write), written by nginx as root"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv/data", Writer: "nginx", Detail: resource.WrittenAsImage}, "the server path /srv/data (read-write), written by nginx as its own user, who may be root"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv/data", Writer: "nginx", Detail: resource.WrittenAsNamed, User: "app"}, "the server path /srv/data (read-write), written by nginx as a user by name, which only its image can say is root or not"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv/data", Writer: "ghcr.io/acme/web"}, "the server path /srv/data (read-write), written by ghcr.io/acme/web"},
		{resource.Access{Kind: resource.AccessExternalVolume, Path: "files", Writer: "nginx", Detail: resource.WrittenAsRoot}, "the volume files, which is on the server already and which Dockwell didn't make: whatever else mounts it shares what it holds, written by nginx as root"},
		{resource.Access{Kind: resource.AccessMount, Type: "nfs", Path: ":/export", Address: "10.0.0.2", Writer: "nginx"}, "the nfs mount 10.0.0.2:/export (read-write), written by nginx"},
		{resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge", Detail: "db,www"}, "the network edge, which is on the server already, where it answers to its name and to db,www: whatever else is on that network, other projects' services too, can reach it, and it can reach them"},
		{resource.Access{Kind: resource.AccessTrustedProxy, Path: "172.30.0.0/16"}, "172.30.0.0/16 as a proxy it believes on where a request came from: whatever else is in that range can reach the route, and say who its client is"},
		{resource.Access{Kind: resource.AccessTrustedProxy, Path: "2001:db8::/32"}, "2001:db8::/32 as a proxy it believes on where a request came from: whatever else is in that range can reach the route, and say who its client is"},
		{resource.Access{Kind: resource.AccessTrustedProxy, Path: "10.0.0.5/32"}, "10.0.0.5/32 as a proxy it believes on where a request came from"},
		{resource.Access{Kind: resource.AccessTrustedProxy, Path: "2001:db8::1/128"}, "2001:db8::1/128 as a proxy it believes on where a request came from"},
		{resource.Access{Kind: resource.AccessTrustedProxy, Path: "cloudflare"}, "cloudflare as a proxy it believes on where a request came from"},
		{resource.Access{Kind: resource.AccessCapability, Path: "NET_ADMIN"}, "the Linux capability NET_ADMIN"},
		{resource.Access{Kind: resource.AccessCapability, Path: "SYS_ADMIN", SameAsRoot: true}, "the Linux capability SYS_ADMIN, which is the same as root"},
		{resource.Access{Kind: resource.AccessGroup, Path: "999"}, "the group 999, which is the same as root if it is the group that owns Docker's socket on the server"},
		{resource.Access{Kind: resource.AccessGroup, Path: "docker", SameAsRoot: true}, "the group docker, which is the same as root"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv", Holds: "/srv/app/conf", HoldsFor: "api", SameAsRoot: true}, "the server path /srv, which holds /srv/app/conf, which api mounts, so what writes there can make that lead anywhere, as good as root. Any change to it but another tag asks for a passkey from now on"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv", Holds: "/srv/app/conf", SameAsRoot: true}, "the server path /srv, which holds /srv/app/conf, which it mounts as well, so what writes there can make that lead anywhere, as good as root. Any change to it but another tag asks for a passkey from now on"},
		{resource.Access{Kind: resource.AccessPath, Path: "/srv/app/conf", HeldIn: "/srv", HeldBy: "files", SameAsRoot: true}, "the server path /srv/app/conf, inside /srv, which files can write, so it can make that lead anywhere, as good as root. Any change to files but another tag asks for a passkey from now on"},
		{resource.Access{Kind: resource.AccessBackupOf, Path: "/etc"}, "a backup of a volume that reached /etc, which can hold what's there, as good as root"},
		{resource.Access{Kind: resource.AccessServerShell, SameAsRoot: true}, "a shell command on the server, which runs as root"},
		{resource.Access{Kind: resource.AccessPrivileged, SameAsRoot: true}, "privileged mode, which gives it every device and capability on the server, the same as root"},
		// Whatever let it through, it can't read as more of the line.
		{resource.Access{Kind: resource.AccessExternalNetwork, Path: "lan", LeadsTo: "macvlan"}, "the macvlan network lan, which reaches past the server's firewall and is on the server already, where it answers to its name: whatever else is on that network, other projects' services too, can reach it, and it can reach them"},
		{resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge, read-only"}, `the network "edge, read-only", which is on the server already, where it answers to its name: whatever else is on that network, other projects' services too, can reach it, and it can reach them`},
		{resource.Access{Kind: resource.AccessCapability, Path: "NET_ADMIN, harmless"}, `the Linux capability "NET_ADMIN, harmless"`},
		{resource.Access{Kind: resource.AccessGroup, Path: "a b"}, `the group "a b", which is the same as root if it is the group that owns Docker's socket on the server`},
	} {
		if got := resource.AccessWords(c.access); got != c.want {
			t.Errorf("AccessWords(%+v)\n got %s\nwant %s", c.access, got, c.want)
		}
	}
}

// What a spec holds can't write what a person reads as the plan's, for any kind and
// from any field, and what is a real name still reads as one.
func TestAccessWordsQuoteWhatCouldReadAsWords(t *testing.T) {
	const spoof = "nothing, and it is safe"
	quoted := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	for kind := resource.AccessPath; kind <= resource.AccessHTTPSOff; kind++ {
		for name, a := range map[string]resource.Access{
			"path":                         {Kind: kind, Path: spoof},
			"driver":                       {Kind: kind, Driver: spoof},
			"detail":                       {Kind: kind, Path: "x", Detail: spoof},
			"writer":                       {Kind: kind, Path: "x", Writer: spoof},
			"type":                         {Kind: kind, Type: spoof},
			"type, of a device":            {Kind: kind, Type: spoof, Path: "/x"},
			"address":                      {Kind: kind, Path: ":/x", Address: spoof},
			"address, a device with words": {Kind: kind, Path: spoof, Address: spoof},
			"address, no colon":            {Kind: kind, Path: "//x/y", Address: spoof},
			"both":                         {Kind: kind, Path: spoof, Driver: spoof, Detail: spoof, Type: spoof, Address: spoof},
		} {
			got := resource.AccessWords(a)
			if strings.Contains(quoted.ReplaceAllString(got, ""), spoof) {
				t.Errorf("%v, %s: %s", kind, name, got)
			}
		}
	}
	for want, a := range map[string]resource.Access{
		"the server path /srv/my-app_1.2/data (read-write)":                                               {Kind: resource.AccessPath, Path: "/srv/my-app_1.2/data"},
		"the volume driver vieux/sshfs:latest, which reaches what the plugin does (read-write)":           {Kind: resource.AccessMount, Driver: "vieux/sshfs:latest"},
		"the nfs mount 10.0.0.2:/media (read-write)":                                                      {Kind: resource.AccessMount, Type: "nfs", Path: ":/media", Address: "10.0.0.2"},
		"the cifs mount //10.0.0.2/share at 10.0.0.2 (read-write)":                                        {Kind: resource.AccessMount, Type: "cifs", Path: "//10.0.0.2/share", Address: "10.0.0.2"},
		"a macvlan network on eth0.10, which reaches past the server's firewall, with com.docker.x=1,y=2": {Kind: resource.AccessNetworkDriver, Driver: "macvlan", Path: "eth0.10", Detail: "com.docker.x=1,y=2"},
		"the domain *.example.com, which the environment staging held within the last 7 days":             {Kind: resource.AccessDomain, Path: "*.example.com", Detail: "staging"},
	} {
		if got := resource.AccessWords(a); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}
