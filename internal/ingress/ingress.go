// SPDX-License-Identifier: AGPL-3.0-only

// Package ingress runs a server's routes on a proxy: what stays when a second
// proxy arrives. It reads who holds the server's public ports and what DNS
// says, keeps the addresses services are reached at, starts the proxy on a
// server's first route and stops it on its last, moves the panel behind it
// and back, and gives rollouts their upstreams. What it knows of a proxy is
// the Proxy interface; internal/ingress/caddy is the one there is. See
// docs/adr/0024-the-caddy-ingress-driver.md.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// Proxy is a process that holds a server's public ports and serves its
// routes. Every change to one keeps the connections it has.
type Proxy interface {
	// Name says what it is, for people, such as "Caddy".
	Name() string
	// Status says whether it exists and runs here. It works with the proxy
	// down, and reports an error only when the runtime it runs on can't say.
	Status(ctx context.Context) (Status, error)
	// Prepare gets what it needs to start, such as its image, with nothing
	// stopped yet, as that can take minutes. It says how it goes through
	// resource.Progress.
	Prepare(ctx context.Context, panel *PanelSite) error
	// Start makes it run, with its last config, or with panel's site and no
	// routes if it has none, and returns once it answers. A proxy of an older
	// version is replaced. plainOnly starts one with no config on its plain HTTP
	// port alone, for routes that take no certificate.
	Start(ctx context.Context, panel *PanelSite, plainOnly bool) error
	// Stop stops it and frees its ports, keeping its certificates.
	Stop(ctx context.Context) error
	// Routes returns the routes Dockwell gave it, as it has them now, or as
	// it last had them if it is down.
	Routes(ctx context.Context) ([]Route, error)
	// PutRoute adds a route, or changes the one with its ID.
	PutRoute(ctx context.Context, r Route) error
	// PutRoutes changes several routes as one change. One that changes only which
	// single upstream a route dials reloads nothing, and works with the proxy down.
	PutRoutes(ctx context.Context, routes []Route) error
	// DeleteRoute removes a route, if it has it. It works with the proxy down,
	// and doesn't start it: removing a route must always work, and starting it
	// may be just what another proxy on its ports stops.
	DeleteRoute(ctx context.Context, id string) error
	// Recover puts right a last config that a full disk or a power cut
	// damaged, and reports whether it did. It works with the proxy down, and
	// keeps what the proxy runs now for the next time.
	Recover(ctx context.Context) (bool, error)
	// SetClosed has every route answer 503, ahead of what it dials, or takes
	// that away, if it isn't so already. The routes themselves stay as they
	// are, certificates go on renewing, and the panel's site stays up. It works
	// with the proxy down, and a proxy that starts again stays as this left it.
	SetClosed(ctx context.Context, closed bool) error
	// SetPanel makes it serve the panel's site, or stop if panel is nil, if
	// it isn't already. It serves panel.Cert until it has its own certificate
	// for the domain, and drops that then.
	SetPanel(ctx context.Context, panel *PanelSite) error
	// InFlight says how many connections the proxy has open to an upstream, which
	// is above zero for as long as a request is on its way there, and for no longer
	// than a moment past that, in any config old or new.
	InFlight(ctx context.Context, upstream string) (int, error)
	// Certificates says where each domain's certificate stands. It works with
	// the proxy down.
	Certificates(ctx context.Context, domains []string) (map[string]CertState, error)
	// Retry starts a pending or failed certificate's next try now.
	Retry(ctx context.Context) error
}

// Status is whether a proxy exists and runs.
type Status struct {
	// Exists is true when it is on the server, running or not.
	Exists bool
	// Running is true when it runs.
	Running bool
	// Outdated is true when it isn't the version this build pins.
	Outdated bool
	// Version is the version it runs, and Pinned the one this build pins, for
	// people. Version is empty when it isn't known.
	Version, Pinned string
	// Image is what it runs, by reference, as a move to a newer version needs
	// to go back to.
	Image string
}

// Route is what a proxy serves. It holds what the proxy has, not intent:
// upstreams are addresses, and the service they belong to comes from the
// table of instances.
type Route struct {
	// ID names it in the proxy.
	ID string
	// Domain and Path are what it matches, Path empty for the whole domain.
	Domain, Path string
	// Upstreams are the addresses it sends to, as ip:port.
	Upstreams []string
	// Plain is true when TLS ends before this server: it is served over plain
	// HTTP alone, with no certificate and no redirect.
	Plain bool
	// TrustedProxies are the ranges whose X-Forwarded-For it believes, for a request
	// from one of them, and EdgeProxies those of a network whose CF-Connecting-IP it
	// believes. With neither, it believes nothing.
	TrustedProxies, EdgeProxies []string
	// PlainForAnyone is true for a Plain route that answers anyone. Without it, one
	// answers only what it trusts, or this server's own loopback if that's nothing.
	PlainForAnyone bool
	// Edited is true when it holds something Dockwell doesn't put there, as
	// after a hand edit, so what it reads as isn't the whole of it.
	Edited bool
}

// PanelSite is the panel's site on the proxy: its domain, sent to its socket,
// and the certificate the panel has for it, if it has one that hasn't expired.
type PanelSite struct {
	Domain string
	// Cert is served until the proxy has its own for the domain, so that moving
	// behind the proxy can't take a working panel dark. Nil means none.
	Cert *PanelCert
}

// PanelCert is a certificate and its key, in PEM.
type PanelCert struct {
	Chain, Key []byte
}

// Panel is the panel on this server, when it has a domain. The control plane
// gives the control server's own agent one; a server that joined has none.
type Panel struct {
	// Domain is the panel's domain.
	Domain string
	// Release stops the panel's listeners on ports 80 and 443, and Retake
	// starts them again. Both are nil when the panel holds neither port, as
	// with --https-listen on another, so there is nothing to hand over: the
	// proxy doesn't serve its site, and it stays as it is.
	Release, Retake func(ctx context.Context) error
	// Certificate returns the certificate the panel has, or nil if it has none
	// that serves. Nil means it can't say.
	Certificate func(ctx context.Context) (*PanelCert, error)
}

// Moves reports a panel that holds the ports the proxy needs.
func (p *Panel) Moves() bool { return p != nil && p.Release != nil }

