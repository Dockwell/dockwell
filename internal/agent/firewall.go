// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/docker"
)

// firewallWait bounds each command the agent reads a firewall with.
// firewall-cmd starts Python and asks firewalld over D-Bus, which takes a
// second or two on a small server.
const firewallWait = 10 * time.Second

// firewallReadWait bounds a whole read, which those who ask while it runs share.
const firewallReadWait = time.Minute

// firewallTools are the tools the agent reads firewalls with, at the paths the
// distributions put them. None is looked for on PATH.
var firewallTools = map[string]hosttool.Tool{
	"ufw":                   {Name: "ufw", Paths: []string{"/usr/sbin/ufw"}},
	"firewall-cmd":          {Name: "firewall-cmd", Paths: []string{"/usr/bin/firewall-cmd"}},
	"nft":                   {Name: "nft", Paths: []string{"/usr/sbin/nft", "/sbin/nft"}},
	"iptables-save":         {Name: "iptables-save", Paths: []string{"/usr/sbin/iptables-save", "/sbin/iptables-save"}},
	"ip6tables-save":        {Name: "ip6tables-save", Paths: []string{"/usr/sbin/ip6tables-save", "/sbin/ip6tables-save"}},
	"iptables-legacy-save":  {Name: "iptables-legacy-save", Paths: []string{"/usr/sbin/iptables-legacy-save", "/sbin/iptables-legacy-save"}},
	"ip6tables-legacy-save": {Name: "ip6tables-legacy-save", Paths: []string{"/usr/sbin/ip6tables-legacy-save", "/sbin/ip6tables-legacy-save"}},
}

// firewallReader says what filters the server's inbound traffic, reading each
// firewall with its own tool and Docker's published ports, and changes nothing
// (ADR 0052).
type firewallReader struct {
	docker *docker.Client
	// tools runs the commands, one at a time, from their fixed paths, as ADR
	// 0043 has it.
	tools *hosttool.Runner
	// run runs a command, for a test. Nil runs it with tools.
	run func(ctx context.Context, name string, args ...string) (hosttool.Result, error)
	// has says whether a command is installed, for a test. Nil asks tools.
	has func(name string) bool
	// proc reads /proc/net, for a test. Nil reads the real one.
	proc fs.FS
	now  func() time.Time
	// euid is the agent's user, for a test. Nil asks the kernel.
	euid func() int

	mu sync.Mutex
	// reading is the read in flight, which another who asks shares.
	reading *sharedRead
}

// sharedRead is one read, and what came of it, for everyone who asked while it ran.
type sharedRead struct {
	done chan struct{}
	out  *dockwellv1.ServerFirewall
}

// newFirewallReader reads with the agent's tools, or, with none given, as in a
// test, runs none of the machine's.
func newFirewallReader(cfg Config) *firewallReader {
	tools := cfg.Tools
	if tools == nil {
		tools = &hosttool.Runner{Off: true}
	}
	return &firewallReader{docker: cfg.Docker, tools: tools}
}

// command runs one of firewallTools, and returns how it ended.
func (f *firewallReader) command(ctx context.Context, name string, args ...string) (hosttool.Result, error) {
	if f.run != nil {
		ctx, cancel := context.WithTimeout(ctx, firewallWait)
		defer cancel()
		return f.run(ctx, name, args...)
	}
	return f.tools.Run(ctx, firewallTools[name], firewallWait, nil, args...)
}

// output runs a command that says what it found by exiting 0, and returns what
// it printed.
func (f *firewallReader) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	res, err := f.command(ctx, name, args...)
	if err == nil && res.Code != 0 {
		err = hosttool.Fail(name, res)
	}
	return res.Out, err
}

func (f *firewallReader) installed(name string) bool {
	if f.has != nil {
		return f.has(name)
	}
	_, ok := f.tools.Find(firewallTools[name])
	return ok
}

