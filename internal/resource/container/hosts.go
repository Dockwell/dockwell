// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// hostnameRE is a name /etc/hosts can hold: labels of letters, digits and dashes,
// joined by dots, such as host.docker.internal.
var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// HostGateway is the address Docker gives for the server as its containers reach it.
const HostGateway = "host-gateway"

// maxExtraHosts is how many names a container may add to its /etc/hosts.
const maxExtraHosts = 64

// validateExtraHosts checks the names a container reaches at addresses of its own,
// each as hostname:address, where the address is an IP address or host-gateway, and
// puts them in one form and in order. They add to its /etc/hosts and reach nothing
// it couldn't reach by address, so they're no host access.
func validateExtraHosts(c *resourcev1.Container) error {
	if n := len(c.GetExtraHosts()); n > maxExtraHosts {
		return fmt.Errorf("the container adds %d hosts, and Dockwell takes up to %d. Add fewer", n, maxExtraHosts)
	}
	for i, h := range c.GetExtraHosts() {
		name, addr, ok := strings.Cut(h, ":")
		if !ok || len(name) > 253 || !hostnameRE.MatchString(name) {
			return fmt.Errorf("%q isn't a host to add. Give hostname:address, such as host.docker.internal:host-gateway", h)
		}
		if addr != HostGateway {
			ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]"))
			if err != nil {
				return fmt.Errorf("the host %s has the address %q, which isn't an IP address or host-gateway. Give one, such as 10.0.0.5", name, addr)
			}
			addr = ip.String()
		}
		c.ExtraHosts[i] = name + ":" + addr
	}
	slices.Sort(c.ExtraHosts)
	c.ExtraHosts = slices.Compact(c.ExtraHosts)
	return nil
}

// extraHostsOf reads a container's extra hosts as a spec keeps them, hostname:address,
// as Docker 25 and later also take hostname=address.
func extraHostsOf(hosts []string) []string {
	if hosts == nil {
		return nil
	}
	out := make([]string, len(hosts))
	for i, h := range hosts {
		if name, addr, ok := strings.Cut(h, "="); ok && !strings.Contains(name, ":") {
			h = name + ":" + addr
		}
		out[i] = h
	}
	slices.Sort(out)
	return out
}