// Manager runs a server's routes on a proxy, for the route kind and for
// rollouts. It is safe for concurrent use.
type Manager struct {
	Client *docker.Client
	Proxy  Proxy
	// DrainPoll is how often Drain looks at what is in flight: drainPoll when zero.
	DrainPoll time.Duration
	// Dir is the ingress's directory on the server.
	Dir string
	// Panel is the panel on this server, or nil.
	Panel *Panel
	// Ports are the ports the proxy holds, from the server's side, its plain HTTP
	// port first. Nil means DefaultPorts.
	Ports []Port
	// OwnPorts are the ports Dockwell listens on itself on this server, such as
	// the panel's and the agent listener's, which no route may send to.
	OwnPorts []int
	// Holders finds what holds a port. Nil means NewHolders' default, on this
	// server's /proc and the Docker Engine.
	Holders *Holders
	// DNS reads what a domain points at, and what this server's addresses
	// are. Nil means this server's resolver and interfaces.
	DNS *DNS
	// KeeperImage is the image the container that keeps the ingress network
	// from being pruned runs, which the server has already for its proxy. Empty
	// runs none.
	KeeperImage string
	// Logger hears what the manager does on its own time.
	Logger *slog.Logger
	// Now is the time. Nil means time.Now.
	Now func() time.Time

	mu    gate
	table table
	// handed says the panel's ports are with the proxy now.
	handed bool
	status statusBoard
	// wakes has Watch read again now, once wakesOnce has made it.
	wakes     chan struct{}
	wakesOnce sync.Once
	// rollouts counts the rollouts moving each service's instances, which Reconcile
	// leaves alone. It has a lock of its own, as asking takes no time and has no
	// context to give up with.
	rolloutsMu sync.Mutex
	rollouts   map[Service]int
}

var _ Upstreams = (*Manager)(nil)

// gate is a lock that a caller can give up waiting for. What holds it can be
// waiting on Docker or the proxy, and a plan has a deadline of its own, which a
// hung proxy must not take from it.
type gate struct {
	once sync.Once
	held chan struct{}
}

func (g *gate) lock(ctx context.Context) error {
	g.once.Do(func() { g.held = make(chan struct{}, 1) })
	select {
	case g.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the ingress is busy with something else on this server, so this stopped waiting for it. Try again; if it goes on, check that its container answers with docker logs %s: %w", ContainerName, ctx.Err())
	}
}

func (g *gate) unlock() { <-g.held }

func (m *Manager) ports() []Port {
	if m.Ports != nil {
		return m.Ports
	}
	return DefaultPorts
}

// plainPorts are what a proxy holds for routes that take no certificate: its
// plain HTTP port alone, as a tunnel or a proxy in front needs nothing else.
func (m *Manager) plainPorts() []Port { return m.ports()[:1] }

// portsFor are the ports a proxy holds, given whether anything it serves needs
// HTTPS. The panel's site does, wherever the proxy serves it.
func (m *Manager) portsFor(https bool) []Port {
	if https || m.Panel.Moves() {
		return m.ports()
	}
	return m.plainPorts()
}

// plainOnly reports ports that leave out what HTTPS needs.
func (m *Manager) plainOnly(ports []Port) bool { return len(minus(m.ports(), ports)) > 0 }

// minus is what of ports isn't in other.
func minus(ports, other []Port) []Port {
	return slices.DeleteFunc(slices.Clone(ports), func(p Port) bool { return slices.Contains(other, p) })
}

func (m *Manager) holders() *Holders {
	if m.Holders != nil {
		return m.Holders
	}
	return &Holders{Client: m.Client}
}

func (m *Manager) dns() *DNS {
	if m.DNS != nil {
		return m.DNS
	}
	return &DNS{}
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// panelSite is the panel's site on the proxy, if it is to serve one, with the
// certificate the panel has. A panel with no certificate to give is no reason to
// hold up a proxy, which gets its own.
func (m *Manager) panelSite(ctx context.Context) *PanelSite {
	if !m.Panel.Moves() {
		return nil
	}
	site := &PanelSite{Domain: m.Panel.Domain}
	if m.Panel.Certificate != nil {
		cert, err := m.Panel.Certificate(ctx)
		if err != nil {
			m.logger().WarnContext(ctx, "can't read the panel's certificate to serve it through the ingress, so the panel's domain doesn't answer over HTTPS until the ingress has its own; an SSH tunnel reaches it meanwhile",
				slog.String("domain", m.Panel.Domain), slog.String("error", err.Error()))
		}
		site.Cert = cert
	}
	return site
}

// activeFile is what says a server's ports are with its proxy, which
// dockwell run reads before it would take them for the panel. It is written
// before the proxy starts, and removed after it stops.
const activeFile = "active"

// Active reports a server whose ports are with its proxy, by the ingress's
// directory: dockwell run then leaves them alone, whether the proxy is up or
// not, as it would take them from Caddy when Docker starts Caddy again.
func Active(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, activeFile))
	return err == nil
}

func (m *Manager) setActive(on bool) error {
	path := filepath.Join(m.Dir, activeFile)
	if !on {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't record that the ingress stopped: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", m.Dir, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("can't record that the ingress runs: %w", err)
	}
	return nil
}

// RouteID is what names a route in a proxy, from its resource.
func RouteID(ref resource.Ref) string {
	return "dockwell." + docker.Project(ref.Scope) + "." + ref.Name
}

// serviceOf is the service a route's instance belongs to.
func serviceOf(ref resource.Ref, spec *resourcev1.Route) Service {
	return Service{Project: docker.Project(ref.Scope), Name: spec.GetService()}
}

