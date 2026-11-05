// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// fakeProxy is a proxy in memory, which says what was done to it.
type fakeProxy struct {
	mu       sync.Mutex
	status   Status
	routes   []Route
	panel    *PanelSite
	log      []string
	startErr error
	// prepareErr fails getting ready, as a pull that can't reach its registry.
	prepareErr error
	// onStart runs as it starts.
	onStart  func()
	certs    map[string]CertState
	inFlight map[string]int
	retries  int
	// recovered and recoverErr are what Recover says.
	recovered  bool
	recoverErr error
	// closed is what SetClosed last said.
	closed bool
	// plainOnly is what it last started with.
	plainOnly bool
}

func (p *fakeProxy) Name() string { return "Caddy" }

func (p *fakeProxy) Status(context.Context) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, nil
}

func (p *fakeProxy) Prepare(context.Context, *PanelSite) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prepareErr != nil {
		return p.prepareErr
	}
	return nil
}

func (p *fakeProxy) Start(_ context.Context, panel *PanelSite, plainOnly bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "start")
	if p.onStart != nil {
		p.onStart()
	}
	if p.startErr != nil {
		return p.startErr
	}
	p.status, p.panel, p.plainOnly = Status{Exists: true, Running: true}, panel, plainOnly
	return nil
}

func (p *fakeProxy) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "stop")
	p.status = Status{}
	return nil
}

func (p *fakeProxy) Routes(context.Context) ([]Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.routes), nil
}

func (p *fakeProxy) PutRoute(_ context.Context, r Route) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.status.Running {
		return errors.New("caddy isn't running")
	}
	p.log = append(p.log, "put "+r.ID)
	r.Upstreams = slices.Clone(r.Upstreams)
	for i := range p.routes {
		if p.routes[i].ID == r.ID {
			p.routes[i] = r
			return nil
		}
	}
	p.routes = append(p.routes, r)
	return nil
}

// DeleteRoute works with the proxy down, as a real one edits its last config.
func (p *fakeProxy) DeleteRoute(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "delete "+id)
	p.routes = slices.DeleteFunc(p.routes, func(r Route) bool { return r.ID == id })
	return nil
}

func (p *fakeProxy) SetClosed(_ context.Context, closed bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed != closed {
		p.log = append(p.log, fmt.Sprintf("closed %v", closed))
	}
	p.closed = closed
	return nil
}

func (p *fakeProxy) Recover(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recovered, p.recoverErr
}

func (p *fakeProxy) SetPanel(_ context.Context, panel *PanelSite) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.panel = panel
	return nil
}

func (p *fakeProxy) InFlight(_ context.Context, up string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight[up], nil
}

func (p *fakeProxy) Certificates(_ context.Context, domains []string) (map[string]CertState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]CertState{}
	for _, d := range domains {
		out[d] = p.certs[d]
	}
	return out, nil
}

func (p *fakeProxy) Retry(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retries++
	return nil
}

const envID = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// movesPanel is what a plan says of a first route on the control server, which a
// person approved.
var movesPanel = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: "Caddy", PanelDomain: "panel.example.com"}

// setup is a manager on a fake engine and proxy, on a server whose ports are
// free, with a service web running.
type setup struct {
	*Manager
	engine *dockertest.Engine
	proxy  *fakeProxy
	proc   *proc
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	e := dockertest.New(t)
	p := newProc(t)
	proxy := &fakeProxy{}
	m := &Manager{
		Client: e.Client(), Proxy: proxy, Dir: t.TempDir(),
		Holders: holdersOn(t, e, p),
		DNS: &DNS{
			Lookup: func(context.Context, string) ([]netip.Addr, error) { return nil, nil },
			Addrs:  func() ([]netip.Addr, error) { return nil, nil },
		},
	}
	s := &setup{Manager: m, engine: e, proxy: proxy, proc: p}
	s.service("web")
	return s
}

// service puts a running container of a service on the fake engine.
func (s *setup) service(name string) {
	project := docker.Project(envID)
	s.engine.AddContainer(docker.Container{
		Name:  fmt.Sprintf("/%s-%s-1", project, name),
		State: docker.State{Running: true, Status: "running"},
		NetworkSettings: struct {
			Networks map[string]*docker.EndpointSettings `json:"Networks"`
		}{Networks: map[string]*docker.EndpointSettings{"bridge": {}}},
	})
}

func routeRef(name string) resource.Ref {
	return resource.Ref{Kind: routeKind, Scope: envID, Name: name}
}

func spec(domain, path, service string, port uint32) *resourcev1.Route {
	return &resourcev1.Route{Domain: domain, Path: path, Service: service, Port: port}
}

// apply plans and applies a change to one route, as the agent does.
func (s *setup) apply(t *testing.T, name string, r *resourcev1.Route) *resource.IngressEffect {
	t.Helper()
	ref := routeRef(name)
	desired := []resource.Desired{{Ref: ref}}
	if r != nil {
		desired[0].Spec = r
		if err := s.Check(t.Context(), ref, desired); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	effect, _, err := s.Judge(t.Context(), ref, r, desired)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if err := s.Apply(t.Context(), ref, r, effect); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return effect
}

func TestNoRouteNoProxy(t *testing.T) {
	s := newSetup(t)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.proxy.log) != 0 {
		t.Errorf("a server with no routes did %v", s.proxy.log)
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("a server with no routes has the ingress network")
	}
	if Active(s.Dir) {
		t.Error("a server with no routes reads as having its ports with its proxy")
	}
}

func TestTheFirstRouteStartsTheProxyAndNamesThePorts(t *testing.T) {
	s := newSetup(t)
	effect := s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	if effect == nil || effect.Change != resource.IngressStarts {
		t.Fatalf("effect = %+v, want the proxy starting", effect)
	}
	if !slices.Equal(effect.Ports, []string{"80/tcp", "443/tcp", "443/udp"}) || effect.Proxy != "Caddy" {
		t.Errorf("effect = %+v", effect)
	}
	if !slices.Equal(s.proxy.log, []string{"start", "put " + RouteID(routeRef("www"))}) {
		t.Errorf("the proxy did %v", s.proxy.log)
	}
	if !Active(s.Dir) {
		t.Error("the ports are with the proxy, and nothing says so")
	}
	// A second route starts nothing.
	if effect := s.apply(t, "api", spec("app.example.com", "/api", "web", 8080)); effect != nil {
		t.Errorf("a second route's effect = %+v", effect)
	}
	if n := strings.Count(strings.Join(s.proxy.log, " "), "start"); n != 1 {
		t.Errorf("the proxy started %d times", n)
	}
}

func TestTheIngressNetworkIsKeptToItself(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	n := s.engine.Network(docker.IngressNetwork)
	if n == nil {
		t.Fatal("no ingress network")
	}
	if !n.Internal {
		t.Error("the ingress network isn't internal, so a service on it could get out through it, and its default route could move")
	}
	if n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		t.Errorf("the ingress network lets containers reach each other: %v", n.Options)
	}
	if len(n.IPAM.Config) != 1 || n.IPAM.Config[0].Subnet == "" {
		t.Errorf("the ingress network has no configured subnet, so no static addresses: %+v", n.IPAM)
	}
	if s.engine.Network(docker.IngressNetwork+"_probe") != nil {
		t.Error("the network that found the subnet is still there")
	}
}

func TestARouteDoesntRecreateItsService(t *testing.T) {
	s := newSetup(t)
	before := s.engine.Container(docker.Project(envID) + "-web-1")
	creates := s.engine.Creates
	s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	after := s.engine.Container(docker.Project(envID) + "-web-1")
	if after.ID != before.ID || s.engine.Creates != creates {
		t.Error("adding a route made the service's container again, which is downtime")
	}
	ep := after.NetworkSettings.Networks[docker.IngressNetwork]
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address == "" {
		t.Fatalf("the service isn't on the ingress network at a static address: %+v", ep)
	}
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || routes[0].Upstreams[0] != ep.IPAMConfig.IPv4Address+":8080" {
		t.Errorf("the route's upstreams are %+v, want the instance's address and the port", routes)
	}
}

func TestEachServiceGetsItsOwnAddress(t *testing.T) {
	s := newSetup(t)
	s.service("api")
	s.apply(t, "www", spec("www.example.com", "", "web", 80))
	s.apply(t, "api", spec("api.example.com", "", "api", 80))
	a := s.engine.Container(docker.Project(envID) + "-web-1").NetworkSettings.Networks[docker.IngressNetwork].IPAMConfig.IPv4Address
	b := s.engine.Container(docker.Project(envID) + "-api-1").NetworkSettings.Networks[docker.IngressNetwork].IPAMConfig.IPv4Address
	if a == b {
		t.Errorf("two services have the address %s", a)
	}
}

func TestTheAddressesOutliveTheAgent(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	want := s.engine.Container(docker.Project(envID) + "-web-1").NetworkSettings.Networks[docker.IngressNetwork].IPAMConfig.IPv4Address
	again := &Manager{Client: s.Client, Proxy: s.Proxy, Dir: s.Dir, Holders: s.Holders, DNS: s.DNS}
	got, err := again.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetService() != "web" || got.GetPort() != 8080 {
		t.Errorf("a new agent reads %v from the address %s", got, want)
	}
	if err := again.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.Container(docker.Project(envID) + "-web-1").NetworkSettings.Networks[docker.IngressNetwork].IPAMConfig.IPv4Address; got != want {
		t.Errorf("the address moved from %s to %s", want, got)
	}
}

func TestPortsTakenAreRefusedAtPlanAndOnApply(t *testing.T) {
	s := newSetup(t)
	ref := routeRef("www")
	r := spec("app.example.com", "", "web", 80)
	desired := []resource.Desired{{Ref: ref, Spec: r}}
	if err := s.Check(t.Context(), ref, desired); err != nil {
		t.Fatalf("free ports: %v", err)
	}

	// Taken after the plan, before applying: the check there catches it.
	s.proc.listen("tcp", 443, "0A", "51")
	s.proc.process(77, "nginx", "0::/system.slice/nginx.service", "51")
	if err := s.Check(t.Context(), ref, desired); err == nil || !strings.Contains(err.Error(), "held by nginx") {
		t.Fatalf("Check = %v, want it naming nginx", err)
	}
	err := s.Apply(t.Context(), ref, r, nil)
	if err == nil || !strings.Contains(err.Error(), "held by nginx") {
		t.Fatalf("Apply = %v, want it naming nginx", err)
	}
	if slices.Contains(s.proxy.log, "start") {
		t.Error("the proxy started with its ports taken")
	}
	if Active(s.Dir) {
		t.Error("a route that couldn't start says the ports are with the proxy")
	}
}

