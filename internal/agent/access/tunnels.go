// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// wg reads a WireGuard tunnel's port, where WireGuard's tools are installed.
var wg = hosttool.Tool{Name: "wg", Paths: []string{"/usr/bin/wg", "/usr/sbin/wg", "/usr/local/bin/wg"}}

// WireGuardTunnels lists the server's WireGuard tunnels: each interface whose
// kind, as /sys/class/net says it, is wireguard, with the UDP port it listens
// on where wg can say. net is /sys/class/net, or a stand-in. SSH over such a
// tunnel passes the confirmation's test while the tunnel's flow is tracked,
// and stops at its next handshake if deny everything else drops its port, so
// the plan names each one no rule allows (ADR 0052).
func WireGuardTunnels(net fs.FS, tools *hosttool.Runner) func(ctx context.Context) []Tunnel {
	return func(ctx context.Context) []Tunnel {
		entries, err := fs.ReadDir(net, ".")
		if err != nil {
			return nil
		}
		var out []Tunnel
		for _, e := range entries {
			b, err := fs.ReadFile(net, e.Name()+"/uevent")
			if err != nil || !slices.Contains(strings.Split(strings.TrimSpace(string(b)), "\n"), "DEVTYPE=wireguard") {
				continue
			}
			t := Tunnel{Name: e.Name()}
			if tools != nil {
				if res, err := tools.Run(ctx, wg, 5*time.Second, nil, "show", e.Name(), "listen-port"); err == nil && res.Code == 0 {
					if n, err := strconv.ParseUint(strings.TrimSpace(string(res.Out)), 10, 16); err == nil {
						t.Port = uint16(n)
					}
				}
			}
			out = append(out, t)
		}
		return out
	}
}
