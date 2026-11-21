// SPDX-License-Identifier: AGPL-3.0-only

package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

func TestExplicitIPAMPoolsAndGateways(t *testing.T) {
	const pool = v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_POOL
	const gateway = v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_INVALID_GATEWAY
	for _, tc := range []struct {
		subnet, gateway string
		want            v1.DockerNetworkCheckCode
	}{
		{"192.0.2.0/24", "192.0.2.1", 0},
		{"192.0.2.0/31", "", pool},
		{"192.0.2.1/32", "", pool},
		{"fd00:1234::/64", "fd00:1234::1", 0},
		{"fd00:1234::/127", "", pool},
		{"fd00:1234::1/128", "", pool},
		{"192.0.2.32/27", "192.0.2.63", gateway},
		{"192.0.2.32/27", "192.0.2.62", 0},
		{"127.0.0.0/8", "", pool},
		{"169.254.0.0/16", "", pool},
		{"224.0.0.0/4", "", pool},
		{"fe80::/64", "", pool},
		{"ff02::/64", "", pool},
		{"10.233.1.8/29", "", pool},
		{"10.232.0.0/15", "", pool},
	} {
		t.Run(tc.subnet+" "+tc.gateway, func(t *testing.T) {
			yes := true
			n := &spec.Network{Ipam: &spec.NetworkIPAM{EnableIpv6: &yes, Pools: []*spec.NetworkIPAMPool{{Subnet: tc.subnet, Gateway: tc.gateway}}}}
			err := CheckIPAM(n)
			var bad *IPAMError
			if tc.want == 0 && err != nil || tc.want != 0 && (!errors.As(err, &bad) || bad.Code != tc.want) {
				t.Fatalf("CheckIPAM = %v, want code %v", err, tc.want)
			}
		})
	}
}

func TestHostPoolConflictsNameTheirRoute(t *testing.T) {
	v4, err := procRoutes(strings.NewReader(
		"Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n"+
			fmt.Sprintf("eth0 %08X %08X 0003 0 0 0 %08X 0 0 0\n", 0, binary.NativeEndian.Uint32([]byte{192, 0, 2, 1}), 0)+
			fmt.Sprintf("eth1 %08X 00000000 0001 0 0 0 %08X 0 0 0\n", binary.NativeEndian.Uint32([]byte{198, 51, 100, 0}), binary.NativeEndian.Uint32([]byte{255, 255, 255, 0}))), false)
	if err != nil {
		t.Fatal(err)
	}
	v6, err := procRoutes(strings.NewReader(
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 20010db8000000000000000000000001 00000400 00000000 00000000 00000003 eth2\n"+
			"20010db8000100000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000000 00000000 00000001 eth2\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append(v4, v6...), hostPrefix{netip.MustParsePrefix("203.0.113.0/24"), "host interface eth3"})
	for subnet, where := range map[string]string{
		"192.0.2.0/28": "host gateway on eth0", "198.51.100.0/24": "host route on eth1",
		"2001:db8::/64": "host gateway on eth2", "2001:db8:1::/64": "host route on eth2",
		"203.0.113.32/27": "host interface eth3", "10.252.0.0/24": "",
	} {
		n := &spec.Network{Ipam: &spec.NetworkIPAM{Pools: []*spec.NetworkIPAMPool{{Subnet: subnet}}}}
		got := hostPoolConflict(n, all)
		if got.GetConflictingNetwork() != where {
			t.Errorf("%s: got %v, want %q", subnet, got, where)
		}
	}
	n := &spec.Network{Ipam: &spec.NetworkIPAM{Pools: []*spec.NetworkIPAMPool{{Subnet: "203.0.113.32/27"}}}}
	seen := []docker.Network{{Name: "fixture-bridge", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "203.0.113.0/24"}}}}}
	if got := PoolConflict(n, seen); got.GetConflictingNetwork() != "fixture-bridge" {
		t.Fatalf("Docker pool clash = %v", got)
	}
}