func TestPortsAreNotCheckedOnceTheProxyHoldsThem(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	// Caddy's own sockets, now.
	s.proc.listen("tcp", 443, "0A", "61")
	if err := s.Check(t.Context(), routeRef("api"), nil); err != nil {
		t.Errorf("Check with the proxy holding its own ports: %v", err)
	}
}

func TestTheLastRouteStopsTheProxyAndFreesEverything(t *testing.T) {
	s := newSetup(t)
	s.service("api")
	s.apply(t, "www", spec("www.example.com", "", "web", 80))
	s.apply(t, "api", spec("api.example.com", "", "api", 80))
	if effect := s.apply(t, "api", nil); effect != nil {
		t.Errorf("removing one of two routes = %+v", effect)
	}
	if s.proxy.status != (Status{Exists: true, Running: true}) {
		t.Error("removing one of two routes stopped the proxy")
	}
	api := s.engine.Container(docker.Project(envID) + "-api-1")
	if _, on := api.NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Error("a service with no routes stayed on the ingress network")
	}

	effect := s.apply(t, "www", nil)
	if effect == nil || effect.Change != resource.IngressStops || !slices.Equal(effect.Ports, []string{"80/tcp", "443/tcp", "443/udp"}) {
		t.Errorf("removing the last route = %+v, want the proxy stopping and the ports", effect)
	}
	if !slices.Contains(s.proxy.log, "stop") {
		t.Error("the proxy didn't stop")
	}
	if Active(s.Dir) {
		t.Error("the marker stayed, so the panel would leave the ports alone")
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("the ingress network stayed")
	}
	web := s.engine.Container(docker.Project(envID) + "-web-1")
	if _, on := web.NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Error("the service stayed on the ingress network")
	}

	// A route after that starts it all again.
	if effect := s.apply(t, "www", spec("www.example.com", "", "web", 80)); effect == nil || effect.Change != resource.IngressStarts {
		t.Errorf("a route after the last = %+v", effect)
	}
}

func TestAPlanThatRemovesEveryRouteStopsOnTheLastToRun(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.apply(t, "b", spec("b.example.com", "", "web", 80))
	desired := []resource.Desired{{Ref: routeRef("a")}, {Ref: routeRef("b")}}
	// Deletes run last first, so the first listed runs last.
	first, _, err := s.Judge(t.Context(), routeRef("a"), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Judge(t.Context(), routeRef("b"), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Change != resource.IngressStops || second != nil {
		t.Errorf("effects = %+v and %+v, want the one that runs last to stop the proxy", first, second)
	}
}

func TestAPlanThatStartsWithTwoRoutesStartsOnTheFirst(t *testing.T) {
	s := newSetup(t)
	r := spec("a.example.com", "", "web", 80)
	desired := []resource.Desired{{Ref: routeRef("a"), Spec: r}, {Ref: routeRef("b"), Spec: r}}
	first, _, err := s.Judge(t.Context(), routeRef("a"), r, desired)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Judge(t.Context(), routeRef("b"), r, desired)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Change != resource.IngressStarts || second != nil {
		t.Errorf("effects = %+v and %+v", first, second)
	}
}

func TestAnOutdatedProxyRestartsOnlyAsAPlanSays(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	s.proxy.status.Outdated = true
	ref := routeRef("api")
	r := spec("app.example.com", "/api", "web", 80)

	// An agent upgrade alone restarts nothing.
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(s.proxy.log, " "), "start"); n != 1 {
		t.Errorf("an upgrade restarted the proxy, %d starts", n)
	}
	effect, _, err := s.Judge(t.Context(), ref, r, []resource.Desired{{Ref: ref, Spec: r}})
	if err != nil {
		t.Fatal(err)
	}
	if effect == nil || effect.Change != resource.IngressRestarts {
		t.Fatalf("effect = %+v, want a restart the plan says", effect)
	}
	if err := s.Apply(t.Context(), ref, r, effect); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(s.proxy.log, " "), "start"); n != 2 {
		t.Errorf("the proxy started %d times, want it restarted once", n)
	}
}

// panelSetup is a server whose panel holds the ports.
type panelCalls struct {
	mu  sync.Mutex
	log []string
	err error
}

func (p *panelCalls) panel() *Panel {
	return &Panel{
		Domain: "panel.example.com",
		Release: func(context.Context) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.log = append(p.log, "release")
			return p.err
		},
		Retake: func(context.Context) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.log = append(p.log, "retake")
			return nil
		},
	}
}

func TestThePanelHandsOverAndTakesBack(t *testing.T) {
	s := newSetup(t)
	calls := &panelCalls{}
	s.Panel = calls.panel()
	// The panel holds its ports: this process's sockets.
	s.proc.listen("tcp6", 443, "0A", "71")
	s.proc.process(99, "dockwell", "0::/system.slice/dockwell.service", "71")

	effect := s.apply(t, "www", spec("app.example.com", "", "web", 80))
	if effect == nil || effect.PanelDomain != "panel.example.com" {
		t.Errorf("effect = %+v, want it saying the panel moves", effect)
	}
	if !slices.Equal(calls.log, []string{"release"}) || s.proxy.panel == nil || s.proxy.panel.Domain != "panel.example.com" {
		t.Errorf("the panel did %v, and the proxy has %+v", calls.log, s.proxy.panel)
	}
	// Before the proxy starts, so it can bind.
	if !slices.Equal(s.proxy.log[:1], []string{"start"}) {
		t.Errorf("the proxy did %v", s.proxy.log)
	}

	effect = s.apply(t, "www", nil)
	if effect == nil || effect.PanelDomain != "panel.example.com" {
		t.Errorf("effect = %+v, want it saying the panel takes the ports back", effect)
	}
	if !slices.Equal(calls.log, []string{"release", "retake"}) {
		t.Errorf("the panel did %v", calls.log)
	}
}

// Ports taken between the check and the start are still refused by name.
func TestAPortTakenAfterTheCheckIsStillNamed(t *testing.T) {
	s := newSetup(t)
	s.proxy.startErr = errors.New("caddy didn't start")
	// The check finds nothing, and by the start something holds the port.
	s.Ports = DefaultPorts
	checked := false
	s.proxy.onStart = func() {
		if !checked {
			checked = true
			s.proc.listen("tcp", 443, "0A", "81")
			s.proc.process(82, "haproxy", "0::/system.slice/haproxy.service", "81")
		}
	}
	err := s.Apply(t.Context(), routeRef("www"), spec("app.example.com", "", "web", 80), nil)
	if err == nil || !strings.Contains(err.Error(), "held by haproxy") || !strings.Contains(err.Error(), "point that proxy at it") {
		t.Errorf("Apply = %v, want it naming haproxy", err)
	}
}

func TestThePanelTakesThePortsBackWhenTheProxyCantStart(t *testing.T) {
	s := newSetup(t)
	calls := &panelCalls{}
	s.Panel = calls.panel()
	s.proxy.startErr = errors.New("caddy didn't start")
	ref := routeRef("www")
	r := spec("app.example.com", "", "web", 80)
	err := s.Apply(t.Context(), ref, r, movesPanel)
	if err == nil || !strings.Contains(err.Error(), "didn't start") {
		t.Fatalf("Apply = %v", err)
	}
	if !slices.Equal(calls.log, []string{"release", "retake"}) {
		t.Errorf("the panel did %v, want the ports back", calls.log)
	}
	if Active(s.Dir) {
		t.Error("the marker stayed, so the next start of Dockwell would leave the ports with no one")
	}
}

// A pull that fails, or takes minutes, comes before the panel gives up anything.
func TestThePanelKeepsItsPortsUntilTheProxyIsReadyToStart(t *testing.T) {
	s := newSetup(t)
	calls := &panelCalls{}
	s.Panel = calls.panel()
	s.proxy.prepareErr = errors.New("can't pull Caddy")
	err := s.Apply(t.Context(), routeRef("www"), spec("app.example.com", "", "web", 80), movesPanel)
	if err == nil || !strings.Contains(err.Error(), "can't pull") {
		t.Fatalf("Apply = %v", err)
	}
	if len(calls.log) != 0 {
		t.Errorf("the panel did %v for a proxy that couldn't get ready", calls.log)
	}
	if Active(s.Dir) {
		t.Error("the marker stayed")
	}
}

// A first route that couldn't start leaves nothing behind it: rolling back has
// no step to run, as the proxy never had the route.
func TestAFirstRouteThatCantStartLeavesNoTrace(t *testing.T) {
	s := newSetup(t)
	s.proxy.startErr = errors.New("caddy didn't start")
	if err := s.Apply(t.Context(), routeRef("www"), spec("app.example.com", "", "web", 80), nil); err == nil {
		t.Fatal("Apply took a proxy that didn't start")
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("the ingress network stayed")
	}
	if _, on := s.engine.Container(docker.Project(envID) + "-web-1").NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Error("the service stayed on the ingress network")
	}
	if Active(s.Dir) {
		t.Error("the marker stayed")
	}
}

