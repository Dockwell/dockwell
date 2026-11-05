// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/loadtest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// A rollout under load: the whole stack, real Caddy on high ports, the route and
// service kinds, the manager, the rollout, and real HTTP servers standing where
// containers would, each on its own address on loopback, for what the fake Docker
// Engine starts and stops. A container's start begins its server after a while,
// as an app takes a moment to come up, and its stop is what SIGTERM does to a
// server that handles it: it stops listening, and finishes what it has within its
// grace. Only Docker itself is left out, which make docker-e2e has.

// fleetOf holds the servers standing in for containers.
type fleetOf struct {
	t       *testing.T
	engine  *dockertest.Engine
	port    int
	startIn time.Duration
	// grace is how long a stopping server has for what it is still doing, as
	// docker stop's timeout.
	grace time.Duration

	mu      sync.Mutex
	running map[string]*appServer // by container ID
	started atomic.Int64
}

// appServer is one container's app.
type appServer struct {
	*loadtest.App
	name string
	srv  *http.Server
	mu   sync.Mutex
	lis  net.Listener
}

// intercept stands the servers in for what the fake engine starts and stops.
func (f *fleetOf) intercept(r *http.Request) error {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		return nil
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[len(parts)-3] != "containers" && parts[len(parts)-2] != "containers" {
		return nil
	}
	switch {
	case r.Method == http.MethodPost && parts[len(parts)-1] == "start":
		f.start(parts[len(parts)-2])
	case r.Method == http.MethodPost && parts[len(parts)-1] == "stop":
		f.stop(parts[len(parts)-2])
	case r.Method == http.MethodDelete:
		f.stop(parts[len(parts)-1])
	}
	return nil
}

func (f *fleetOf) start(id string) {
	c := f.engine.Container(id)
	if c == nil {
		return
	}
	ep := c.NetworkSettings.Networks[docker.IngressNetwork]
	if ep == nil || ep.IPAMConfig == nil {
		f.t.Errorf("%s started with no address on the ingress network", c.Name)
		return
	}
	name, broken := strings.TrimPrefix(c.Name, "/"), c.Config.Image == "never:1"
	a := &appServer{name: name, App: &loadtest.App{Name: name, Ready: func() (int, string) {
		if broken {
			return http.StatusServiceUnavailable, "not ready"
		}
		return http.StatusOK, "ok"
	}}}
	a.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: a.Handler()}
	f.mu.Lock()
	f.running[id] = a
	f.mu.Unlock()
	f.started.Add(1)
	address := fmt.Sprintf("%s:%d", ep.IPAMConfig.IPv4Address, f.port)
	go func() {
		// An app takes a moment to come up, and nothing answers until it has.
		time.Sleep(f.startIn)
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", address)
		if err != nil {
			f.t.Errorf("%s can't listen on %s: %v", a.name, address, err)
			return
		}
		a.mu.Lock()
		a.lis = l
		a.mu.Unlock()
		_ = a.srv.Serve(l)
	}()
}

// stop is SIGTERM to a server that handles it, then the end of the grace: it
// stops listening at once, finishes what it can, and what is left ends with it.
func (f *fleetOf) stop(id string) {
	f.mu.Lock()
	a := f.running[id]
	delete(f.running, id)
	f.mu.Unlock()
	if a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.grace)
	defer cancel()
	_ = a.srv.Shutdown(ctx)
	_ = a.srv.Close()
	a.CloseWebSockets()
}

