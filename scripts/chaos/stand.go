// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// server is what the stand names the server that joins.
const server = "chaos-1"

// stand is one control server and one server that joined it, on one Docker
// Engine, which every scenario breaks in turn.
type stand struct {
	bin, agentBin string
	// dir holds everything the stand makes, and goes with it.
	dir         string
	keep        bool
	controlData string
	// agentData is chaos-1's data folder: with root, a small disk of its own,
	// which a scenario fills.
	agentData string
	mounted   bool

	apiPort, agentPort int
	proxy              *proxy
	control, agent     *proc
	fake               *proc

	socket string
	docker *docker.Client
	// real is a real Docker Engine, root is sudo -n working.
	real, root bool

	serverID string
	// scope is the environment the services are in, and project its Compose
	// project.
	scope, project string

	webPort, sparePort int
	webImage           string
	// images are the two images slow is deployed between. Each scenario chooses
	// the one that differs from the store, including after a rollback.
	images [2]string

	probe *probe
	clock clock
}

func newStand(ctx context.Context, bin, agentBin, fakeBin string) (*stand, error) {
	s := &stand{clock: newClock()}
	var err error
	if s.bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	if s.agentBin, err = filepath.Abs(agentBin); err != nil {
		return nil, err
	}
	if s.dir, err = os.MkdirTemp("", "chaos-"); err != nil {
		return nil, err
	}
	s.controlData = filepath.Join(s.dir, "control")
	s.agentData = filepath.Join(s.dir, "agent")
	s.root = os.Getenv("CHAOS_HOST_FAULTS") == "1" && exec.CommandContext(ctx, "sudo", "-n", "true").Run() == nil
	say("the stand is in %s; root through sudo: %t", s.dir, s.root)
	steps := []struct {
		what string
		do   func(context.Context, string) error
	}{
		{"Docker", s.startEngine},
		{"chaos-1's disk", func(ctx context.Context, _ string) error { return s.makeDisk(ctx) }},
		{"the control server", func(ctx context.Context, _ string) error { return s.startControl(ctx) }},
		{"the install's owner", func(ctx context.Context, _ string) error { return s.claim(ctx) }},
		{"chaos-1", func(ctx context.Context, _ string) error { return s.join(ctx) }},
		{"what chaos-1 runs", func(ctx context.Context, _ string) error { return s.workloads(ctx) }},
	}
	for _, step := range steps {
		start := time.Now()
		if err := step.do(ctx, fakeBin); err != nil {
			return s, fmt.Errorf("%s: %w", step.what, err)
		}
		say("set up %s in %s", step.what, since(start))
	}
	if s.real {
		s.probe = newProbe(fmt.Sprintf("http://127.0.0.1:%d/", s.webPort))
		if err := s.probe.resume(ctx); err != nil {
			return s, err
		}
	}
	return s, nil
}

// env is what every process the stand starts runs with.
func (s *stand) env(more ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DOCKWELL_") && !strings.HasPrefix(kv, "DOCKER_HOST=") {
			out = append(out, kv)
		}
	}
	out = append(out, "DOCKWELL_TEST=1", "DOCKER_HOST=unix://"+s.socket)
	return append(out, more...)
}

func (s *stand) startEngine(ctx context.Context, fakeBin string) error {
	if fakeBin != "" {
		path, err := filepath.Abs(fakeBin)
		if err != nil {
			return err
		}
		s.fake = &proc{name: "the fake Engine", path: path, args: []string{filepath.Join(s.dir, "docker")}, log: filepath.Join(s.dir, "fakedocker.log")}
		if err := s.fake.start(os.Environ()); err != nil {
			return err
		}
		s.socket = filepath.Join(s.dir, "docker", "docker.sock")
		s.webImage, s.images = "chaos/web:1", [2]string{"chaos/one:slow", "chaos/two:slow"}
	} else {
		socket, err := docker.SocketFrom(os.Getenv)
		if err != nil {
			return err
		}
		s.socket, s.real = socket, true
		s.webImage, s.images = "nginx:1.27-alpine", [2]string{"nginx:1.28-alpine", "nginx:1.27-alpine"}
	}
	s.docker = docker.New(s.socket)
	if err := s.waitEngine(ctx, 30*time.Second); err != nil {
		return err
	}
	if s.real {
		for _, img := range []string{s.webImage, s.images[0]} {
			if err := s.docker.PullImage(ctx, img, nil); err != nil {
				return fmt.Errorf("can't pull %s: %w", img, err)
			}
		}
	}
	return nil
}

