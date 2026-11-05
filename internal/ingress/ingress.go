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
	// version is replaced.
	Start(ctx context.Context, panel *PanelSite) error
	// Stop stops it and frees its ports, keeping its certificates.
	Stop(ctx context.Context) error
	// Routes returns the routes Dockwell gave it, as it has them now, or as
	// it last had them if it is down.
	Routes(ctx context.Context) ([]Route, error)
	// PutRoute adds a route, or changes the one with its ID.
	PutRoute(ctx context.Context, r Route) error
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
	// InFlight says how many requests an upstream has now, across its configs
	// old and new.
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
	// Dir is the ingress's directory on the server.
	Dir string
	// Panel is the panel on this server, or nil.
	Panel *Panel
	// Ports are the ports the proxy holds, from the server's side. Nil means
	// DefaultPorts.
	Ports []Port
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
	out := &resourcev1.Route{Domain: r.Domain, Path: r.Path}
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
		if err != nil || !known || inst.Project != docker.Project(ref.Scope) {
			out.Service, out.Port = up, 0
			return out, nil
		}
		if out.Service != "" && (out.Service != inst.Service || out.Port != port) {
			out.Service, out.Port = up, 0
			return out, nil
		}
		out.Service, out.Port = inst.Service, port
	}
	return out, nil
}

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
	for _, r := range have {
		if !in[r.ID] {
			left++
		}
	}
	return left, first, last
}

// routeKind is the route kind, which internal/resource/route names. That
// package names no proxy, and this one needs none of it but this.
const routeKind resource.Kind = "route"

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
	var effect *resource.IngressEffect
	switch {
	case spec != nil && !st.Running && first == ref:
		effect = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: m.Proxy.Name(), Ports: portNames(m.ports())}
	case spec != nil && st.Outdated && first == ref:
		effect = &resource.IngressEffect{Change: resource.IngressRestarts, Proxy: m.Proxy.Name()}
	case spec == nil && left == 0 && last == ref:
		effect = &resource.IngressEffect{Change: resource.IngressStops, Proxy: m.Proxy.Name(), Ports: portNames(m.ports())}
	}
	if effect != nil && effect.Change != resource.IngressRestarts && m.Panel.Moves() {
		effect.PanelDomain = m.Panel.Domain
	}
	var check *resource.DomainCheck
	if spec != nil {
		check = m.dns().Check(ctx, spec.GetDomain())
	}
	return effect, check, nil
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
	return m.holders().Returning(ctx, m.ports())
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
	if err := m.checkLimits(ctx, desired); err != nil {
		return err
	}
	return m.checkStart(ctx)
}

// checkStart refuses what would stop the proxy starting, if it needs to: a daemon
// it can't run under, or something else on its ports. Both come before the panel
// lets go of the ports, as they're no use to anyone then.
func (m *Manager) checkStart(ctx context.Context) error {
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return err
	}
	if st.Running {
		return nil
	}
	info, err := m.Client.Info(ctx)
	if err != nil {
		return err
	}
	if info.NoNewPrivileges() {
		return errors.New("docker on this server sets no-new-privileges for every container, so " + m.Proxy.Name() + " couldn't bind ports 80 and 443, as it gets that from a file capability, which that setting ignores. Nothing changed. To use routes here, remove no-new-privileges from /etc/docker/daemon.json and restart Docker")
	}
	held, err := m.othersOnPorts(ctx)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		return nil
	}
	return portsTaken(held, m.Proxy.Name())
}

