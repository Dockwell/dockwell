// SPDX-License-Identifier: AGPL-3.0-only

// Package control is the control plane: the API every surface uses, the
// panel, the agent listener servers connect to, and later the revision
// store and the scheduler.
//
// It listens twice. The API listener serves the Connect, gRPC and gRPC-Web
// protocols over HTTP/1.1 and HTTP/2, the panel at every other path, and
// /healthz for process supervisors. The agent listener speaks only to
// agents, over TLS 1.3 with client certificates (ADR 0007).
package control

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"

	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/control/system"
	"github.com/dockwell/dockwell/internal/panel"
	"github.com/dockwell/dockwell/internal/serve"
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
	// Identity is the install's authority. Without it there is no agent listener.
	Identity *identity.Authority
	// AgentHosts are the names and addresses agents dial; the agent
	// listener's certificate is valid for them.
	AgentHosts []string
}

// Server is a configured control plane, ready to serve.
type Server struct {
	logger   *slog.Logger
	api      http.Handler
	agents   http.Handler
	agentTLS *tls.Config
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
	s := &Server{logger: cfg.Logger, api: apiRoutes(cfg, ui)}
	if cfg.Identity != nil {
		s.agents = agentapi.NewHandler(cfg.Identity, cfg.Logger)
		s.agentTLS = agentapi.TLSConfig(cfg.Identity, cfg.AgentHosts)
	}
	return s, nil
}

// Handler returns the API listener's root handler.
func (s *Server) Handler() http.Handler { return s.api }

func apiRoutes(cfg Config, ui http.Handler) http.Handler {
	opts := serve.HandlerOptions(cfg.Logger)
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewSystemServiceHandler(system.New(cfg.Build), opts...))

	services := []string{dockwellv1connect.SystemServiceName}
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

	return serve.Recover(cfg.Logger, mux)
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

// Run serves the API on api and, when the server has an identity, agents on
// agents, until ctx is done or either listener fails. A failure stops the
// other listener too.
func (s *Server) Run(ctx context.Context, api, agents net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() {
		errs[0] = s.Serve(ctx, api)
		cancel()
	})
	if agents != nil {
		wg.Go(func() {
			errs[1] = s.ServeAgents(ctx, agents)
			cancel()
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