func (s *stand) waitEngine(ctx context.Context, within time.Duration) error {
	return wait(ctx, within, func() error { return s.docker.Ping(ctx) })
}

// makeDisk gives chaos-1's data folder a small disk of its own, which a
// scenario can fill: 16 MiB of tmpfs, as that needs no device. Without root it's
// a folder like any other, and the scenarios that fill it don't run.
func (s *stand) makeDisk(ctx context.Context) error {
	if err := os.MkdirAll(s.agentData, 0o700); err != nil {
		return err
	}
	if !s.root {
		return nil
	}
	opts := fmt.Sprintf("size=16m,mode=0700,uid=%d,gid=%d", os.Getuid(), os.Getgid())
	if err := sudo(ctx, "mount", "-t", "tmpfs", "-o", opts, "tmpfs", s.agentData); err != nil {
		return err
	}
	s.mounted = true
	return nil
}

func (s *stand) startControl(ctx context.Context) error {
	var err error
	if s.apiPort, err = freePort(ctx); err != nil {
		return err
	}
	if s.agentPort, err = freePort(ctx); err != nil {
		return err
	}
	if s.webPort, err = freePort(ctx); err != nil {
		return err
	}
	if s.sparePort, err = freePort(ctx); err != nil {
		return err
	}
	if s.proxy, err = newProxy(ctx, fmt.Sprintf("127.0.0.1:%d", s.agentPort)); err != nil {
		return err
	}
	s.control = &proc{
		name: "the control server", path: s.bin, log: filepath.Join(s.dir, "control.log"),
		args: []string{
			"run", "--listen", fmt.Sprintf("127.0.0.1:%d", s.apiPort), "--agent-listen", fmt.Sprintf("127.0.0.1:%d", s.agentPort),
			"--advertise-url", "https://" + s.proxy.addr(), "--data-dir", s.controlData, "--log-format", "json",
		},
	}
	return s.startControlAgain(ctx)
}

// startControlAgain starts the control server and waits for it to answer.
func (s *stand) startControlAgain(ctx context.Context) error {
	if err := s.control.start(s.env()); err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", s.apiPort)
	client := &http.Client{Timeout: 2 * time.Second}
	return wait(ctx, 60*time.Second, func() error {
		if !s.control.alive() {
			return stop(fmt.Errorf("the control server exited; its log is %s", s.control.log))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("healthz says %s", res.Status)
		}
		return nil
	})
}

// claim makes the install's owner with a software passkey, as the panel would,
// and lets local access make changes, as the Docker job does.
func (s *stand) claim(ctx context.Context) error {
	out, err := s.cli(ctx, "claim")
	if err != nil {
		return err
	}
	link := regexp.MustCompile(`http://\S+/claim#[A-Za-z0-9_-]+`).Find(out)
	if link == nil {
		return fmt.Errorf("dockwell claim printed no link: %s", out)
	}
	cmd := exec.CommandContext(ctx, "go", "run", "./scripts/claim", string(link), filepath.Join(s.dir, "cookie"), filepath.Join(s.dir, "passkey")) //nolint:gosec // the repository's own claim helper, with the stand's claim link
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("can't claim the install, from the repository's root: %w: %s", err, b)
	}
	_, err = s.cli(ctx, "login", "--local")
	return err
}