// othersOnPorts says what holds the proxy's ports, but for the panel when it
// gives them up as the proxy starts, and the proxy itself.
func (m *Manager) othersOnPorts(ctx context.Context) ([]Holder, error) {
	held, err := m.holders().Of(ctx, m.ports())
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(held, func(h Holder) bool {
		return (h.Self && (m.Panel.Moves() || m.handed)) || h.Container == ContainerName
	}), nil
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
	how := ""
	for _, h := range held {
		if h.Unknown {
			how = " To see what holds it, run " + h.Port.FindCommand() + "."
			break
		}
	}
	return fmt.Errorf("%s, so %s can't start on this server and this route can't serve traffic. What runs there keeps running.%s "+
		"Stop that proxy, or publish the service's port and point that proxy at it",
		strings.Join(parts, ", and "), proxy, how)
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
	// As a plan made a while ago can have seen fewer than there are now.
	if err := m.checkLimits(ctx, []resource.Desired{{Ref: ref, Spec: spec}}); err != nil {
		return err
	}
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Running && m.Panel.Moves() && !m.handed && (effect == nil || effect.PanelDomain == "") {
		return fmt.Errorf("this plan didn't say that the panel's domain, %s, moves behind %s, as its server wasn't reachable when it was made, and that step needs a person. Nothing changed. Make the plan again", m.Panel.Domain, m.Proxy.Name())
	}
	if err := m.ensureNetwork(ctx); err != nil {
		return err
	}
	svc := serviceOf(ref, spec)
	instances, err := m.reserveAll(ctx, svc)
	if err != nil {
		return err
	}
	start := !st.Running || (st.Outdated && effect != nil && effect.Change == resource.IngressRestarts)
	if start {
		if err := m.start(ctx, st); err != nil {
			m.leaveAsItWas(ctx)
			return err
		}
	}
	r := Route{ID: RouteID(ref), Domain: spec.GetDomain(), Path: spec.GetPath(), Upstreams: m.dialsOf(instances, spec.GetPort())}
	m.logReplaced(ctx, r)
	if err := m.noteDomain(ctx, r.Domain); err != nil {
		return err
	}
	if err := m.Proxy.PutRoute(ctx, r); err != nil {
		return fmt.Errorf("%s refused the route %s: %w", m.Proxy.Name(), ref.Name, err)
	}
	// An edit may have left the service it sent to with no route.
	if err := m.releaseUnused(ctx); err != nil {
		m.logger().WarnContext(ctx, "can't take a service with no route left off the ingress network, which does no harm; the agent tries again",
			slog.String("error", err.Error()))
	}
	m.ensureKeeper(ctx)
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
	for _, old := range routes {
		if old.ID != r.ID && old.Domain == r.Domain && project(old.ID) != project(r.ID) {
			m.logger().WarnContext(ctx, "a route replaced one for the same domain, left by another control plane or an older database, which the engine says can't stand beside it",
				slog.String("domain", r.Domain), slog.String("old", old.ID), slog.String("new", r.ID))
		}
	}
}

// start takes the panel's ports, if it holds them, and starts the proxy. When
// the proxy can't start, the panel takes them back.
func (m *Manager) start(ctx context.Context, st Status) error {
	if !st.Running {
		if err := m.checkStart(ctx); err != nil {
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
	if err := m.Proxy.Start(ctx, m.panelSite(ctx)); err != nil {
		if moved {
			// The proxy didn't start, so nothing else holds them.
			m.retake(ctx)
		}
		if !st.Running {
			_ = m.setActive(false)
		}
		return m.named(ctx, err)
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

// named says who holds the ports when a proxy couldn't start for them: they
// can be taken between the check and the start, and a port taken is refused by
// name, whenever it is found out.
func (m *Manager) named(ctx context.Context, startErr error) error {
	held, err := m.holders().Of(context.WithoutCancel(ctx), m.ports())
	if err != nil {
		return startErr
	}
	held = slices.DeleteFunc(held, func(h Holder) bool { return h.Self || h.Container == ContainerName })
	if len(held) == 0 {
		return startErr
	}
	return portsTaken(held, m.Proxy.Name())
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
// service can't publish it.
func (m *Manager) PublicPort(_ context.Context, port uint32, udp bool) (bool, error) {
	return Active(m.Dir) && slices.ContainsFunc(m.ports(), func(p Port) bool { return int64(p.Number) == int64(port) && p.UDP == udp }), nil
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
	// The network first: a route that dials a subnet someone else has now mustn't
	// get a request, even from a proxy that is starting.
	networkErr := m.ensureNetwork(ctx)
	if !st.Running {
		held, err := m.othersOnPorts(ctx)
		if err != nil {
			return errors.Join(err, networkErr)
		}
		if len(held) > 0 {
			err := portsHeldNow(held, m.Proxy.Name(), m.panelDomain())
			m.report(&problem{kind: dockwellv1.IngressProblem_INGRESS_PROBLEM_PORTS_TAKEN, message: err.Error(), holders: holderNames(held)})
			return errors.Join(err, networkErr)
		}
		m.logger().InfoContext(ctx, "starting the ingress, which wasn't running", slog.String("proxy", m.Proxy.Name()))
		if err := m.Proxy.Start(ctx, m.panelSite(ctx)); err != nil {
			err = m.named(ctx, err)
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
	// Each instance is back at its own address, so a route may dial it again.
	if err := m.Proxy.SetClosed(ctx, false); err != nil {
		return err
	}
	m.ensureKeeper(ctx)
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
