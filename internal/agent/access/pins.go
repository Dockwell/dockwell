// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
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
	// SSHPorts are every port SSH listens on now: what the SSH server holds,
	// what systemd holds for it, or, with neither, what sshd -T says.
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

// sshNames are what SSH servers' processes are called: OpenSSH's sshd, which
// from 9.8 runs each session as sshd-session and from 10.0 its sign-in as
// sshd-auth, and Dropbear's dropbear.
var sshNames = []string{"sshd", "sshd-session", "sshd-auth", "dropbear"}

// daemonNames are the SSH servers' processes that listen for new sessions:
// the one whose parent isn't an SSH server's process too.
var daemonNames = []string{"sshd", "dropbear"}

var (
	// systemctl reads ssh.socket and sshd.socket, where systemd holds SSH's
	// port for sshd.
	systemctl = hosttool.Tool{Name: "systemctl", Paths: []string{"/usr/bin/systemctl", "/bin/systemctl"}}
	// sshd says what ports its configuration names, with -T.
	sshd = hosttool.Tool{Name: "sshd", Paths: []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/sbin/sshd"}}
)

// PinReader reads the pins from the kernel's socket table, through /proc.
type PinReader struct {
	// Proc is /proc, or a stand-in for a test.
	Proc fs.FS
	// Tools runs systemctl, to read SSH's socket units, and sshd -T. Nil runs
	// neither.
	Tools *hosttool.Runner
	// Own are the control plane's ports on this server.
	Own []uint16
}

// socket is a TCP socket an SSH server's process holds, as /proc/net has it.
type socket struct {
	local, peer netip.AddrPort
	listening   bool
	// daemon is held by the process that listens for new sessions.
	daemon bool
}

// Read reads the pins as the server has them now, and, with an error, the
// control plane's own ports still, which never needed /proc. SSH's ports are those its
// server listens on, away from loopback, as a session's X11 or -R listener is;
// its sessions are those to one of those ports, so a -L forward's connection
// out isn't one.
func (r *PinReader) Read(ctx context.Context) (Pins, error) {
	holders, err := r.sshSockets()
	if err != nil {
		return Pins{Own: slices.Clone(r.Own)}, fmt.Errorf("can't read the server's processes in /proc: %w", err)
	}
	var sockets []socket
	for _, file := range []string{"net/tcp", "net/tcp6"} {
		err := r.scan(file, holders, &sockets)
		if file == "net/tcp6" && errors.Is(err, fs.ErrNotExist) {
			continue // the kernel has no IPv6
		}
		if err != nil {
			return Pins{Own: slices.Clone(r.Own)}, fmt.Errorf("can't read the kernel's socket table, /proc/%s: %w", file, err)
		}
	}
	pins := Pins{Own: slices.Clone(r.Own)}
	for _, s := range sockets {
		if s.listening && s.daemon && !s.local.Addr().IsLoopback() {
			pins.SSHPorts = appendNew(pins.SSHPorts, s.local.Port())
		}
	}
	for _, port := range r.socketUnitPorts(ctx) {
		pins.SSHPorts = appendNew(pins.SSHPorts, port)
	}
	if len(pins.SSHPorts) == 0 {
		pins.SSHPorts = r.configuredPorts(ctx)
	}
	for _, s := range sockets {
		if s.listening || !slices.Contains(pins.SSHPorts, s.local.Port()) || s.peer.Addr().IsLoopback() || s.local.Addr().IsLoopback() {
			continue
		}
		pins.SSHFrom = appendNew(pins.SSHFrom, netip.PrefixFrom(s.peer.Addr(), s.peer.Addr().BitLen()))
	}
	slices.Sort(pins.SSHPorts)
	slices.SortFunc(pins.SSHFrom, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return pins, nil
}

// scan reads one of /proc/net's socket tables a line at a time, keeping the
// sockets an SSH server's process holds.
func (r *PinReader) scan(file string, holders map[string]bool, out *[]socket) error {
	f, err := r.Proc.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		daemon, ok := holders[fields[9]]
		if !ok {
			continue
		}
		local, lok := address(fields[1])
		peer, pok := address(fields[2])
		if !lok || !pok {
			continue
		}
		switch fields[3] {
		case "0A": // listening
			*out = append(*out, socket{local: local, peer: peer, listening: true, daemon: daemon})
		case "01": // a connection
			*out = append(*out, socket{local: local, peer: peer, daemon: daemon})
		}
	}
	return sc.Err()
}

// sshSockets are the socket inodes SSH servers' processes hold, each saying
// whether the process that listens for new sessions holds it.
func (r *PinReader) sshSockets() (map[string]bool, error) {
	entries, err := fs.ReadDir(r.Proc, ".")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		name := r.comm(e.Name())
		if !slices.Contains(sshNames, name) {
			continue
		}
		daemon := slices.Contains(daemonNames, name) && !slices.Contains(sshNames, r.comm(r.parent(e.Name())))
		fds, err := fs.ReadDir(r.Proc, e.Name()+"/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := readLink(r.Proc, e.Name()+"/fd/"+fd.Name())
			if inode, ok := strings.CutPrefix(target, "socket:["); err == nil && ok {
				inode = strings.TrimSuffix(inode, "]")
				out[inode] = out[inode] || daemon
			}
		}
	}
	return out, nil
}

