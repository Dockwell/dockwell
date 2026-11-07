// SPDX-License-Identifier: AGPL-3.0-only

// Package control is the control plane: the API every surface uses, the
// panel, the agent listener servers connect to, and later the revision
// store and the scheduler.
//
// The API listener serves the Connect, gRPC and gRPC-Web protocols over
// HTTP/1.1 and HTTP/2, REST paths under /v1/ through Vanguard with their
// OpenAPI document (ADR 0015), the panel at every other path, and /healthz
// for process supervisors. It stays on loopback. With a domain, the same
// handler also serves HTTPS on the panel's domain, and a plain-HTTP
// listener answers certificate challenges and redirects to HTTPS (ADR
// 0011). Every procedure but sign-in needs a session or an API token whose
// scope covers it (internal/auth, ADR 0016), whatever the protocol.
//
// The agent listener speaks only to agents, over TLS 1.3 with client
// certificates (ADR 0007).
package control

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/vanguard"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/apidoc"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/control/databases"
	"github.com/dockwell/dockwell/internal/control/domains"
	"github.com/dockwell/dockwell/internal/control/events"
	"github.com/dockwell/dockwell/internal/control/files"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/control/identity"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/resources"
	"github.com/dockwell/dockwell/internal/control/servers"
	serviceapi "github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/control/system"
	"github.com/dockwell/dockwell/internal/control/updates"
	"github.com/dockwell/dockwell/internal/control/variables"
	"github.com/dockwell/dockwell/internal/control/websites"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/panel"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

// Default listen addresses. The API stays on loopback until the operator
// chooses to expose it; agents on other servers must reach the agent
// listener, which only speaks mTLS and join tickets.
const (
	DefaultListen      = "127.0.0.1:7600"
	DefaultAgentListen = ":7443"
)

// Config configures a Server.
type Config struct {
	// Logger receives the server's logs. Required.
	Logger *slog.Logger
	// DNSLookup resolves names for DomainService.CheckDomain. Nil means the control
	// server's resolver.
	DNSLookup func(ctx context.Context, name string) ([]netip.Addr, error)
	// Build is reported by SystemService.GetVersion.
	Build buildinfo.Info
	// Panel is the panel build to serve. Nil means the one embedded in the binary.
	Panel fs.FS
	// Identity is the install's authority, and Fleet its servers' live state.
	// Without both there is no agent listener and no ServerService.
	Identity *identity.Authority
	Fleet    *fleet.Hub
	// AgentHosts are the names and addresses agents dial; the agent
	// listener's certificate is valid for them.
	AgentHosts []string
	// Auth decides who may use the API. Without it, only the procedures
	// that need no session answer.
	Auth *auth.Service
	// Public serves the panel on its domain over HTTPS. Nil keeps the panel
	// on loopback.
	Public https.Source
	// PanelListen is the loopback address the panel answers on, HTTPSListen
	// where it serves Public's domain itself, PanelURL its address there, and
	// PanelCA who issues its certificate, for the settings to say how the panel
	// is served. PanelHandsOver is true when the server's ingress takes the
	// public ports while the server has a route, and serves the panel's domain
	// then (ADR 0024): the settings ask PublicPorts which holds them now.
	PanelListen, HTTPSListen, PanelURL, PanelCA string
	PanelHandsOver                              bool
	// PublicAddress is this server's public IP address, shown on the claim
	// screen for pointing a domain at. Empty when it isn't known.
	PublicAddress string
	// ClaimCommand prints a new claim link on the control server, as it runs
	// there, such as "sudo dockwell claim", for the panel to show while the
	// install has no owner.
	ClaimCommand string
	// Changes is the revision engine, which ChangeService serves, and
	// Agents how it reaches each server's agent over its stream. Store is
	// the state they read. Without Changes there is no ChangeService.
	Changes *revision.Engine
	Agents  *agentapi.Agents
	Store   *store.DB
	// Secrets keeps what may be secret encrypted, and reveals a secret to
	// a person who confirms it with a passkey. With Changes and Auth, it
	// serves VariableService.
	Secrets *secrets.Keeper
	// Logs changes the limits of reading logs, as tests do.
	Logs logapi.Options
	// EventPoll is how often a watch of the timeline reads the store again,
	// for events another process recorded; events.DefaultPoll when zero.
	EventPoll time.Duration
	// StepGrace is how long the steps under way get to finish when the
	// control plane stops, before they are cancelled; DefaultStepGrace when
	// zero. Their changes finish when it starts again.
	StepGrace time.Duration
	// Releases is the URL of Dockwell's releases, in GitHub's layout, which
	// UpdateService reads the newest release from. Empty reads none.
	Releases string
}