// Observe returns a route as the proxy has it, or nil if it has none. Its
// upstreams read back through the table of instances, as the service and the
// port they are; one the table doesn't know, or a route Dockwell didn't write
// that way, reads as a difference (ADR 0024), never as what was asked for.
func (m *Manager) Observe(ctx context.Context, ref resource.Ref) (*resourcev1.Route, error) {
	if err := m.mu.lock(ctx); err != nil {
		return nil, err
	}
	defer m.mu.unlock()
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil, err
	}
	id := RouteID(ref)
	i := slices.IndexFunc(routes, func(r Route) bool { return r.ID == id })
	if i < 0 {
		return nil, nil
	}
	r := routes[i]
	out := &resourcev1.Route{Domain: r.Domain, Path: r.Path, TrustedProxies: proxyNames(r.TrustedProxies, r.EdgeProxies), PlainForAnyone: r.PlainForAnyone}
	if r.Plain {
		out.Tls = TLSOff
	}
	if r.Edited {
		out.Service = editedByHand
		return out, nil
	}
	if err := m.table.load(m.Dir); err != nil {
		return nil, err
	}
	for _, up := range r.Upstreams {
		host, port, err := splitUpstream(up)
		inst, known := m.table.instanceAt(host)
		switch {
		case err == nil && !known && host == ServerHost && len(r.Upstreams) == 1:
			out.HostPort, out.Service = port, m.table.Owners[id]
			continue
		case err != nil || !known || inst.Project != docker.Project(ref.Scope):
			out.Service, out.Port = up, 0
			return out, nil
		case out.Service != "" && (out.Service != inst.Service || out.Port != port):
			out.Service, out.Port = up, 0
			return out, nil
		}
		out.Service, out.Port = inst.Service, port
	}
	return out, nil
}

// ServerHost is what a route to a port on the server itself dials, from the
// proxy on the server's own network.
const ServerHost = "127.0.0.1"

// editedByHand is what a route reads as in place of its service, when its
// config holds what Dockwell doesn't put there.
const editedByHand = "(edited by hand)"

func splitUpstream(up string) (string, uint32, error) {
	i := strings.LastIndexByte(up, ':')
	if i < 0 {
		return "", 0, fmt.Errorf("%s has no port", up)
	}
	p, err := strconv.ParseUint(up[i+1:], 10, 16)
	if err != nil {
		return "", 0, err
	}
	return up[:i], uint32(p), nil
}

// routesAfter counts this server's routes once a plan applies, and says which
// of the plan's route items is the first to make or edit one and which is the
// last to delete one, in the order the steps run: makes and edits in order,
// then deletes last first.
func routesAfter(have []Route, desired []resource.Desired) (left int, first, last resource.Ref) {
	in := map[string]bool{}
	for _, d := range desired {
		if d.Ref.Kind != routeKind {
			continue
		}
		in[RouteID(d.Ref)] = true
		if d.Spec != nil {
			left++
			if first == (resource.Ref{}) {
				first = d.Ref
			}
		} else if last == (resource.Ref{}) {
			last = d.Ref
		}
	}
	for i := range have {
		if !in[have[i].ID] {
			left++
		}
	}
	return left, first, last
}

// routeKind is the route kind, which internal/resource/route names. That
// package names no proxy, and this one needs none of it but this.
const routeKind resource.Kind = "route"

// routeSpec is a plan's item as a route, or nil for one that isn't, or that
// deletes.
func routeSpec(d resource.Desired) *resourcev1.Route {
	if d.Ref.Kind != routeKind {
		return nil
	}
	spec, ok := d.Spec.(*resourcev1.Route)
	if !ok {
		return nil
	}
	return spec
}

// needsHTTPS reports routes that need a certificate once a plan applies: those it
// makes or edits, and those it leaves. A route edited by hand counts, as nothing
// here can say what it needs.
func needsHTTPS(have []Route, desired []resource.Desired) bool {
	in := map[string]bool{}
	for _, d := range desired {
		if d.Ref.Kind != routeKind {
			continue
		}
		in[RouteID(d.Ref)] = true
		if spec := routeSpec(d); spec != nil && spec.GetTls() != TLSOff {
			return true
		}
	}
	return slices.ContainsFunc(have, func(r Route) bool { return !in[r.ID] && !r.Plain })
}

// Judge says what a step does to the ingress, given all the plan touches on
// this server, and what DNS says of the route's domain.
func (m *Manager) Judge(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, desired []resource.Desired) (*resource.IngressEffect, *resource.DomainCheck, error) {
	if err := m.mu.lock(ctx); err != nil {
		return nil, nil, err
	}
	defer m.mu.unlock()
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return nil, nil, err
	}
	have, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil, nil, err
	}
	left, first, last := routesAfter(have, desired)
	held, after := m.portsFor(needsHTTPS(have, nil)), m.portsFor(needsHTTPS(have, desired))
	proxy := m.Proxy.Name()
	var effect *resource.IngressEffect
	switch {
	case spec != nil && !st.Running && first == ref:
		effect = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: proxy, Ports: portNames(after)}
	case spec != nil && st.Outdated && first == ref:
		effect = &resource.IngressEffect{Change: resource.IngressRestarts, Proxy: proxy}
	case spec == nil && left == 0 && last == ref:
		effect = &resource.IngressEffect{Change: resource.IngressStops, Proxy: proxy, Ports: portNames(held)}
	case st.Running && left > 0 && (first == ref || (first == resource.Ref{} && last == ref)):
		if more := minus(after, held); len(more) > 0 {
			effect = &resource.IngressEffect{Change: resource.IngressTakesPorts, Proxy: proxy, Ports: portNames(more)}
		} else if less := minus(held, after); len(less) > 0 {
			effect = &resource.IngressEffect{Change: resource.IngressFreesPorts, Proxy: proxy, Ports: portNames(less)}
		}
	}
	if effect != nil && m.Panel.Moves() {
		// Starting again behind the proxy it is behind already moves nothing, so a
		// brief outage doesn't turn a token's route plan into a person's.
		starting := effect.Change == resource.IngressStarts && !m.behindProxy()
		if starting || effect.Change == resource.IngressStops {
			effect.PanelDomain = m.Panel.Domain
		}
	}
	// A domain behind a tunnel or a proxy points at that, so DNS has nothing to
	// say of a certificate that nothing asks for.
	var check *resource.DomainCheck
	if spec != nil && spec.GetTls() != TLSOff {
		check = m.dns().Check(ctx, spec.GetDomain())
	}
	return effect, check, nil
}

// behindProxy reports a panel whose ports are with the proxy, whether or not the
// proxy is up: dockwell run left them alone if the marker was there as it started.
func (m *Manager) behindProxy() bool { return m.handed || Active(m.Dir) }

