// SPDX-License-Identifier: AGPL-3.0-only

// Package serve runs dockwell's HTTP listeners: the control plane's API and
// panel, and its mTLS agent listener. It owns what they share: timeouts,
// HTTP/2 (including unencrypted HTTP/2 for local gRPC clients), graceful
// shutdown, panic recovery and RPC logging.
package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/human"
)

// Limits applied to every request.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 64 << 10
	maxMessageBytes   = 8 << 20
	compressMinBytes  = 1 << 10
)

// Shutdown timing. In-flight requests get StreamGrace to finish on their
// own; after that, request contexts are cancelled so long-lived streams end,
// and anything still open at ShutdownTimeout is closed.
const (
	DefaultStreamGrace     = 2 * time.Second
	DefaultShutdownTimeout = 15 * time.Second
)

// Options configures Serve.
type Options struct {
	// Name says what is serving, in logs and errors: "control plane".
	Name string
	// Logger receives the listener's logs. Required.
	Logger *slog.Logger
	// TLS, when set, serves HTTPS with this configuration. Otherwise the
	// listener speaks plaintext HTTP/1.1 and unencrypted HTTP/2.
	TLS *tls.Config
	// StreamGrace and ShutdownTimeout override the defaults when nonzero.
	StreamGrace     time.Duration
	ShutdownTimeout time.Duration
}

// Serve accepts connections on ln until ctx is done, then shuts down
// gracefully: it stops accepting, lets in-flight requests finish, cancels
// the contexts of requests still running after the stream grace (so streams
// end), and closes whatever is left at the shutdown timeout. It returns nil
// after a clean shutdown.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, opts Options) error {
	grace := orDefault(opts.StreamGrace, DefaultStreamGrace)
	timeout := orDefault(opts.ShutdownTimeout, DefaultShutdownTimeout)
	logger := opts.Logger.With(slog.String("listener", opts.Name))

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	if opts.TLS == nil {
		protocols.SetUnencryptedHTTP2(true)
	}

	// Requests outlive ctx briefly, so the drain below can let them finish.
	baseCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()

	srv := &http.Server{
		Handler:           h,
		Protocols:         &protocols,
		TLSConfig:         opts.TLS,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	errc := make(chan error, 1)
	go func() {
		if opts.TLS != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	logger.Info("listening", slog.String("addr", ln.Addr().String()), slog.Bool("tls", opts.TLS != nil))

	select {
	case err := <-errc:
		return fmt.Errorf("%s stopped: %w", opts.Name, err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	endStreams := time.AfterFunc(grace, cancelRequests)
	defer endStreams.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("%s didn't stop within %s, so its remaining connections were closed: %w", opts.Name, human.Duration(timeout), err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s stopped: %w", opts.Name, err)
	}
	logger.Info("stopped")
	return nil
}

// Listen listens on addr. A failure wraps the underlying error, so callers
// can tell a busy port (syscall.EADDRINUSE) from a privileged one
// (syscall.EACCES) or a malformed address (*net.AddrError).
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("can't listen on %s: %w", addr, err)
	}
	return ln, nil
}

// HandlerOptions are the Connect options every service uses: logging,
// limits, compression and panic recovery.
func HandlerOptions(logger *slog.Logger) []connect.HandlerOption {
	return []connect.HandlerOption{
		connect.WithInterceptors(logInterceptor{logger: logger}),
		connect.WithReadMaxBytes(maxMessageBytes),
		connect.WithCompressMinBytes(compressMinBytes),
		connect.WithRecover(recoverRPC(logger)),
	}
}

func recoverRPC(logger *slog.Logger) func(context.Context, connect.Spec, http.Header, any) error {
	return func(ctx context.Context, spec connect.Spec, _ http.Header, p any) error {
		logger.ErrorContext(ctx, "rpc panicked",
			slog.String("procedure", spec.Procedure),
			slog.Any("panic", p),
			slog.String("stack", string(debug.Stack())),
		)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}

// Recover keeps a panic in a plain HTTP handler from taking down the
// process. Connect handlers recover on their own (HandlerOptions).
func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				panic(p)
			}
			logger.ErrorContext(r.Context(), "http handler panicked",
				slog.String("path", r.URL.Path),
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}
