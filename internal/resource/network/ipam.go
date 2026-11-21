// SPDX-License-Identifier: AGPL-3.0-only

package network

import (
	"fmt"
	"net/netip"
	"strconv"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// IPAMError locates an invalid pool or gateway without echoing arbitrary input.
type IPAMError struct {
	Code v1.DockerNetworkCheckCode
	Path string
	Why  string
}

func (e *IPAMError) Error() string { return e.Why }

var reservedPools = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/127"),
	netip.MustParsePrefix("::ffff:0:0/96"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func availablePool(subnet netip.Prefix) bool {
	for _, reserved := range reservedPools {
		if reserved.Overlaps(subnet) {
			return false
		}
	}
	return subnet.Bits() < subnet.Addr().BitLen()-1
}

func broadcast(subnet netip.Prefix, gateway netip.Addr) bool {
	if !gateway.Is4() {
		return false
	}
	start, end := subnet.Addr().As4(), gateway.As4()
	for i := range start {
		mask := byte(0xff) >> min(max(subnet.Bits()-8*i, 0), 8)
		if end[i] != start[i]|mask {
			return false
		}
	}
	return true
}

// CheckIPAM validates only explicit settings. Docker chooses automatic pools.
func CheckIPAM(n *spec.Network) error {
	ipam := n.GetIpam()
	if ipam == nil {
		return nil
	}
	if n.GetSubnet() != "" {
		return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL, "ipam", "choose either typed IPAM or the legacy subnet"}
	}
	if n.GetDriver() != "" && n.GetDriver() != "bridge" {
		return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_DRIVER_UNSUPPORTED, "driver", "explicit IPAM is supported for local bridge networks"}
	}
	var seen [2]bool
	for i, pool := range ipam.GetPools() {
		field := "ipam.pools[" + strconv.Itoa(i) + "]"
		subnet, err := netip.ParsePrefix(pool.GetSubnet())
		if err != nil || subnet.Masked() != subnet || subnet.Bits() == 0 || subnet.Addr().Is4In6() {
			return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL, field + ".subnet", "enter the network address as a CIDR subnet"}
		}
		if LinkRange.Overlaps(subnet) {
			return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL, field + ".subnet", fmt.Sprintf("the subnet overlaps %s, which Dockwell reserves for link networks", LinkRange)}
		}
		if !availablePool(subnet) {
			return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL, field + ".subnet", "choose a usable subnet with room for a gateway and containers"}
		}
		family := 0
		if subnet.Addr().Is6() {
			family = 1
			if !ipam.GetEnableIpv6() {
				return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL, field + ".subnet", "enable IPv6 to use an IPv6 subnet"}
			}
		}
		if seen[family] {
			return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL, field + ".subnet", "a bridge takes one subnet per address family"}
		}
		seen[family] = true
		if pool.GetGateway() != "" {
			gateway, err := netip.ParseAddr(pool.GetGateway())
			if err != nil || gateway.Zone() != "" || !subnet.Contains(gateway) || subnet.Addr() == gateway || broadcast(subnet, gateway) {
				return &IPAMError{v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_GATEWAY, field + ".gateway", "choose a gateway address inside the subnet"}
			}
		}
	}
	return nil
}

// PoolConflict reports the first matching live Engine pool.
func PoolConflict(n *spec.Network, live []docker.Network) *v1.DockerNetworkFinding {
	for i, pool := range n.GetIpam().GetPools() {
		wanted, err := netip.ParsePrefix(pool.GetSubnet())
		if err != nil {
			continue
		}
		for j := range live {
			existing := &live[j]
			for _, entry := range existing.IPAM.Config {
				held, err := netip.ParsePrefix(entry.Subnet)
				if err == nil && wanted.Overlaps(held) {
					return &v1.DockerNetworkFinding{
						Code:               v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_SUBNET_OVERLAP,
						FieldPath:          fmt.Sprintf("ipam.pools[%d].subnet", i),
						ConflictingNetwork: existing.Name, ConflictingSubnet: held.String(),
					}
				}
			}
		}
	}
	return nil
}