// Goes says what someone visiting a route finds once the plan takes it away, from
// what stays on this server. Nil when the proxy doesn't have it.
func (m *Manager) Goes(ctx context.Context, ref resource.Ref, desired []resource.Desired) *resource.RouteGoes {
	if err := m.mu.lock(ctx); err != nil {
		return nil
	}
	defer m.mu.unlock()
	have, err := m.Proxy.Routes(ctx)
	if err != nil {
		return nil
	}
	id := RouteID(ref)
	i := slices.IndexFunc(have, func(r Route) bool { return r.ID == id })
	if i < 0 {
		return nil
	}
	gone := have[i]
	goes := &resource.RouteGoes{Where: gone.Domain + gone.Path, Domain: gone.Domain}
	left, _, _ := routesAfter(have, desired)
	sameDomain := false
	in := map[string]bool{}
	for _, d := range desired {
		if d.Ref.Kind != routeKind {
			continue
		}
		in[RouteID(d.Ref)] = true
		if spec := routeSpec(d); spec != nil && spec.GetDomain() == gone.Domain {
			sameDomain = true
		}
	}
	for k := range have {
		if !in[have[k].ID] && have[k].Domain == gone.Domain {
			sameDomain = true
		}
	}
	switch {
	case left == 0 && gone.Plain:
		goes.Find = resource.VisitorsNothingBehind
	case left == 0:
		goes.Find = resource.VisitorsRefused
	case sameDomain:
		goes.Find = resource.VisitorsRest
	case gone.Plain:
		goes.Find = resource.VisitorsNotFound
	case needsHTTPS(have, desired):
		goes.Find = resource.VisitorsTLSError
	default:
		// What stays takes no certificate, so nothing holds 443 any more.
		goes.Find = resource.VisitorsRefused
	}
	return goes
}

// Returning names what the plan should warn of on this server: stopped containers
// that Docker starts again when it next restarts, and that may take the ports
// from the proxy, once, on the plan's first route.
func (m *Manager) Returning(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, desired []resource.Desired) []string {
	if spec == nil {
		return nil
	}
	if _, first, _ := routesAfter(nil, desired); first != ref {
		return nil
	}
	ports := m.ports()
	if have, err := m.Proxy.Routes(ctx); err == nil {
		ports = m.portsFor(needsHTTPS(have, desired))
	}
	return m.holders().Returning(ctx, ports)
}

// Check refuses what stops a plan's routes running on this server: another
// proxy holding its public ports, as it needs them when it starts. It runs as
// a plan is made, and again as a step applies, as the ports can be taken in
// between (ADR 0024).
func (m *Manager) Check(ctx context.Context, _ resource.Ref, desired []resource.Desired) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	if err := m.checkLimits(ctx, desired, 1); err != nil {
		return err
	}
	have, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	if err := m.checkHostPorts(ctx, desired); err != nil {
		return err
	}
	if err := m.checkServersNetwork(ctx, desired); err != nil {
		return err
	}
	return m.checkPorts(ctx, have, m.portsFor(needsHTTPS(have, desired)))
}

// serversNetwork says why a route can't send to a service on the server's own
// network by its name, and what can: Docker can't put a container there on any
// other network, so the proxy has no address for it (ADR 0030).
func serversNetwork(service string) error {
	return fmt.Errorf("the service %[1]s runs on the server's own network, and Docker can't put a container there on the ingress's as well, so a route can't send to it by name. "+
		"Send the route to the port %[1]s listens on with host_port, or take %[1]s off host_network", service)
}

// checkServersNetwork refuses a route to a service on the server's own network. What
// the plan runs says first, as a service in it may be going onto that network or
// coming off it, and then what the server runs.
func (m *Manager) checkServersNetwork(ctx context.Context, desired []resource.Desired) error {
	planned := map[Service]bool{}
	for _, d := range desired {
		if spec, ok := d.Spec.(*resourcev1.Service); ok {
			planned[Service{Project: docker.Project(d.Ref.Scope), Name: d.Ref.Name}] = spec.GetApp().GetHostNetwork()
		}
	}
	for _, d := range desired {
		spec := routeSpec(d)
		if spec == nil || spec.GetHostPort() != 0 {
			continue
		}
		s := serviceOf(d.Ref, spec)
		on, inPlan := planned[s]
		if !inPlan {
			c, err := m.newestOf(ctx, s)
			switch {
			case err != nil:
				return err
			case c == nil:
				continue
			}
			on = c.HostConfig.NetworkMode == hostNetworkMode
		}
		if on {
			return serversNetwork(s.Name)
		}
	}
	return nil
}

// RoutedAfter reports a route that still sends to an instance's service once the
// plan under way applies. Those the plan touches say for themselves, in Check.
func (m *Manager) RoutedAfter(ctx context.Context, i Instance, desired []resource.Desired) (bool, error) {
	if err := m.mu.lock(ctx); err != nil {
		return false, err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return false, err
	}
	have, err := m.Proxy.Routes(ctx)
	if err != nil {
		return false, err
	}
	in := map[string]bool{}
	for _, d := range desired {
		if d.Ref.Kind == routeKind {
			in[RouteID(d.Ref)] = true
		}
	}
	for k := range have {
		r := &have[k]
		if in[r.ID] {
			continue
		}
		for _, up := range r.Upstreams {
			if got, known := m.instanceOf(up); known && got.service() == i.service() {
				return true, nil
			}
		}
	}
	return false, nil
}

// Listening names what listens on a port on the server, for a plan to say, or says
// nothing does. With neither, it couldn't say, and it never stops a plan.
func (m *Manager) Listening(ctx context.Context, port uint32) (who string, nothing bool) {
	held, err := m.holders().Dialled(ctx, []Port{{Number: int(port)}})
	if err != nil {
		return "", false
	}
	if len(held) == 0 {
		return "", true
	}
	return held[0].Words, false
}

// checkHostPorts refuses a route to a port on the server that is Dockwell's own:
// the panel's and its other listeners, or the proxy's, which would dial itself. It
// refuses Docker's own listener on any port too: route.Validate refuses 2375 and
// 2376 by number, and dockerd can be told to listen anywhere.
func (m *Manager) checkHostPorts(ctx context.Context, desired []resource.Desired) error {
	for _, d := range desired {
		spec := routeSpec(d)
		if spec == nil || spec.GetHostPort() == 0 {
			continue
		}
		port := int(spec.GetHostPort())
		if slices.Contains(m.OwnPorts, port) {
			return fmt.Errorf("port %d on this server is one Dockwell listens on itself, and a route to it would open the panel or the agent's own listener to the internet, past what guards them. Choose the port of what you mean to serve", port)
		}
		if slices.ContainsFunc(m.ports(), func(p Port) bool { return p.Number == port && !p.UDP }) {
			return fmt.Errorf("port %d on this server is the one %s listens on, so a route to it would send requests round in a circle. Choose the port of what you mean to serve", port, m.Proxy.Name())
		}
		held, err := m.holders().Dialled(ctx, []Port{{Number: port}})
		if err == nil && len(held) > 0 && held[0].Process == dockerDaemon {
			return fmt.Errorf("port %d on this server is Docker's API, as %s listens there, and that's root on the server to anyone who reaches it, so no route may send to it. If you run something else there, move Docker's API off that port", port, held[0].Words)
		}
	}
	return nil
}

