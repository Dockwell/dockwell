// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	routekind "github.com/dockwell/dockwell/internal/resource/route"
)

// wholeStack is the route kind, the manager and real Caddy on high ports, with
// a fake Docker Engine alone left out. The fake picks a subnet on loopback, so
// a service's static address is one a server in this test can listen on, as a
// container would on the ingress network.
type wholeStack struct {
	agent  resource.Local
	mgr    *ingress.Manager
	caddy  *Caddy
	proc   *process
	engine *dockertest.Engine
	env    string
	tls    int
	http   int
}

func newWholeStack(t *testing.T, panel *ingress.Panel) *wholeStack {
	t.Helper()
	bin := needCaddy(t)
	dir := shortDir(t)
	e := dockertest.New(t)
	e.PickSubnet = "127.77.0.0/16"
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t), issuerForTest: map[string]any{"module": "internal"}}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	m := &ingress.Manager{
		Client: e.Client(), Proxy: c, Dir: dir, Panel: panel,
		Ports: []ingress.Port{{Number: c.HTTPPort}, {Number: c.HTTPSPort}, {Number: c.HTTPSPort, UDP: true}},
	}
	return &wholeStack{
		agent: resource.Local{Drivers: resource.NewRegistry(&routekind.Driver{Ingress: m})},
		mgr:   m, caddy: c, proc: proc, engine: e, env: "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", tls: c.HTTPSPort, http: c.HTTPPort,
	}
}

// container puts a running container of a service on the fake engine, with the
// labels Dockwell gives one, which is how the ingress finds it.
func (s *wholeStack) container(name string) *docker.Container {
	project := docker.Project(s.env)
	return s.engine.AddContainer(docker.Container{
		Name: "/" + docker.ContainerName(project, name),
		Config: docker.Config{Labels: map[string]string{
			docker.LabelProject: project, docker.LabelService: name, docker.LabelNumber: "1", docker.LabelOneoff: "False",
		}},
		State: docker.State{Running: true, Status: "running"},
	})
}

// addressOf is where the manager put a service's container.
func (s *wholeStack) addressOf(t *testing.T, name string) string {
	t.Helper()
	c := s.engine.Container(docker.ContainerName(docker.Project(s.env), name))
	ep := c.NetworkSettings.Networks[docker.IngressNetwork]
	if ep == nil || ep.IPAMConfig == nil {
		t.Fatalf("%s isn't on the ingress network: %+v", name, c.NetworkSettings.Networks)
	}
	return ep.IPAMConfig.IPv4Address
}

// change plans and applies one route, as the agent does: its plan's checks,
// and what it reads back after.
func (s *wholeStack) change(t *testing.T, name string, spec *resourcev1.Route) resource.Step {
	t.Helper()
	ref := resource.Ref{Kind: routekind.Kind, Scope: s.env, Name: name}
	var want proto.Message
	if spec != nil {
		want = spec
	}
	steps, err := s.agent.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: want}})
	if err != nil {
		t.Fatalf("planning %s: %v", name, err)
	}
	if err := s.agent.Apply(t.Context(), steps[0], want); err != nil {
		out, _ := os.ReadFile(filepath.Join(s.proc.dir, "caddy.out"))
		t.Fatalf("applying %s: %v\n%s", name, err, tailOf(out))
	}
	return steps[0]
}

func tailOf(b []byte) string {
	if len(b) > 2000 {
		b = b[len(b)-2000:]
	}
	return string(b)
}

// serve answers on a service's address, as its container would.
func (s *wholeStack) serve(t *testing.T, name, addr string, port int) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf("%s:%d", addr, port))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s saw %s", name, r.URL.Path)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func (s *wholeStack) body(t *testing.T, path string) string {
	t.Helper()
	r := &realCaddy{Caddy: s.caddy, tls: s.tls}
	res := r.waitServing(t, "app.example.net", path)
	defer res.Body.Close()
	b := make([]byte, 256)
	n, _ := res.Body.Read(b)
	return string(b[:n])
}

