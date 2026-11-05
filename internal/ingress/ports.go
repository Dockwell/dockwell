// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dockwell/dockwell/internal/docker"
)

// Port is a port on the server.
type Port struct {
	Number int
	UDP    bool
}

// DefaultPorts are what a proxy holds: 80 and 443 over TCP, and 443 over UDP
// for HTTP/3.
var DefaultPorts = []Port{{Number: 80}, {Number: 443}, {Number: 443, UDP: true}}

// Words names a port for people, such as "80", or "443 over UDP".
func (p Port) Words() string {
	if p.UDP {
		return strconv.Itoa(p.Number) + " over UDP"
	}
	return strconv.Itoa(p.Number)
}

// Name is how a plan lists a port, such as "80/tcp".
func (p Port) Name() string {
	if p.UDP {
		return strconv.Itoa(p.Number) + "/udp"
	}
	return strconv.Itoa(p.Number) + "/tcp"
}

// FindCommand is what shows who holds a port.
func (p Port) FindCommand() string {
	flags := "-ltnp"
	if p.UDP {
		flags = "-lunp"
	}
	return "ss " + flags + " 'sport = :" + strconv.Itoa(p.Number) + "'"
}

func portNames(ports []Port) []string {
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = p.Name()
	}
	return out
}

// Holder is what holds a port the proxy needs.
type Holder struct {
	Port Port
	// Words name it, such as "nginx, in the container 1Panel-openresty-abc".
	Words string
	// Self is this process: the panel, on the control server.
	Self bool
	// Unknown is something the agent can't name, as one that isn't root can't
	// read other users' processes.
	Unknown bool
	// Container is the container it is in, if any.
	Container string
	// Process is the name the kernel has for it, such as dockerd, when the agent
	// can read it.
	Process string
}

// Holders finds what holds a server's ports, from what the kernel says and
// what Docker publishes: a published port with Docker's userland proxy off has
// no listening socket, yet Docker's rules take its traffic.
type Holders struct {
	Client *docker.Client
	// Proc is where /proc is. Empty means /proc.
	Proc string
	// PID is this process's. Zero means its own.
	PID int
}

func (h *Holders) proc() string {
	if h.Proc != "" {
		return h.Proc
	}
	return "/proc"
}

// Of says what holds each port, in the order of ports. A socket on one address
// holds the port for a proxy that listens on every one, so every address counts.
func (h *Holders) Of(ctx context.Context, ports []Port) ([]Holder, error) {
	return h.of(ctx, ports, func(netip.Addr) bool { return true })
}

// Dialled says what a proxy reaches on each port as it dials 127.0.0.1, as for a
// route to a port on the server: only what listens on every address, or on that
// one. What listens on another of the server's addresses alone, or on IPv6 alone,
// isn't reached, and isn't named.
func (h *Holders) Dialled(ctx context.Context, ports []Port) ([]Holder, error) {
	return h.of(ctx, ports, func(a netip.Addr) bool {
		a = a.Unmap()
		return a.IsUnspecified() || a == serverLoopback
	})
}

// serverLoopback is where a proxy dials a port on the server (ServerHost).
var serverLoopback = netip.MustParseAddr(ServerHost)

func (h *Holders) of(ctx context.Context, ports []Port, on func(netip.Addr) bool) ([]Holder, error) {
	published, err := h.published(ctx, on)
	if err != nil {
		return nil, err
	}
	sockets, err := h.sockets(on)
	if err != nil {
		return nil, err
	}
	var out []Holder
	var owners map[string]int
	for _, p := range ports {
		if name, ok := published[p]; ok {
			out = append(out, Holder{Port: p, Words: "the container " + name + ", which publishes it", Container: name})
			continue
		}
		inodes := sockets[p]
		if len(inodes) == 0 {
			continue
		}
		if owners == nil {
			owners = h.socketOwners()
		}
		out = append(out, h.holder(ctx, p, inodes, owners))
	}
	return out, nil
}

// published are the ports containers publish on an address on takes, and who by.
// Docker says "0.0.0.0" or "::" for every address.
func (h *Holders) published(ctx context.Context, on func(netip.Addr) bool) (map[Port]string, error) {
	list, err := h.Client.ListContainers(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[Port]string{}
	for _, c := range list {
		for _, p := range c.Ports {
			if p.PublicPort == 0 || len(c.Names) == 0 {
				continue
			}
			if addr, err := netip.ParseAddr(p.IP); err == nil && !on(addr) {
				continue
			}
			port := Port{Number: int(p.PublicPort), UDP: p.Type == "udp"}
			if _, ok := out[port]; !ok {
				out[port] = strings.TrimPrefix(c.Names[0], "/")
			}
		}
	}
	return out, nil
}

// procAddress reads an address as the kernel's tables write it: in hex, IPv4 as one
// 32-bit word and IPv6 as four, each in the machine's byte order, which is little
// endian on every architecture Dockwell is built for.
func procAddress(hexAddr string) (netip.Addr, bool) {
	b, err := hex.DecodeString(hexAddr)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.Addr{}, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	addr, ok := netip.AddrFromSlice(b)
	return addr, ok
}

// sockets reads the kernel's table of listening TCP sockets and bound UDP
// ones, by port, on the addresses on takes.
func (h *Holders) sockets(on func(netip.Addr) bool) (map[Port][]string, error) {
	out := map[Port][]string{}
	for _, f := range []struct {
		name  string
		udp   bool
		state string
	}{{"tcp", false, "0A"}, {"tcp6", false, "0A"}, {"udp", true, "07"}, {"udp6", true, "07"}} {
		path := filepath.Join(h.proc(), "net", f.name)
		file, err := os.Open(path) //nolint:gosec // the kernel's table, under /proc
		if err != nil {
			if os.IsNotExist(err) {
				continue // no IPv6 here
			}
			return nil, fmt.Errorf("can't read which ports are taken from %s: %w", path, err)
		}
		sc := bufio.NewScanner(file)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != f.state {
				continue
			}
			hexAddr, hexPort, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if addr, ok := procAddress(hexAddr); !ok || !on(addr) {
				continue
			}
			n, err := strconv.ParseUint(hexPort, 16, 16)
			if err != nil {
				continue
			}
			p := Port{Number: int(n), UDP: f.udp}
			if inode := fields[9]; !slices.Contains(out[p], inode) {
				out[p] = append(out[p], inode)
			}
		}
		_ = file.Close()
	}
	return out, nil
}