func (f *fleetOf) stopAll() {
	f.mu.Lock()
	ids := make([]string, 0, len(f.running))
	for id := range f.running {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	for _, id := range ids {
		f.stop(id)
	}
}

type loadStack struct {
	*wholeStack
	fleet *fleetOf
	real  *realCaddy
	apply resource.Local
	ref   resource.Ref
	drain string
	check *resourcev1.Healthcheck
}

func newLoadStack(t *testing.T, startIn, grace time.Duration, drain string, check func(port int) *resourcev1.Healthcheck) *loadStack {
	t.Helper()
	s := newWholeStack(t, nil)
	port := freePort(t)
	fleet := &fleetOf{t: t, engine: s.engine, port: port, startIn: startIn, grace: grace, running: map[string]*appServer{}}
	s.engine.Registry["app:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	s.engine.Registry["never:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	s.engine.Intercept(fleet.intercept)
	t.Cleanup(fleet.stopAll)
	rt := &container.Docker{Client: s.engine.Client(), Settle: 10 * time.Millisecond, Poll: 10 * time.Millisecond, Soak: 200 * time.Millisecond, Ingress: s.mgr}
	drivers := container.Driver{Runtime: rt}
	ls := &loadStack{
		wholeStack: s, fleet: fleet, real: &realCaddy{Caddy: s.caddy, proc: s.proc, http: s.http, tls: s.tls},
		apply: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: drivers})},
		ref:   resource.Ref{Kind: service.Kind, Scope: s.env, Name: "web"},
		drain: drain,
	}
	if check != nil {
		ls.check = check(port)
	}
	return ls
}

func (s *loadStack) spec(image string, deploy int) *resourcev1.Service {
	c := &resourcev1.Container{
		Image: image, Env: map[string]string{"DEPLOY": strconv.Itoa(deploy)}, Rollout: &resourcev1.Rollout{Drain: s.drain},
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED, Healthcheck: s.check,
	}
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
}

func (s *loadStack) deploy(ctx context.Context, spec *resourcev1.Service) (resource.Step, error) {
	steps, err := s.apply.Plan(ctx, []resource.Desired{{Ref: s.ref, Spec: spec}})
	if err != nil {
		return resource.Step{}, err
	}
	return steps[0], s.apply.Apply(ctx, steps[0], spec)
}

func (s *loadStack) up(t *testing.T) {
	t.Helper()
	// The route first, so a server has an address to stand on from the start: a
	// real container takes one when its first route comes, with no gap.
	s.change(t, "www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: uint32(s.fleet.port)})
	if _, err := s.deploy(t.Context(), s.spec("app:1", 0)); err != nil {
		t.Fatalf("the first deploy: %v", err)
	}
	res := s.real.waitServing(t, "app.example.net", "/who")
	_ = res.Body.Close()
}

// target is the load's way to this stack's Caddy.
func (s *loadStack) target() loadtest.Target {
	return loadtest.Target{
		Domain: "app.example.net",
		Client: func() *http.Client { return s.real.client("app.example.net") },
		ClientHTTP1: func() *http.Client {
			c := s.real.client("app.example.net")
			tr := c.Transport.(*http.Transport)
			tr.ForceAttemptHTTP2 = false
			tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
			return c
		},
		Dial: func(ctx context.Context) (net.Conn, error) {
			d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, ServerName: "app.example.net", NextProtos: []string{"http/1.1"}}}
			return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", s.tls))
		},
	}
}

var instanceName = regexp.MustCompile(`-web-(\d+)$`)