// Removing a route must always work, so it doesn't start a proxy someone
// stopped, or one that can't start. The route comes out of its last config, and
// the agent's reconcile starts it when it can.
func TestRemovingARouteLeavesAStoppedProxyStopped(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.apply(t, "b", spec("b.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true}
	s.proxy.log = nil
	if err := s.Apply(t.Context(), routeRef("a"), nil, nil); err != nil {
		t.Fatal(err)
	}
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || routes[0].Domain != "b.example.com" || s.proxy.status.Running {
		t.Errorf("routes %+v, status %+v", routes, s.proxy.status)
	}
	if slices.Contains(s.proxy.log, "start") {
		t.Errorf("the proxy did %v", s.proxy.log)
	}
}

// The owner stopped Caddy and another proxy took the ports back. A route still
// goes, and so does the last, which stops what is left of Caddy.
func TestARouteCanBeRemovedWithAnotherProxyOnThePorts(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("a.example.com", "", "web", 80))
	if err := s.Proxy.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.proxy.status = Status{Exists: true}
	s.proxy.startErr = errors.New("address already in use")
	s.proc.listen("tcp", 80, "0A", "9001")
	s.proc.process(1234, "nginx", "0::/", "9001")
	ref := routeRef("www")
	desired := []resource.Desired{{Ref: ref}}
	effect, _, err := s.Judge(t.Context(), ref, nil, desired)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if effect == nil || effect.Change != resource.IngressStops {
		t.Errorf("effect = %+v, want the proxy stopping", effect)
	}
	if err := s.Apply(t.Context(), ref, nil, effect); err != nil {
		t.Errorf("removing the route fails until the other proxy stops: %v", err)
	}
	if routes, _ := s.proxy.Routes(t.Context()); len(routes) != 0 {
		t.Errorf("routes %+v", routes)
	}
	if Active(s.Dir) {
		t.Error("the marker stayed, so the panel would stay off the ports")
	}
}

// docker pause is one way to get here: an admin API that accepts a connection
// and never answers, while Reconcile holds the lock with no deadline of its own.
type hungProxy struct{ fakeProxy }

func (*hungProxy) Routes(ctx context.Context) ([]Route, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAHungProxyDoesntHoldUpPlans(t *testing.T) {
	s := newSetup(t)
	s.Proxy = &hungProxy{}
	if err := os.WriteFile(filepath.Join(s.Dir, activeFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	agent, stop := context.WithCancel(t.Context())
	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		_ = s.Reconcile(agent)
	}()
	defer func() {
		stop()
		<-reconciled
	}()
	// Reconcile holds the lock once it is waiting on the proxy.
	time.Sleep(100 * time.Millisecond)

	plan, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Check(plan, routeRef("www"), nil) }()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "docker logs dockwell-ingress") {
			t.Errorf("Check = %v, want it to say it stopped waiting, and how to look", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a plan with a 200 ms deadline is still waiting after 5 s, on a lock Reconcile holds")
	}
	for name, call := range map[string]func(context.Context) error{
		"Apply":     func(ctx context.Context) error { return s.Apply(ctx, routeRef("www"), nil, nil) },
		"Verify":    func(ctx context.Context) error { return s.Verify(ctx, routeRef("www")) },
		"Reconcile": s.Reconcile,
		"Observe":   func(ctx context.Context) error { _, err := s.Observe(ctx, routeRef("www")); return err },
		"Judge": func(ctx context.Context) error {
			_, _, err := s.Judge(ctx, routeRef("www"), nil, nil)
			return err
		},
		"Endpoint": func(ctx context.Context) error {
			_, _, _, err := s.Endpoint(ctx, Instance{Project: "p", Service: "s", Number: 1})
			return err
		},
		"AddUpstream": func(ctx context.Context) error {
			return s.AddUpstream(ctx, Instance{Project: "p", Service: "s", Number: 1})
		},
		"Drain": func(ctx context.Context) error { return s.Drain(ctx, Instance{Project: "p", Service: "s", Number: 1}) },
		"RemoveUpstream": func(ctx context.Context) error {
			return s.RemoveUpstream(ctx, Instance{Project: "p", Service: "s", Number: 1})
		},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		if err := call(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s = %v, want it to give up at its deadline", name, err)
		}
		cancel()
	}
}

// A Release that ends early has stopped the listeners all the same, so the panel
// must get its ports back, or nothing would until Dockwell restarts.
func TestAReleaseThatEndsEarlyGivesThePortsBack(t *testing.T) {
	s := newSetup(t)
	retakes, failFirst := 0, true
	s.Panel = &Panel{
		Domain:  "panel.example.com",
		Release: func(context.Context) error { return context.Canceled },
		Retake: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				t.Errorf("Retake ran under an ended context: %v", err)
			}
			retakes++
			if failFirst {
				failFirst = false
				return errors.New("address already in use")
			}
			return nil
		},
	}
	if err := s.Apply(t.Context(), routeRef("www"), spec("a.example.com", "", "web", 80), movesPanel); err == nil {
		t.Fatal("Apply worked, with a release that failed")
	}
	if retakes != 1 {
		t.Errorf("the panel was asked to retake its ports %d times, want at once", retakes)
	}
	if Active(s.Dir) {
		t.Error("the marker stayed, so a restart of Dockwell would leave the panel off the ports")
	}
	// The old listeners may take a moment to let go, and the next reconcile
	// tries again.
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if retakes != 2 {
		t.Errorf("after a reconcile, %d retakes, want the failed one tried again", retakes)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if retakes != 2 {
		t.Errorf("after the retake worked, %d retakes", retakes)
	}
}

// A first-come route must not read as serving while another serves its domain.
func TestAShadowedRouteDoesntVerify(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("a.example.com", "", "web", 80))
	s.proxy.routes = append([]Route{{ID: "dockwell.env_old.www", Domain: "a.example.com"}}, s.proxy.routes...)
	err := s.Verify(t.Context(), routeRef("www"))
	if err == nil || !strings.Contains(err.Error(), "dockwell.env_old.www") {
		t.Errorf("Verify = %v, want it to name the route ahead of it", err)
	}
}

// A last config that was damaged is put right before anything reads it, and is
// said once, with what it means.
func TestReconcileRecoversFirstAndSaysSo(t *testing.T) {
	s := newSetup(t)
	var logs bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.apply(t, "www", spec("a.example.com", "", "web", 80))
	s.proxy.recovered = true
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "put back its own copy") {
		t.Errorf("the log says: %s", logs.String())
	}
	// Failing to keep a copy doesn't stop the rest.
	s.proxy.recovered, s.proxy.recoverErr = false, errors.New("disk full")
	s.proxy.status = Status{Exists: true}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.proxy.status.Running {
		t.Error("a failure to keep a copy of the config kept the proxy from starting")
	}
}

func TestAPanelOnOtherPortsStaysAsItIs(t *testing.T) {
	s := newSetup(t)
	s.Panel = &Panel{Domain: "panel.example.com"}
	effect := s.apply(t, "www", spec("app.example.com", "", "web", 80))
	if effect == nil || effect.PanelDomain != "" {
		t.Errorf("effect = %+v, want no word of the panel", effect)
	}
	if s.proxy.panel != nil {
		t.Errorf("the proxy serves the panel: %+v", s.proxy.panel)
	}
}

func TestAStartOfDockwellWithTheActiveMarkerKnowsThePortsAreHandedOver(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	calls := &panelCalls{}
	next := &Manager{Client: s.Client, Proxy: s.Proxy, Dir: s.Dir, Holders: s.Holders, DNS: s.DNS, Panel: calls.panel()}
	if err := next.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(calls.log) != 0 {
		t.Errorf("the panel did %v, with the ports the proxy's", calls.log)
	}
	if s.proxy.panel == nil || s.proxy.panel.Domain != "panel.example.com" {
		t.Errorf("the proxy has %+v, want the panel's site as this start has it", s.proxy.panel)
	}
	// The last route goes: they come back.
	if err := next.Apply(t.Context(), routeRef("www"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls.log, []string{"retake"}) {
		t.Errorf("the panel did %v", calls.log)
	}
}

func TestReadingBackASwapIsntDrift(t *testing.T) {
	s := newSetup(t)
	r := spec("app.example.com", "/api", "web", 8080)
	s.apply(t, "www", r)
	got, err := s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDomain() != r.GetDomain() || got.GetPath() != r.GetPath() || got.GetService() != "web" || got.GetPort() != 8080 {
		t.Errorf("Observe = %v, want %v", got, r)
	}
	// A rollout's second instance reads the same.
	if err := s.AddUpstream(t.Context(), Instance{Project: docker.Project(envID), Service: "web", Number: 2}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetService() != "web" || got.GetPort() != 8080 {
		t.Errorf("with a second instance, Observe = %v", got)
	}
}

func TestReadingBackAHandEditIsDrift(t *testing.T) {
	s := newSetup(t)
	r := spec("app.example.com", "", "web", 8080)
	s.apply(t, "www", r)
	id := RouteID(routeRef("www"))

	// Upstreams pointed somewhere the table doesn't know.
	s.proxy.routes[0].Upstreams = []string{"203.0.113.9:8080"}
	got, err := s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetService() == "web" {
		t.Errorf("a route sent to an address the table doesn't know reads as %v", got)
	}

	// Something Dockwell doesn't write.
	s.apply(t, "www", r)
	s.proxy.routes[0] = Route{ID: id, Domain: "app.example.com", Edited: true}
	got, err = s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetService() != editedByHand {
		t.Errorf("a hand-edited route reads as %v", got)
	}

	// Reconciling leaves it for a person: drift is never silently reverted,
	// and the service stays where a revert needs it.
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.proxy.routes[0].Edited {
		t.Error("reconciling reverted a hand edit")
	}
	if _, on := s.engine.Container(docker.Project(envID) + "-web-1").NetworkSettings.Networks[docker.IngressNetwork]; !on {
		t.Error("a hand edit took its service off the ingress network")
	}

	// Gone by hand reads as gone.
	s.proxy.routes = nil
	if got, err := s.Observe(t.Context(), routeRef("www")); err != nil || got != nil {
		t.Errorf("a route gone by hand reads as %v, %v", got, err)
	}
}

// A service in another environment can't be read as this route's, though the
// table knows it.
func TestAnUpstreamOfAnotherEnvironmentIsDrift(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	if err := s.table.load(s.Dir); err != nil {
		t.Fatal(err)
	}
	s.table.Addresses["env_other/web/1"] = "172.18.0.50"
	s.proxy.routes[0].Upstreams = []string{"172.18.0.50:8080"}
	got, err := s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetService() == "web" {
		t.Errorf("reads as %v", got)
	}
}

func TestReconcileStartsAStoppedProxyAndAttachesInstances(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	name := docker.Project(envID) + "-web-1"
	addr := s.engine.Container(name).NetworkSettings.Networks[docker.IngressNetwork].IPAMConfig.IPv4Address

	// Stopped by hand, and its container made again by docker compose up.
	s.proxy.status = Status{Exists: true}
	s.engine.Edit(name, func(c *docker.Container) { delete(c.NetworkSettings.Networks, docker.IngressNetwork) })
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.proxy.status.Running {
		t.Error("the proxy stayed stopped")
	}
	ep := s.engine.Container(name).NetworkSettings.Networks[docker.IngressNetwork]
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != addr {
		t.Errorf("the instance is on the network as %+v, want its address, %s", ep, addr)
	}
}

func TestReconcileFinishesAStopThatDidntEnd(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("app.example.com", "", "web", 8080))
	s.proxy.routes = nil // the last route went, and the agent stopped before the rest
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.proxy.log, "stop") || Active(s.Dir) {
		t.Errorf("the proxy did %v, active %v", s.proxy.log, Active(s.Dir))
	}
}