// DefaultStepGrace is how long the steps under way get to finish when the
// control plane stops. With the API's and the agent listener's own
// shutdown, it keeps a stop well within the 90 seconds systemd waits.
const DefaultStepGrace = 30 * time.Second

// Server is a configured control plane, ready to serve.
type Server struct {
	logger   *slog.Logger
	api      http.Handler
	agents   http.Handler
	agentTLS *tls.Config
	// addAgentHost has the agent listener's certificate name one more address.
	addAgentHost func(string)
	// logs reads service logs, and tells servers what deletions put out of reach.
	logs *logapi.Handler
	// backups takes backups, and those under way end with the control plane.
	backups   *backups.Handler
	public    https.Source
	changes   *revision.Engine
	stepGrace time.Duration
	ports     *PublicPorts
}

// New builds a Server from cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		return nil, errors.New("control: Config.Logger is required")
	}
	if cfg.Panel == nil {
		cfg.Panel = panel.Embedded()
	}
	ui, err := panel.New(cfg.Panel)
	if err != nil {
		return nil, err
	}
	if !ui.Built() {
		cfg.Logger.Warn("this binary has no panel built in; the API works, and make build adds the panel")
	}
	if cfg.Public != nil && (cfg.Auth == nil || cfg.Auth.Sites().Domain != cfg.Public.Domain()) {
		return nil, errors.New("control: Config.Public needs Config.Auth for the same domain")
	}
	var logs *logapi.Handler
	var backupHandler *backups.Handler
	// Made first, so the settings can ask it who holds the public ports now.
	ports := &PublicPorts{ready: make(chan struct{})}
	api, err := apiRoutes(cfg, ui, ports, &logs, &backupHandler)
	if err != nil {
		return nil, err
	}
	s := &Server{logger: cfg.Logger, api: api, logs: logs, backups: backupHandler, public: cfg.Public, changes: cfg.Changes, stepGrace: cfg.StepGrace}
	ports.server = s
	s.ports = ports
	if s.stepGrace <= 0 {
		s.stepGrace = DefaultStepGrace
	}
	if (cfg.Identity == nil) != (cfg.Fleet == nil) {
		return nil, errors.New("control: Config.Identity and Config.Fleet go together")
	}
	if cfg.Changes != nil && (cfg.Agents == nil || cfg.Store == nil) {
		return nil, errors.New("control: Config.Changes needs Config.Agents and Config.Store")
	}
	// Each stream sends a step the keeper's values it needs, and asks the keeper
	// what on a server names one, of an agent that doesn't write them in. Without a
	// keeper, such an agent is refused all it would read (agentapi.Agents.CheckNaming).
	if cfg.Agents != nil && cfg.Secrets != nil {
		cfg.Agents.SendValues(cfg.Secrets)
		cfg.Agents.CheckNaming(cfg.Secrets)
	}
	// A service's containers are named after what people call its environment.
	if cfg.Agents != nil && cfg.Store != nil {
		cfg.Agents.NameScopes(scopeNames{db: cfg.Store})
	}
	if cfg.Identity != nil {
		s.agents = agentapi.NewHandler(cfg.Identity, cfg.Fleet, cfg.Logger, agentapi.WithAgents(cfg.Agents))
		s.agentTLS, s.addAgentHost = agentapi.TLSConfig(cfg.Identity, cfg.AgentHosts)
	}
	return s, nil
}

// Handler returns the API listener's root handler.
func (s *Server) Handler() http.Handler { return s.api }

