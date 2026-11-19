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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/health"
)

// What reading a service's ports may take (#791): the containers it reads, the
// ports it lists and the ones it asks, how long each may take, how many at once,
// and how much of an answer it reads.
const (
	mostPortContainers = 4
	mostListed         = 128
	mostAsked          = 24
	portWait           = 2 * time.Second
	portBanner         = 300 * time.Millisecond
	portsAtOnce        = 4
	portRead           = 256
	portSaid           = 40
)

// Why a service's ports weren't asked, as the result's unread says it.
const (
	unreadRootless = "Docker runs rootless here, so the agent lists its ports and asks none"
	unreadOwnNet   = "it's on the server's own network, where its ports are the server's"
	unreadNoAddr   = "its container has no address of its own the agent can ask"
	unreadChanged  = "it restarted while its ports were asked. Check again"
	unreadBinds    = "its network lets a socket use an address it doesn't have, so its ports aren't asked"
	unreadPowers   = "it can change its own network (privileged, NET_ADMIN or SYS_ADMIN), so its ports aren't asked"
)

// errNotAContainer refuses an address a probe never dials: not the container's own.
var errNotAContainer = errors.New("that isn't the container's own address, so Dockwell doesn't ask it")

// netns is a container's network namespace, held open by its file, where a probe's
// dials are made: from inside, from the container's own address to that same
// address, so a dial reaches the container and nothing past it (see dialLocal).
type netns struct {
	// owns refuses an address that isn't one of the namespace's own.
	owns func(address string) error
	// dial connects to one of its own addresses, from inside it.
	dial  func(ctx context.Context, address string) (net.Conn, error)
	close func()
}

// portReader reads a service's ports. Its fields stand in for the server's in tests.
type portReader struct {
	docker *docker.Client
	// proc is where the server's /proc is.
	proc string
	// enter holds open the network namespace of process pid. Nil enters it for real.
	enter func(pid int) (*netns, error)
}

// appPorts reads the TCP ports of a service's running containers, and asks each
// whether it answers at the container's own address, from inside its network (#791).
func (c *calls) appPorts(ctx context.Context, call *agentv1.AppPortsCall) (*agentv1.AppPortsResult, error) {
	if c.cfg.Docker == nil {
		return nil, errors.New("this agent has no Docker to read an app's ports from")
	}
	r := portReader{docker: c.cfg.Docker, proc: "/proc"}
	return r.read(ctx, call.GetEnvironment(), call.GetService())
}

// errOwnNetwork says a process shares the agent's own network, so there's nothing
// of the container's own to ask.
var errOwnNetwork = errors.New("it shares the server's own network")

// enterNetwork holds open the network namespace of process pid, refusing the
// agent's own.
func enterNetwork(proc string) func(pid int) (*netns, error) {
	return func(pid int) (*netns, error) {
		fd, err := unix.Open(filepath.Join(proc, strconv.Itoa(pid), "ns", "net"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open process %d's network: %w", pid, err)
		}
		var theirs, ours unix.Stat_t
		if err := unix.Fstat(fd, &theirs); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		if err := unix.Stat("/proc/thread-self/ns/net", &ours); err != nil || theirs.Ino == ours.Ino && theirs.Dev == ours.Dev {
			_ = unix.Close(fd)
			if err != nil {
				return nil, err
			}
			return nil, errOwnNetwork
		}
		n := &netns{close: func() { _ = unix.Close(fd) }}
		n.owns = func(address string) error {
			_, err := inNetwork(fd, func() (struct{}, error) { return struct{}{}, ownAddress(address) })
			return err
		}
		n.dial = func(ctx context.Context, address string) (net.Conn, error) {
			return inNetwork(fd, func() (net.Conn, error) { return dialLocal(ctx, address) })
		}
		return n, nil
	}
}

// inNetwork runs fn on a thread of its own inside the network namespace fd, then
// takes the thread back to the agent's network. A thread that can't go back is
// never used again: it ends locked, with its goroutine. A socket fn makes stays in
// the namespace it was made in, wherever it's used after.
func inNetwork[T any](fd int, fn func() (T, error)) (T, error) {
	type done struct {
		v   T
		err error
	}
	ch := make(chan done, 1)
	go func() {
		runtime.LockOSThread()
		var zero T
		own, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			ch <- done{zero, err}
			return
		}
		defer func() { _ = unix.Close(own) }()
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			ch <- done{zero, fmt.Errorf("enter the container's network: %w", err)}
			return
		}
		v, ferr := fn()
		if err := unix.Setns(own, unix.CLONE_NEWNET); err != nil {
			if c, ok := any(v).(io.Closer); ok && c != nil {
				_ = c.Close()
			}
			ch <- done{zero, fmt.Errorf("leave the container's network: %w", err)}
			return
		}
		runtime.UnlockOSThread()
		ch <- done{v, ferr}
	}()
	d := <-ch
	return d.v, d.err
}