func TestAServiceWithNoRoutesHasNoEndpoint(t *testing.T) {
	s := newSetup(t)
	i := Instance{Project: docker.Project(envID), Service: "web", Number: 1}
	_, _, ok, err := s.Endpoint(t.Context(), i)
	if err != nil || ok {
		t.Errorf("Endpoint = %v, %v, want none", ok, err)
	}
	if err := s.AddUpstream(t.Context(), i); err != nil {
		t.Errorf("AddUpstream for a service with no routes: %v", err)
	}
	if err := s.Drain(t.Context(), i); err != nil {
		t.Errorf("Drain for a service with no routes: %v", err)
	}
	if err := s.RemoveUpstream(t.Context(), i); err != nil {
		t.Errorf("RemoveUpstream for a service with no routes: %v", err)
	}
	if len(s.proxy.log) != 0 {
		t.Errorf("the proxy did %v", s.proxy.log)
	}
}

// What #28 does: a new instance, on a new address; its upstream added; the old
// one drained, and gone.
func TestARolloutSwapsAnInstanceWithoutAPlan(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("www.example.com", "", "web", 8080))
	s.apply(t, "api", spec("www.example.com", "/api", "web", 9090))
	project := docker.Project(envID)
	old, next := Instance{Project: project, Service: "web", Number: 1}, Instance{Project: project, Service: "web", Number: 2}

	network, ep, ok, err := s.Endpoint(t.Context(), next)
	if err != nil || !ok || network != docker.IngressNetwork || ep.IPAMConfig == nil {
		t.Fatalf("Endpoint = %s, %+v, %v, %v", network, ep, ok, err)
	}
	if err := s.table.load(s.Dir); err != nil {
		t.Fatal(err)
	}
	oldAddr, nextAddr := s.table.Addresses[old.String()], ep.IPAMConfig.IPv4Address
	if oldAddr == nextAddr {
		t.Fatalf("two instances have %s", nextAddr)
	}
	if err := s.AddUpstream(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	want := func(id string, ups ...string) {
		t.Helper()
		routes, _ := s.proxy.Routes(t.Context())
		for _, r := range routes {
			if r.ID == id && slices.Equal(r.Upstreams, ups) {
				return
			}
		}
		t.Errorf("%s doesn't have %v: %+v", id, ups, routes)
	}
	www, api := RouteID(routeRef("www")), RouteID(routeRef("api"))
	want(www, oldAddr+":8080", nextAddr+":8080")
	want(api, oldAddr+":9090", nextAddr+":9090")

	// Drain waits for what is in flight, and no longer than it is given.
	s.proxy.inFlight = map[string]int{oldAddr + ":8080": 1}
	done := make(chan error, 1)
	go func() { done <- s.Drain(t.Context(), old) }()
	select {
	case err := <-done:
		t.Fatalf("Drain returned with a request in flight: %v", err)
	case <-time.After(3 * drainPoll):
	}
	want(www, nextAddr+":8080")
	want(api, nextAddr+":9090")
	s.proxy.mu.Lock()
	s.proxy.inFlight = nil
	s.proxy.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUpstream(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.table.Addresses[old.String()]; ok {
		t.Error("the old instance kept its address")
	}
	got, err := s.Observe(t.Context(), routeRef("www"))
	if err != nil || got.GetService() != "web" || got.GetPort() != 8080 {
		t.Errorf("after the swap, Observe = %v, %v, want no drift", got, err)
	}
}

// A rollout's new instance has an address before it has traffic, and a route
// changing on the same server meanwhile mustn't take it away.
func TestARouteChangeLeavesARolloutsInstanceAlone(t *testing.T) {
	s := newSetup(t)
	s.service("api")
	s.apply(t, "www", spec("www.example.com", "", "web", 8080))
	s.apply(t, "api", spec("api.example.com", "", "api", 8080))
	next := Instance{Project: docker.Project(envID), Service: "web", Number: 2}
	_, ep, ok, err := s.Endpoint(t.Context(), next)
	if err != nil || !ok {
		t.Fatalf("Endpoint = %v, %v", ok, err)
	}
	s.apply(t, "api", nil)
	_, again, ok, err := s.Endpoint(t.Context(), next)
	if err != nil || !ok || again.IPAMConfig.IPv4Address != ep.IPAMConfig.IPv4Address {
		t.Errorf("after an unrelated route went, the instance's endpoint is %+v, %v, %v, want %+v", again, ok, err, ep)
	}
}

func TestDrainGivesUpWhenItsTimeRunsOut(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("www.example.com", "", "web", 8080))
	project := docker.Project(envID)
	old, next := Instance{Project: project, Service: "web", Number: 1}, Instance{Project: project, Service: "web", Number: 2}
	if err := s.AddUpstream(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if err := s.table.load(s.Dir); err != nil {
		t.Fatal(err)
	}
	s.proxy.inFlight = map[string]int{s.table.Addresses[old.String()] + ":8080": 1}
	ctx, cancel := context.WithTimeout(t.Context(), 2*drainPoll)
	defer cancel()
	if err := s.Drain(ctx, old); err != nil {
		t.Errorf("Drain whose time ran out = %v, want no error: what remains ends with the container", err)
	}
}

func TestARouteKeepsItsLastUpstream(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("www.example.com", "", "web", 8080))
	only := Instance{Project: docker.Project(envID), Service: "web", Number: 1}
	if err := s.Drain(t.Context(), only); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUpstream(t.Context(), only); err != nil {
		t.Fatal(err)
	}
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || len(routes[0].Upstreams) != 1 {
		t.Errorf("Routes = %+v, want the route's only upstream kept", routes)
	}
}

func TestANetworkOfTheNameThatIsntOursIsRefused(t *testing.T) {
	s := newSetup(t)
	s.engine.AddNetwork(docker.Network{Name: docker.IngressNetwork, Driver: "bridge"})
	err := s.Apply(t.Context(), routeRef("www"), spec("a.example.com", "", "web", 80), nil)
	if err == nil || !strings.Contains(err.Error(), "didn't make it") {
		t.Errorf("Apply = %v, want it refused, saying so", err)
	}
}

func TestRootlessDockerIsRefusedByName(t *testing.T) {
	s := newSetup(t)
	s.engine.Rootless = true
	err := s.Ready(t.Context())
	if err == nil || !strings.Contains(err.Error(), "rootless") {
		t.Errorf("Ready = %v, want it naming rootless", err)
	}
}

func TestAServiceCantPublishWhatTheProxyHolds(t *testing.T) {
	s := newSetup(t)
	for _, p := range []struct {
		port uint32
		udp  bool
	}{{80, false}, {443, false}, {443, true}} {
		if held, _ := s.PublicPort(t.Context(), p.port, p.udp); held {
			t.Errorf("a server with no routes reads as holding %d", p.port)
		}
	}
	s.apply(t, "www", spec("a.example.com", "", "web", 80))
	for _, tc := range []struct {
		port uint32
		udp  bool
		want bool
	}{{80, false, true}, {443, false, true}, {443, true, true}, {80, true, false}, {8080, false, false}} {
		if held, _ := s.PublicPort(t.Context(), tc.port, tc.udp); held != tc.want {
			t.Errorf("port %d, udp %v: held %v, want %v", tc.port, tc.udp, held, tc.want)
		}
	}
}

func TestDomainsReportEachStateAndRetryWhenDNSComesHere(t *testing.T) {
	s := newSetup(t)
	var mu sync.Mutex
	found := []netip.Addr{}
	s.DNS = &DNS{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			mu.Lock()
			defer mu.Unlock()
			return found, nil
		},
		Addrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil },
	}
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	s.proxy.certs = map[string]CertState{"app.example.com": {Phase: CertFailed, Problem: ProblemDNS, Detail: "NXDOMAIN"}}

	changed := s.DomainsChanged()
	if !s.watchOnce(t.Context()) {
		t.Error("watching says nothing is waiting, with a certificate failed")
	}
	select {
	case <-changed:
	default:
		t.Error("the first report didn't signal")
	}
	list := s.Domains()
	if len(list) != 1 || list[0].GetDomain() != "app.example.com" {
		t.Fatalf("Domains = %v", list)
	}
	if c := list[0].GetCertificate(); c.GetState().String() != "CERTIFICATE_STATE_FAILED" || c.GetProblem().String() != "CERTIFICATE_PROBLEM_DNS" || !strings.Contains(c.GetMessage(), "Point an A or AAAA record") {
		t.Errorf("the certificate reads %v", c)
	}
	if s.proxy.retries != 0 {
		t.Errorf("retried %d times with DNS still not here", s.proxy.retries)
	}

	// Nothing new: no signal.
	s.watchOnce(t.Context())
	select {
	case <-changed:
		t.Error("the same report signalled")
	default:
	}

	// DNS comes here: one try at once.
	mu.Lock()
	found = []netip.Addr{netip.MustParseAddr("203.0.113.9")}
	mu.Unlock()
	s.status.mu.Lock()
	s.status.checked["app.example.com"] = time.Now().Add(-time.Hour)
	s.status.mu.Unlock()
	s.watchOnce(t.Context())
	if s.proxy.retries != 1 {
		t.Errorf("retried %d times as DNS came here, want once", s.proxy.retries)
	}
	s.status.mu.Lock()
	s.status.checked["app.example.com"] = time.Now().Add(-time.Hour)
	s.status.mu.Unlock()
	s.watchOnce(t.Context())
	if s.proxy.retries != 1 {
		t.Errorf("retried %d times with DNS staying here, want once", s.proxy.retries)
	}

	// Issued: nothing waits, and it says so.
	s.proxy.certs = map[string]CertState{"app.example.com": {Phase: CertIssued, NotAfter: time.Now().Add(time.Hour)}}
	if s.watchOnce(t.Context()) {
		t.Error("watching says something is waiting, with the certificate issued")
	}
	if c := s.Domains()[0].GetCertificate(); c.GetState().String() != "CERTIFICATE_STATE_ISSUED" || c.GetExpireTime() == nil || c.GetMessage() != "" {
		t.Errorf("the certificate reads %v", c)
	}
}