// join adds chaos-1 through the proxy, and starts its agent.
func (s *stand) join(ctx context.Context) error {
	out, err := s.cli(ctx, "token", "create")
	if err != nil {
		return err
	}
	m := regexp.MustCompile(`dockwell join (\S+)`).FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("dockwell token create printed no ticket: %s", out)
	}
	cmd := exec.CommandContext(ctx, s.agentBin, "join", "--data-dir", s.agentData, "--name", server, string(m[1])) //nolint:gosec // the binary under test, with the stand's join ticket
	cmd.Env = s.env()
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the agent build didn't join: %w: %s", err, b)
	}
	s.agent = &proc{
		name: "chaos-1's agent", path: s.agentBin, log: filepath.Join(s.dir, "agent.log"),
		args: []string{"run", "--data-dir", s.agentData, "--log-format", "json"},
	}
	if err := s.startAgent(); err != nil {
		return err
	}
	return wait(ctx, 60*time.Second, func() error {
		var list struct {
			Servers []serverJSON `json:"servers"`
		}
		if err := s.cliJSON(ctx, &list, "servers", "list"); err != nil {
			return err
		}
		for _, sv := range list.Servers {
			if sv.Name == server && sv.Connection == connected {
				s.serverID = sv.ID
				return nil
			}
		}
		return errors.New("chaos-1 isn't connected")
	})
}

// startAgent starts chaos-1's agent, which acts on its own 2 s after it loses
// its stream, and looks every 2 s then.
func (s *stand) startAgent() error {
	return s.agent.start(s.env("DOCKWELL_TEST_AWAY=2s"))
}

// workloads makes what chaos-1 runs, and has a person stop held.
func (s *stand) workloads(ctx context.Context) error {
	ports := func(p int) string {
		return fmt.Sprintf(`{"hostPort":%d,"containerPort":80,"hostIp":"127.0.0.1"}`, p)
	}
	slow := []string{"--app-image", s.images[0]}
	if s.real {
		// About 8 s to turn healthy, so a deploy of it is a change in flight.
		slow = append(slow, "--app-command", "sh", "--app-command", "-c", "--app-command", "sleep 8; exec nginx -g 'daemon off;'",
			"--app-healthcheck-http-port", "80", "--app-healthcheck-interval", "1s", "--app-healthcheck-timeout", "1s",
			"--app-healthcheck-retries", "3", "--app-healthcheck-start-period", "30s")
	}
	for _, svc := range []struct {
		name string
		args []string
	}{
		{"web", []string{"--app-image", s.webImage, "--app-ports", ports(s.webPort)}},
		{"slow", slow},
		{"spare", []string{"--app-image", s.webImage, "--app-ports", ports(s.sparePort)}},
		{"held", []string{"--app-image", s.webImage}},
	} {
		restart := "unless-stopped"
		if svc.name == "spare" {
			restart = "always" // a daemon restart must try it even after a stop
		}
		args := append([]string{"services", "create", svc.name, "--on", s.serverID, "--app-restart", restart, "--yes"}, svc.args...)
		var c changeJSON
		if err := s.cliJSON(ctx, &c, args...); err != nil {
			return err
		}
		if c.Change.State != applied {
			return fmt.Errorf("creating %s ended %s", svc.name, c.Change.State)
		}
	}
	var res struct {
		Resources []resourceJSON `json:"resources"`
	}
	if err := s.cliJSON(ctx, &res, "resources", "list"); err != nil {
		return err
	}
	for _, r := range res.Resources {
		if r.Resource.Kind == "service" && r.Resource.Name == "web" {
			s.scope, s.project = r.Resource.Scope, docker.Project(r.Resource.Scope)
		}
	}
	if s.project == "" {
		return errors.New("no service web among the resources")
	}
	held, err := s.one(ctx, "held")
	if err != nil {
		return err
	}
	return s.docker.StopContainer(ctx, held.ID, 2*time.Second)
}