// dockerDaemon is the name the kernel has for Docker's engine.
const dockerDaemon = "dockerd"

// checkPorts refuses what would stop the proxy holding the ports it needs: all of
// them if it has to start, and otherwise those it doesn't hold yet. This comes
// before the panel lets go of the ports, as they're no use to anyone then.
func (m *Manager) checkPorts(ctx context.Context, have []Route, want []Port) error {
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return err
	}
	if st.Running {
		want = minus(want, m.portsFor(needsHTTPS(have, nil)))
		if len(want) == 0 {
			return nil
		}
	}
	held, err := m.othersOn(ctx, want)
	if err != nil {
		return err
	}
	switch {
	case len(held) == 0:
		return nil
	case st.Running:
		return portsWanted(held, m.Proxy.Name())
	}
	return portsTaken(held, m.Proxy.Name())
}

// othersOn says what holds these of the proxy's ports, but for the panel when it
// gives them up as the proxy starts, and the proxy itself.
func (m *Manager) othersOn(ctx context.Context, ports []Port) ([]Holder, error) {
	held, err := m.holders().Of(ctx, ports)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(held, func(h Holder) bool {
		return (h.Self && (m.Panel.Moves() || m.handed)) || h.Container == ContainerName
	}), nil
}

// holdersWords says who holds what, as the start of a sentence.
func holdersWords(held []Holder) string {
	parts := make([]string, 0, len(held))
	for i, h := range held {
		switch {
		case i > 0 && h.Words == held[i-1].Words:
			parts = append(parts, "port "+h.Port.Words()+" by the same")
		case i == 0:
			parts = append(parts, "Port "+h.Port.Words()+" is held by "+h.Words)
		default:
			parts = append(parts, "port "+h.Port.Words()+" by "+h.Words)
		}
	}
	return strings.Join(parts, ", and ")
}

// findWords says how to find out what holds a port the agent can't name, if there's
// one, with a space ahead of it.
func findWords(held []Holder) string {
	for _, h := range held {
		if h.Unknown {
			return " To see what holds it, run " + h.Port.FindCommand() + "."
		}
	}
	return ""
}

// portsWanted says who holds what a running proxy needs for a route that wants a
// certificate, as it runs for routes that didn't. They go on, and the route may not
// need one at all.
func portsWanted(held []Holder, proxy string) error {
	return fmt.Errorf("%s, so %s, which runs for this server's other routes, can't take it for this route's certificate. What it serves goes on.%s Free that port, or if TLS ends before this server, as at a tunnel or a proxy in front of it, set tls to off on this route",
		holdersWords(held), proxy, findWords(held))
}

