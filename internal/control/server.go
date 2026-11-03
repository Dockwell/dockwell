// SPDX-License-Identifier: AGPL-3.0-only

// Package control is the control plane: the API every surface uses and,
// later, the panel, the revision store and the scheduler.
//
// One HTTP listener serves the Connect, gRPC and gRPC-Web protocols over
// HTTP/1.1 and HTTP/2 (including unencrypted HTTP/2 for local gRPC clients),
// plus a plain /healthz for process supervisors.
package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"

	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/system"
)

// DefaultListen is where the control plane listens unless told otherwise. It
// stays on loopback so nothing is exposed until the operator chooses to.
const DefaultListen = "127.0.0.1:7600"

// Limits applied to every request.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 64 << 10
	maxMessageBytes   = 8 << 20
	compressMinBytes  = 1 << 10
)

// Shutdown timing. In-flight requests get streamGrace to finish on their
// own; after that, request contexts are cancelled so long-lived streams end,
// and anything still open at shutdownTimeout is closed.
const (
	defaultStreamGrace     = 2 * time.Second
	defaultShutdownTimeout = 15 * time.Second
)

// Config configures a Server.
type Config struct {
	// Logger receives the server's logs. Required.
	Logger *slog.Logger
	// Build is reported by SystemService.GetVersion.
	Build buildinfo.Info
}

// Server is a configured control plane, ready to serve on a listener.
type Server struct {
	logger          *slog.Logger
	handler         http.Handler
	streamGrace     time.Duration
	shutdownTimeout time.Duration
}

// New builds a Server from cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		return nil, errors.New("control: Config.Logger is required")
	}
	s := &Server{
		logger:          cfg.Logger,
		streamGrace:     defaultStreamGrace,
		shutdownTimeout: defaultShutdownTimeout,
	}
	s.handler = s.routes(cfg)
	return s, nil
}

// Handler returns the server's root HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes(cfg Config) http.Handler {
	opts := []connect.HandlerOption{
		connect.WithInterceptors(logInterceptor{logger: s.logger}),
		connect.WithReadMaxBytes(maxMessageBytes),
		connect.WithCompressMinBytes(compressMinBytes),
		connect.WithRecover(s.recoverRPC),
	}

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

	return s.recoverHTTP(mux)
}

// Serve accepts connections on ln until ctx is done, then shuts down
// gracefully: it stops accepting, lets in-flight requests finish, cancels
// the contexts of requests still running after streamGrace (so streams end),
// and closes whatever is left at shutdownTimeout. It returns nil after a
// clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)

	// Requests outlive ctx briefly, so the drain below can let them finish.
	baseCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()

	srv := &http.Server{
		Handler:           s.handler,
		Protocols:         &protocols,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.logger.Info("control plane listening", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-errc:
		return fmt.Errorf("control plane stopped: %w", err)
	case <-ctx.Done():
	}

	s.logger.Info("control plane shutting down")
	endStreams := time.AfterFunc(s.streamGrace, cancelRequests)
	defer endStreams.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("control plane didn't stop within %s, so its remaining connections were closed: %w",
			seconds(s.shutdownTimeout), err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control plane stopped: %w", err)
	}
	s.logger.Info("control plane stopped")
	return nil
}

// ListenAndServe listens on addr and serves until ctx is done. A failure to
// listen wraps the underlying error, so callers can tell a busy port
// (syscall.EADDRINUSE) from a privileged one (syscall.EACCES) or a malformed
// address (*net.AddrError).
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("can't listen on %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// seconds formats d the way Dockwell writes durations: a number, a space and
// a unit, such as "15 s".
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " s"
}

func (s *Server) recoverRPC(ctx context.Context, spec connect.Spec, _ http.Header, p any) error {
	s.logger.ErrorContext(ctx, "rpc panicked",
		slog.String("procedure", spec.Procedure),
		slog.Any("panic", p),
		slog.String("stack", string(debug.Stack())),
	)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// recoverHTTP keeps a panic in a plain HTTP handler from taking down the
// process. Connect handlers recover on their own (see recoverRPC).
func (s *Server) recoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				panic(p)
			}
			s.logger.ErrorContext(r.Context(), "http handler panicked",
				slog.String("path", r.URL.Path),
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