// comm is a process's name, or "" once it's gone.
func (r *PinReader) comm(pid string) string {
	if pid == "" {
		return ""
	}
	b, err := fs.ReadFile(r.Proc, pid+"/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// parent is a process's parent's ID, from its stat, whose name, in brackets,
// may hold anything: the fields after the last ) are its state and its
// parent's ID. "" when it can't be read.
func (r *PinReader) parent(pid string) string {
	b, err := fs.ReadFile(r.Proc, pid+"/stat")
	if err != nil {
		return ""
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return ""
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// socketUnitPorts are the ports ssh.socket or sshd.socket holds, where systemd
// listens for sshd, as on Ubuntu 22.10 and later, and Fedora, Arch and Flatcar
// when it's on.
func (r *PinReader) socketUnitPorts(ctx context.Context) []uint16 {
	if r.Tools == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := r.Tools.Run(ctx, systemctl, 5*time.Second, nil, "show", "ssh.socket", "sshd.socket", "--property=Listen", "--value")
	if err != nil || res.Code != 0 {
		return nil
	}
	// "0.0.0.0:22 (Stream)" and "[::]:22 (Stream)", a line each, and a blank
	// line between the units; nothing for a unit that isn't there.
	var out []uint16
	for line := range strings.SplitSeq(string(res.Out), "\n") {
		addr, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if ap, err := netip.ParseAddrPort(addr); err == nil {
			out = appendNew(out, ap.Port())
		} else if n, err := strconv.ParseUint(addr, 10, 16); err == nil && n > 0 {
			out = appendNew(out, uint16(n))
		}
	}
	return out
}

// configuredPorts are the ports sshd's configuration names, as sshd -T says
// them: for a server where nothing Dockwell knows holds SSH's port, such as
// inetd.
func (r *PinReader) configuredPorts(ctx context.Context) []uint16 {
	if r.Tools == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := r.Tools.Run(ctx, sshd, 5*time.Second, nil, "-T")
	if err != nil || res.Code != 0 {
		return nil
	}
	var out []uint16
	for line := range strings.SplitSeq(string(res.Out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "port "); ok {
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 16); err == nil && n > 0 {
				out = appendNew(out, uint16(n))
			}
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
func address(field string) (netip.AddrPort, bool) {
	hexAddr, hexPort, ok := strings.Cut(field, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	addr, _ := netip.AddrFromSlice(raw)
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}

func appendNew[T comparable](list []T, v T) []T {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// DockerNetworks are Docker's bridge networks, as the firewall needs them.
type DockerNetworks struct {
	// None is a server without Docker.
	None bool
	// Interfaces are the bridges' names, which the table leaves to their
	// containers.
	Interfaces []string
	// Subnets are where their containers' addresses come from, so a
	// container's SSH session to the server isn't taken for a person's.
	Subnets []netip.Prefix
}

// ErrNoDocker is a server whose Docker socket isn't there.
var ErrNoDocker = errors.New("there's no Docker socket on this server")

// DockerReader lists Docker's bridge networks, at socket: each bridge's name as
// its network sets it, docker0 for Docker's default, and br- with the network's
// ID's first 12 characters otherwise. With no socket there, it says
// ErrNoDocker.
func DockerReader(client *docker.Client, socket string) func(ctx context.Context) (DockerNetworks, error) {
	return func(ctx context.Context) (DockerNetworks, error) {
		if _, err := os.Stat(socket); errors.Is(err, fs.ErrNotExist) {
			return DockerNetworks{}, ErrNoDocker
		}
		nets, err := client.ListNetworks(ctx)
		if err != nil {
			return DockerNetworks{}, err
		}
		var out DockerNetworks
		for _, n := range nets {
			if n.Driver != "bridge" {
				continue
			}
			for _, c := range n.IPAM.Config {
				if p, err := netip.ParsePrefix(c.Subnet); err == nil {
					out.Subnets = append(out.Subnets, p.Masked())
				}
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
			out.Interfaces = append(out.Interfaces, name)
		}
		slices.Sort(out.Interfaces)
		return out, nil
	}
}
