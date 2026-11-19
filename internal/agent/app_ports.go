// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/health"
)

// What reading a service's ports may take (#791): the containers it reads, the
// ports it asks, how long each may take, how many at once, and how much of an
// answer it reads.
const (
	mostPortContainers = 4
	mostPorts          = 24
	portWait           = 2 * time.Second
	portBanner         = 300 * time.Millisecond
	portsAtOnce        = 4
	portRead           = 256
	portSaid           = 40
)

// portReader reads a service's ports. Its fields stand in for the server's in tests.
type portReader struct {
	docker *docker.Client
	// proc is where the server's /proc is.
	proc string
	// own are the server's own addresses, which a probe never dials.
	own func() ([]netip.Addr, error)
	// dial connects to an address a probe may ask. Nil dials it, guarded.
	dial func(ctx context.Context, address string) (net.Conn, error)
}

// appPorts reads the TCP ports of a service's running containers, and asks each
// whether it answers at the container's own address (#791).
func (c *calls) appPorts(ctx context.Context, call *agentv1.AppPortsCall) (*agentv1.AppPortsResult, error) {
	if c.cfg.Docker == nil {
		return nil, errors.New("this agent has no Docker to read an app's ports from")
	}
	r := portReader{docker: c.cfg.Docker, proc: "/proc", own: ownAddresses}
	return r.read(ctx, call.GetEnvironment(), call.GetService())
}

// ownAddresses are the addresses on the server's interfaces.
func ownAddresses() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap())
		}
	}
	return out, nil
}

// errNotAContainer refuses an address a probe never dials: not a container's own.
var errNotAContainer = errors.New("that isn't a container's own address, so Dockwell doesn't ask it")

// probeable reports an address a probe may dial: a container's own on a Docker
// network, so never loopback, link-local, unspecified, multicast, or one of the
// server's own interfaces.
func probeable(a netip.Addr, own []netip.Addr) error {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsLoopback(), a.IsUnspecified(), a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return errNotAContainer
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return errLinkLocal
	case slices.Contains(own, a):
		return errNotAContainer
	}
	return nil
}

// guardedDial dials address only where probeable says it may, checked again as
// the socket connects, whatever an address resolves to.
func guardedDial(own []netip.Addr) func(ctx context.Context, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: portWait, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		a, err := netip.ParseAddr(host)
		if err != nil {
			return err
		}
		return probeable(a, own)
	}}
	return func(ctx context.Context, address string) (net.Conn, error) {
		return d.DialContext(ctx, "tcp", address)
	}
}

// read reads the ports of a service's running containers, Dockwell's own by the
// revision it deploys, never one only labeled as the service.
func (r portReader) read(ctx context.Context, environment, service string) (*agentv1.AppPortsResult, error) {
	if environment == "" || service == "" {
		return nil, errors.New("the call names no service")
	}
	list, err := r.docker.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(environment), docker.LabelService + "=" + service, docker.LabelRevision}})
	if err != nil {
		return nil, fmt.Errorf("read %s's containers: %w", service, err)
	}
	var running []string
	for i := range list {
		if list[i].State == "running" {
			running = append(running, list[i].ID)
		}
	}
	if len(running) == 0 {
		return &agentv1.AppPortsResult{Unread: "no container of it runs"}, nil
	}
	slices.Sort(running)
	own, err := r.own()
	if err != nil {
		return nil, fmt.Errorf("read the server's own addresses: %w", err)
	}
	dial := r.dial
	if dial == nil {
		dial = guardedDial(own)
	}
	ports := map[uint32]*dockwellv1.ServicePortCheck{}
	var heard []uint32 // ports only something listening named, in the order found
	address := ""
	for _, id := range running[:min(len(running), mostPortContainers)] {
		c, err := r.docker.InspectContainer(ctx, id)
		if err != nil {
			continue
		}
		if c.HostConfig.NetworkMode == "host" {
			return &agentv1.AppPortsResult{Unread: "it's on the server's own network, where its ports are the server's"}, nil
		}
		for key := range c.Config.ExposedPorts {
			if p, ok := tcpPort(key); ok {
				at(ports, p).Exposed = true
			}
		}
		for key, bindings := range c.HostConfig.PortBindings {
			p, ok := tcpPort(key)
			if !ok {
				continue
			}
			for _, b := range bindings {
				if hp, err := strconv.ParseUint(b.HostPort, 10, 16); err == nil && hp > 0 && at(ports, p).GetServerPort() == 0 {
					at(ports, p).ServerPort = uint32(hp)
				}
			}
		}
		if c.State.Pid > 0 {
			for _, l := range listeners(os.DirFS(filepath.Join(r.proc, strconv.Itoa(c.State.Pid)))) {
				if l.GetProtocol() != "tcp" {
					continue
				}
				if _, known := ports[l.GetPort()]; !known {
					heard = append(heard, l.GetPort())
				}
				at(ports, l.GetPort()).Listening = true
			}
		}
		if address == "" {
			if a, err := netip.ParseAddr(health.Address(c.NetworkSettings.Networks)); err == nil && probeable(a, own) == nil {
				address = a.Unmap().String()
			}
		}
	}
	// What it exposes or publishes first, by number, then what only listens.
	order := slices.Sorted(maps.Keys(ports))
	order = slices.DeleteFunc(order, func(p uint32) bool { return slices.Contains(heard, p) })
	slices.Sort(heard)
	order = append(order, slices.Compact(heard)...)
	out := make([]*dockwellv1.ServicePortCheck, 0, min(len(order), mostPorts))
	for _, p := range order[:min(len(order), mostPorts)] {
		out = append(out, ports[p])
	}
	if len(out) == 0 {
		return &agentv1.AppPortsResult{Unread: "it exposes, publishes and listens on no TCP port"}, nil
	}
	if address == "" {
		return &agentv1.AppPortsResult{Ports: out, Unread: "its container has no address the agent can reach"}, nil
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, portsAtOnce)
	for _, p := range out {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			p.Answer, p.Said = askPort(ctx, dial, net.JoinHostPort(address, strconv.FormatUint(uint64(p.GetPort()), 10)))
		}()
	}
	wg.Wait()
	return &agentv1.AppPortsResult{Ports: out}, nil
}