// read reads the firewalls the server has, and the ports Docker publishes. Two
// tabs asking at once share one read.
func (f *firewallReader) read(ctx context.Context) (*dockwellv1.ServerFirewall, error) {
	if f == nil {
		return nil, errors.New("this agent can't read the firewall")
	}
	f.mu.Lock()
	r := f.reading
	if r == nil {
		r = &sharedRead{done: make(chan struct{})}
		f.reading = r
		// The read is everyone's, so it doesn't end as the first who asked leaves.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), firewallReadWait)
		go func() {
			defer cancel()
			r.out = f.readNow(readCtx)
			f.mu.Lock()
			f.reading = nil
			f.mu.Unlock()
			close(r.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-r.done:
		return r.out, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// readNow reads what's there. What it can't read, it says in what it returns,
// so a read always says what it could.
func (f *firewallReader) readNow(ctx context.Context) *dockwellv1.ServerFirewall {
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	out := &dockwellv1.ServerFirewall{Time: timestamppb.New(now())}
	ufwThere := f.installed("ufw")
	if ufwThere {
		out.Tools = append(out.Tools, f.ufw(ctx))
	}
	if f.installed("firewall-cmd") {
		out.Tools = append(out.Tools, f.firewalld(ctx)...)
	}
	kernel, dockerUser := f.kernelRules(ctx, ufwThere)
	out.Tools = append(out.Tools, kernel...)
	out.DockerUserRules = dockerUser
	// A Docker that doesn't answer leaves the firewalls to say: the ports it
	// publishes are what's missing.
	if f.docker != nil {
		ports, err := dockerPublished(ctx, f.docker)
		if err != nil {
			out.PublishedUnreadable = fmt.Sprintf("Docker didn't say which ports it publishes: %v", err)
		}
		out.Published = ports
	}
	proc := f.proc
	if proc == nil {
		proc = os.DirFS("/proc")
	}
	found := listeners(proc)
	ipv4Apart := boundOnIPv4(found)
	opts := reachOptions{dockerUnknown: out.GetPublishedUnreadable() != ""}
	for _, l := range found {
		// Docker's proxy listens on each port Docker publishes, but what reaches
		// that port is forwarded past the input rules, so the port is said with
		// what Docker publishes, not weighed against them. Where the proxy itself
		// takes the traffic, as IPv6 to a container without it, it's weighed.
		if publishedPast(out.Published, l) {
			continue
		}
		opts.dualStack = l.GetAddress() == "::" && !ipv4Apart[listenerKey(l)]
		reachOf(l, out.Tools, opts)
		out.Listening = append(out.Listening, l)
	}
	return out
}

// publishedPast says whether Docker publishes the port a listener holds, on its
// family, past the input rules: not through its proxy.
func publishedPast(ports []*dockwellv1.PublishedPort, l *dockwellv1.HostListener) bool {
	v6 := strings.Contains(l.GetAddress(), ":")
	return slices.ContainsFunc(ports, func(p *dockwellv1.PublishedPort) bool {
		return p.GetHostPort() == l.GetPort() && p.GetProtocol() == l.GetProtocol() &&
			strings.Contains(p.GetHostIp(), ":") == v6 && !p.GetProxied()
	})
}

// unreadable says why a command couldn't read a firewall, in words a person acts
// on. It starts with Dockwell, not the tool, so a tool's name keeps its case.
func (f *firewallReader) unreadable(name string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("Dockwell can't read %s: it didn't answer within %s", name, firewallWait)
	}
	msg := err.Error()
	if strings.Contains(msg, "root") || strings.Contains(msg, "ermission") || strings.Contains(msg, "not permitted") {
		euid := os.Geteuid
		if f.euid != nil {
			euid = f.euid
		}
		if euid() != 0 {
			return fmt.Sprintf("Dockwell can't read %s: it needs root, and the agent doesn't run as root here. Run the agent as root, as the install script does", name)
		}
		return fmt.Sprintf("Dockwell can't read %s, though the agent runs as root: %s. An LXC or OpenVZ container, or SELinux, can refuse it", name, msg)
	}
	return fmt.Sprintf("Dockwell can't read %s: %s", name, msg)
}

// dockerPublished lists the ports Docker publishes for its containers now, and
// which of them Docker's proxy takes: IPv6 to a container on no network with
// IPv6, which the server's input rules cover.
func dockerPublished(ctx context.Context, client *docker.Client) ([]*dockwellv1.PublishedPort, error) {
	nets, err := client.ListNetworks(ctx)
	if err != nil {
		return nil, err
	}
	ipv6 := map[string]bool{}
	for _, n := range nets {
		ipv6[n.ID] = n.EnableIPv6
	}
	var out []*dockwellv1.PublishedPort
	err = client.EachContainer(ctx, nil, func(s *docker.ContainerSummary) error {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		hasIPv6 := false
		for _, e := range s.NetworkSettings.Networks {
			if e != nil && ipv6[e.NetworkID] {
				hasIPv6 = true
			}
		}
		for _, p := range s.Ports {
			if p.PublicPort == 0 {
				continue
			}
			v6 := false
			if a, err := netip.ParseAddr(p.IP); err == nil {
				v6 = a.Is6() && !a.Is4In6()
			}
			out = append(out, &dockwellv1.PublishedPort{
				Container: name, HostIp: p.IP, HostPort: uint32(p.PublicPort), Protocol: p.Type, ContainerPort: uint32(p.PrivatePort),
				Proxied: v6 && !hasIPv6,
			})
		}
		return nil
	})
	slices.SortFunc(out, func(a, b *dockwellv1.PublishedPort) int {
		if a.GetHostPort() != b.GetHostPort() {
			return int(a.GetHostPort()) - int(b.GetHostPort())
		}
		return strings.Compare(a.GetHostIp(), b.GetHostIp())
	})
	return out, err
}