// socketOwners reads which process holds each socket, as far as this process
// may: another user's fds can't be read unless it is root. It is only asked
// when a port is taken, which a server sees once, on its first route.
func (h *Holders) socketOwners() map[string]int {
	out := map[string]int{}
	entries, err := os.ReadDir(h.proc())
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(h.proc(), e.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(h.proc(), e.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if rest, ok := strings.CutPrefix(link, "socket:["); ok {
				out[strings.TrimSuffix(rest, "]")] = pid
			}
		}
	}
	return out
}

// holder names what holds a port.
func (h *Holders) holder(ctx context.Context, p Port, inodes []string, owners map[string]int) Holder {
	self := h.PID
	if self == 0 {
		self = os.Getpid()
	}
	for _, inode := range inodes {
		pid, ok := owners[inode]
		if !ok {
			continue
		}
		if pid == self {
			return Holder{Port: p, Words: "this Dockwell's panel", Self: true}
		}
		name := h.processName(pid)
		if c := h.containerOf(ctx, pid); c != "" {
			return Holder{Port: p, Words: name + ", in the container " + c, Container: c, Process: name}
		}
		return Holder{Port: p, Words: name, Process: name}
	}
	return Holder{Port: p, Words: "a process the agent can't name", Unknown: true}
}

func (h *Holders) processName(pid int) string {
	b, err := os.ReadFile(filepath.Join(h.proc(), strconv.Itoa(pid), "comm"))
	if err != nil || len(b) == 0 {
		return "process " + strconv.Itoa(pid)
	}
	return strings.TrimSpace(string(b))
}

// containerID finds a container's ID in a process's cgroup, as
// /docker/<id> with cgroups v1 and /system.slice/docker-<id>.scope with v2.
var containerID = regexp.MustCompile(`(?:docker[-/]|/)([0-9a-f]{64})(?:\.scope)?\s*$`)

// containerOf is the name of the container a process is in, or empty.
func (h *Holders) containerOf(ctx context.Context, pid int) string {
	file, err := os.Open(filepath.Join(h.proc(), strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		m := containerID.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		c, err := h.Client.InspectContainer(ctx, m[1])
		if err != nil {
			return ""
		}
		return strings.TrimPrefix(c.Name, "/")
	}
	return ""
}

// Returning names the stopped containers that Docker starts again when it next
// restarts, as after a reboot, and that may then take one of the ports: those
// that publish one, and those on the server's own network, which can't be asked
// what they bind while they're stopped. 1Panel's OpenResty is one: restart:
// always and network_mode: host, so a manual stop lasts until the next reboot.
//
// Moby starts such a container if its policy says so, whatever stopped it:
// always, and on-failure with a failing exit, which docker stop's is. It leaves
// one with unless-stopped that was stopped by hand, and it can only have been: a
// crash would have restarted it (daemon/internal/restartmanager).
//
// It only warns, so it says nothing when it can't find out.
func (h *Holders) Returning(ctx context.Context, ports []Port) []string {
	list, err := h.Client.ListContainers(ctx, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range list {
		if c.State != "exited" || c.Labels[docker.OwnPrefix+"ingress"] != "" {
			continue
		}
		got, err := h.Client.InspectContainer(ctx, c.ID)
		if err != nil {
			continue
		}
		if comesBack(got) && mayHold(got, ports) {
			out = append(out, strings.TrimPrefix(got.Name, "/"))
		}
	}
	slices.Sort(out)
	return out
}

// comesBack reports a stopped container Docker starts again when it restarts.
func comesBack(c *docker.Container) bool {
	switch c.HostConfig.RestartPolicy.Name {
	case "always":
		return true
	case "on-failure":
		return c.State.ExitCode != 0
	}
	return false
}

// mayHold reports a stopped container that publishes one of the ports, or that
// may bind any of them, on the server's own network.
func mayHold(c *docker.Container, ports []Port) bool {
	if c.HostConfig.NetworkMode == "host" {
		return true
	}
	for key, bindings := range c.HostConfig.PortBindings {
		_, proto, _ := strings.Cut(key, "/")
		for _, b := range bindings {
			n, err := strconv.Atoi(b.HostPort)
			if err != nil {
				continue
			}
			if slices.Contains(ports, Port{Number: n, UDP: proto == "udp"}) {
				return true
			}
		}
	}
	return false
}
