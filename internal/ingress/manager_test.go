// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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

func (p *fakeProxy) Start(_ context.Context, panel *PanelSite) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "start")
	if p.onStart != nil {
		p.onStart()
	}
	if p.startErr != nil {
		return p.startErr
	}
	p.status, p.panel = Status{Exists: true, Running: true}, panel
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

func (p *fakeProxy) DeleteRoute(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.status.Running {
		return errors.New("caddy isn't running")
	}
	p.log = append(p.log, "delete "+id)
	p.routes = slices.DeleteFunc(p.routes, func(r Route) bool { return r.ID == id })
	return nil
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
	err := s.Apply(t.Context(), ref, r, nil)
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
	err := s.Apply(t.Context(), routeRef("www"), spec("app.example.com", "", "web", 80), nil)
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

// A route in a stopped proxy's last config still goes, through the proxy.
func TestRemovingARouteStartsAProxySomeoneStopped(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	s.apply(t, "b", spec("b.example.com", "", "web", 80))
	s.proxy.status = Status{Exists: true}
	if err := s.Apply(t.Context(), routeRef("a"), nil, nil); err != nil {
		t.Fatal(err)
	}
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || !s.proxy.status.Running {
		t.Errorf("routes %+v, status %+v", routes, s.proxy.status)
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

	// Reconciling leaves it for a person: drift is never silently reverted.
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.proxy.routes[0].Edited {
		t.Error("reconciling reverted a hand edit")
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
		a, err := tb.give(dir, Instance{Project: "p", Service: "s", Number: i}, "172.30.0.1")
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