// The issue's test: sustained requests of every kind through rollouts, and a
// rollout that fails its check, counting every failure.
func TestARolloutDropsNoRequestOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 150*time.Millisecond, 10*time.Second, "5s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}, Interval: "1s", Timeout: "1s", Retries: 3}
	})
	s.up(t)
	l := loadtest.NewTally()
	stop := make(chan struct{})
	wg := s.target().Run(t.Context(), l, stop)
	time.Sleep(500 * time.Millisecond)
	const rollouts = 6
	start := time.Now()
	for i := 1; i <= rollouts; i++ {
		step, err := s.deploy(t.Context(), s.spec("app:1", i))
		if err != nil {
			t.Fatalf("rollout %d: %v", i, err)
		}
		if step.Downtime != 0 || step.Rollout == nil || !step.Rollout.Rolling {
			t.Errorf("rollout %d's step = %+v", i, step)
		}
		time.Sleep(200 * time.Millisecond)
	}
	took := time.Since(start)
	// One that never turns healthy: what runs goes on serving through it, and the
	// rollback that follows.
	bad := s.spec("never:1", rollouts+1)
	if _, err := s.deploy(t.Context(), bad); err == nil || !strings.Contains(err.Error(), "fails its health check") || !strings.Contains(err.Error(), "503") {
		t.Errorf("deploying what never turns healthy = %v", err)
	}
	if _, err := s.deploy(t.Context(), s.spec("app:1", rollouts)); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()

	t.Logf("%d rollouts in %s, then a failed one and its rollback. %s", rollouts, took.Round(time.Millisecond), l)
	for _, said := range l.Errors() {
		t.Errorf("a request failed: %s (%d times)", said.What, said.Count)
	}
	if n := l.Failures(); n != 0 {
		t.Errorf("%d requests failed: %s", n, l)
	}
	// A rollout reloads nothing, so nothing is even reset on its way in.
	if n := l.Resets(); n != 0 {
		t.Errorf("%d connections were reset on the way in, with no reload to cut them: %s", n, l)
	}
	for _, kind := range loadtest.Kinds {
		if l.Passed(kind) < 5 {
			t.Errorf("only %d of %s requests ran, too few to mean anything", l.Passed(kind), kind)
		}
	}
	if l.Passed("short") < 500 {
		t.Errorf("only %d short requests ran, too few to mean anything", l.Passed("short"))
	}
	// Each instance served: it wasn't all the first, or all the last.
	served := map[string]bool{}
	for name := range l.ServedBy() {
		if m := instanceName.FindStringSubmatch(name); m != nil {
			served[m[1]] = true
		}
	}
	if len(served) < rollouts/2 {
		t.Errorf("only %d instances served requests, of %d: %v", len(served), rollouts+1, served)
	}
	if got := s.engine.Instances(docker.Project(s.env), "web"); len(got) != 1 {
		t.Errorf("Docker has %v", got)
	}
}

