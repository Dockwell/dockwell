// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// listenAt adds a socket on one address to the kernel's table, written as the
// kernel writes it.
func (p *proc) listenAt(table, hexAddr string, port int, inode string) {
	p.t.Helper()
	path := filepath.Join(p.root, "net", table)
	b, err := os.ReadFile(path)
	if err != nil {
		b = []byte(procHeader)
	}
	zeros := strings.Repeat("0", len(hexAddr))
	line := fmt.Sprintf("   0: %s:%04X %s:0000 0A 00000000:00000000 00:00000000 00000000     0        0 %s 1 0000000000000000 100 0 0 10 0\n", hexAddr, port, zeros, inode)
	if err := os.WriteFile(path, append(b, line...), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// Caddy dials 127.0.0.1 for a route to a port on the server, so what the plan
// names as listening there is what that reaches, and nothing on another address.
// The ports Caddy takes for itself still count on every address.
func TestAPlanNamesOnlyWhatTheProxyReachesOnAPort(t *testing.T) {
	for name, tc := range map[string]struct {
		table, addr string
		reached     bool
	}{
		"every IPv4 address":     {"tcp", "00000000", true},
		"127.0.0.1":              {"tcp", "0100007F", true},
		"another of its own":     {"tcp", "0501A8C0", false},
		"127.0.0.53":             {"tcp", "3500007F", false},
		"every address":          {"tcp6", strings.Repeat("0", 32), true},
		"::1 alone":              {"tcp6", "00000000000000000000000001000000", false},
		"127.0.0.1, as IPv6 has": {"tcp6", "0000000000000000FFFF00000100007F", true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSetup(t)
			s.proc.listenAt(tc.table, tc.addr, 8080, "7001")
			s.proc.process(7001, "myapp", "0::/", "7001")
			who, nothing := s.Listening(t.Context(), 8080)
			switch {
			case tc.reached && who != "myapp":
				t.Errorf("what listens there reads %q, %v, want myapp", who, nothing)
			case !tc.reached && (who != "" || !nothing):
				t.Errorf("what the proxy can't reach reads as listening: %q, %v", who, nothing)
			}
			held, err := s.holders().Of(t.Context(), []Port{{Number: 8080}})
			if err != nil || len(held) != 1 || held[0].Words != "myapp" {
				t.Errorf("as a port the proxy would take, it's held by %+v, %v", held, err)
			}
		})
	}
	t.Run("published on another address", func(t *testing.T) {
		s := newSetup(t)
		for name, ip := range map[string]string{"lan-only": "192.168.1.5", "loopback": "127.0.0.1"} {
			s.engine.AddContainer(docker.Container{
				Name: "/" + name, State: docker.State{Running: true, Status: "running"},
				HostConfig: docker.HostConfig{PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostIP: ip, HostPort: map[string]string{"lan-only": "8080", "loopback": "8081"}[name]}}}},
			})
		}
		if who, nothing := s.Listening(t.Context(), 8080); who != "" || !nothing {
			t.Errorf("a container published on another address reads as listening: %q, %v", who, nothing)
		}
		if who, _ := s.Listening(t.Context(), 8081); !strings.Contains(who, "loopback") {
			t.Errorf("a container published on 127.0.0.1 reads %q", who)
		}
	})
}

// Docker's API is root on the server to anyone who reaches it. Validate refuses
// 2375 and 2376 by number, and dockerd listens wherever it's told, so a route to
// any port it listens on is refused too, for everyone, as the plan is made and as
// it applies.
func TestDockersAPIOnAnotherPortIsRefused(t *testing.T) {
	for name, cgroup := range map[string]string{"on the server": "0::/system.slice/docker.service", "in a container": "0::/system.slice/docker-" + strings.Repeat("a", 64) + ".scope"} {
		t.Run(name, func(t *testing.T) {
			s := newSetup(t)
			s.proc.listen("tcp", 4243, "0A", "7002")
			s.proc.process(7002, "dockerd", cgroup, "7002")
			ref := routeRef("www")
			err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: hostPortSpec(4243)}})
			if err == nil || !strings.Contains(err.Error(), "Docker's API") || !strings.Contains(err.Error(), "dockerd") || !strings.Contains(err.Error(), "root on the server") {
				t.Errorf("Check = %v, want it refused, naming Docker's API", err)
			}
			if err := s.Apply(t.Context(), ref, hostPortSpec(4243), nil); err == nil || !strings.Contains(err.Error(), "Docker's API") {
				t.Errorf("Apply = %v, want it refused", err)
			}
			if routes, _ := s.proxy.Routes(t.Context()); len(routes) != 0 {
				t.Errorf("the proxy has %+v", routes)
			}
		})
	}
	// Anything else on a port is a person's to read and decide.
	s := newSetup(t)
	s.take(t, 4243, "myapp")
	ref := routeRef("www")
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: hostPortSpec(4243)}}); err != nil {
		t.Errorf("Check of a port something else listens on = %v", err)
	}
}

// The same against this machine's own kernel, for the byte order of its tables.
func TestTheKernelsTableReadsAsTheProxyDials(t *testing.T) {
	h := &Holders{Client: dockertest.New(t).Client(), PID: 1}
	listen := func(addr string) int {
		t.Helper()
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("can't listen on %s here: %v", addr, err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln.Addr().(*net.TCPAddr).Port
	}
	named := func(port int) (dialled, held bool) {
		t.Helper()
		d, err := h.Dialled(t.Context(), []Port{{Number: port}})
		if err != nil {
			t.Fatal(err)
		}
		o, err := h.Of(t.Context(), []Port{{Number: port}})
		if err != nil {
			t.Fatal(err)
		}
		return len(d) == 1, len(o) == 1
	}
	if dialled, held := named(listen("127.0.0.1:0")); !dialled || !held {
		t.Errorf("a listener on 127.0.0.1 is dialled %v, held %v", dialled, held)
	}
	// Ports are kept per address, so the kernel may give ::1 or 127.0.0.2 a port that
	// something else on this machine listens on at 127.0.0.1, and that one is dialled.
	// So these take a port nothing holds there.
	alone := func(host string) int {
		t.Helper()
		for range 20 {
			port := listen(net.JoinHostPort(host, "0"))
			free, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err == nil {
				_ = free.Close()
				return port
			}
		}
		t.Fatalf("every port %s got is held on 127.0.0.1 too", host)
		return 0
	}
	if dialled, held := named(alone("::1")); dialled || !held {
		t.Errorf("a listener on ::1 alone is dialled %v, held %v", dialled, held)
	}
	if dialled, held := named(alone("127.0.0.2")); dialled || !held {
		t.Errorf("a listener on 127.0.0.2 is dialled %v, held %v", dialled, held)
	}
}
