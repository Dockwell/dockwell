// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"bufio"
	"context"
	"fmt"
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

// Of says what holds each port, in the order of ports.
func (h *Holders) Of(ctx context.Context, ports []Port) ([]Holder, error) {
	published, err := h.published(ctx)
	if err != nil {
		return nil, err
	}
	sockets, err := h.sockets()
	if err != nil {
		return nil, err
	}
	var out []Holder
	var owners map[string]int
	for _, p := range ports {
		if name, ok := published[p]; ok {
			out = append(out, Holder{Port: p, Words: "the container " + name + ", which publishes it"})
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

// published are the ports containers publish, and who by.
func (h *Holders) published(ctx context.Context) (map[Port]string, error) {
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
			port := Port{Number: int(p.PublicPort), UDP: p.Type == "udp"}
			if _, ok := out[port]; !ok {
				out[port] = strings.TrimPrefix(c.Names[0], "/")
			}
		}
	}
	return out, nil
}

// sockets reads the kernel's table of listening TCP sockets and bound UDP
// ones, by port. A socket on one address holds the port for a proxy that
// listens on every one, so every address counts.
func (h *Holders) sockets() (map[Port][]string, error) {
	out := map[Port][]string{}
	for _, f := range []struct {
		name  string
		udp   bool
		state string
	}{{"tcp", false, "0A"}, {"tcp6", false, "0A"}, {"udp", true, "07"}, {"udp6", true, "07"}} {
		file, err := os.Open(filepath.Join(h.proc(), "net", f.name))
		if err != nil {
			if os.IsNotExist(err) {
				continue // no IPv6 here
			}
			return nil, fmt.Errorf("can't read which ports are taken from %s: %w", file.Name(), err)
		}
		sc := bufio.NewScanner(file)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != f.state {
				continue
			}
			_, hexPort, ok := strings.Cut(fields[1], ":")
			if !ok {
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
			return Holder{Port: p, Words: name + ", in the container " + c}
		}
		return Holder{Port: p, Words: name}
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