// The panel's domain isn't a route, so the API has no word of it, and someone
// who can't reach the panel reads the server's log.
func TestAPanelCertificateThatFailsIsLogged(t *testing.T) {
	s := newSetup(t)
	var logged bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	calls := &panelCalls{}
	s.Panel = calls.panel()
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	s.proxy.certs = map[string]CertState{
		"panel.example.com": {Phase: CertFailed, Problem: ProblemConnection},
		"app.example.com":   {Phase: CertIssued, NotAfter: time.Now().Add(time.Hour)},
	}
	s.watchOnce(t.Context())
	if !strings.Contains(logged.String(), "SSH tunnel") || !strings.Contains(logged.String(), "port 80 or 443") {
		t.Errorf("the log says %q", logged.String())
	}
	for _, d := range s.Domains() {
		if d.GetDomain() == "panel.example.com" {
			t.Error("the panel's domain is listed as a route's")
		}
	}
	// Once is enough, while nothing changes.
	logged.Reset()
	s.watchOnce(t.Context())
	if logged.Len() != 0 {
		t.Errorf("the same state was logged again: %q", logged.String())
	}
}

func TestDomainsAreEmptyWithNoRoutes(t *testing.T) {
	s := newSetup(t)
	s.watchOnce(t.Context())
	if len(s.Domains()) != 0 {
		t.Errorf("Domains = %v", s.Domains())
	}
}

func TestTheAddressFileIsOwnerOnly(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "www", spec("a.example.com", "", "web", 80))
	info, err := os.Stat(filepath.Join(s.Dir, addressesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}

func TestAddressesAreInTheSubnetAndNeverTheGateway(t *testing.T) {
	dir := t.TempDir()
	tb := table{Subnet: "172.30.0.0/29", Addresses: map[string]string{}, loaded: true}
	seen := map[string]bool{}
	for i := 1; i <= 7; i++ {
		a, err := tb.give(dir, Instance{Project: "p", Service: "s", Number: i}, "172.30.0.1", nil)
		if err != nil {
			if i < 6 {
				t.Fatalf("instance %d: %v", i, err)
			}
			if !strings.Contains(err.Error(), "no address left") {
				t.Errorf("out of addresses says %q", err)
			}
			return
		}
		if a == "172.30.0.0" || a == "172.30.0.1" || a == "172.30.0.7" || seen[a] {
			t.Errorf("gave %s", a)
		}
		seen[a] = true
	}
	t.Error("a /29 gave more addresses than it has past its gateway")
}

// stopAndPrune stops every service, and removes the ingress network as docker
// network prune does once none of them runs. Each keeps its endpoint's settings,
// with the old network's ID, as a stopped container does.
func (s *setup) stopAndPrune(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		s.engine.Stop(docker.Project(envID) + "-" + name + "-1")
	}
	if err := s.Client.RemoveNetwork(t.Context(), docker.IngressNetwork); err != nil {
		t.Fatal(err)
	}
}

// The review's case: docker network prune takes the network, Docker reuses the
// first free subnet, and an empty table gives api the address web's route still
// dials, so web's users and cookies reach api.
func TestLosingTheNetworkNeverSendsOneAppsTrafficToAnother(t *testing.T) {
	s := newSetup(t)
	s.engine.PickSubnet = "172.30.0.0/16" // Docker picks the first free pool, which is the one just freed
	s.service("api")
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.apply(t, "b", spec("b.example.com", "", "api", 80))
	before := maps.Clone(s.table.Addresses)
	s.stopAndPrune(t, "web", "api")

	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	n := s.engine.Network(docker.IngressNetwork)
	if n == nil || n.IPAM.Config[0].Subnet != "172.30.0.0/16" {
		t.Fatalf("the network is %+v, want it back on its own subnet", n)
	}
	if !maps.Equal(s.table.Addresses, before) {
		t.Errorf("the table is %v, want %v, as Caddy's routes still dial those", s.table.Addresses, before)
	}
	if !n.Internal || n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		t.Errorf("it came back without what keeps it to itself: %+v", n)
	}

	// Any later change to b's route, such as its port.
	s.apply(t, "b", spec("b.example.com", "", "api", 8080))
	routes, _ := s.Proxy.Routes(t.Context())
	hosts := map[string]string{}
	for _, r := range routes {
		host, _, _ := splitUpstream(r.Upstreams[0])
		hosts[r.Domain] = host
	}
	if hosts["a.example.com"] == hosts["b.example.com"] {
		t.Errorf("web and api both dial %s, so web's users reach api", hosts["a.example.com"])
	}
	if inst, known := s.table.instanceAt(hosts["a.example.com"]); !known || inst.Service != "web" {
		t.Errorf("a.example.com dials %s, which the table says is %+v, not web", hosts["a.example.com"], inst)
	}
	// A stopped container names the old network's ID, and can't start until it
	// comes off it. Reconcile has put each back, so each starts, at its address.
	for _, name := range []string{"web", "api"} {
		id := docker.Project(envID) + "-" + name + "-1"
		if err := s.Client.StartContainer(t.Context(), id); err != nil {
			t.Errorf("%s can't start: %v", name, err)
		}
		c := s.engine.Container(id)
		if ep := c.NetworkSettings.Networks[docker.IngressNetwork]; ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != before[Instance{Project: docker.Project(envID), Service: name, Number: 1}.String()] {
			t.Errorf("%s is on the network as %+v", name, ep)
		}
	}
}

// Even with a table that lost its record, what Caddy dials isn't given out.
func TestAnAddressCaddyDialsIsNeverGiven(t *testing.T) {
	s := newSetup(t)
	s.service("api")
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	dialled, _, _ := splitUpstream(s.proxy.routes[0].Upstreams[0])
	s.table.Addresses = map[string]string{}
	s.apply(t, "b", spec("b.example.com", "", "api", 80))
	if got := s.table.Addresses[Instance{Project: docker.Project(envID), Service: "api", Number: 1}.String()]; got == dialled {
		t.Errorf("api was given %s, which a.example.com's route dials", got)
	}
	// And none at all if what the proxy dials can't be read.
	hung := &hungProxy{}
	s.Proxy = hung
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if a, err := s.give(ctx, Instance{Project: "p", Service: "new", Number: 1}); err == nil {
		t.Errorf("gave %s with what the proxy dials unknown", a)
	}
}

// Docker gives out the first free subnet, so another network can take the one
// that was freed. Routes dial those addresses whichever network has them, so they
// answer 503 until a person sorts it out, and no address is reused.
func TestASubnetTakenFailsClosedAndNamesWhoHasIt(t *testing.T) {
	s := newSetup(t)
	s.engine.PickSubnet = "172.30.0.0/16"
	s.service("api")
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	before := maps.Clone(s.table.Addresses)
	s.stopAndPrune(t, "web", "api")
	s.engine.AddNetwork(docker.Network{Name: "someones-app", Driver: "bridge", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "172.30.0.0/16"}}}})

	err := s.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "someones-app") || !strings.Contains(err.Error(), "172.30.0.0/16") ||
		!strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "docker network rm someones-app") {
		t.Fatalf("Reconcile = %v, want it to say who has the subnet, what that costs, and the fix", err)
	}
	if !s.proxy.closed {
		t.Error("the routes still dial addresses another network has")
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("the network was made anyway")
	}
	if !maps.Equal(s.table.Addresses, before) {
		t.Errorf("the table is %v, want %v", s.table.Addresses, before)
	}
	s.publishIngress(t.Context())
	st := s.Ingress()
	if st.GetProblem() != dockwellv1.IngressProblem_INGRESS_PROBLEM_SUBNET_TAKEN || !strings.Contains(st.GetMessage(), "someones-app") {
		t.Errorf("the API says %+v", st)
	}
	// A step fails the same way, rather than giving out an address.
	if err := s.Apply(t.Context(), routeRef("b"), spec("b.example.com", "", "api", 80), nil); err == nil || !strings.Contains(err.Error(), "someones-app") {
		t.Errorf("Apply = %v", err)
	}
	if routes, _ := s.proxy.Routes(t.Context()); len(routes) != 1 {
		t.Errorf("routes = %+v", routes)
	}
	// So does a container being replaced, before anything stops.
	if _, _, _, err := s.Endpoint(t.Context(), Instance{Project: docker.Project(envID), Service: "web", Number: 1}); err == nil || !strings.Contains(err.Error(), "someones-app") {
		t.Errorf("Endpoint = %v", err)
	}
	// Removing a route must still work.
	if err := s.Apply(t.Context(), routeRef("a"), nil, nil); err != nil {
		t.Errorf("removing a route = %v", err)
	}
	s.apply(t, "a", nil) // nothing left: that goes through
	if s.table.Subnet != "" {
		t.Errorf("after the last route, the table says %+v", s.table)
	}
}

