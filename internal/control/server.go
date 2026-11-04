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
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/control/servers"
	"github.com/dockwell/dockwell/internal/control/system"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/panel"
	"github.com/dockwell/dockwell/internal/revision"
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
	// PublicAddress is this server's public IP address, shown on the claim
	// screen for pointing a domain at. Empty when it isn't known.
	PublicAddress string
	// Changes is the revision engine, which ChangeService serves, and
	// Agents how it reaches each server's agent over its stream. Store is
	// the state they read. Without Changes there is no ChangeService.
	Changes *revision.Engine
	Agents  *agentapi.Agents
	Store   *store.DB
	// StepGrace is how long the steps under way get to finish when the
	// control plane stops, before they are cancelled; DefaultStepGrace when
	// zero. Their changes finish when it starts again.
	StepGrace time.Duration
}

// DefaultStepGrace is how long the steps under way get to finish when the
// control plane stops. With the API's and the agent listener's own
// shutdown, it keeps a stop well within the 90 seconds systemd waits.
const DefaultStepGrace = 30 * time.Second

// Server is a configured control plane, ready to serve.
type Server struct {
	logger    *slog.Logger
	api       http.Handler
	agents    http.Handler
	agentTLS  *tls.Config
	public    https.Source
	changes   *revision.Engine
	stepGrace time.Duration
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
	api, err := apiRoutes(cfg, ui)
	if err != nil {
		return nil, err
	}
	s := &Server{logger: cfg.Logger, api: api, public: cfg.Public, changes: cfg.Changes, stepGrace: cfg.StepGrace}
	if s.stepGrace <= 0 {
		s.stepGrace = DefaultStepGrace
	}
	if (cfg.Identity == nil) != (cfg.Fleet == nil) {
		return nil, errors.New("control: Config.Identity and Config.Fleet go together")
	}
	if cfg.Changes != nil && (cfg.Agents == nil || cfg.Store == nil) {
		return nil, errors.New("control: Config.Changes needs Config.Agents and Config.Store")
	}
	if cfg.Identity != nil {
		s.agents = agentapi.NewHandler(cfg.Identity, cfg.Fleet, cfg.Logger, agentapi.WithAgents(cfg.Agents))
		s.agentTLS = agentapi.TLSConfig(cfg.Identity, cfg.AgentHosts)
	}
	return s, nil
}

// Handler returns the API listener's root handler.
func (s *Server) Handler() http.Handler { return s.api }

func apiRoutes(cfg Config, ui http.Handler) (http.Handler, error) {
	opts := append(serve.HandlerOptions(cfg.Logger), connect.WithInterceptors(authapi.Interceptor(cfg.Auth, cfg.Logger)))
	mux := http.NewServeMux()
	var services []string
	var rest []*vanguard.Service
	// handle serves a service's procedures, and its REST paths through
	// Vanguard, which calls the same handler, interceptors and all.
	handle := func(path string, h http.Handler) {
		mux.Handle(path, h)
		rest = append(rest, vanguard.NewService(path, h))
		services = append(services, strings.Trim(path, "/"))
	}
	handle(dockwellv1connect.NewSystemServiceHandler(system.New(cfg.Build), opts...))
	if cfg.Auth != nil {
		controlServer := func() *dockwellv1.Server { return nil }
		if cfg.Fleet != nil {
			controlServer = cfg.Fleet.ControlServer
		}
		h := authapi.New(authapi.Config{Auth: cfg.Auth, Logger: cfg.Logger, ControlServer: controlServer, PublicAddress: cfg.PublicAddress})
		handle(dockwellv1connect.NewAuthServiceHandler(h, opts...))
		handle(dockwellv1connect.NewTokenServiceHandler(h, opts...))
	}
	if cfg.Fleet != nil {
		handle(dockwellv1connect.NewServerServiceHandler(servers.New(cfg.Fleet), opts...))
	}
	if cfg.Changes != nil {
		handle(dockwellv1connect.NewChangeServiceHandler(changes.New(cfg.Changes, cfg.Store, cfg.Logger), opts...))
	}
	restPaths, err := restHandler(rest)
	if err != nil {
		return nil, err
	}
	mux.Handle(authapi.RESTPrefix, restPaths)
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
	if (ls.HTTPS != nil || ls.HTTP != nil) && s.public == nil {
		return errors.New("control: the HTTPS and HTTP listeners need Config.Public")
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
	if ls.HTTPS != nil {
		run(func() error {
			return serve.Serve(apiCtx, ls.HTTPS, s.api, serve.Options{Name: "panel over HTTPS", Logger: s.logger, TLS: s.public.TLSConfig()})
		})
	}
	if ls.HTTP != nil {
		run(func() error {
			return serve.Serve(apiCtx, ls.HTTP, s.public.HTTPHandler(), serve.Options{Name: "HTTP redirect", Logger: s.logger})
		})
	}
	if ls.Agents != nil {
		agents.Go(func() {
			failed(s.ServeAgents(agentCtx, ls.Agents))
			stopAPI()
		})
	}
	api.Wait()
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
