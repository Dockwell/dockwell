// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

const containerIP = "172.18.0.5"

// serve listens on loopback and answers each connection with answer, returning its port.
func serve(t *testing.T, answer func(net.Conn)) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				answer(c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// procWith writes a /proc whose process pid listens as lines say, in /proc/net/tcp's form.
func procWith(t *testing.T, pid int, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	folder := filepath.Join(dir, strconv.Itoa(pid), "net")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, l := range lines {
		fmt.Fprintf(&body, "   %d: %s 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0\n", i, l)
	}
	if err := os.WriteFile(filepath.Join(folder, "tcp"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// listen is /proc/net/tcp's local address for 0.0.0.0, or 127.0.0.1, at port.
func listen(port int, loopback bool) string {
	addr := "00000000"
	if loopback {
		addr = "0100007F"
	}
	return fmt.Sprintf("%s:%04X", addr, port)
}

// webApp adds a running container of the service shop/web, Dockwell's own, and one
// only labeled as it, which is never read.
func webApp(t *testing.T, edit func(*docker.Container)) *dockertest.Engine {
	t.Helper()
	e := dockertest.New(t)
	labels := map[string]string{docker.LabelProject: docker.Project("env_shop"), docker.LabelService: "web"}
	other := e.AddContainer(docker.Container{ID: "aa-other", Name: "/aa-other", Config: docker.Config{Image: "acme/other:1", Labels: labels, ExposedPorts: map[string]struct{}{"8080/tcp": {}}}})
	other.State.Running, other.State.Status = true, "running"
	ours := map[string]string{docker.LabelRevision: "1"}
	for k, v := range labels {
		ours[k] = v
	}
	c := docker.Container{ID: "web-1", Name: "/web-1", Config: docker.Config{Image: "acme/app:1", Labels: ours}}
	c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"dockwell": {IPAddress: containerIP}}
	c.State.Running, c.State.Status = true, "running"
	edit(&c)
	e.AddContainer(c)
	return e
}

// toLoopback dials what a probe asks of the container at a loopback port, as ports
// says, and records each address asked.
func toLoopback(ports map[uint32]int, asked *[]string, mu *sync.Mutex) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, address string) (net.Conn, error) {
		mu.Lock()
		*asked = append(*asked, address)
		mu.Unlock()
		host, port, _ := net.SplitHostPort(address)
		if host != containerIP {
			return nil, fmt.Errorf("asked %s, not the container", address)
		}
		p, _ := strconv.Atoi(port)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[uint32(p)])))
	}
}

// A service's ports are what its container exposes, publishes and listens on beyond
// loopback, each asked at the container's own address: an HTTP status line, SSH's
// banner, or nothing. A container only labeled as the service isn't read (#791).
func TestAppPortsSayWhatEachIsAndWhatAnswers(t *testing.T) {
	t.Parallel()
	web := serve(t, func(c net.Conn) {
		buf := make([]byte, 512)
		_, _ = c.Read(buf)
		_, _ = io.WriteString(c, "HTTP/1.1 302 Found\r\nLocation: /user/login\r\n\r\n")
	})
	ssh := serve(t, func(c net.Conn) { _, _ = io.WriteString(c, "SSH-2.0-OpenSSH_9.6\r\n") })
	e := webApp(t, func(c *docker.Container) {
		c.Config.ExposedPorts = map[string]struct{}{"3000/tcp": {}, "22/tcp": {}, "53/udp": {}}
		c.HostConfig.PortBindings = map[string][]docker.PortBinding{"22/tcp": {{HostIP: "", HostPort: "2222"}}}
		c.State.Pid = 4242
	})
	proc := procWith(t, 4242, listen(3000, false), listen(22, false), listen(9000, true), listen(9100, false))
	var asked []string
	var mu sync.Mutex
	r := portReader{
		docker: e.Client(), proc: proc,
		own:  func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil },
		dial: toLoopback(map[uint32]int{3000: web, 22: ssh, 9100: closedPort(t)}, &asked, &mu),
	}
	got, err := r.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"22 exposed=true listening=true server=2222 PORT_ANSWER_OTHER SSH",
		"3000 exposed=true listening=true server=0 PORT_ANSWER_HTTP 302 Found",
		"9100 exposed=false listening=true server=0 PORT_ANSWER_NOTHING nothing listens",
	}
	var said []string
	for _, p := range got.GetPorts() {
		said = append(said, fmt.Sprintf("%d exposed=%t listening=%t server=%d %s %s", p.GetPort(), p.GetExposed(), p.GetListening(), p.GetServerPort(), p.GetAnswer(), p.GetSaid()))
	}
	if strings.Join(said, "\n") != strings.Join(want, "\n") || got.GetUnread() != "" {
		t.Fatalf("ports:\n%s\nwant:\n%s\nunread %q", strings.Join(said, "\n"), strings.Join(want, "\n"), got.GetUnread())
	}
	for _, a := range asked {
		if host, _, _ := net.SplitHostPort(a); host != containerIP {
			t.Errorf("asked %s", a)
		}
	}
}