func TestASubnetFreedAgainReopensTheRoutes(t *testing.T) {
	s := newSetup(t)
	s.engine.PickSubnet = "172.30.0.0/16"
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	before := maps.Clone(s.table.Addresses)
	s.stopAndPrune(t, "web")
	s.engine.AddNetwork(docker.Network{Name: "someones-app", Driver: "bridge", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "172.30.0.0/16"}}}})
	if err := s.Reconcile(t.Context()); err == nil {
		t.Fatal("Reconcile worked")
	}
	if err := s.Client.RemoveNetwork(t.Context(), "someones-app"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.proxy.closed {
		t.Error("the routes stayed closed")
	}
	if !maps.Equal(s.table.Addresses, before) {
		t.Errorf("the table is %v, want %v", s.table.Addresses, before)
	}
	s.publishIngress(t.Context())
	if st := s.Ingress(); st.GetProblem() != dockwellv1.IngressProblem_INGRESS_PROBLEM_UNSPECIFIED {
		t.Errorf("the API still says %+v", st)
	}
}

// A subnet Docker can't give back for any other reason fails closed too.
func TestASubnetDockerRefusesFailsClosed(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.stopAndPrune(t, "web")
	s.engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/networks/create") {
			return errors.New("nothing doing")
		}
		return nil
	})
	err := s.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "nothing doing") || !s.proxy.closed {
		t.Errorf("Reconcile = %v, closed %v", err, s.proxy.closed)
	}
}

// A network that isn't on the subnet the routes dial is as bad as none.
func TestANetworkOnAnotherSubnetFailsClosed(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.stopAndPrune(t, "web")
	s.engine.AddNetwork(docker.Network{Name: docker.IngressNetwork, Driver: "bridge", Labels: networkLabels, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "10.99.0.0/16"}}}})
	if err := s.Reconcile(t.Context()); err == nil || !strings.Contains(err.Error(), "10.99.0.0/16") || !s.proxy.closed {
		t.Errorf("Reconcile = %v, closed %v", err, s.proxy.closed)
	}
}

// docker network prune leaves a network with a running container on it, which is
// what the keeper is for: the rest is for when it's removed on purpose.
func TestTheKeeperKeepsTheNetworkFromBeingPruned(t *testing.T) {
	s := newSetup(t)
	const image = "caddy@sha256:abc"
	s.KeeperImage = image
	s.engine.Registry[image] = dockertest.Image{}
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	keeper := s.engine.Container(KeeperName)
	if keeper == nil || !keeper.State.Running {
		t.Fatalf("the keeper is %+v", keeper)
	}
	if keeper.Config.Image != image || keeper.HostConfig.NetworkMode != docker.IngressNetwork || keeper.Config.User == "" || keeper.Config.User == "0:0" {
		t.Errorf("the keeper runs as %+v, %+v", keeper.Config, keeper.HostConfig)
	}
	if !slices.Equal(keeper.HostConfig.CapDrop, []string{"ALL"}) || !keeper.HostConfig.ReadonlyRootfs || keeper.HostConfig.Memory == 0 || keeper.HostConfig.PidsLimit == 0 || len(keeper.Mounts) != 0 {
		t.Errorf("the keeper can do more than sleep: %+v", keeper.HostConfig)
	}
	ep := keeper.NetworkSettings.Networks[docker.IngressNetwork]
	want, _ := keeperAddress(s.table.Subnet)
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != want {
		t.Fatalf("the keeper is on the network as %+v, want %s", ep, want)
	}
	if _, taken := s.table.instanceAt(want); taken {
		t.Errorf("an instance has the keeper's address, %s", want)
	}
	// With every service stopped, the network can't be removed.
	s.engine.Stop(docker.Project(envID) + "-web-1")
	if err := s.Client.RemoveNetwork(t.Context(), docker.IngressNetwork); err == nil {
		t.Error("the network was removed with the keeper on it")
	}
	// Someone stops it, and it comes back.
	s.engine.Stop(KeeperName)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if k := s.engine.Container(KeeperName); k == nil || !k.State.Running {
		t.Errorf("the keeper is %+v after a reconcile", k)
	}
	// Someone removes it and the network: both come back, on the subnet.
	subnet := s.table.Subnet
	s.engine.Stop(KeeperName)
	if err := s.Client.RemoveNetwork(t.Context(), docker.IngressNetwork); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if k := s.engine.Container(KeeperName); k == nil || !k.State.Running {
		t.Errorf("after losing its network, the keeper is %+v", k)
	}
	if n := s.engine.Network(docker.IngressNetwork); n == nil || n.IPAM.Config[0].Subnet != subnet {
		t.Errorf("the network is %+v", n)
	}
	// The last route takes it all away, so the network can go.
	s.apply(t, "a", nil)
	if s.engine.Container(KeeperName) != nil {
		t.Error("the keeper stayed after the last route")
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("the network stayed after the last route")
	}
}

func TestTheKeepersAddressIsTheLastHost(t *testing.T) {
	for subnet, want := range map[string]string{"172.30.0.0/16": "172.30.255.254", "10.1.2.0/24": "10.1.2.254", "192.168.0.0/30": "192.168.0.2"} {
		if got, ok := keeperAddress(subnet); !ok || got != want {
			t.Errorf("keeperAddress(%s) = %s, %v, want %s", subnet, got, ok, want)
		}
	}
	for _, subnet := range []string{"", "nonsense", "fd00::/64", "10.0.0.1/32"} {
		if got, ok := keeperAddress(subnet); ok {
			t.Errorf("keeperAddress(%s) = %s", subnet, got)
		}
	}
}

// A reboot brought another proxy back, and it bound first. The agent says who
// has the ports, each time on the API and when it changes in the log, and doesn't
// make Caddy again until they're free, which would only churn.
func TestAProxyThatCameBackIsNamedAndNotFoughtWith(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true}
	s.proxy.log = nil
	s.proc.listen("tcp", 80, "0A", "9001")
	s.proc.listen("tcp", 443, "0A", "9002")
	id := s.engine.AddContainer(docker.Container{Name: "/1Panel-openresty-abc12", State: docker.State{Running: true, Status: "running"}}).ID
	s.proc.process(4242, "nginx", "0::/system.slice/docker-"+id+".scope", "9001", "9002")

	err := s.Reconcile(t.Context())
	if err == nil {
		t.Fatal("Reconcile says nothing")
	}
	for _, want := range []string{"nginx", "1Panel-openresty-abc12", "docker update --restart=no 1Panel-openresty-abc12", "docker stop 1Panel-openresty-abc12", "routes don't answer", "within a minute"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error doesn't say %q: %v", want, err)
		}
	}
	if slices.Contains(s.proxy.log, "start") {
		t.Errorf("the proxy was started with the ports taken: %v", s.proxy.log)
	}
	s.publishIngress(t.Context())
	st := s.Ingress()
	if st.GetProblem() != dockwellv1.IngressProblem_INGRESS_PROBLEM_PORTS_TAKEN || !slices.Equal(st.GetHolders(), []string{"1Panel-openresty-abc12"}) || !strings.Contains(st.GetMessage(), "docker update") {
		t.Errorf("the API says %+v", st)
	}

	// It goes on trying, and takes the ports once they're free.
	if err := s.Client.RemoveContainer(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	s.proc = newProc(t)
	s.Holders = holdersOn(t, s.engine, s.proc)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.proxy.log, "start") {
		t.Errorf("the proxy wasn't started with the ports free: %v", s.proxy.log)
	}
	s.publishIngress(t.Context())
	if st := s.Ingress(); st.GetProblem() != dockwellv1.IngressProblem_INGRESS_PROBLEM_UNSPECIFIED {
		t.Errorf("the API still says %+v", st)
	}
}

// With the panel's domain behind Caddy, that's down too, and the error says so.
func TestAProxyThatCameBackSaysThePanelsDomainIsDown(t *testing.T) {
	s := newSetup(t)
	s.Panel = &Panel{Domain: "panel.example.com", Release: func(context.Context) error { return nil }, Retake: func(context.Context) error { return nil }}
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true}
	s.proc.listen("tcp", 443, "0A", "9001")
	s.proc.process(4242, "nginx", "0::/", "9001")
	err := s.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "panel.example.com") || !strings.Contains(err.Error(), "SSH tunnel") {
		t.Errorf("Reconcile = %v", err)
	}
}

// A failure that doesn't change is said once, and again after a long while.
func TestReconcilingSaysAFailureOnce(t *testing.T) {
	s := newSetup(t)
	var logs bytes.Buffer
	var mu sync.Mutex
	s.Logger = slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(b)
	}), nil))
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true}
	s.proc.listen("tcp", 80, "0A", "9001")
	s.proc.process(4242, "nginx", "0::/", "9001")
	ctx, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()
	s.KeepReconciled(ctx, 10*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logs.String(), "can't put the ingress as its routes need it"); n != 1 {
		t.Errorf("it said so %d times:\n%s", n, logs.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// Each restart policy as Moby's restart manager reads it. A manual stop lasts
// only until the daemon restarts for always, which is 1Panel's OpenResty.
func TestAPlanNamesWhatComesBackAfterAReboot(t *testing.T) {
	s := newSetup(t)
	add := func(name string, policy string, exit int, network string, ports map[string][]docker.PortBinding) {
		s.engine.AddContainer(docker.Container{
			Name: "/" + name, State: docker.State{Status: "exited", ExitCode: exit},
			HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: policy}, NetworkMode: network, PortBindings: ports},
		})
	}
	on := func(key, port string) map[string][]docker.PortBinding {
		return map[string][]docker.PortBinding{key: {{HostPort: port}}}
	}
	add("openresty", "always", 137, "host", nil)
	add("publishes-443", "always", 0, "bridge", on("8443/tcp", "443"))
	add("publishes-udp", "always", 0, "bridge", on("443/udp", "443"))
	add("on-failure-killed", "on-failure", 137, "bridge", on("80/tcp", "80"))
	add("on-failure-clean", "on-failure", 0, "bridge", on("80/tcp", "80"))
	add("stopped-by-hand", "unless-stopped", 137, "bridge", on("80/tcp", "80"))
	add("no-policy", "no", 0, "bridge", on("80/tcp", "80"))
	add("another-port", "always", 0, "bridge", on("80/tcp", "8080"))
	add("udp-80", "always", 0, "bridge", on("80/udp", "80"))
	s.engine.AddContainer(docker.Container{
		Name: "/" + ContainerName, State: docker.State{Status: "exited"}, Config: docker.Config{Labels: map[string]string{docker.OwnPrefix + "ingress": "caddy"}},
		HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "always"}, NetworkMode: "host"},
	})

	ref := routeRef("www")
	route := spec("a.example.com", "", "web", 80)
	desired := []resource.Desired{{Ref: ref, Spec: route}}
	got := s.Returning(t.Context(), ref, route, desired)
	want := []string{"on-failure-killed", "openresty", "publishes-443", "publishes-udp"}
	if !slices.Equal(got, want) {
		t.Errorf("Returning = %v, want %v", got, want)
	}
	// Once a plan, on its first route. A removal has nothing to warn of.
	second := routeRef("api")
	desired = append(desired, resource.Desired{Ref: second, Spec: route})
	if got := s.Returning(t.Context(), second, route, desired); got != nil {
		t.Errorf("the second route warns too: %v", got)
	}
	if got := s.Returning(t.Context(), ref, nil, []resource.Desired{{Ref: ref}}); got != nil {
		t.Errorf("a removal warns: %v", got)
	}
}