// No protocol loses a request to a rollout, and none is sent twice: a POST is never
// tried again by a client, and a browser's first choice is HTTP/3 once Caddy's
// Alt-Svc says so. A reload closes a kept HTTP/1.1 connection with nothing to tell the
// client first, so this holds because a rollout doesn't reload.
func TestNoProtocolLosesARequestToARolloutOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 150*time.Millisecond, 10*time.Second, "5s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}, Interval: "1s", Timeout: "1s", Retries: 3}
	})
	s.up(t)
	const domain = "app.example.net"
	addr := fmt.Sprintf("127.0.0.1:%d", s.tls)
	http3Client := func() *http.Client {
		return &http.Client{Timeout: 20 * time.Second, Transport: &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: domain, NextProtos: []string{http3.NextProtoH3}}}}
	}
	target := s.target()
	l := loadtest.NewTally()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	run := func(kind, method, url string, client func() *http.Client) {
		for range 8 {
			wg.Go(func() {
				c := client()
				for {
					select {
					case <-stop:
						return
					default:
					}
					var body io.Reader = http.NoBody
					if method == http.MethodPost {
						body = strings.NewReader(`{"order":1}`)
					}
					req, err := http.NewRequestWithContext(t.Context(), method, url, body)
					if err != nil {
						l.Fail(kind, err.Error())
						return
					}
					req.Host = domain
					res, err := c.Do(req)
					if err != nil {
						l.Fail(kind, err.Error())
						time.Sleep(5 * time.Millisecond)
						continue
					}
					b, err := io.ReadAll(res.Body)
					_ = res.Body.Close()
					switch {
					case err != nil:
						l.Fail(kind, "reading: "+err.Error())
					case res.StatusCode != http.StatusOK:
						l.Fail(kind, fmt.Sprintf("%d %s", res.StatusCode, strings.TrimSpace(string(b))))
					default:
						l.Pass(kind)
					}
					time.Sleep(2 * time.Millisecond)
				}
			})
		}
	}
	run("post-http1", http.MethodPost, "https://"+domain+"/who", target.ClientHTTP1)
	run("post-http2", http.MethodPost, "https://"+domain+"/who", target.Client)
	run("get-http3", http.MethodGet, "https://"+addr+"/who", http3Client)
	run("post-http3", http.MethodPost, "https://"+addr+"/who", http3Client)
	time.Sleep(time.Second)
	before := reloadsIn(t, s.proc.dir)
	const rollouts = 5
	for i := 1; i <= rollouts; i++ {
		if _, err := s.deploy(t.Context(), s.spec("app:1", i)); err != nil {
			t.Fatalf("rollout %d: %v", i, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	t.Logf("across %d rollouts, with %d reloads: post-http1 %d, post-http2 %d, get-http3 %d, post-http3 %d passed", rollouts, reloadsIn(t, s.proc.dir)-before,
		l.Passed("post-http1"), l.Passed("post-http2"), l.Passed("get-http3"), l.Passed("post-http3"))
	for _, said := range l.Errors() {
		t.Errorf("a request failed: %s (%d times)", said.What, said.Count)
	}
	if got := reloadsIn(t, s.proc.dir) - before; got != 0 {
		t.Errorf("%d rollouts reloaded Caddy %d times", rollouts, got)
	}
	for _, kind := range []string{"post-http1", "post-http2", "get-http3", "post-http3"} {
		if l.Passed(kind) < 100 {
			t.Errorf("only %d of %s ran, too few to mean anything", l.Passed(kind), kind)
		}
	}
}

// A rollout needs nothing of Caddy, as moving traffic is writing a file: with Caddy
// down, as with another proxy on its ports, a deploy goes on, and Caddy finds the new
// container when it is back. Nothing here says Caddy serves.
func TestARolloutNeedsNoCaddyOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 0, time.Second, "1s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}, Interval: "1s", Timeout: "1s", Retries: 3}
	})
	s.up(t)
	s.proc.stop(syscall.SIGTERM)
	if _, err := s.deploy(t.Context(), s.spec("app:1", 1)); err != nil {
		t.Fatalf("a rollout with Caddy down = %v", err)
	}
	if err := s.proc.Run(t.Context(), filepath.Join(s.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	res := s.real.waitServing(t, "app.example.net", "/who")
	defer res.Body.Close()
	if got, _ := io.ReadAll(res.Body); !strings.Contains(string(got), "-web-2") {
		t.Errorf("when Caddy came back, the route answers %s", got)
	}
}

// An app that doesn't handle SIGTERM dies as it is stopped, so what it has in
// flight lives or dies by the drain: it has to wait for each request, however few
// are left, and even one a reload's old config sent late.
func TestADrainProtectsAnAppThatDoesntHandleSIGTERMOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 0, 0, "10s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}}
	})
	s.up(t)
	l := loadtest.NewTally()
	stop := make(chan struct{})
	wg := s.target().Run(t.Context(), l, stop)
	time.Sleep(300 * time.Millisecond)
	for i := 1; i <= 3; i++ {
		if _, err := s.deploy(t.Context(), s.spec("app:1", i)); err != nil {
			t.Fatalf("rollout %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	t.Log(l)
	for _, said := range l.Errors() {
		t.Errorf("a request failed: %s (%d times)", said.What, said.Count)
	}
	if l.Passed("slow") == 0 || l.Passed("stream") == 0 {
		t.Errorf("too few long requests ran to mean anything: %s", l)
	}
}

// A long-lived connection is the one thing a rollout does end: it outlasts its
// drain, and goes with the container, at the end of the drain and no sooner. A
// new one opened then finds the new instance.
func TestAWebSocketLastsUntilItsDrainEndsOnRealCaddy(t *testing.T) {
	const drain = 2 * time.Second
	s := newLoadStack(t, 0, 5*time.Second, drain.String(), func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}}
	})
	s.up(t)
	opened := time.Now()
	ended := make(chan string, 1)
	go func() { ended <- s.target().WebSocket(t.Context(), time.Hour) }()
	time.Sleep(300 * time.Millisecond)
	rolled := time.Now()
	if _, err := s.deploy(t.Context(), s.spec("app:1", 1)); err != nil {
		t.Fatalf("the rollout: %v", err)
	}
	select {
	case why := <-ended:
		// It lasted through the drain, as the drain waited on it.
		if lasted := time.Since(rolled); lasted < drain {
			t.Errorf("it ended %s into the rollout, before its %s drain: %s", lasted, drain, why)
		}
		if why == "" {
			t.Error("it never ended")
		}
		t.Logf("a WebSocket opened %s before the rollout ended %s into it, with a drain of %s: %s", rolled.Sub(opened).Round(time.Millisecond), time.Since(rolled).Round(time.Millisecond), drain, why)
	case <-time.After(30 * time.Second):
		t.Fatal("the WebSocket went on after its container went")
	}
	if why := s.target().WebSocket(t.Context(), 200*time.Millisecond); why != "" {
		t.Errorf("a new WebSocket after the rollout: %s", why)
	}
}