// at is the port's check, made as it's first named.
func at(ports map[uint32]*dockwellv1.ServicePortCheck, p uint32) *dockwellv1.ServicePortCheck {
	if c, ok := ports[p]; ok {
		return c
	}
	c := &dockwellv1.ServicePortCheck{Port: p}
	ports[p] = c
	return c
}

// tcpPort reads Docker's "3000/tcp", or "3000", as a TCP port.
func tcpPort(key string) (uint32, bool) {
	port, protocol, _ := strings.Cut(key, "/")
	if protocol != "" && protocol != "tcp" {
		return 0, false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	return uint32(p), err == nil && p > 0
}

// askPort asks what answers at address, for portWait at most: a banner it sends
// first, such as SSH's, or else the status line of a GET / that follows no
// redirect. It reads portRead bytes at most and keeps a few words of them.
func askPort(ctx context.Context, dial func(context.Context, string) (net.Conn, error), address string) (dockwellv1.PortAnswer, string) {
	ctx, cancel := context.WithTimeout(ctx, portWait)
	defer cancel()
	conn, err := dial(ctx, address)
	if err != nil {
		return dockwellv1.PortAnswer_PORT_ANSWER_NOTHING, portWords(err)
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	buf := make([]byte, portRead)
	// A server that speaks first, as SSH and SMTP do, says so before anything is sent.
	banner := time.Now().Add(portBanner)
	if deadline.Before(banner) {
		banner = deadline
	}
	_ = conn.SetReadDeadline(banner)
	if n, _ := io.ReadAtLeast(conn, buf, 1); n > 0 {
		return spoke(buf[:n])
	}
	_ = conn.SetDeadline(deadline)
	host, _, _ := net.SplitHostPort(address)
	if _, err := io.WriteString(conn, "GET / HTTP/1.0\r\nHost: "+host+"\r\nUser-Agent: Dockwell\r\nConnection: close\r\n\r\n"); err != nil {
		return dockwellv1.PortAnswer_PORT_ANSWER_OTHER, "took the connection, then closed it"
	}
	n := 0
	for n < len(buf) && !bytes.Contains(buf[:n], []byte("\n")) {
		m, err := conn.Read(buf[n:])
		n += m
		if err != nil {
			break
		}
	}
	if n == 0 {
		return dockwellv1.PortAnswer_PORT_ANSWER_OTHER, "took the connection and said nothing"
	}
	return spoke(buf[:n])
}

// spoke reads what a server said first: an HTTP status line, SSH's banner, or a
// few printable words of anything else.
func spoke(b []byte) (dockwellv1.PortAnswer, string) {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	line = bytes.TrimRight(line, "\r")
	if rest, ok := bytes.CutPrefix(line, []byte("HTTP/1.")); ok && len(rest) > 2 && rest[1] == ' ' {
		return dockwellv1.PortAnswer_PORT_ANSWER_HTTP, printable(rest[2:])
	}
	if bytes.HasPrefix(line, []byte("SSH-")) {
		return dockwellv1.PortAnswer_PORT_ANSWER_OTHER, "SSH"
	}
	if said := printable(line); said != "" {
		return dockwellv1.PortAnswer_PORT_ANSWER_OTHER, said
	}
	return dockwellv1.PortAnswer_PORT_ANSWER_OTHER, "took the connection, and it isn't HTTP"
}

// printable keeps the printable ASCII of b, portSaid characters at most.
func printable(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		if s.Len() >= portSaid {
			break
		}
		if c >= 0x20 && c < 0x7f {
			s.WriteByte(c)
		}
	}
	return strings.TrimSpace(s.String())
}

// portWords says why nothing answered, in a few words.
func portWords(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing listens"
	case errors.Is(err, errLinkLocal), errors.Is(err, errNotAContainer):
		return "not asked: not the container's own address"
	case errors.Is(err, context.DeadlineExceeded), timedOut(err):
		return "no answer in 2 s"
	}
	return "couldn't connect"
}

// timedOut reports an error that says it timed out, as a dial's does.
func timedOut(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