// holderNames are the names to act on, each once: a container's, or else what the
// process is called.
func holderNames(held []Holder) []string {
	var out []string
	for _, h := range held {
		name := h.Container
		if name == "" {
			name = h.Words
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// portsHeldNow says what holds the ports of a server that has routes, what that
// costs, and how to keep it from happening again, as what comes back after a
// reboot is the likely reason.
func portsHeldNow(held []Holder, proxy, panelDomain string) error {
	parts := make([]string, 0, len(held))
	for i, h := range held {
		switch {
		case i > 0 && h.Words == held[i-1].Words:
			parts = append(parts, "port "+h.Port.Words()+" by the same")
		case i == 0:
			parts = append(parts, "port "+h.Port.Words()+" is held by "+h.Words)
		default:
			parts = append(parts, "port "+h.Port.Words()+" by "+h.Words)
		}
	}
	down := "this server's routes don't answer"
	if panelDomain != "" {
		down = "this server's routes and the panel's domain, " + panelDomain + ", don't answer. The panel still answers through an SSH tunnel"
	}
	var containers []string
	how := ""
	for _, h := range held {
		switch {
		case h.Container != "":
			if !slices.Contains(containers, h.Container) {
				containers = append(containers, h.Container)
			}
		case h.Unknown:
			how = " To see what holds it, run " + h.Port.FindCommand() + "."
		}
	}
	fix := "Stop it, and keep it from starting again."
	if len(containers) > 0 {
		list := strings.Join(containers, " ")
		fix = "Stop it and keep it stopped, with docker update --restart=no " + list + " and docker stop " + list + "."
	}
	return fmt.Errorf("%s, so %s can't start, and %s.%s %s The agent starts %s within a minute of the ports being free",
		strings.Join(parts, ", and "), proxy, down, how, fix, proxy)
}

// portsTaken says who holds what, what follows, and how to fix it.
func portsTaken(held []Holder, proxy string) error {
	return fmt.Errorf("%s, so %s can't start on this server and this route can't serve traffic. What runs there keeps running.%s "+
		"Stop that proxy, or publish the service's port and point that proxy at it",
		holdersWords(held), proxy, findWords(held))
}

// Ready says why routes can't run on this server now.
func (m *Manager) Ready(ctx context.Context) error {
	if err := m.Client.Ping(ctx); err != nil {
		return err
	}
	info, err := m.Client.Info(ctx)
	if err != nil {
		return err
	}
	if info.Rootless() {
		return errors.New("docker runs rootless on this server, so its host network isn't the server's, and Caddy couldn't take ports 80 and 443 here. Run Docker as root to use routes on this server")
	}
	return nil
}

// ReadyKey is the manager, as what it asks of the engine is more than whether
// it answers.
func (m *Manager) ReadyKey() any { return m }

// Apply makes the proxy serve a route, or stop serving it when spec is nil.
func (m *Manager) Apply(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, effect *resource.IngressEffect) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	if spec == nil {
		return m.remove(ctx, ref)
	}
	return m.put(ctx, ref, spec, effect)
}

// put serves one route, starting the proxy if this is the first.
func (m *Manager) put(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, effect *resource.IngressEffect) error {
	// As a plan made a while ago can have seen fewer than there are now. Its own
	// later deletes aren't known here, so this allows for them (applyMargin).
	step := []resource.Desired{{Ref: ref, Spec: spec}}
	if err := m.checkLimits(ctx, step, applyMargin); err != nil {
		return err
	}
	if err := m.checkHostPorts(ctx, step); err != nil {
		return err
	}
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return err
	}
	if m.Panel.Moves() && Active(m.Dir) {
		m.handed = true // dockwell run left the ports alone, as they are the proxy's
	}
	if !st.Running && m.Panel.Moves() && !m.handed && (effect == nil || effect.PanelDomain == "") {
		return fmt.Errorf("this plan didn't say that the panel's domain, %s, moves behind %s, as its server wasn't reachable when it was made, and that step needs a person. Make the plan again", m.Panel.Domain, m.Proxy.Name())
	}
	have, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	want := m.portsFor(needsHTTPS(have, step))
	// What a running proxy doesn't hold yet comes before anything else changes.
	if st.Running {
		if err := m.checkPorts(ctx, have, want); err != nil {
			return err
		}
	}
	var dials []string
	if port := spec.GetHostPort(); port != 0 {
		dials = []string{ServerHost + ":" + strconv.Itoa(int(port))}
	} else {
		if err := m.ensureNetwork(ctx); err != nil {
			return err
		}
		instances, err := m.reserveAll(ctx, serviceOf(ref, spec))
		if err != nil {
			return err
		}
		dials = m.dialsOf(instances, spec.GetPort())
	}
	start := !st.Running || (st.Outdated && effect != nil && effect.Change == resource.IngressRestarts)
	if start {
		if err := m.start(ctx, st, have, want); err != nil {
			m.leaveAsItWas(ctx)
			return err
		}
	}
	named, edge := splitProxies(spec.GetTrustedProxies())
	r := Route{
		ID: RouteID(ref), Domain: spec.GetDomain(), Path: spec.GetPath(), Upstreams: dials,
		Plain: spec.GetTls() == TLSOff, TrustedProxies: named, EdgeProxies: edge, PlainForAnyone: spec.GetPlainForAnyone(),
	}
	m.logReplaced(ctx, r)
	if !r.Plain {
		if err := m.noteDomain(ctx, r.Domain); err != nil {
			return err
		}
	}
	if err := m.Proxy.PutRoute(ctx, r); err != nil {
		// A port taken since the check is refused by name, as at any other time.
		if strings.Contains(err.Error(), "address already in use") {
			err = m.named(ctx, want, err)
		}
		return fmt.Errorf("%s refused the route %s: %w", m.Proxy.Name(), ref.Name, err)
	}
	owner := ""
	if spec.GetHostPort() != 0 {
		owner = spec.GetService()
	}
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	if err := m.table.own(m.Dir, r.ID, owner); err != nil {
		return err
	}
	// An edit may have left the service it sent to with no route.
	if err := m.releaseUnused(ctx); err != nil {
		m.logger().WarnContext(ctx, "can't take a service with no route left off the ingress network, which does no harm; the agent tries again",
			slog.String("error", err.Error()))
	}
	m.ensureKeeper(ctx, true)
	m.watchNow()
	return nil
}

// logReplaced says when a route takes a domain from one left by another control
// plane, which the proxy replaces with it, as that route is someone's traffic.
func (m *Manager) logReplaced(ctx context.Context, r Route) {
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return
	}
	project := func(id string) string {
		parts := strings.SplitN(id, ".", 3)
		if len(parts) != 3 {
			return ""
		}
		return parts[1]
	}
	for i := range routes {
		old := &routes[i]
		if old.ID != r.ID && old.Domain == r.Domain && project(old.ID) != project(r.ID) {
			m.logger().WarnContext(ctx, "a route replaced one for the same domain, left by another control plane or an older database, which the engine says can't stand beside it",
				slog.String("domain", r.Domain), slog.String("old", old.ID), slog.String("new", r.ID))
		}
	}
}

// start takes the panel's ports, if it holds them, and starts the proxy. When
// the proxy can't start, the panel takes them back.
func (m *Manager) start(ctx context.Context, st Status, have []Route, want []Port) error {
	if !st.Running {
		if err := m.checkPorts(ctx, have, want); err != nil {
			return err
		}
	}
	// What can take minutes comes before anything stops, so the panel's
	// domain isn't down for a pull.
	if err := m.Proxy.Prepare(ctx, m.panelSite(ctx)); err != nil {
		return err
	}
	if err := m.setActive(true); err != nil {
		return err
	}
	moved := false
	if m.Panel.Moves() && !m.handed {
		if err := m.Panel.Release(ctx); err != nil {
			_ = m.setActive(false)
			m.retake(ctx)
			return fmt.Errorf("can't take ports 80 and 443 from the panel: %w", err)
		}
		m.handed, moved = true, true
	}
	if err := m.Proxy.Start(ctx, m.panelSite(ctx), m.plainOnly(want)); err != nil {
		if moved {
			// The proxy didn't start, so nothing else holds them.
			m.retake(ctx)
		}
		if !st.Running {
			_ = m.setActive(false)
		}
		return m.named(ctx, want, err)
	}
	// A proxy resuming its last config ignores the bootstrap, so the panel's
	// site goes in now if this start of Dockwell has one it hadn't.
	return m.Proxy.SetPanel(ctx, m.panelSite(ctx))
}

// retake gives the panel its ports back after a step that couldn't start the
// proxy, whatever became of the step's context. A release that ended early has
// stopped the listeners all the same, and they may take a moment longer to let
// go. So what doesn't work now is left marked as handed over, and Reconcile,
// which runs within a minute, tries again.
func (m *Manager) retake(ctx context.Context) {
	if err := m.Panel.Retake(context.WithoutCancel(ctx)); err != nil {
		m.handed = true
		m.logger().ErrorContext(ctx, "can't take ports 80 and 443 back for the panel yet, so it doesn't answer on its domain; the agent tries again within a minute, and an SSH tunnel reaches it meanwhile",
			slog.String("error", err.Error()))
		return
	}
	m.handed = false
}

