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
	"os/exec"
	"path/filepath"
	"slices"
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
	c.State.Running, c.State.Status, c.State.Pid = true, "running", 4242
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

// inside is a container's network as a test has it: its own addresses, and dials
// to them carried to loopback listeners by port.
func inside(own []string, ports map[uint32]int, asked *[]string, mu *sync.Mutex) func(int) (*netns, error) {
	return func(int) (*netns, error) {
		return &netns{
			owns: func(address string) error {
				if host, _, _ := net.SplitHostPort(address); slices.Contains(own, host) {
					return nil
				}
				return errNotAContainer
			},
			dial:  toLoopback(ports, asked, mu),
			close: func() {},
		}, nil
	}
}

// portLine is a port as a test compares it.
func portLine(p *dockwellv1.ServicePortCheck) string {
	return fmt.Sprintf("%d exposed=%t listening=%t server=%d %s %s", p.GetPort(), p.GetExposed(), p.GetListening(), p.GetServerPort(), p.GetAnswer(), p.GetSaid())
}

// A service's ports are what its container exposes, publishes and listens on beyond
// loopback, each asked at the container's own address from inside its network: an
// HTTP status line, SSH's banner, or nothing. A container only labeled as the
// service isn't read (#791).
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
	})
	proc := procWith(t, 4242, listen(3000, false), listen(22, false), listen(9000, true), listen(9100, false))
	var asked []string
	var mu sync.Mutex
	r := portReader{docker: e.Client(), proc: proc, enter: inside([]string{containerIP}, map[uint32]int{3000: web, 22: ssh, 9100: closedPort(t)}, &asked, &mu)}
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
		said = append(said, portLine(p))
	}
	if strings.Join(said, "\n") != strings.Join(want, "\n") || got.GetUnread() != "" || got.GetCut() {
		t.Fatalf("ports:\n%s\nwant:\n%s\nunread %q, cut %t", strings.Join(said, "\n"), strings.Join(want, "\n"), got.GetUnread(), got.GetCut())
	}
	for _, a := range asked {
		if host, _, _ := net.SplitHostPort(a); host != containerIP {
			t.Errorf("asked %s", a)
		}
	}
}

// localAddresses are the test machine's own addresses, beyond loopback.
func localAddresses(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && !p.Addr().IsLoopback() && !p.Addr().IsLinkLocalUnicast() {
			out = append(out, p.Addr().Unmap().String())
		}
	}
	return out
}

// The review's H1: a developer's network may have any subnet, so a container's
// endpoint may say a LAN or internet address with no bridge of this server behind
// it. From inside the container's network, an address that isn't one of its own is
// refused before any dial, and a dial is bound to its loopback device, so it's
// delivered locally or not at all. Run here in the test's own network, as the
// agent runs it in the container's: whatever isn't local is listed and never asked.
func TestAPortProbeNeverLeavesTheContainer(t *testing.T) {
	t.Parallel()
	local := localAddresses(t)
	for _, a := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "192.168.1.2", "10.0.0.6", "203.0.113.6", "100.101.102.102", "8.8.8.8", "::ffff:10.1.2.3"} {
		if slices.Contains(local, strings.TrimPrefix(a, "::ffff:")) {
			continue
		}
		if ownAddress(net.JoinHostPort(a, "22")) == nil {
			t.Errorf("%s may be asked", a)
		}
		start := time.Now()
		if c, err := dialLocal(t.Context(), net.JoinHostPort(a, "22")); err == nil {
			_ = c.Close()
			t.Errorf("dialed %s", a)
		} else if took := time.Since(start); took > 100*time.Millisecond {
			t.Errorf("%s was dialed before it was refused: %s", a, took)
		}
	}
	if _, err := dialLocal(t.Context(), "169.254.169.254:80"); !errors.Is(err, errLinkLocal) {
		t.Errorf("the metadata address said %v", err)
	}
	// One of the network's own addresses is dialed through its loopback device.
	if len(local) > 0 {
		l, err := net.Listen("tcp", net.JoinHostPort(local[0], "0"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			if c, err := l.Accept(); err == nil {
				_ = c.Close()
			}
		}()
		c, err := dialLocal(t.Context(), l.Addr().String())
		if err != nil {
			t.Fatalf("its own address %s wasn't dialed: %v", l.Addr(), err)
		}
		_ = c.Close()
	}

	for _, endpoint := range []string{"192.168.1.2", "203.0.113.6"} {
		if slices.Contains(local, endpoint) {
			continue
		}
		e := webApp(t, func(c *docker.Container) {
			c.Config.ExposedPorts = map[string]struct{}{"22/tcp": {}, "445/tcp": {}, "5432/tcp": {}}
			c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"lan": {IPAddress: endpoint}}
		})
		var dialed []string
		var mu sync.Mutex
		enter := func(int) (*netns, error) {
			return &netns{owns: ownAddress, close: func() {}, dial: func(ctx context.Context, address string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, address)
				mu.Unlock()
				return dialLocal(ctx, address)
			}}, nil
		}
		got, err := portReader{docker: e.Client(), proc: t.TempDir(), enter: enter}.read(t.Context(), "env_shop", "web")
		if err != nil {
			t.Fatal(err)
		}
		if len(dialed) != 0 || len(got.GetPorts()) != 3 || got.GetUnread() != unreadNoAddr {
			t.Errorf("an endpoint at %s: dialed %v, got %v", endpoint, dialed, got)
		}
		for _, p := range got.GetPorts() {
			if p.GetAnswer() != dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED {
				t.Errorf("an endpoint at %s: %s", endpoint, portLine(p))
			}
		}
	}
}