// askable reads address, refusing what a probe never asks: loopback, link-local,
// unspecified or multicast.
func askable(address string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return netip.Addr{}, err
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	a = a.Unmap()
	switch {
	case a.IsLoopback(), a.IsUnspecified(), a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return a, errNotAContainer
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return a, errLinkLocal
	}
	return a, nil
}

// ownAddress refuses an address that askable refuses, that isn't on one of the
// current network's interfaces, or in a network that lets a socket take an address
// it doesn't hold. It reads every interface's addresses, so it runs once per call,
// never per dial (the re-check's N3).
func ownAddress(address string) error {
	a, err := askable(address)
	if err != nil {
		return err
	}
	if bindsAnything(a) {
		return errBindsAnything
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, x := range addrs {
		if p, err := netip.ParsePrefix(x.String()); err == nil && p.Addr().Unmap() == a {
			return nil
		}
	}
	return errNotAContainer
}

// errBindsAnything refuses to dial from a network that lets a socket take a source
// address it doesn't hold, as then the source can't prove the destination is its own.
var errBindsAnything = errors.New("its network lets a socket use an address it doesn't have, so Dockwell doesn't ask it")

// bindsAnything reports a current network whose ip_nonlocal_bind, for a's family,
// lets a socket take a source address it doesn't hold. /proc/sys/net is the reading
// thread's own network's.
func bindsAnything(a netip.Addr) bool {
	file := "/proc/sys/net/ipv4/ip_nonlocal_bind"
	if a.Is6() {
		file = "/proc/sys/net/ipv6/ip_nonlocal_bind"
	}
	b, err := os.ReadFile(file)
	return err != nil || strings.TrimSpace(string(b)) != "0"
}

// dialLocal dials one of the current network's own addresses from that same
// address, so it's delivered within the network, by the route a connection to one's
// own address always takes:
//
//   - The source is the address itself. The kernel refuses a socket a source
//     address the network doesn't hold (EADDRNOTAVAIL), before any packet, so the
//     only address a dial can reach is one the network has. A network that lets a
//     socket take any source (ip_nonlocal_bind) is refused, read just before the
//     socket is made.
//   - That holds while the container can't change its own network: its addresses,
//     routes, policy rules or sysctls. One that can (privileged, NET_ADMIN or
//     SYS_ADMIN) is never asked (mayChangeItsNetwork), as it could route even its
//     own address out; whatever left would carry its own address both ways, as it
//     can send itself.
//   - The socket isn't bound to a device. Bound to lo, as this first was, it reached
//     nothing on a real server: lo gives it 127.0.0.1 as its source, which the
//     kernel drops on its way to another interface's address wherever
//     route_localnet is 0, its default (CI found it on #872). Bound to the
//     interface holding the address, as it was next, an address moved to another
//     interface would fall through to the default route (the second re-check's N1).
func dialLocal(ctx context.Context, address string) (net.Conn, error) {
	a, err := askable(address)
	if err != nil {
		return nil, err
	}
	if bindsAnything(a) {
		return nil, errBindsAnything
	}
	d := &net.Dialer{Timeout: portWait, LocalAddr: &net.TCPAddr{IP: a.AsSlice()}}
	return d.DialContext(ctx, "tcp", address)
}

// mayChangeItsNetwork reports a container that can change its own network, so its
// source address no longer proves where a dial goes: one that's privileged, adds
// NET_ADMIN, SYS_ADMIN or ALL, or sets ip_nonlocal_bind as it's made.
func mayChangeItsNetwork(c *docker.Container) bool {
	if c.HostConfig.Privileged {
		return true
	}
	for _, added := range c.HostConfig.CapAdd {
		switch strings.TrimPrefix(strings.ToUpper(added), "CAP_") {
		case "NET_ADMIN", "SYS_ADMIN", "ALL":
			return true
		}
	}
	for key, value := range c.HostConfig.Sysctls {
		switch strings.ReplaceAll(key, "/", ".") {
		case "net.ipv4.ip_nonlocal_bind", "net.ipv6.ip_nonlocal_bind":
			if strings.TrimSpace(value) != "0" {
				return true
			}
		}
	}
	return false
}

// instance is what says a container is the same one running: its process and when
// it started, and the address a probe asks.
type instance struct {
	pid     int
	started time.Time
	address string
}

func instanceOf(c *docker.Container) instance {
	address := ""
	if a, err := netip.ParseAddr(health.Address(c.NetworkSettings.Networks)); err == nil {
		address = a.Unmap().String()
	}
	return instance{pid: c.State.Pid, started: c.State.StartedAt, address: address}
}

// read reads the ports of a service's running containers, Dockwell's own by the
// revision it deploys, never one only labeled as the service. It lists every port
// they expose, publish and listen on, up to mostListed, and asks the first
// mostAsked, from inside the first container's network.
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
	info, err := r.docker.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask Docker about itself: %w", err)
	}
	enter := r.enter
	if enter == nil {
		enter = enterNetwork(r.proc)
	}
	// More replicas than are read may listen on more: the list is then cut.
	cut := len(running) > mostPortContainers
	ports := map[uint32]*dockwellv1.ServicePortCheck{}
	var heard []uint32 // ports only something listening named
	var probe *netns
	var probed instance
	probeID := ""
	defer func() {
		if probe != nil {
			probe.close()
		}
	}()
	unread := ""
	ownNet := false
	powers := false
	for _, id := range running[:min(len(running), mostPortContainers)] {
		// A running container whose listeners aren't counted leaves the list cut short,
		// so nothing is said not to listen (the re-check's N2).
		c, err := r.docker.InspectContainer(ctx, id)
		if err != nil || !c.State.Running {
			cut = true
			continue
		}
		if c.HostConfig.NetworkMode == "host" {
			return &agentv1.AppPortsResult{Unread: unreadOwnNet}, nil
		}
		// One that can change its own network is listed and never asked (the second
		// re-check's N1).
		if mayChangeItsNetwork(c) {
			powers = true
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
		was := instanceOf(c)
		if was.pid <= 0 {
			cut = true
			continue
		}
		// The network to ask from is held before what listens is read, and both count
		// only if the container is still the same process after: a process ID freed
		// and taken again in between would be another's (the review's L1). A process
		// on the server's own network has neither: what listens there is the server's.
		n, err := enter(was.pid)
		own := errors.Is(err, errOwnNetwork)
		if err != nil {
			n = nil
		}
		var heardHere []*dockwellv1.HostListener
		if !own {
			heardHere = listeners(os.DirFS(filepath.Join(r.proc, strconv.Itoa(was.pid))))
		}
		again, err := r.docker.InspectContainer(ctx, id)
		if err != nil || !again.State.Running || instanceOf(again) != was {
			// It restarted between the reads: what was read may be another process's.
			if n != nil {
				n.close()
			}
			cut = true
			continue
		}
		if own {
			// The same process, on the server's own network: what listens there is the
			// server's, so none of it is counted.
			ownNet, cut = true, true
			continue
		}
		for _, l := range heardHere {
			if l.GetProtocol() != "tcp" {
				continue
			}
			if _, known := ports[l.GetPort()]; !known {
				heard = append(heard, l.GetPort())
			}
			at(ports, l.GetPort()).Listening = true
		}
		switch {
		case n != nil && probe == nil:
			probe, probed, probeID = n, was, id
		case n != nil:
			n.close()
		}
	}
	// What it exposes or publishes first, by number, then what only listens.
	order := slices.Sorted(maps.Keys(ports))
	order = slices.DeleteFunc(order, func(p uint32) bool { return slices.Contains(heard, p) })
	slices.Sort(heard)
	order = append(order, slices.Compact(heard)...)
	if len(order) > mostListed {
		order, cut = order[:mostListed], true
	}
	out := make([]*dockwellv1.ServicePortCheck, 0, len(order))
	for _, p := range order {
		out = append(out, ports[p])
	}
	switch {
	case len(out) == 0 && ownNet:
		return &agentv1.AppPortsResult{Unread: unreadOwnNet, Cut: cut}, nil
	case len(out) == 0:
		return &agentv1.AppPortsResult{Unread: "it exposes, publishes and listens on no TCP port", Cut: cut}, nil
	case info.Rootless():
		return &agentv1.AppPortsResult{Ports: out, Unread: unreadRootless, Cut: cut}, nil
	case powers:
		return &agentv1.AppPortsResult{Ports: out, Unread: unreadPowers, Cut: cut}, nil
	case probe == nil && ownNet:
		unread = unreadOwnNet
	case probe == nil || probed.address == "":
		unread = unreadNoAddr
	default:
		err := probe.owns(net.JoinHostPort(probed.address, "0"))
		if errors.Is(err, errBindsAnything) {
			unread = unreadBinds
			break
		}
		if err != nil {
			unread = unreadNoAddr
			break
		}
		unread = r.ask(ctx, probe, probeID, probed, out[:min(len(out), mostAsked)])
	}
	return &agentv1.AppPortsResult{Ports: out, Unread: unread, Cut: cut}, nil
}