// A daemon that sets no-new-privileges for every container isn't refused: Caddy's
// container says otherwise for itself, as Moby lets it (TestCaddySaysNoNewPrivileges
// holds what it asks for), and the keeper needs nothing it would take away.
func TestADaemonThatSetsNoNewPrivilegesRunsRoutes(t *testing.T) {
	s := newSetup(t)
	s.engine.NoNewPrivileges = true
	ref := routeRef("www")
	route := spec("a.example.com", "", "web", 80)
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: route}}); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}
	s.apply(t, "www", route)
	if st, _ := s.proxy.Status(t.Context()); !st.Running {
		t.Error("the proxy didn't start")
	}
}

// The keeper only sleeps, so it keeps no-new-privileges, whatever Caddy needs.
func TestTheKeeperKeepsNoNewPrivileges(t *testing.T) {
	s := newSetup(t)
	const image = "caddy@sha256:abc"
	s.KeeperImage = image
	s.engine.Registry[image] = dockertest.Image{}
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	keeper := s.engine.Container(KeeperName)
	if keeper == nil || !slices.Contains(keeper.HostConfig.SecurityOpt, "no-new-privileges") {
		t.Fatalf("the keeper has %+v", keeper)
	}
}

// A plan that didn't know the panel moves can't move it: it got no person's
// approval, as a server that wasn't reachable made none of it clear.
func TestThePanelMovesOnlyForAPlanThatSaidSo(t *testing.T) {
	s := newSetup(t)
	released := false
	s.Panel = &Panel{Domain: "panel.example.com", Release: func(context.Context) error { released = true; return nil }, Retake: func(context.Context) error { return nil }}
	err := s.Apply(t.Context(), routeRef("www"), spec("a.example.com", "", "web", 80), nil)
	if err == nil || !strings.Contains(err.Error(), "panel.example.com") || released {
		t.Errorf("Apply = %v, released %v", err, released)
	}
	if routes, _ := s.proxy.Routes(t.Context()); len(routes) != 0 {
		t.Errorf("routes %+v", routes)
	}
}

// What moves behind the proxy can't go dark: the panel's certificate goes with it.
func TestThePanelsCertificateGoesToTheProxy(t *testing.T) {
	s := newSetup(t)
	cert := &PanelCert{Chain: []byte("chain"), Key: []byte("key")}
	s.Panel = &Panel{
		Domain: "panel.example.com", Release: func(context.Context) error { return nil }, Retake: func(context.Context) error { return nil },
		Certificate: func(context.Context) (*PanelCert, error) { return cert, nil },
	}
	s.apply(t, "www", spec("a.example.com", "", "web", 80))
	if s.proxy.panel == nil || s.proxy.panel.Cert != cert {
		t.Errorf("the proxy has %+v", s.proxy.panel)
	}
	// One it can't read doesn't stop the proxy.
	s.Panel.Certificate = func(context.Context) (*PanelCert, error) { return nil, errors.New("disk") }
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.proxy.panel == nil || s.proxy.panel.Cert != nil {
		t.Errorf("the proxy has %+v", s.proxy.panel)
	}
}

// One server takes 1,000 routes. Removing or editing past that always works.
func TestAServerTakesAtMostItsLimitOfRoutes(t *testing.T) {
	s := newSetup(t)
	s.proxy.status = Status{Exists: true, Running: true}
	for i := range MaxRoutes {
		s.proxy.routes = append(s.proxy.routes, Route{ID: RouteID(routeRef(fmt.Sprintf("r%d", i))), Domain: fmt.Sprintf("r%d.example.com", i)})
	}
	ref := routeRef("over")
	err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: spec("over.example.com", "", "web", 80)}})
	if err == nil || !strings.Contains(err.Error(), "1001 routes") || !strings.Contains(err.Error(), "at most 1000") || !strings.Contains(err.Error(), "Delete routes") {
		t.Errorf("Check = %v, want it refused, with what to do", err)
	}
	// An edit of a route it has adds nothing.
	existing := routeRef("r1")
	if err := s.Check(t.Context(), existing, []resource.Desired{{Ref: existing, Spec: spec("r1.example.com", "", "web", 8080)}}); err != nil {
		t.Errorf("an edit past the limit = %v", err)
	}
	// A plan that makes room for what it adds passes.
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: spec("over.example.com", "", "web", 80)}, {Ref: existing}}); err != nil {
		t.Errorf("a plan that deletes one and adds one = %v", err)
	}
	// A plan that passed swaps a route at the limit: its make runs before its
	// delete, as the engine orders them, and a step doesn't know of the delete.
	if err := s.Apply(t.Context(), ref, spec("over.example.com", "", "web", 80), nil); err != nil {
		t.Fatalf("the first step of a swap at the limit = %v", err)
	}
	if err := s.Apply(t.Context(), existing, nil, nil); err != nil {
		t.Fatalf("the second step of a swap at the limit = %v", err)
	}
	if routes, _ := s.proxy.Routes(t.Context()); len(routes) != MaxRoutes {
		t.Errorf("a swap leaves %d routes, want %d", len(routes), MaxRoutes)
	}
	// Plans made a while ago, which saw fewer than there are now, can't add up
	// without end.
	for i := range MaxRoutes {
		s.proxy.routes = append(s.proxy.routes, Route{ID: RouteID(routeRef(fmt.Sprintf("s%d", i))), Domain: fmt.Sprintf("s%d.example.com", i)})
	}
	late := routeRef("late")
	if err := s.Apply(t.Context(), late, spec("late.example.com", "", "web", 80), nil); err == nil || !strings.Contains(err.Error(), "at most 1000") {
		t.Errorf("Apply far past the limit = %v", err)
	}
}

// Fixing a typo in a domain, at the limit, adds a name and takes one away. A plan
// can do that as an edit, or as a new route and a delete.
func TestAPlanThatLeavesThePendingCountAsItIsPasses(t *testing.T) {
	s := newSetup(t)
	s.proxy.status = Status{Exists: true, Running: true}
	for i := range MaxPendingDomains {
		s.apply(t, fmt.Sprintf("r%d", i), spec(fmt.Sprintf("r%d.example.com", i), "", "web", 80))
	}
	ref := routeRef("r1")
	right := spec("right.example.com", "", "web", 80)
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: right}}); err != nil {
		t.Errorf("an edit of a domain at the limit = %v", err)
	}
	if err := s.Apply(t.Context(), ref, right, nil); err != nil {
		t.Errorf("applying the edit = %v", err)
	}
	// A swap, with its make before its delete.
	add, del := routeRef("new"), routeRef("r2")
	fresh := spec("new.example.com", "", "web", 80)
	if err := s.Check(t.Context(), add, []resource.Desired{{Ref: add, Spec: fresh}, {Ref: del}}); err != nil {
		t.Errorf("a swap of a domain at the limit = %v", err)
	}
	if err := s.Apply(t.Context(), add, fresh, nil); err != nil {
		t.Errorf("the first step of a swap at the limit = %v", err)
	}
}

// What someone adding too many domains at once reads says what is true, and what
// to do: a plan that alone is past the limit has nothing waiting to name.
func TestTooManyDomainsInOnePlanSaysToSplitIt(t *testing.T) {
	s := newSetup(t)
	s.proxy.status = Status{Exists: true, Running: true}
	var many []resource.Desired
	for i := range 60 {
		many = append(many, resource.Desired{Ref: routeRef(fmt.Sprintf("m%d", i)), Spec: spec(fmt.Sprintf("m%d.example.com", i), "", "web", 80)})
	}
	err := s.Check(t.Context(), many[0].Ref, many)
	if err == nil {
		t.Fatal("60 new domains went in")
	}
	for _, want := range []string{"adds 60 new domains", "at most 50", "Split it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("it says %q, want %q in it", err, want)
		}
	}
	for _, not := range []string{"0 domains", "such as", "none"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("it says %q, which has %q, of what doesn't apply", err, not)
		}
	}
}