// A route from its plan to a request and back: the first starts Caddy, one
// that reads back unchanged has nothing to do, the last stops it and frees its
// ports, and undoing that serves from the certificate that stayed.
func TestARouteFromPlanToRequestAndBackOnRealCaddy(t *testing.T) {
	s := newWholeStack(t, nil)
	web := s.container("web")
	webPort, apiPort, nextPort := freePort(t), freePort(t), freePort(t)
	www := &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: uint32(webPort)}

	if ingress.Active(s.mgr.Dir) {
		t.Fatal("no route, and the ports are with Caddy")
	}
	step := s.change(t, "www", www)
	if step.Ingress == nil || step.Ingress.Change != resource.IngressStarts || step.Action != resource.ActionCreate {
		t.Errorf("the first route's step = %+v", step)
	}
	if step.Downtime != 0 {
		t.Errorf("a new route has %s of downtime", step.Downtime)
	}
	if got := s.engine.Container(docker.ContainerName(docker.Project(s.env), "web")); got.ID != web.ID {
		t.Error("adding a route replaced the service's container")
	}
	addr := s.addressOf(t, "web")
	s.serve(t, "web", addr, webPort)
	if got := s.body(t, "/x"); got != "web saw /x" {
		t.Errorf("the route answers %q", got)
	}

	// Read back, it is what was asked for: no step, and no drift.
	ref := resource.Ref{Kind: routekind.Kind, Scope: s.env, Name: "www"}
	steps, err := s.agent.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: www}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionNone {
		t.Errorf("a route just made has a step: %+v", steps[0])
	}

	// A path to another service, and it goes first.
	s.container("api")
	s.change(t, "api", &resourcev1.Route{Domain: "app.example.net", Path: "/api", Service: "api", Port: uint32(apiPort)})
	s.serve(t, "api", s.addressOf(t, "api"), apiPort)
	if s.addressOf(t, "api") == addr {
		t.Errorf("two services share %s", addr)
	}
	if got := s.body(t, "/api/users"); got != "api saw /api/users" {
		t.Errorf("/api/users = %q", got)
	}
	if got := s.body(t, "/apis"); got != "web saw /apis" {
		t.Errorf("/apis = %q", got)
	}

	// A hand edit reads as drift.
	if _, _, err := s.caddy.call(t.Context(), http.MethodPatch, "/id/"+ingress.RouteID(ref)+"/handle/0/upstreams", []byte(`[{"dial":"203.0.113.9:80"}]`), nil); err != nil {
		t.Fatal(err)
	}
	steps, err = s.agent.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: www}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionUpdate || len(steps[0].Changes) == 0 {
		t.Errorf("a hand edit has the step %+v, want it read as a difference", steps[0])
	}
	s.change(t, "www", www)
	if got := s.body(t, "/x"); got != "web saw /x" {
		t.Errorf("after putting it back, the route answers %q", got)
	}

	// Removing one of two leaves the other serving, and frees what it used.
	s.change(t, "api", nil)
	if got := s.body(t, "/api/users"); got != "web saw /api/users" {
		t.Errorf("after removing api, /api/users = %q", got)
	}
	if _, on := s.engine.Container(docker.ContainerName(docker.Project(s.env), "api")).NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Error("a service with no routes stayed on the ingress network")
	}

	// The last route: Caddy stops, and its ports and the network are free.
	cert, _ := filepath.Glob(filepath.Join(s.mgr.Dir, "data", "caddy", "certificates", "*", "app.example.net", "app.example.net.crt"))
	if len(cert) != 1 {
		t.Fatalf("no certificate on disk: %v", cert)
	}
	last := s.change(t, "www", nil)
	if last.Ingress == nil || last.Ingress.Change != resource.IngressStops {
		t.Errorf("the last route's step = %+v", last)
	}
	st, _ := s.caddy.Status(t.Context())
	if st.Running {
		t.Error("Caddy still runs")
	}
	for _, port := range []int{s.http, s.tls} {
		if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			_ = conn.Close()
			t.Errorf("port %d still answers", port)
		}
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("the ingress network stayed")
	}
	if _, err := os.Stat(cert[0]); err != nil {
		t.Errorf("the certificate went: %v", err)
	}

	// Undo: the same route, from the same certificate.
	again := s.change(t, "www", www)
	if again.Ingress == nil || again.Ingress.Change != resource.IngressStarts {
		t.Errorf("a route after the last = %+v", again)
	}
	s.serve(t, "web", s.addressOf(t, "web"), nextPort)
	if err := s.agent.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionUpdate}},
		&resourcev1.Route{Domain: "app.example.net", Service: "web", Port: uint32(nextPort)}); err != nil {
		t.Fatal(err)
	}
	if got := s.body(t, "/y"); got != "web saw /y" {
		t.Errorf("after coming back, the route answers %q", got)
	}
}

// Decision 2 of ADR 0024, for real: a real process on a port the proxy needs.
func TestAPortInUseIsRefusedByNameOnRealCaddy(t *testing.T) {
	s := newWholeStack(t, nil)
	s.container("web")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf(":%d", s.http))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ref := resource.Ref{Kind: routekind.Kind, Scope: s.env, Name: "www"}
	_, err = s.agent.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 80}}})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("Port %d is held", s.http)) || !strings.Contains(err.Error(), "point that proxy at it") {
		t.Errorf("planning with the port taken = %v, want it refused, with the fix", err)
	}
	if st, _ := s.caddy.Status(t.Context()); st.Exists {
		t.Error("Caddy started with its port taken")
	}
}

// When Caddy can't start for a port taken after the check, the panel takes its
// ports back, and nothing is left saying they are Caddy's.
func TestAProxyThatCantBindGivesThePortsBackOnRealCaddy(t *testing.T) {
	var log []string
	panel := &ingress.Panel{
		Domain:  "panel.example.net",
		Release: func(context.Context) error { log = append(log, "release"); return nil },
		Retake:  func(context.Context) error { log = append(log, "retake"); return nil },
	}
	s := newWholeStack(t, panel)
	s.container("web")
	s.mgr.Ports = []ingress.Port{} // the check found nothing, and then something took it
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf(":%d", s.tls))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ref := resource.Ref{Kind: routekind.Kind, Scope: s.env, Name: "www"}
	effect := &resource.IngressEffect{Change: resource.IngressStarts, Proxy: "Caddy", PanelDomain: "panel.example.net"}
	err = s.mgr.Apply(t.Context(), ref, &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 80}, effect)
	if err == nil || !strings.Contains(err.Error(), "didn't start") {
		t.Fatalf("Apply = %v, want Caddy not starting", err)
	}
	if fmt.Sprint(log) != "[release retake]" {
		t.Errorf("the panel did %v", log)
	}
	if ingress.Active(s.mgr.Dir) {
		t.Error("the marker stayed")
	}
}
