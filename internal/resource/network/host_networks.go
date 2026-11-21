// SPDX-License-Identifier: AGPL-3.0-only

package network

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

type controlPeerKey struct{}

// WithControlPeer retains the actual peer of the agent stream, including when it uses a proxy.
func WithControlPeer(ctx context.Context, peer netip.Addr) context.Context {
	return context.WithValue(ctx, controlPeerKey{}, peer)
}

type hostPrefix struct {
	subnet netip.Prefix
	where  string
}

const maxHostPrefixes = 4096

// HostPoolConflict reads current host addresses and routes only for explicit pools.
func HostPoolConflict(ctx context.Context, n *spec.Network) (*v1.DockerNetworkFinding, error) {
	if len(n.GetIpam().GetPools()) == 0 {
		return nil, nil
	}
	peer, _ := ctx.Value(controlPeerKey{}).(netip.Addr)
	if peer.IsValid() {
		peer = peer.Unmap().WithZone("")
		if conflict := hostPoolConflict(n, []hostPrefix{{netip.PrefixFrom(peer, peer.BitLen()), "the control connection"}}); conflict != nil {
			return conflict, nil
		}
	}
	networks, err := hostPrefixes()
	if err != nil {
		return nil, fmt.Errorf("could not read the server's addresses and routes, so Dockwell cannot check this pool. Check the server and review again: %w", err)
	}
	return hostPoolConflict(n, networks), nil
}

func hostPoolConflict(n *spec.Network, networks []hostPrefix) *v1.DockerNetworkFinding {
	for i, pool := range n.GetIpam().GetPools() {
		wanted, err := netip.ParsePrefix(pool.GetSubnet())
		if err != nil {
			continue
		}
		for _, found := range networks {
			if wanted.Overlaps(found.subnet) {
				return &v1.DockerNetworkFinding{
					Code:      v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_SUBNET_OVERLAP,
					FieldPath: fmt.Sprintf("ipam.pools[%d].subnet", i), ConflictingNetwork: found.where, ConflictingSubnet: found.subnet.String(),
				}
			}
		}
	}
	return nil
}

func hostPrefixes() ([]hostPrefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	if len(ifaces) > maxHostPrefixes {
		return nil, errors.New("too many interfaces")
	}
	var out []hostPrefix
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if p, err := netip.ParsePrefix(addr.String()); err == nil {
				out = append(out, hostPrefix{p.Masked(), "host interface " + iface.Name})
			}
			if len(out) > maxHostPrefixes {
				return nil, errors.New("too many interface addresses")
			}
		}
	}
	if runtime.GOOS != "linux" {
		return out, nil
	}
	for _, ipv6 := range []bool{false, true} {
		routes, err := readProcRoutes(ipv6)
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	return out, nil
}

func readProcRoutes(ipv6 bool) ([]hostPrefix, error) {
	path := "/proc/net/route"
	if ipv6 {
		path = "/proc/net/ipv6_route"
	}
	f, err := os.Open(path)
	if ipv6 && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return procRoutes(f, ipv6)
}

func procRoutes(r io.Reader, ipv6 bool) ([]hostPrefix, error) {
	limited := &io.LimitedReader{R: r, N: 2 << 20}
	s := bufio.NewScanner(limited)
	var routes []hostPrefix
	for lines := 0; s.Scan(); lines++ {
		if lines >= maxHostPrefixes {
			return nil, errors.New("too many host routes")
		}
		f := strings.Fields(s.Text())
		if len(f) == 0 || !ipv6 && lines == 0 && f[0] == "Iface" {
			continue
		}
		route, err := decodeRoute(f, ipv6)
		if err != nil {
			return nil, err
		}
		if route.flags&1 == 0 || route.flags&0x200 != 0 { // down or rejected
			continue
		}
		// Catch-all destinations overlap everything. Their gateways still have to stay reachable.
		if route.dst.Bits() > 0 {
			routes = append(routes, hostPrefix{route.dst.Masked(), "host route on " + route.iface})
		}
		if gateway := route.gateway; !gateway.IsUnspecified() {
			routes = append(routes, hostPrefix{netip.PrefixFrom(gateway, gateway.BitLen()), "host gateway on " + route.iface})
		}
	}
	if limited.N == 0 {
		return nil, errors.New("the host routing table is too large")
	}
	return routes, s.Err()
}

type hostRoute struct {
	dst     netip.Prefix
	gateway netip.Addr
	iface   string
	flags   uint64
}

func decodeRoute(f []string, ipv6 bool) (hostRoute, error) {
	bad := errors.New("could not decode a host route")
	if ipv6 {
		if len(f) < 10 {
			return hostRoute{}, bad
		}
		var d, g [16]byte
		b, eb := strconv.ParseUint(f[1], 16, 8)
		flag, ef := strconv.ParseUint(f[8], 16, 32)
		n, ed := hex.Decode(d[:], []byte(f[0]))
		m, eg := hex.Decode(g[:], []byte(f[4]))
		if eb != nil || ef != nil || ed != nil || eg != nil || n != 16 || m != 16 || b > 128 {
			return hostRoute{}, bad
		}
		return hostRoute{netip.PrefixFrom(netip.AddrFrom16(d), int(b)), netip.AddrFrom16(g), f[9], flag}, nil
	}
	if len(f) < 8 {
		return hostRoute{}, bad
	}
	d, ed := strconv.ParseUint(f[1], 16, 32)
	g, eg := strconv.ParseUint(f[2], 16, 32)
	mask, em := strconv.ParseUint(f[7], 16, 32)
	flag, ef := strconv.ParseUint(f[3], 16, 32)
	if ed != nil || eg != nil || em != nil || ef != nil {
		return hostRoute{}, bad
	}
	var da, ga, ma [4]byte
	binary.NativeEndian.PutUint32(da[:], uint32(d))
	binary.NativeEndian.PutUint32(ga[:], uint32(g))
	binary.NativeEndian.PutUint32(ma[:], uint32(mask))
	ones, size := net.IPMask(ma[:]).Size()
	if size != 32 {
		return hostRoute{}, bad
	}
	return hostRoute{netip.PrefixFrom(netip.AddrFrom4(da), ones), netip.AddrFrom4(ga), f[0], flag}, nil
}