// leaveAsItWas leaves a server whose first route couldn't start as it was
// before it: no service on the ingress network, and no network. Nothing else
// will: rolling a route back that never got to the proxy has no step to run.
func (m *Manager) leaveAsItWas(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	routes, err := m.Proxy.Routes(ctx)
	if err != nil || len(routes) > 0 {
		return
	}
	err = m.releaseUnused(ctx)
	if err == nil {
		err = m.removeNetwork(ctx)
	}
	if err != nil {
		m.logger().WarnContext(ctx, "can't remove the ingress network after its first route failed, which does no harm; remove it with docker network rm",
			slog.String("network", docker.IngressNetwork), slog.String("error", err.Error()))
	}
}

// named says who holds the ports a proxy wanted, when it couldn't start: they can
// be taken between the check and the start, and a port taken is refused by name,
// whenever it is found out. What the proxy itself said goes with it, as the ports
// may not be why, and one it didn't want says nothing of why.
func (m *Manager) named(ctx context.Context, want []Port, startErr error) error {
	held, err := m.holders().Of(context.WithoutCancel(ctx), want)
	if err != nil {
		return startErr
	}
	held = slices.DeleteFunc(held, func(h Holder) bool { return h.Self || h.Container == ContainerName })
	if len(held) == 0 {
		return startErr
	}
	return fmt.Errorf("%w. %s said: %w", portsTaken(held, m.Proxy.Name()), m.Proxy.Name(), startErr)
}