// An app that takes its time, with no check at all, gets no traffic until its
// route's port takes a connection, which is the one thing the agent can know of
// it. "It runs" alone would send the first requests to nothing.
func TestAnAppWithNoCheckGetsNoTrafficUntilItListensOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 1500*time.Millisecond, 5*time.Second, "2s", nil)
	s.up(t)
	l := loadtest.NewTally()
	stop := make(chan struct{})
	wg := s.target().Run(t.Context(), l, stop)
	time.Sleep(200 * time.Millisecond)
	for i := 1; i <= 2; i++ {
		if _, err := s.deploy(t.Context(), s.spec("app:1", i)); err != nil {
			t.Fatalf("rollout %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	t.Log(l)
	for _, said := range l.Errors() {
		t.Errorf("a request failed: %s", said.What)
	}
}

// What the proxy and table say while a route changes under a rollout: a route's
// edit doesn't send traffic to a new instance that hasn't passed its check.
func TestARouteEditDuringARolloutKeepsToWhatServesOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 0, 5*time.Second, "2s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}, Interval: "1s", Retries: 2}
	})
	s.up(t)
	// The new one fails its check for as long as it takes to edit the route.
	done := make(chan error, 1)
	go func() {
		_, err := s.deploy(context.Background(), s.spec("never:1", 1))
		done <- err
	}()
	project := docker.Project(s.env)
	deadline := time.Now().Add(10 * time.Second)
	for len(s.engine.Instances(project, "web")) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.change(t, "api", &resourcev1.Route{Domain: "app.example.net", Path: "/api", Service: "web", Port: uint32(s.fleet.port)})
	routes, err := s.caddy.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if len(r.Upstreams) != 1 {
			t.Errorf("mid-rollout the route %s sends to %v, want what already serves alone", r.ID, r.Upstreams)
		}
	}
	if err := <-done; err == nil {
		t.Error("the rollout of what never turns healthy passed")
	}
	if _, err := s.deploy(t.Context(), s.spec("app:1", 0)); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	if got := s.body(t, "/who"); !strings.Contains(got, "-web-1") {
		t.Errorf("after rolling back, it answers %s", got)
	}
}

// Two routes of one service move in one reload: each reload costs what the old
// socket's queue holds, reloads close together are what the next one catches
// clients coming back from, and each is one more chance of a connection left on
// an old config.
func TestARolloutReloadsNothingOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 0, 2*time.Second, "1s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}}
	})
	s.up(t)
	s.change(t, "api", &resourcev1.Route{Domain: "app.example.net", Path: "/api", Service: "web", Port: uint32(s.fleet.port)})
	before := reloadsIn(t, s.proc.dir)
	if _, err := s.deploy(t.Context(), s.spec("app:1", 1)); err != nil {
		t.Fatal(err)
	}
	// Each route dials through its file, so no site on the server pays for this.
	if got := reloadsIn(t, s.proc.dir) - before; got != 0 {
		t.Errorf("a rollout of a service with two routes reloaded Caddy %d times, want none", got)
	}
	if got := s.body(t, "/who"); !strings.Contains(got, "-web-2") {
		t.Errorf("after it, the route answers %s", got)
	}
}

// reloadsIn counts the configs Caddy has loaded, as its log says.
func reloadsIn(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "caddy.out"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "autosaved config")
}