func apiRoutes(cfg Config, ui http.Handler, ports *PublicPorts, logsOut **logapi.Handler, backupsOut **backups.Handler) (http.Handler, error) {
	opts := append(serve.HandlerOptions(cfg.Logger), connect.WithInterceptors(authapi.Interceptor(cfg.Auth, cfg.Logger)))
	mux := http.NewServeMux()
	var services []string
	var rest []*vanguard.Service
	guardREST := func(h http.Handler) http.Handler { return h }
	// handle serves a service's procedures, and its REST paths through
	// Vanguard, which calls the same handler, interceptors and all.
	handle := func(path string, h http.Handler) {
		mux.Handle(path, h)
		rest = append(rest, vanguard.NewService(path, h))
		services = append(services, strings.Trim(path, "/"))
	}
	handle(dockwellv1connect.NewSystemServiceHandler(system.New(cfg.Build), opts...))
	if cfg.Store != nil {
		serving := settings.Serving{Listen: cfg.PanelListen, HTTPSListen: cfg.HTTPSListen, URL: cfg.PanelURL, CA: cfg.PanelCA}
		if cfg.Public != nil {
			serving.Domain = cfg.Public.Domain()
		}
		if cfg.PanelHandsOver {
			serving.Proxied = func() bool { return !ports.Serving() }
		}
		handle(dockwellv1connect.NewSettingsServiceHandler(settings.New(cfg.Store, serving), opts...))
	}
	if cfg.Auth != nil {
		controlServer := func() *dockwellv1.Server { return nil }
		if cfg.Fleet != nil {
			controlServer = cfg.Fleet.ControlServer
		}
		h := authapi.New(authapi.Config{
			Auth: cfg.Auth, Logger: cfg.Logger, ControlServer: controlServer, PublicAddress: cfg.PublicAddress, ClaimCommand: cfg.ClaimCommand,
		})
		handle(dockwellv1connect.NewAuthServiceHandler(h, opts...))
		handle(dockwellv1connect.NewTokenServiceHandler(h, opts...))
	}
	if cfg.Fleet != nil {
		handle(dockwellv1connect.NewServerServiceHandler(servers.New(cfg.Fleet, cfg.Store, cfg.Agents), opts...))
	}
	if cfg.Changes != nil {
		ch := changes.New(cfg.Changes, cfg.Auth, cfg.Store, cfg.Logger)
		handle(dockwellv1connect.NewChangeServiceHandler(ch, opts...))
		own := containers.New(cfg.Agents, ch, cfg.Store, cfg.Logger)
		imports := composeapi.New(ch, own, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Logger)
		handle(dockwellv1connect.NewComposeServiceHandler(imports, opts...))
		handle(dockwellv1connect.NewCatalogServiceHandler(catalogapi.New(imports, cfg.Store, cfg.Secrets, cfg.Build.Commit, cfg.Logger), opts...))
		handle(dockwellv1connect.NewWebsiteServiceHandler(websites.New(ch, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Agents, cfg.Logger), opts...))
		handle(dockwellv1connect.NewEventServiceHandler(events.New(cfg.Changes, cfg.Store, cfg.Logger, cfg.EventPoll), opts...))
		handle(dockwellv1connect.NewResourceServiceHandler(resources.New(cfg.Changes.Kinds(), cfg.Store, cfg.Logger), opts...))
		if cfg.Secrets != nil && cfg.Auth != nil {
			handle(dockwellv1connect.NewVariableServiceHandler(
				variables.New(cfg.Changes.Kinds(), cfg.Store, cfg.Secrets, cfg.Auth, cfg.Logger), opts...))
		}
		domainHandler := domains.New(cfg.Changes.Kinds(), cfg.Store, cfg.Agents, fleetOf(cfg), cfg.Logger)
		domainHandler.Lookup = cfg.DNSLookup
		handle(dockwellv1connect.NewDomainServiceHandler(domainHandler, opts...))
		if cfg.Fleet != nil {
			var releases *updates.Releases
			if cfg.Releases != "" {
				releases = updates.NewReleases(cfg.Releases, cfg.Build.Version, cfg.Logger)
			}
			handle(dockwellv1connect.NewUpdateServiceHandler(updates.New(cfg.Fleet, cfg.Agents, releases, cfg.Build, cfg.Logger), opts...))
		}
		// A person's, so it needs sign-in; the agent decides what it reads.
		if cfg.Fleet != nil && cfg.Auth != nil {
			handle(dockwellv1connect.NewFileServiceHandler(files.New(cfg.Fleet, cfg.Agents, cfg.Auth, cfg.Store, cfg.Logger), opts...))
		}
		handle(dockwellv1connect.NewServiceServiceHandler(serviceapi.New(ch, cfg.Changes, cfg.Store, cfg.Secrets, cfg.Logger), opts...))
		logs := logapi.New(cfg.Agents, ch, cfg.Store, cfg.Logger, cfg.Logs)
		*logsOut = logs
		path, h := dockwellv1connect.NewLogServiceHandler(logs, opts...)
		mux.Handle(path, logs.Guard(h))
		rest = append(rest, vanguard.NewService(path, h))
		services = append(services, strings.Trim(path, "/"))
		guardREST = logs.GuardREST
		handle(dockwellv1connect.NewHealthServiceHandler(health.New(cfg.Store, cfg.Agents, cfg.Logger), opts...))
		handle(dockwellv1connect.NewContainerServiceHandler(own, opts...))
		*backupsOut = backups.New(ch, cfg.Changes, cfg.Agents, cfg.Store, cfg.Secrets, cfg.Logger)
		handle(dockwellv1connect.NewBackupServiceHandler(*backupsOut, opts...))
		handle(dockwellv1connect.NewDatabaseServiceHandler(databases.New(cfg.Agents, *backupsOut, cfg.Store, cfg.Secrets, cfg.Logger), opts...))
	}
	restPaths, err := restHandler(rest)
	if err != nil {
		return nil, err
	}
	mux.Handle(authapi.RESTPrefix, guardREST(restPaths))
	mux.Handle("GET "+apidoc.Path, apidoc.Handler())

	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...), opts...))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector, opts...))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector, opts...))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	// Everything else is the panel; it answers 404 for paths it doesn't own.
	mux.Handle("/", ui)

	var sites auth.Sites
	if cfg.Auth != nil {
		sites = cfg.Auth.Sites()
	}
	return serve.Recover(cfg.Logger, authapi.Guard(sites, cfg.Logger, mux)), nil
}