// Each domain waiting for a certificate may be costing orders at the CA, and a
// burst of names that don't resolve could use up the server's limit, the panel's
// domain too. One that stays failed mustn't block others for ever.
func TestPendingDomainsLimitNewOnes(t *testing.T) {
	s := newSetup(t)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	s.proxy.status = Status{Exists: true, Running: true}
	if s.proxy.certs == nil {
		s.proxy.certs = map[string]CertState{}
	}
	check := func(ref resource.Ref, domain string) error {
		return s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: spec(domain, "", "web", 80)}})
	}
	for i := range MaxPendingDomains {
		s.apply(t, fmt.Sprintf("r%d", i), spec(fmt.Sprintf("r%d.example.com", i), "", "web", 80))
	}
	ref := routeRef("over")
	err := check(ref, "over.example.com")
	if err == nil || !strings.Contains(err.Error(), "50 domains") || !strings.Contains(err.Error(), "limit of 50") ||
		!strings.Contains(err.Error(), "Point their DNS here") || !strings.Contains(err.Error(), "45 more") || !strings.Contains(err.Error(), "3 hours") {
		t.Errorf("Check = %v, want it refused, with what to do", err)
	}
	// A path under a domain it has asks the CA for nothing.
	if err := check(routeRef("api"), "r1.example.com"); err != nil {
		t.Errorf("a path under a domain it has = %v", err)
	}
	// One that gets its certificate makes room.
	s.proxy.certs["r1.example.com"] = CertState{Phase: CertIssued}
	if err := check(ref, "over.example.com"); err != nil {
		t.Errorf("with a certificate come = %v", err)
	}
	s.apply(t, "over", spec("over.example.com", "", "web", 80))
	if err := check(routeRef("over2"), "over2.example.com"); err == nil {
		t.Error("a 51st pending domain went in")
	}
	// A failed one still counts, as it may still cost orders, until its window.
	s.proxy.certs["r2.example.com"] = CertState{Phase: CertFailed, Problem: ProblemDNS}
	if err := check(routeRef("over2"), "over2.example.com"); err == nil {
		t.Error("a failed domain doesn't count")
	}
	// After which a domain that stays failed blocks nothing.
	clock = clock.Add(PendingWindow + time.Minute)
	if err := check(routeRef("over2"), "over2.example.com"); err != nil {
		t.Errorf("3 hours on = %v", err)
	}
	// One plan can't bring more than the limit in at once.
	var many []resource.Desired
	for i := range MaxPendingDomains + 1 {
		many = append(many, resource.Desired{Ref: routeRef(fmt.Sprintf("m%d", i)), Spec: spec(fmt.Sprintf("m%d.example.com", i), "", "web", 80)})
	}
	if err := s.Check(t.Context(), many[0].Ref, many); err == nil || !strings.Contains(err.Error(), "at most 50") {
		t.Errorf("a plan of 51 new domains = %v", err)
	}
}

// A step can go past the limit for what its plan removes later, and no further: a
// plan made a while ago saw fewer than there are now.
func TestPendingDomainsLimitStepsToo(t *testing.T) {
	s := newSetup(t)
	s.proxy.status = Status{Exists: true, Running: true}
	for i := range MaxPendingDomains * applyMargin {
		if err := s.Apply(t.Context(), routeRef(fmt.Sprintf("r%d", i)), spec(fmt.Sprintf("r%d.example.com", i), "", "web", 80), nil); err != nil {
			t.Fatal(err)
		}
	}
	ref := routeRef("over")
	if err := s.Apply(t.Context(), ref, spec("over.example.com", "", "web", 80), nil); err == nil || !strings.Contains(err.Error(), "limit of 50") {
		t.Errorf("Apply far past the limit = %v", err)
	}
}

// When a domain was added survives the agent, so a restart can't keep a domain
// that stays failed counting for ever.
func TestPendingSurvivesARestart(t *testing.T) {
	s := newSetup(t)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	clock = clock.Add(PendingWindow + time.Minute)
	again := &Manager{Client: s.Client, Proxy: s.Proxy, Dir: s.Dir, Holders: s.Holders, DNS: s.DNS, Now: s.Now}
	routes, _ := s.proxy.Routes(t.Context())
	pending, err := again.pendingDomains(t.Context(), routes)
	if err != nil || len(pending) != 0 {
		t.Errorf("after 3 hours and a restart, pending = %v, %v", pending, err)
	}
}

// A pending security update must be visible, as nothing moves a server to it but
// its next route change (issue #78).
func TestAnOutdatedProxyIsLoggedOnceAndOnTheAPI(t *testing.T) {
	s := newSetup(t)
	var logs bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true, Running: true, Outdated: true, Version: "2.11.6-alpine", Pinned: "2.11.7-alpine"}
	for range 3 {
		if err := s.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "may be missing security fixes"); n != 1 {
		t.Errorf("it said so %d times:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "2.11.6-alpine") || !strings.Contains(logs.String(), "2.11.7-alpine") {
		t.Errorf("the log doesn't name the versions: %s", logs.String())
	}
	s.publishIngress(t.Context())
	st := s.Ingress()
	if !st.GetOutdated() || st.GetVersion() != "2.11.6-alpine" || st.GetPinnedVersion() != "2.11.7-alpine" || !st.GetRunning() {
		t.Errorf("the API says %+v", st)
	}
	// A server with no routes has nothing to say.
	s.apply(t, "a", nil)
	s.publishIngress(t.Context())
	if st := s.Ingress(); st != nil {
		t.Errorf("with no routes, the API says %+v", st)
	}
}

// A plan on a server whose panel is behind its proxy already doesn't say the panel
// moves, with the proxy down for a moment or the agent just started, as that would
// need a person for a token's route plan. Taking the ports back, it still says.
func TestAPanelBehindItsProxyDoesntMoveAgain(t *testing.T) {
	s := newSetup(t)
	calls := &panelCalls{}
	s.Panel = calls.panel()
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true}
	ref := routeRef("a")
	want := spec("a.example.com", "", "web", 8080)
	effect, _, err := s.Judge(t.Context(), ref, want, []resource.Desired{{Ref: ref, Spec: want}})
	if err != nil {
		t.Fatal(err)
	}
	if effect == nil || effect.Change != resource.IngressStarts || effect.PanelDomain != "" {
		t.Fatalf("the proxy down, with the panel behind it, plans %+v, want it to start with nothing about the panel", effect)
	}
	// As an agent just started: the marker says so before anything else has.
	s.handed = false
	effect, _, err = s.Judge(t.Context(), ref, want, []resource.Desired{{Ref: ref, Spec: want}})
	if err != nil || effect == nil || effect.PanelDomain != "" {
		t.Fatalf("a new agent with the marker there plans %+v, %v", effect, err)
	}
	// And the step applies, on a plan that rightly didn't say so.
	if err := s.Apply(t.Context(), ref, want, effect); err != nil {
		t.Errorf("Apply = %v", err)
	}
	// A plan for the last route says the panel takes the ports back.
	effect, _, err = s.Judge(t.Context(), ref, nil, []resource.Desired{{Ref: ref}})
	if err != nil || effect == nil || effect.Change != resource.IngressStops || effect.PanelDomain != "panel.example.com" {
		t.Errorf("the last route plans %+v, %v", effect, err)
	}
}

// A new release's pin moves nothing without a plan. The keeper goes on sleeping on
// what it has, a reconcile never pulls, and a plan isn't held up by a pull.
func TestANewPinLeavesTheKeeperAlone(t *testing.T) {
	s := newSetup(t)
	const oldImage, newImage = "caddy@sha256:old", "caddy@sha256:new"
	s.engine.Registry[oldImage] = dockertest.Image{}
	s.engine.Registry[newImage] = dockertest.Image{}
	s.KeeperImage = oldImage
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	before := s.engine.Container(KeeperName)
	if before == nil {
		t.Fatal("no keeper")
	}
	var pulls int
	s.engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/images/create") {
			pulls++
			return errors.New("the registry can't be reached")
		}
		return nil
	})
	defer s.engine.Intercept(nil)
	s.KeeperImage = newImage
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pulls != 0 {
		t.Errorf("a reconcile pulled %d times, with the manager's lock held", pulls)
	}
	after := s.engine.Container(KeeperName)
	if after == nil || after.ID != before.ID || !after.State.Running {
		t.Fatalf("the keeper was %+v, and is %+v after a reconcile on a new pin", before, after)
	}
	// Stopped, it starts again as it was.
	s.engine.Stop(KeeperName)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if k := s.engine.Container(KeeperName); k == nil || k.ID != before.ID || !k.State.Running {
		t.Errorf("a stopped keeper is %+v after a reconcile, want it started as it was", k)
	}
	// Gone, it comes back on what the server has, and no pull.
	s.proxy.status.Image = oldImage
	if err := s.Client.RemoveContainer(t.Context(), before.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if k := s.engine.Container(KeeperName); k == nil || !k.State.Running || k.Config.Image != oldImage {
		t.Errorf("a removed keeper is %+v after a reconcile, want it back on what the proxy runs", k)
	}
	if pulls != 0 {
		t.Errorf("a reconcile pulled %d times", pulls)
	}
	// A route's step, under a plan, is what moves it.
	s.engine.Intercept(nil)
	s.apply(t, "a", spec("a.example.com", "", "web", 8080))
	if k := s.engine.Container(KeeperName); k == nil || !k.State.Running || k.Config.Image != newImage {
		t.Errorf("after a route's step the keeper is %+v, want it on the new pin", k)
	}
}

// A plan isn't held up by a keeper's image: nothing pulls under the lock.
func TestAPullThatHangsDoesntReachAReconcile(t *testing.T) {
	s := newSetup(t)
	const oldImage, newImage = "caddy@sha256:old", "caddy@sha256:new"
	s.engine.Registry[oldImage] = dockertest.Image{}
	s.KeeperImage = oldImage
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	release := make(chan struct{})
	defer close(release)
	s.engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/images/create") {
			<-release
		}
		return nil
	})
	defer s.engine.Intercept(nil)
	s.KeeperImage = newImage
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("a reconcile with a registry that hangs = %v", err)
	}
	ref := routeRef("b")
	if err := s.Check(ctx, ref, []resource.Desired{{Ref: ref, Spec: spec("b.example.com", "", "web", 80)}}); err != nil {
		t.Errorf("a plan after it = %v", err)
	}
}

// A reconcile doesn't move a running keeper even with the new pin's image on the
// server: only a route's step, under a plan, moves anything.
func TestAReconcileDoesntMoveAKeeperWithTheImageHere(t *testing.T) {
	s := newSetup(t)
	const oldImage, newImage = "caddy@sha256:old", "caddy@sha256:new"
	s.engine.Registry[oldImage] = dockertest.Image{}
	s.engine.Registry[newImage] = dockertest.Image{}
	s.KeeperImage = oldImage
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	before := s.engine.Container(KeeperName)
	if before == nil {
		t.Fatal("no keeper")
	}
	if err := s.Client.PullImage(t.Context(), newImage, func(string) {}); err != nil {
		t.Fatal(err)
	}
	s.KeeperImage = newImage
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if k := s.engine.Container(KeeperName); k == nil || k.ID != before.ID || k.Config.Image != oldImage {
		t.Errorf("a reconcile moved the keeper to %+v", k)
	}
}
