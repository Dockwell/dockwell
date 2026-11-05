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
		{resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge", Detail: "db,www"}, "the network edge, which is on the server already, where it answers to its name and to db,www: whatever else is on that network, other projects' services too, can reach it, and it can reach them"},
		{resource.Access{Kind: resource.AccessCapability, Path: "NET_ADMIN"}, "the Linux capability NET_ADMIN"},
		{resource.Access{Kind: resource.AccessCapability, Path: "SYS_ADMIN", SameAsRoot: true}, "the Linux capability SYS_ADMIN, which is the same as root"},
		{resource.Access{Kind: resource.AccessGroup, Path: "999"}, "the group 999, which is the same as root if it is the group that owns Docker's socket on the server"},
		{resource.Access{Kind: resource.AccessGroup, Path: "docker", SameAsRoot: true}, "the group docker, which is the same as root"},
		{resource.Access{Kind: resource.AccessPrivileged, SameAsRoot: true}, "privileged mode, which gives it every device and capability on the server, the same as root"},
		// Whatever let it through, it can't read as more of the line.
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
	for kind := resource.AccessPath; kind <= resource.AccessPrivileged; kind++ {
		for name, a := range map[string]resource.Access{
			"path":                         {Kind: kind, Path: spoof},
			"driver":                       {Kind: kind, Driver: spoof},
			"detail":                       {Kind: kind, Path: "x", Detail: spoof},
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
