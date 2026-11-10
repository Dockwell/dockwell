// SPDX-License-Identifier: AGPL-3.0-only

// Package netif says which of a server's network interfaces are physical: a
// network card, or what's built on one, such as a bridge with a card in it, a
// bond, a VLAN or a macvlan. Their addresses are the server's own; a Docker or
// Podman bridge's, a VPN's or a TUN's aren't, whatever they're called.
package netif

import (
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// Sys is /sys/class/net, where Linux says what each interface is.
var Sys fs.FS = os.DirFS("/sys/class/net")

// maxDepth bounds following lower links, as a loop in what sysfs says ends it.
const maxDepth = 8

// arphrdPPP is a PPP link's type in sysfs.
const arphrdPPP = "512"

// Physical reports whether sys, /sys/class/net, says name is a network card, or
// is built on one through its lower links: a bridge with a card among its ports,
// a bond, a VLAN, a macvlan; or is a PPP link, as PPPoE gives a home server its
// public address on, which sysfs gives no device. A bridge whose ports are only
// veths or taps, a TUN, WireGuard or a veth isn't, whatever it's called.
func Physical(sys fs.FS, name string) bool {
	return physical(sys, name, 0)
}

func physical(sys fs.FS, name string, depth int) bool {
	if depth > maxDepth || name == "" || strings.ContainsAny(name, "/") {
		return false
	}
	if _, err := fs.Stat(sys, path.Join(name, "device")); err == nil {
		return true
	}
	if kind, err := fs.ReadFile(sys, path.Join(name, "type")); err == nil && strings.TrimSpace(string(kind)) == arphrdPPP {
		return true
	}
	entries, err := fs.ReadDir(sys, name)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
		lower, ok := strings.CutPrefix(e.Name(), "lower_")
		return ok && physical(sys, lower, depth+1)
	})
}

// Physicals says which of names are physical, and whether sysfs could say: it
// can't when none is, as in a container, whose interfaces have no device of
// their own. Then the caller falls back to names (Virtual).
func Physicals(sys fs.FS, names []string) (map[string]bool, bool) {
	out := map[string]bool{}
	for _, name := range names {
		if Physical(sys, name) {
			out[name] = true
		}
	}
	return out, len(out) > 0
}

// virtualNames are what virtual interfaces are called by default, for when sysfs
// can't say: they carry traffic already counted on a physical one, or none.
var virtualNames = []string{
	"lo", "docker", "veth", "br-", "dw-", "virbr", "cni", "flannel", "cali", "cilium", "vxlan", "tun", "tap",
	"dummy", "wg", "zt", "nebula", "podman", "lxdbr", "incusbr", "kube-",
}

// Virtual reports a name virtual interfaces go by by default, for when sysfs can't
// say what an interface is.
func Virtual(name string) bool {
	return slices.ContainsFunc(virtualNames, func(p string) bool { return strings.HasPrefix(name, p) })
}