// Serve serves the API and panel on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return serve.Serve(ctx, ln, s.api, serve.Options{Name: "control plane", Logger: s.logger})
}

// ServeAgents serves the agent listener on ln until ctx is done.
func (s *Server) ServeAgents(ctx context.Context, ln net.Listener) error {
	if s.agents == nil {
		return errors.New("control: the agent listener needs Config.Identity")
	}
	return serve.Serve(ctx, ln, s.agents, serve.Options{Name: "agent listener", Logger: s.logger, TLS: s.agentTLS})
}

// AddAgentHost has the agent listener's certificate name host too, from the next
// connection.
func (s *Server) AddAgentHost(host string) {
	if s.addAgentHost != nil {
		s.addAgentHost(host)
	}
}

// PublicPorts returns what serves the panel on the public ports, for this
// server's ingress to take them from and give them back to: the addresses are
// where it listens again.
func (s *Server) PublicPorts(httpsAddr, httpAddr string) *PublicPorts {
	s.ports.httpsAddr, s.ports.httpAddr = httpsAddr, httpAddr
	return s.ports
}

// PublicPorts serves the panel over HTTPS and redirects plain HTTP, until this
// server's proxy takes ports 80 and 443, and again once it gives them back.
// Only one process can hold them (ADR 0024).
type PublicPorts struct {
	server              *Server
	httpsAddr, httpAddr string
	ready               chan struct{}
	readyOnce           sync.Once

	mu     sync.Mutex
	base   context.Context
	failed func(error)
	stop   context.CancelFunc
	done   *sync.WaitGroup
	// ended says the server has stopped, so nothing starts from now on, and
	// every is all that was started, for that stop to wait for.
	ended bool
	every sync.WaitGroup
}

// begin says the listeners may start, under base, and fail through failed.
func (p *PublicPorts) begin(base context.Context, failed func(error)) {
	p.mu.Lock()
	p.base, p.failed = base, failed
	p.mu.Unlock()
	p.readyOnce.Do(func() { close(p.ready) })
}

// end stops what serves, and returns once it has, for the server to stop.
func (p *PublicPorts) end() {
	p.mu.Lock()
	p.ended = true
	if p.stop != nil {
		p.stop()
	}
	p.mu.Unlock()
	p.every.Wait()
}

// serve serves the panel on the two listeners.
func (p *PublicPorts) serve(httpsLn, httpLn net.Listener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		_ = httpsLn.Close()
		_ = httpLn.Close()
		return
	}
	ctx, stop := context.WithCancel(p.base)
	done := &sync.WaitGroup{}
	p.stop, p.done = stop, done
	s := p.server
	listeners := []struct {
		ln   net.Listener
		h    http.Handler
		opts serve.Options
	}{
		{httpsLn, s.api, serve.Options{Name: "panel over HTTPS", Logger: s.logger, TLS: s.public.TLSConfig()}},
		{httpLn, s.public.HTTPHandler(), serve.Options{Name: "HTTP redirect", Logger: s.logger}},
	}
	for _, l := range listeners {
		if l.ln == nil {
			continue
		}
		done.Add(1)
		p.every.Add(1)
		go func() {
			defer p.every.Done()
			defer done.Done()
			if err := serve.Serve(ctx, l.ln, l.h, l.opts); err != nil {
				p.failed(err)
			}
		}()
	}
}