// remove stops serving a route, and stops the proxy if it was the last. It
// never starts the proxy: a route in the last config of a proxy that isn't
// running comes out of that config, so removing a route works whatever holds the
// ports.
func (m *Manager) remove(ctx context.Context, ref resource.Ref) error {
	id := RouteID(ref)
	if err := m.Proxy.DeleteRoute(ctx, id); err != nil {
		return fmt.Errorf("%s can't remove the route %s: %w", m.Proxy.Name(), ref.Name, err)
	}
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	if err := m.table.own(m.Dir, id, ""); err != nil {
		return err
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	if err := m.releaseUnused(ctx); err != nil {
		m.logger().WarnContext(ctx, "can't take a service with no route left off the ingress network, which does no harm; the agent tries again",
			slog.String("error", err.Error()))
	}
	if len(routes) > 0 {
		return nil
	}
	return m.stop(ctx)
}

// stop stops the proxy on a server with no routes, and leaves the server as
// it was before the first: nothing on its ports, no network, and its
// certificates still there.
func (m *Manager) stop(ctx context.Context) error {
	if err := m.Proxy.Stop(ctx); err != nil {
		return fmt.Errorf("can't stop %s: %w", m.Proxy.Name(), err)
	}
	if err := m.setActive(false); err != nil {
		return err
	}
	if err := m.removeNetwork(ctx); err != nil {
		m.logger().WarnContext(ctx, "can't remove the ingress network, which does no harm; remove it with docker network rm",
			slog.String("network", docker.IngressNetwork), slog.String("error", err.Error()))
	}
	if m.Panel.Moves() && m.handed {
		if err := m.Panel.Retake(context.WithoutCancel(ctx)); err != nil {
			m.logger().ErrorContext(ctx, "can't take ports 80 and 443 back for the panel; reach it through an SSH tunnel. The agent tries again",
				slog.String("error", err.Error()))
			return nil
		}
		m.handed = false
	}
	return nil
}

// Verify confirms the proxy runs and has the route. A certificate that hasn't
// come yet isn't a failure: DNS may not point here yet, and its state is on
// the API.
func (m *Manager) Verify(ctx context.Context, ref resource.Ref) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Running {
		return fmt.Errorf("%s isn't running on this server. See why with docker logs %s", m.Proxy.Name(), ContainerName)
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	id := RouteID(ref)
	i := slices.IndexFunc(routes, func(r Route) bool { return r.ID == id })
	if i < 0 {
		return fmt.Errorf("%s doesn't have the route %s", m.Proxy.Name(), ref.Name)
	}
	// The first that matches serves, and only that is what was asked for.
	mine := routes[i]
	if first := slices.IndexFunc(routes, func(r Route) bool { return r.Domain == mine.Domain && r.Path == mine.Path }); first != i {
		return fmt.Errorf("%s has the route %s, but %s comes ahead of it for %s%s, so that one serves. Remove it through %s's admin API, or edit this route again, which replaces it",
			m.Proxy.Name(), ref.Name, routes[first].ID, mine.Domain, mine.Path, m.Proxy.Name())
	}
	return nil
}

// ContainerName is the name of the proxy's container.
const ContainerName = "dockwell-ingress"

// PublicPort reports a port the proxy holds while the server has routes, so a
// service can't publish it. A server whose routes take no certificate holds its
// plain HTTP port alone; when it can't be read, it counts as holding them all.
func (m *Manager) PublicPort(ctx context.Context, port uint32, udp bool) (bool, error) {
	if !Active(m.Dir) {
		return false, nil
	}
	ports := m.ports()
	if have, err := m.Proxy.Routes(ctx); err == nil {
		ports = m.portsFor(needsHTTPS(have, nil))
	}
	return slices.ContainsFunc(ports, func(p Port) bool { return int64(p.Number) == int64(port) && p.UDP == udp }), nil
}

// Reconcile puts the server as its routes need it, when the agent comes back
// and every so often: the proxy started if it was stopped, each instance on
// the network, the panel's site as this start of Dockwell has it, and the
// panel's ports back when there are no routes left. It never touches a
// route's config: a hand edit reads as drift (ADR 0024).
//
// A proxy that can't start because something else holds its ports isn't tried
// until they're free: making it again each minute only churns, and Docker's own
// restart policy has it take them back if they come free sooner. Who holds them
// goes in the error, and on the API (Ingress).
func (m *Manager) Reconcile(ctx context.Context) error {
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.unlock()
	if err := m.table.load(m.Dir); err != nil {
		return err
	}
	if !Active(m.Dir) {
		m.report(nil)
		if m.Panel.Moves() && m.handed {
			if err := m.Panel.Retake(ctx); err != nil {
				return fmt.Errorf("can't take ports 80 and 443 back for the panel: %w", err)
			}
			m.handed = false
		}
		return nil
	}
	if m.Panel.Moves() {
		m.handed = true // dockwell run left the ports alone, as they are the proxy's
	}
	// Before anything reads a config that a full disk or a power cut damaged,
	// and takes what it finds for a server with no routes.
	recovered, err := m.Proxy.Recover(ctx)
	if err != nil {
		m.logger().WarnContext(ctx, "can't check the ingress's last config, or keep a copy of it; the agent tries again within a minute", slog.String("error", err.Error()))
	}
	if recovered {
		m.logger().WarnContext(ctx, "the ingress's last config was damaged, as a full disk or a power cut can leave it, so the agent put back its own copy. A change made in the moments before the damage may be missing, and reads as drift",
			slog.String("proxy", m.Proxy.Name()))
	}
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return err
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	if len(routes) == 0 {
		// A step that stopped partway, between the last route and the stop.
		if err := m.releaseUnused(ctx); err != nil {
			return err
		}
		m.report(nil)
		return m.stop(ctx)
	}
	m.noteOutdated(ctx, st)
	m.noteOwnPorts(ctx, routes)
	// The network first: a route that dials a subnet someone else has now mustn't
	// get a request, even from a proxy that is starting. Routes to the server's own
	// ports alone have no use for one.
	var networkErr error
	if m.table.Subnet != "" || dialsContainers(routes) {
		networkErr = m.ensureNetwork(ctx)
	}
	if !st.Running {
		want := m.portsFor(needsHTTPS(routes, nil))
		held, err := m.othersOn(ctx, want)
		if err != nil {
			return errors.Join(err, networkErr)
		}
		if len(held) > 0 {
			err := portsHeldNow(held, m.Proxy.Name(), m.panelDomain())
			m.report(&problem{kind: dockwellv1.IngressProblem_INGRESS_PROBLEM_PORTS_TAKEN, message: err.Error(), holders: holderNames(held)})
			return errors.Join(err, networkErr)
		}
		m.logger().InfoContext(ctx, "starting the ingress, which wasn't running", slog.String("proxy", m.Proxy.Name()))
		if err := m.Proxy.Start(ctx, m.panelSite(ctx), m.plainOnly(want)); err != nil {
			err = m.named(ctx, want, err)
			m.report(&problem{kind: dockwellv1.IngressProblem_INGRESS_PROBLEM_NOT_RUNNING, message: err.Error()})
			return errors.Join(err, networkErr)
		}
	}
	// With the proxy started or not, as one resuming its last config ignores
	// what it was started with. Within a minute of its own certificate coming,
	// this is what drops the panel's.
	if err := m.Proxy.SetPanel(ctx, m.panelSite(ctx)); err != nil {
		return errors.Join(err, networkErr)
	}
	if networkErr != nil {
		m.report(&problem{kind: dockwellv1.IngressProblem_INGRESS_PROBLEM_SUBNET_TAKEN, message: networkErr.Error()})
		return networkErr
	}
	if err := m.releaseUnused(ctx); err != nil {
		return err
	}
	if err := m.attachAll(ctx); err != nil {
		return err
	}
	// Ahead of opening the routes, so none opens on an address a rollout that
	// stopped partway left behind.
	if err := m.settle(ctx); err != nil {
		return err
	}
	// Each instance is back at its own address, so a route may dial it again.
	if err := m.Proxy.SetClosed(ctx, false); err != nil {
		return err
	}
	m.ensureKeeper(ctx, false)
	m.report(nil)
	return nil
}

// panelDomain is the panel's domain when it is behind the proxy, for what its
// being down costs.
func (m *Manager) panelDomain() string {
	if !m.Panel.Moves() {
		return ""
	}
	return m.Panel.Domain
}

// noteOutdated says once when the proxy isn't the version this build pins, as a
// security release may be waiting for this server's next route change, which
// nothing else brings about (issue #78).
func (m *Manager) noteOutdated(ctx context.Context, st Status) {
	b := &m.status
	b.mu.Lock()
	said := b.outdated
	b.outdated = st.Outdated
	b.mu.Unlock()
	if st.Outdated && !said {
		m.logger().WarnContext(ctx, "the ingress runs another version than this release of Dockwell pins, which may be missing security fixes. It moves to the pinned one on this server's next route change",
			slog.String("proxy", m.Proxy.Name()), slog.String("version", st.Version), slog.String("pinned", st.Pinned))
	}
}

// noteOwnPorts says once when a route sends to a port Dockwell has come to listen
// on, as a listener moved there after the route was made. A plan refuses that, and
// nothing looks again until now. The panel's listener refuses a public Host, so
// nothing is exposed, but the route doesn't reach what it meant to.
func (m *Manager) noteOwnPorts(ctx context.Context, routes []Route) {
	var found []string
	for k := range routes {
		r := &routes[k]
		for _, up := range r.Upstreams {
			if host, port, err := splitUpstream(up); err == nil && host == ServerHost && slices.Contains(m.OwnPorts, int(port)) {
				found = append(found, r.Domain+" to port "+strconv.Itoa(int(port)))
			}
		}
	}
	said := strings.Join(found, ", ")
	b := &m.status
	b.mu.Lock()
	again := said != b.ownPorts
	b.ownPorts = said
	b.mu.Unlock()
	if again && said != "" {
		m.logger().WarnContext(ctx, "a route sends to a port Dockwell now listens on itself, so it doesn't reach what it was made for. Nothing is exposed, as the panel's listener refuses a public Host. Move Dockwell's listener, or point the route at another port",
			slog.String("routes", said))
	}
}

// logAgain is how often a failure that doesn't change is logged again, for
// someone who comes to the log late.
const logAgain = time.Hour

// KeepReconciled runs Reconcile until ctx is done, and logs what it can't do,
// when that changes. A Docker that is down gets no word each time: the agent says
// so already.
func (m *Manager) KeepReconciled(ctx context.Context, every time.Duration) {
	var last string
	var at time.Time
	run := func() {
		err := m.Reconcile(ctx)
		switch {
		case err == nil:
			last = ""
		case docker.IsUnavailable(err) || ctx.Err() != nil:
		case err.Error() != last || time.Since(at) >= logAgain:
			last, at = err.Error(), time.Now()
			m.logger().WarnContext(ctx, "can't put the ingress as its routes need it; the agent tries again", slog.String("error", last))
		}
		m.publishIngress(ctx)
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