// cli runs the full build's CLI on the control server, as its local access.
func (s *stand) cli(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, args...) //nolint:gosec // the binary under test, with the harness's CLI arguments
	cmd.Env = s.env("DOCKWELL_DATA_DIR="+s.controlData, "DOCKWELL_LOG_FORMAT=text")
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("dockwell %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errs.String()))
	}
	return out.Bytes(), nil
}

// cliJSON runs the CLI with --json, and decodes the last document it printed,
// as one that follows a change prints one as it goes.
func (s *stand) cliJSON(ctx context.Context, v any, args ...string) error {
	out, err := s.cli(ctx, append(args, "--json")...)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var last json.RawMessage
	for {
		var doc json.RawMessage
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("dockwell %s printed what isn't JSON: %w", strings.Join(args, " "), err)
		}
		last = doc
	}
	if last == nil {
		return fmt.Errorf("dockwell %s printed no JSON", strings.Join(args, " "))
	}
	return json.Unmarshal(last, v)
}

// close takes the stand down, and puts back what it changed on the machine.
func (s *stand) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if s.probe != nil {
		s.probe.close()
	}
	if s.real && s.root {
		// A scenario that failed may have left the clock ahead, or Docker down.
		if err := s.clock.back(ctx); err != nil {
			say("cleanup couldn't restore the clock: %v", err)
		}
		_ = sudo(ctx, "systemctl", "start", "docker.service")
	}
	for _, p := range []*proc{s.agent, s.control} {
		if p != nil {
			p.kill()
		}
	}
	if s.proxy != nil {
		s.proxy.close()
	}
	if s.docker != nil && s.project != "" {
		if list, err := s.docker.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + s.project}}); err == nil {
			for i := range list {
				_ = s.docker.RemoveContainer(ctx, list[i].ID, true)
			}
		}
	}
	if s.fake != nil {
		s.fake.kill()
	}
	if s.mounted {
		_ = sudo(ctx, "umount", s.agentData)
	}
	if !s.keep && os.Getenv("CHAOS_KEEP") == "" {
		_ = os.RemoveAll(s.dir) //nolint:gosec // the folder made by os.MkdirTemp for this stand
	}
}

// proc is a process the stand starts, kills and starts again.
type proc struct {
	name, path string
	args       []string
	log        string

	cmd  *exec.Cmd
	done chan struct{}
}

func (p *proc) start(env []string) error {
	f, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the process log in the stand's own temporary folder
	if err != nil {
		return err
	}
	cmd := exec.Command(p.path, p.args...) //nolint:gosec,noctx // the stand's own binaries, which it kills itself
	cmd.Env, cmd.Stdout, cmd.Stderr = env, f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return fmt.Errorf("can't start %s: %w", p.name, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		close(done)
	}()
	p.cmd, p.done = cmd, done
	return nil
}

func (p *proc) alive() bool {
	if p.cmd == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// kill stops it with no warning, as a power cut or the OOM killer would.
func (p *proc) kill() {
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
	p.cmd = nil
}

// stop asks it to stop, and kills it if it hasn't within 20 s.
func (p *proc) stop() {
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		p.cmd = nil
	case <-time.After(20 * time.Second):
		p.kill()
	}
}

func sudo(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n"}, args...)...) //nolint:gosec // the harness's fixed host-fault commands, explicitly enabled
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sudo %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(b))
	}
	return nil
}

func freePort(ctx context.Context) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// stopped is an error wait gives up on at once.
type stopped struct{ error }

func stop(err error) error { return stopped{err} }

// wait calls try until it returns nil, every 200 ms, for at most within, and
// says what it said last if it never did.
func wait(ctx context.Context, within time.Duration, try func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := try()
		if err == nil {
			return nil
		}
		var s stopped
		if errors.As(err, &s) {
			return s.error
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not within %s: %w", within, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func say(format string, args ...any) {
	fmt.Printf("chaos: "+format+"\n", args...)
}

func since(t time.Time) string {
	return strconv.FormatFloat(time.Since(t).Seconds(), 'f', 1, 64) + " s"
}