// Serving reports whether the control plane holds the public ports now,
// rather than this server's proxy.
func (p *PublicPorts) Serving() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stop != nil
}

// Release stops serving on the public ports, and returns once they are free.
func (p *PublicPorts) Release(ctx context.Context) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	stop, done := p.stop, p.done
	p.stop, p.done = nil, nil
	p.mu.Unlock()
	if stop == nil {
		return nil
	}
	stop()
	stopped := make(chan struct{})
	go func() {
		done.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Retake takes the public ports again, and serves on them.
func (p *PublicPorts) Retake(ctx context.Context) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	serving := p.stop != nil
	p.mu.Unlock()
	if serving {
		return nil
	}
	httpsLn, err := serve.Listen(ctx, p.httpsAddr)
	if err != nil {
		return err
	}
	httpLn, err := serve.Listen(ctx, p.httpAddr)
	if err != nil {
		_ = httpsLn.Close()
		return err
	}
	p.serve(httpsLn, httpLn)
	return nil
}

// Listeners are where a Server listens. API is required; the others are
// optional.
type Listeners struct {
	// API serves the API and panel over plain HTTP. It must be on loopback.
	API net.Listener
	// HTTPS serves the same on the panel's domain. It needs Config.Public.
	HTTPS net.Listener
	// HTTP answers certificate challenges and redirects to HTTPS. It needs
	// Config.Public.
	HTTP net.Listener
	// Agents is the agent listener. It needs Config.Identity.
	Agents net.Listener
	// Ingress serves the panel's domain to this server's proxy, while the
	// proxy holds the public ports. It is a unix socket that only the proxy
	// and root reach, which is all that makes what arrives on it HTTPS (ADR
	// 0024). It needs Config.Public.
	Ingress net.Listener
}

// Run serves on every listener in ls until ctx is done or one of them
// fails. A failure stops the others too.
//
// It stops in order, so a change under way isn't failed by the stop
// itself: first the API, so no new change starts; then the revision
// engine, which lets the steps under way finish for StepGrace and leaves
// what remains of their changes for the next start; then the agent
// listener, whose streams those steps need.
func (s *Server) Run(ctx context.Context, ls Listeners) error {
	if (ls.HTTPS != nil || ls.HTTP != nil || ls.Ingress != nil) && s.public == nil {
		return errors.New("control: the HTTPS, HTTP and ingress listeners need Config.Public")
	}
	if s.backups != nil {
		s.backups.Recover(ctx)
	}
	apiCtx, stopAPI := context.WithCancel(ctx)
	defer stopAPI()
	agentCtx, stopAgents := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAgents()

	var api, agents sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	failed := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	}
	run := func(serveFn func() error) {
		api.Go(func() {
			failed(serveFn())
			stopAPI()
		})
	}
	run(func() error { return s.Serve(apiCtx, ls.API) })
	if s.logs != nil {
		api.Go(func() { s.logs.KeepTelling(apiCtx) })
	}
	if s.backups != nil {
		api.Go(func() { s.backups.KeepRunning(apiCtx) })
	}
	if ls.Ingress != nil {
		run(func() error {
			return serve.Serve(apiCtx, ls.Ingress, authapi.ViaIngress(s.api), serve.Options{Name: "panel behind the ingress", Logger: s.logger})
		})
	}
	s.ports.begin(apiCtx, func(err error) {
		failed(err)
		stopAPI()
	})
	if ls.HTTPS != nil {
		s.ports.serve(ls.HTTPS, ls.HTTP)
	}
	if ls.Agents != nil {
		agents.Go(func() {
			failed(s.ServeAgents(agentCtx, ls.Agents))
			stopAPI()
		})
	}
	api.Wait()
	s.ports.end()
	if s.backups != nil {
		s.backups.Stop()
	}
	if s.changes != nil {
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.stepGrace)
		if err := s.changes.Stop(grace); err != nil {
			s.logger.WarnContext(ctx, "the revision engine didn't stop cleanly", slog.String("error", err.Error()))
		}
		cancel()
	}
	stopAgents()
	agents.Wait()
	return errors.Join(errs...)
}

// fleetOf is the fleet a handler reads servers from, as the interface it takes, which
// a nil *fleet.Hub wouldn't read as nil.
func fleetOf(cfg Config) domains.Servers {
	if cfg.Fleet == nil {
		return nil
	}
	return cfg.Fleet
}