// On rootless Docker the ports are listed and none asked, with why; on the server's
// own network nothing is.
func TestAppPortsAskNothingOnRootlessDockerOrTheServersNetwork(t *testing.T) {
	t.Parallel()
	e := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = map[string]struct{}{"80/tcp": {}} })
	e.Rootless = true
	var asked []string
	var mu sync.Mutex
	r := portReader{docker: e.Client(), proc: t.TempDir(), enter: inside([]string{containerIP}, nil, &asked, &mu)}
	got, err := r.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 || got.GetUnread() != unreadRootless || len(got.GetPorts()) != 1 || got.GetPorts()[0].GetAnswer() != dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED {
		t.Errorf("rootless: asked %v, got %v", asked, got)
	}
	own := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = map[string]struct{}{"80/tcp": {}} })
	// What listens in a network that's the server's own is the server's, so it isn't read.
	proc := procWith(t, 4242, listen(9100, false))
	got, err = portReader{docker: own.Client(), proc: proc, enter: func(int) (*netns, error) { return nil, errOwnNetwork }}.read(t.Context(), "env_shop", "web")
	if err != nil || got.GetUnread() != unreadOwnNet || len(got.GetPorts()) != 1 || got.GetPorts()[0].GetPort() != 80 || got.GetPorts()[0].GetAnswer() != dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED {
		t.Errorf("the server's network: %v, %v", got, err)
	}
}

// The review's M1: every port is listed, up to mostListed, and the first mostAsked
// asked; the rest say they weren't, and a list cut short, or of more containers than
// are read, says so.
func TestAppPortsListMoreThanTheyAsk(t *testing.T) {
	t.Parallel()
	exposed := map[string]struct{}{}
	for p := 8000; p < 8200; p++ {
		exposed[strconv.Itoa(p)+"/tcp"] = struct{}{}
	}
	e := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = exposed })
	var asked []string
	var mu sync.Mutex
	r := portReader{docker: e.Client(), proc: t.TempDir(), enter: inside([]string{containerIP}, nil, &asked, &mu)}
	got, err := r.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	ports := got.GetPorts()
	if len(ports) != mostListed || !got.GetCut() || ports[0].GetPort() != 8000 || len(asked) != mostAsked {
		t.Fatalf("%d ports, cut %t, first %d, asked %d", len(ports), got.GetCut(), ports[0].GetPort(), len(asked))
	}
	for i, p := range ports {
		unasked := p.GetAnswer() == dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED
		if unasked != (i >= mostAsked) {
			t.Errorf("port %d of %d: %s", i, len(ports), portLine(p))
		}
	}

	// A fifth replica isn't read, so what only it listens on may be missing.
	five := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = map[string]struct{}{"80/tcp": {}} })
	for i := 2; i <= 5; i++ {
		c := docker.Container{ID: fmt.Sprintf("web-%d", i), Name: fmt.Sprintf("/web-%d", i), Config: docker.Config{Image: "acme/app:1", Labels: map[string]string{docker.LabelProject: docker.Project("env_shop"), docker.LabelService: "web", docker.LabelRevision: "1"}}}
		c.State.Running, c.State.Status, c.State.Pid = true, "running", 5000+i
		five.AddContainer(c)
	}
	got, err = portReader{docker: five.Client(), proc: t.TempDir(), enter: inside([]string{containerIP}, nil, &asked, &mu)}.read(t.Context(), "env_shop", "web")
	if err != nil || !got.GetCut() {
		t.Errorf("five replicas: %v, %v", got, err)
	}
}