// A probe dials only a container's own address: never loopback, link-local, an
// unspecified or multicast address, or one of the server's own, checked as the
// socket connects too.
func TestAPortProbeNeverLeavesTheContainer(t *testing.T) {
	t.Parallel()
	own := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")}
	for _, a := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "192.0.2.10", "::ffff:192.0.2.10", "2001:db8::10"} {
		if probeable(netip.MustParseAddr(a), own) == nil {
			t.Errorf("%s may be probed", a)
		}
	}
	if err := probeable(netip.MustParseAddr(containerIP), own); err != nil {
		t.Errorf("a container's address may not be probed: %v", err)
	}
	// Something listens on loopback, and still isn't asked.
	port := serve(t, func(c net.Conn) { _, _ = io.WriteString(c, "SSH-2.0-x\r\n") })
	dial := guardedDial(own)
	for _, addr := range []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "169.254.169.254:80"} {
		if c, err := dial(t.Context(), addr); err == nil {
			_ = c.Close()
			t.Errorf("dialed %s", addr)
		}
	}
	if _, err := dial(t.Context(), "169.254.169.254:80"); !errors.Is(err, errLinkLocal) {
		t.Errorf("a link-local dial said %v", err)
	}

	// A container whose address is one of the server's own, as on a macvlan made
	// wrong, has its ports listed and none asked.
	e := webApp(t, func(c *docker.Container) {
		c.Config.ExposedPorts = map[string]struct{}{"80/tcp": {}}
		c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"dockwell": {IPAddress: "192.0.2.10"}}
	})
	asked := 0
	r := portReader{
		docker: e.Client(), proc: t.TempDir(), own: func() ([]netip.Addr, error) { return own, nil },
		dial: func(context.Context, string) (net.Conn, error) { asked++; return nil, errors.New("no") },
	}
	got, err := r.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	if asked != 0 || len(got.GetPorts()) != 1 || got.GetUnread() == "" {
		t.Errorf("asked %d, got %v", asked, got)
	}
}

// What a call reads is bounded: ports, an answer's bytes and a probe's time.
func TestAppPortsAreBounded(t *testing.T) {
	t.Parallel()
	exposed := map[string]struct{}{}
	for p := 8000; p < 8040; p++ {
		exposed[strconv.Itoa(p)+"/tcp"] = struct{}{}
	}
	e := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = exposed })
	r := portReader{
		docker: e.Client(), proc: t.TempDir(), own: func() ([]netip.Addr, error) { return nil, nil },
		dial: func(context.Context, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
		},
	}
	got, err := r.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetPorts()) != mostPorts || got.GetPorts()[0].GetPort() != 8000 {
		t.Errorf("%d ports, first %d", len(got.GetPorts()), got.GetPorts()[0].GetPort())
	}

	// A long first line is cut, and a server that never answers is left after portWait.
	long := serve(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 512))
		_, _ = io.WriteString(c, "HTTP/1.1 200 "+strings.Repeat("O", 10_000)+"\r\n")
	})
	quiet := serve(t, func(c net.Conn) { time.Sleep(4 * portWait) })
	var d net.Dialer
	dialTo := func(port int) func(context.Context, string) (net.Conn, error) {
		return func(ctx context.Context, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		}
	}
	answer, said := askPort(t.Context(), dialTo(long), containerIP+":80")
	if answer != dockwellv1.PortAnswer_PORT_ANSWER_HTTP || len(said) > portSaid || !strings.HasPrefix(said, "200 OOO") {
		t.Errorf("a long status line said %s %q", answer, said)
	}
	start := time.Now()
	answer, said = askPort(t.Context(), dialTo(quiet), containerIP+":80")
	if took := time.Since(start); took > portWait+time.Second {
		t.Errorf("a quiet port took %s", took)
	}
	if answer != dockwellv1.PortAnswer_PORT_ANSWER_OTHER {
		t.Errorf("a quiet port said %s %q", answer, said)
	}
}

// A service with no container running, or on the server's own network, says why
// nothing was read, and an agent without Docker refuses the call.
func TestAppPortsSayWhyNothingWasRead(t *testing.T) {
	t.Parallel()
	own := func() ([]netip.Addr, error) { return nil, nil }
	e := dockertest.New(t)
	got, err := portReader{docker: e.Client(), own: own}.read(t.Context(), "env_shop", "web")
	if err != nil || got.GetUnread() != "no container of it runs" {
		t.Errorf("none running: %v, %v", got, err)
	}
	host := webApp(t, func(c *docker.Container) { c.HostConfig.NetworkMode = "host" })
	got, err = portReader{docker: host.Client(), own: own}.read(t.Context(), "env_shop", "web")
	if err != nil || !strings.Contains(got.GetUnread(), "server's own network") || len(got.GetPorts()) != 0 {
		t.Errorf("host network: %v, %v", got, err)
	}
	bare := webApp(t, func(*docker.Container) {})
	got, err = portReader{docker: bare.Client(), proc: t.TempDir(), own: own}.read(t.Context(), "env_shop", "web")
	if err != nil || got.GetUnread() != "it exposes, publishes and listens on no TCP port" {
		t.Errorf("no ports: %v, %v", got, err)
	}
	if _, err := (&calls{}).appPorts(t.Context(), &agentv1.AppPortsCall{Environment: "env_shop", Service: "web"}); err == nil {
		t.Error("an agent without Docker read ports")
	}
}