// ask asks each port, four at once, at the container's own address from inside its
// network. Before each, the container must still be the same one at the same
// address (the review's L2); once it isn't, the rest go unasked, and it says why.
func (r portReader) ask(ctx context.Context, n *netns, id string, was instance, ports []*dockwellv1.ServicePortCheck) string {
	dial := n.dial
	var mu sync.Mutex
	changed := false
	var wg sync.WaitGroup
	slots := make(chan struct{}, portsAtOnce)
	for _, p := range ports {
		slots <- struct{}{}
		mu.Lock()
		stop := changed
		mu.Unlock()
		if stop {
			<-slots
			break
		}
		if c, err := r.docker.InspectContainer(ctx, id); err != nil || !c.State.Running || instanceOf(c) != was {
			mu.Lock()
			changed = true
			mu.Unlock()
			<-slots
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			answer, said := askPort(ctx, dial, net.JoinHostPort(was.address, strconv.FormatUint(uint64(p.GetPort()), 10)))
			mu.Lock()
			defer mu.Unlock()
			p.Answer, p.Said = answer, said
		}()
	}
	wg.Wait()
	if changed {
		// What was asked of it may have been asked of what came after: none of it counts.
		for _, p := range ports {
			p.Answer, p.Said = dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED, ""
		}
		return unreadChanged
	}
	return ""
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
	case errors.Is(err, errLinkLocal), errors.Is(err, errNotAContainer), errors.Is(err, errBindsAnything):
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