// A config knows it is out of date once its file is gone, and tells a connection
// it still has to leave, on the next response: what Caddy can't tell one that a
// reload caught mid-handshake. This stands the file going for the race, which
// can't be had to order, on a connection that is otherwise left alone.
func TestAnOldConfigSendsItsConnectionsAwayOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 0, 2*time.Second, "1s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}}
	})
	s.up(t)
	c := s.real.client("app.example.net")
	var reused atomic.Int64
	var fresh atomic.Int64
	get := func() string {
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			if i.Reused {
				reused.Add(1)
			} else {
				fresh.Add(1)
			}
		}}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, "https://app.example.net/who", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Errorf("a request answered %d: %s", res.StatusCode, b)
		}
		return res.Proto
	}
	for range 5 {
		if proto := get(); proto != "HTTP/2.0" {
			t.Fatalf("the client speaks %s, and the race is HTTP/2's", proto)
		}
	}
	if fresh.Load() != 1 || reused.Load() != 4 {
		t.Fatalf("while its config is current, a connection is opened %d times and reused %d, want it kept", fresh.Load(), reused.Load())
	}
	files, err := filepath.Glob(filepath.Join(s.proc.dir, "run", generationPrefix+"*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the generation files are %v, %v, want one", files, err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	fresh.Store(0)
	get() // it answers, and says to go
	deadline := time.Now().Add(5 * time.Second)
	for fresh.Load() == 0 && time.Now().Before(deadline) {
		get()
		time.Sleep(20 * time.Millisecond)
	}
	if fresh.Load() == 0 {
		t.Error("the connection stayed on, though its config's file is gone")
	}
	// A reload brings back a file, and so what a connection is for.
	s.change(t, "www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: uint32(s.fleet.port), Path: ""})
	if err := s.caddy.PutRoutes(t.Context(), []ingress.Route{{ID: "dockwell.other", Domain: "other.example.net", Upstreams: []string{"127.0.0.1:1"}}}); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(s.proc.dir, "run", generationPrefix+"*")); len(files) != 1 {
		t.Errorf("after a reload the generation files are %v, want one", files)
	}
	c2 := s.real.client("app.example.net")
	c = c2
	fresh.Store(0)
	reused.Store(0)
	for range 5 {
		get()
	}
	if fresh.Load() != 1 {
		t.Errorf("after a reload a connection was opened %d times for 5 requests, want it kept", fresh.Load())
	}
}

// What holds the file apart from the routes Dockwell reads: it isn't one, and
// reads as no drift.
func TestTheGenerationRouteIsntARouteOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "a")))
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].ID != "dockwell.env.www" || routes[0].Edited {
		t.Errorf("Routes = %+v, want the one route, not edited", routes)
	}
	servers, _, _, err := c.readServers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var server struct {
		Routes []json.RawMessage `json:"routes"`
	}
	if err := json.Unmarshal(servers[serverName], &server); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(server.Routes))
	for i := range server.Routes {
		ids[i] = routeID(server.Routes[i])
	}
	// The generation's first, and last is what answers for a path the route doesn't cover.
	if want := []string{generationRouteID, "dockwell.env.www", secureNoneID}; !slices.Equal(ids, want) {
		t.Errorf("Caddy has the routes %v, want %v", ids, want)
	}
}

// Each server has the generation route, as a config on either can outlive a reload,
// and it doesn't make either listen: only a route that names a host makes HTTPS, so a
// server with plain routes alone holds nothing on its HTTPS port, and can sit beside
// another proxy that has it.
func TestAGenerationMakesNothingListenOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", backend(t, "a")))
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("a server with plain routes alone holds its HTTPS port, TCP %t, UDP %t", tcp, udp)
	}
	cfg := configOf(t, c)
	for _, id := range []string{generationRouteID, plainGenerationRouteID} {
		if !strings.Contains(cfg, fmt.Sprintf("%q", id)) {
			t.Errorf("the config has no %s: %s", id, cfg)
		}
	}
	put(t, c, aRoute("dockwell.env.sure", "sure.example.net", "", backend(t, "b")))
	if tcp, _ := bound(t, c.tls); !tcp {
		t.Error("a route with a certificate didn't make HTTPS listen")
	}
	if err := c.DeleteRoute(t.Context(), "dockwell.env.sure"); err != nil {
		t.Fatal(err)
	}
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("with that route gone, it still holds its HTTPS port, TCP %t, UDP %t", tcp, udp)
	}
}