// The review's L1 and L2: what listens counts only if the container is the same
// process after it's read, and each port is asked only while it's the same one at
// the same address; once it isn't, nothing asked counts.
func TestAppPortsCountOnlyWhatTheSameContainerSaid(t *testing.T) {
	t.Parallel()
	proc := procWith(t, 4242, listen(9100, false))
	e := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = map[string]struct{}{"80/tcp": {}} })
	var asked []string
	var mu sync.Mutex
	fake := inside([]string{containerIP}, nil, &asked, &mu)
	restarts := func(pid int) (*netns, error) {
		e.Edit("web-1", func(c *docker.Container) { c.State.Pid = 5555 })
		return fake(pid)
	}
	got, err := portReader{docker: e.Client(), proc: proc, enter: restarts}.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got.GetPorts() {
		if p.GetPort() == 9100 || p.GetListening() || p.GetAnswer() != dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED {
			t.Errorf("after a restart between reads: %s", portLine(p))
		}
	}
	if got.GetUnread() != unreadNoAddr || len(asked) != 0 {
		t.Errorf("after a restart between reads: unread %q, asked %v", got.GetUnread(), asked)
	}

	exposed := map[string]struct{}{}
	for p := 8000; p < 8010; p++ {
		exposed[strconv.Itoa(p)+"/tcp"] = struct{}{}
	}
	moved := webApp(t, func(c *docker.Container) { c.Config.ExposedPorts = exposed })
	var once sync.Once
	n := 0
	enter := func(int) (*netns, error) {
		return &netns{
			owns:  func(string) error { return nil },
			close: func() {},
			dial: func(context.Context, string) (net.Conn, error) {
				mu.Lock()
				n++
				mu.Unlock()
				once.Do(func() {
					moved.Edit("web-1", func(c *docker.Container) {
						c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"dockwell": {IPAddress: "172.18.0.9"}}
					})
				})
				return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
			},
		}, nil
	}
	got, err = portReader{docker: moved.Client(), proc: t.TempDir(), enter: enter}.read(t.Context(), "env_shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetUnread() != unreadChanged || n >= len(exposed) {
		t.Errorf("an address that moved mid-call: unread %q, dialed %d", got.GetUnread(), n)
	}
	for _, p := range got.GetPorts() {
		if p.GetAnswer() != dockwellv1.PortAnswer_PORT_ANSWER_UNSPECIFIED {
			t.Errorf("an address that moved mid-call: %s", portLine(p))
		}
	}
}

// What a call reads is bounded: an answer's bytes and a probe's time.
func TestAPortAnswerIsBounded(t *testing.T) {
	t.Parallel()
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

// A service with no container running, on the server's own network, or with no
// TCP port says why nothing was read, and an agent without Docker refuses the call.
func TestAppPortsSayWhyNothingWasRead(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	got, err := portReader{docker: e.Client()}.read(t.Context(), "env_shop", "web")
	if err != nil || got.GetUnread() != "no container of it runs" {
		t.Errorf("none running: %v, %v", got, err)
	}
	host := webApp(t, func(c *docker.Container) { c.HostConfig.NetworkMode = "host" })
	got, err = portReader{docker: host.Client()}.read(t.Context(), "env_shop", "web")
	if err != nil || got.GetUnread() != unreadOwnNet || len(got.GetPorts()) != 0 {
		t.Errorf("host network: %v, %v", got, err)
	}
	var asked []string
	var mu sync.Mutex
	bare := webApp(t, func(*docker.Container) {})
	got, err = portReader{docker: bare.Client(), proc: t.TempDir(), enter: inside(nil, nil, &asked, &mu)}.read(t.Context(), "env_shop", "web")
	if err != nil || got.GetUnread() != "it exposes, publishes and listens on no TCP port" {
		t.Errorf("no ports: %v, %v", got, err)
	}
	if _, err := (&calls{}).appPorts(t.Context(), &agentv1.AppPortsCall{Environment: "env_shop", Service: "web"}); err == nil {
		t.Error("an agent without Docker read ports")
	}
}

// The agent's own network is never entered as a container's, and a network that
// can't be entered says so and leaves the thread as it was.
func TestEnteringANetworkRefusesTheAgentsOwn(t *testing.T) {
	t.Parallel()
	if _, err := enterNetwork("/proc")(os.Getpid()); !errors.Is(err, errOwnNetwork) {
		t.Errorf("entering the agent's own network said %v", err)
	}
	if _, err := enterNetwork(t.TempDir())(4242); err == nil {
		t.Error("a process with no network was entered")
	}
}

// Inside another network, the agent's own addresses aren't the container's: the
// check runs in the network entered, not the agent's. It needs a network namespace
// to enter, which an unprivileged user can make only where the kernel allows it.
func TestAddressesAreCheckedInsideTheNetworkEntered(t *testing.T) {
	t.Parallel()
	local := localAddresses(t)
	if len(local) == 0 {
		t.Skip("the test machine has no address beyond loopback to compare")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	child := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", "sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("can't make a network namespace here: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = child.Wait() })
	// The child is in its own network once /proc says a namespace other than ours.
	deadline := time.Now().Add(5 * time.Second)
	var n *netns
	for {
		var err error
		n, err = enterNetwork("/proc")(child.Process.Pid)
		if err == nil {
			break
		}
		if !errors.Is(err, errOwnNetwork) || time.Now().After(deadline) {
			t.Skipf("can't enter a network namespace here: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer n.close()
	address := net.JoinHostPort(local[0], "1")
	if err := ownAddress(address); err != nil {
		t.Fatalf("outside, %s isn't the machine's own: %v", local[0], err)
	}
	if err := n.owns(address); err == nil {
		t.Errorf("inside a new network, the machine's %s counts as its own", local[0])
	} else if strings.Contains(err.Error(), "enter the container's network") {
		t.Skipf("can't enter a network namespace here: %v", err)
	}
	if c, err := n.dial(t.Context(), address); err == nil {
		_ = c.Close()
		t.Errorf("inside a new network, the machine's %s was dialed", local[0])
	}
}
