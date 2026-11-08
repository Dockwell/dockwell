// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"io/fs"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/nftables"
	"github.com/dockwell/dockwell/internal/docker"
)

// Pins are what's always allowed beyond what keeps every server reachable,
// before any rule (ADR 0052).
type Pins struct {
	// SSHPorts are every port SSH listens on now: what sshd holds, what
	// ssh.socket holds, and the local port of each session open now.
	SSHPorts []uint16
	// SSHFrom are the addresses of the SSH sessions open now from outside the
	// server, each pinned alone. The request's own address isn't one: over ssh
	// -L, the panel's requests come from 127.0.0.1.
	SSHFrom []netip.Prefix
	// Own are the ports the control plane on this server listens on, for the
	// panel and its agents, open to anywhere. Empty on a server that joined.
	Own []uint16
}

// Rules are the pins as the table writes them.
func (p Pins) Rules() []nftables.Rule {
	var out []nftables.Rule
	if len(p.SSHPorts) > 0 && len(p.SSHFrom) > 0 {
		out = append(out, nftables.Rule{Name: "pin ssh", Protocol: "tcp", Ports: p.SSHPorts, Sources: p.SSHFrom})
	}
	if len(p.Own) > 0 {
		out = append(out, nftables.Rule{Name: "pin control plane", Protocol: "tcp", Ports: p.Own})
	}
	return out
}

// sshNames are what sshd's processes are called: OpenSSH 9.8 and later run
// each session as sshd-session, and 10.0 its sign-in as sshd-auth.
var sshNames = []string{"sshd", "sshd-session", "sshd-auth"}

// systemctl reads ssh.socket, where systemd holds SSH's port for sshd.
var systemctl = hosttool.Tool{Name: "systemctl", Paths: []string{"/usr/bin/systemctl", "/bin/systemctl"}}

// PinReader reads the pins from the kernel's socket table, through /proc.
type PinReader struct {
	// Proc is /proc, or a stand-in for a test.
	Proc fs.FS
	// Tools runs systemctl, to read ssh.socket. Nil reads no socket unit.
	Tools *hosttool.Runner
	// Own are the control plane's ports on this server.
	Own []uint16
}

// Read reads the pins as the server has them now.
func (r *PinReader) Read(ctx context.Context) (Pins, error) {
	inodes := r.sshSockets()
	pins := Pins{Own: slices.Clone(r.Own)}
	for _, file := range []string{"net/tcp", "net/tcp6"} {
		b, err := fs.ReadFile(r.Proc, file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || !inodes[fields[9]] {
				continue
			}
			local, lport, ok := address(fields[1])
			if !ok {
				continue
			}
			switch fields[3] {
			case "0A": // listening
				pins.SSHPorts = appendNew(pins.SSHPorts, lport)
			case "01": // a session
				pins.SSHPorts = appendNew(pins.SSHPorts, lport)
				peer, _, ok := address(fields[2])
				if ok && !peer.IsLoopback() && !local.IsLoopback() {
					p := netip.PrefixFrom(peer, peer.BitLen())
					if !slices.Contains(pins.SSHFrom, p) {
						pins.SSHFrom = append(pins.SSHFrom, p)
					}
				}
			}
		}
	}
	for _, port := range r.socketUnitPorts(ctx) {
		pins.SSHPorts = appendNew(pins.SSHPorts, port)
	}
	slices.Sort(pins.SSHPorts)
	slices.SortFunc(pins.SSHFrom, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return pins, nil
}

// sshSockets are the socket inodes sshd's processes hold.
func (r *PinReader) sshSockets() map[string]bool {
	out := map[string]bool{}
	entries, err := fs.ReadDir(r.Proc, ".")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := fs.ReadFile(r.Proc, e.Name()+"/comm")
		if err != nil || !slices.Contains(sshNames, strings.TrimSpace(string(comm))) {
			continue
		}
		fds, err := fs.ReadDir(r.Proc, e.Name()+"/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := readLink(r.Proc, e.Name()+"/fd/"+fd.Name())
			if inode, ok := strings.CutPrefix(target, "socket:["); err == nil && ok {
				out[strings.TrimSuffix(inode, "]")] = true
			}
		}
	}
	return out
}

// socketUnitPorts are the ports ssh.socket holds, where systemd listens for
// sshd, as on Ubuntu 22.10 and later.
func (r *PinReader) socketUnitPorts(ctx context.Context) []uint16 {
	if r.Tools == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := r.Tools.Run(ctx, systemctl, 5*time.Second, nil, "show", "ssh.socket", "--property=Listen", "--value")
	if err != nil || res.Code != 0 {
		return nil
	}
	// "0.0.0.0:22 (Stream)" and "[::]:22 (Stream)", a line each.
	var out []uint16
	for line := range strings.SplitSeq(string(res.Out), "\n") {
		addr, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if ap, err := netip.ParseAddrPort(addr); err == nil {
			out = appendNew(out, ap.Port())
		} else if n, err := strconv.ParseUint(addr, 10, 16); err == nil {
			out = appendNew(out, uint16(n))
		}
	}
	return out
}

// readLink reads a symlink in an fs.FS that can, as os.DirFS does.
func readLink(fsys fs.FS, name string) (string, error) {
	if rl, ok := fsys.(fs.ReadLinkFS); ok {
		return rl.ReadLink(name)
	}
	return "", fs.ErrInvalid
}

// address reads /proc/net's "0100007F:1F90": the address in the kernel's own
// byte order, four bytes at a time, and the port.
func address(field string) (netip.Addr, uint16, bool) {
	hexAddr, hexPort, ok := strings.Cut(field, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.Addr{}, 0, false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	addr, _ := netip.AddrFromSlice(raw)
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	return addr.Unmap(), uint16(port), true
}

func appendNew[T comparable](list []T, v T) []T {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// DockerInterfaces lists the interfaces of Docker's bridge networks: the
// bridge's name as its network sets it, docker0 for Docker's default, and
// br- with the network's ID's first 12 characters otherwise.
func DockerInterfaces(client *docker.Client) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		nets, err := client.ListNetworks(ctx)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, n := range nets {
			if n.Driver != "bridge" {
				continue
			}
			name := n.Options["com.docker.network.bridge.name"]
			switch {
			case name != "":
			case n.Name == "bridge":
				name = "docker0"
			case len(n.ID) >= 12:
				name = "br-" + n.ID[:12]
			default:
				continue
			}
			out = append(out, name)
		}
		slices.Sort(out)
		return out, nil
	}
}