// A reload leaves only the new config's file, so every config before it reads as out
// of date from then on, which is how a connection left on one hears to go.
func TestAReloadTakesTheOldConfigsFileOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	files := func() []string {
		t.Helper()
		got, err := filepath.Glob(filepath.Join(c.generationDir(), generationPrefix+"*"))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", backend(t, "a")))
	before := files()
	if len(before) != 1 {
		t.Fatalf("the generation files are %v, want one", before)
	}
	put(t, c, plainRoute("dockwell.env.api", "api.example.net", backend(t, "b")))
	after := files()
	if len(after) != 1 || after[0] == before[0] {
		t.Errorf("after a reload the generation files are %v, and were %v, want only the new config's", after, before)
	}
}

// A config on the plain HTTP server knows it is old too, and tells a connection it
// still has to leave.
func TestAnOldPlainConfigSendsItsConnectionsAwayOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", backend(t, "a")))
	closes := func() bool {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/who", c.http), http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "app.example.net"
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if _, err := io.Copy(io.Discard, res.Body); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("it answered %d", res.StatusCode)
		}
		return res.Close
	}
	if closes() {
		t.Error("a current config sent its connection away")
	}
	files, err := filepath.Glob(filepath.Join(c.proc.dir, "run", generationPrefix+"*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the generation files are %v, %v, want one", files, err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if !closes() {
		t.Error("a config whose file has gone kept its connection")
	}
}

// A rollout of a service whose routes take no certificate: what they dial moves with
// nothing reloading, they stay on plain HTTP, nothing listens on HTTPS, and who each
// answers doesn't widen. A server that trusts a proxy changes what its other routes do
// with a header, which isn't a change for a rollout to reload over.
func TestARouteWithNoCertificateGoesThroughARolloutOnRealCaddy(t *testing.T) {
	s := newLoadStack(t, 0, 2*time.Second, "1s", func(port int) *resourcev1.Healthcheck {
		return &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: uint32(port), Path: "/healthz"}}
	})
	port := uint32(s.fleet.port)
	s.change(t, "www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: port, Tls: ingress.TLSOff})
	s.change(t, "far", &resourcev1.Route{Domain: "far.example.net", Service: "web", Port: port, Tls: ingress.TLSOff, TrustedProxies: []string{"10.99.0.0/16"}})
	if _, err := s.deploy(t.Context(), s.spec("app:1", 0)); err != nil {
		t.Fatalf("the first deploy: %v", err)
	}
	answers := func(want string) {
		t.Helper()
		var last plainAnswer
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			last = s.real.plainGet(t, "app.example.net", "/who", nil)
			if last.status == http.StatusOK && strings.Contains(string(last.body), want) {
				return
			}
		}
		t.Fatalf("it answers %d: %s, want %s", last.status, last.body, want)
	}
	answers("-web-1")
	refused := func(when string) {
		t.Helper()
		if a := s.real.plainGet(t, "far.example.net", "/who", nil); a.status == http.StatusOK {
			t.Errorf("%s, a route limited to a proxy answered someone else: %s", when, a.body)
		}
	}
	refused("before the rollout")
	before := reloadsIn(t, s.proc.dir)
	if _, err := s.deploy(t.Context(), s.spec("app:1", 1)); err != nil {
		t.Fatal(err)
	}
	if got := reloadsIn(t, s.proc.dir) - before; got != 0 {
		t.Errorf("the rollout reloaded Caddy %d times, want none", got)
	}
	answers("-web-2")
	refused("after the rollout")
	if tcp, udp := bound(t, s.tls); tcp || udp {
		t.Errorf("routes with no certificate left it holding its HTTPS port, TCP %t, UDP %t", tcp, udp)
	}
	routes, err := s.real.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Edited || !r.Plain {
			t.Errorf("after the rollout, %s reads %+v", r.ID, r)
		}
	}
}
