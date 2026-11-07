// SPDX-License-Identifier: AGPL-3.0-only

package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/dockwell/dockwell/internal/pki"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestShutdownEndsLongRunningRequests(t *testing.T) {
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // a stream that only ends when told to
	})
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, h, Options{Name: "test", Logger: discard(), StreamGrace: 50 * time.Millisecond})
	}()
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/stream", http.NoBody)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	<-started

	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want a clean shutdown", err)
		}
		if took := time.Since(begin); took > 2*time.Second {
			t.Errorf("shutdown took %v; the stream should have been cancelled after the grace period", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return; the open stream held up shutdown")
	}
}

func TestServesHTTP2OverTLS(t *testing.T) {
	ca, err := pki.NewCA("ins_test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.IssueServing([]string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.Proto)
		}), Options{Name: "test", Logger: discard(), TLS: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	var p http.Protocols
	p.SetHTTP2(true)
	client := &http.Client{Transport: &http.Transport{
		Protocols:       &p,
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS13},
	}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+ln.Addr().String()+"/", http.NoBody)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if string(body) != "HTTP/2.0" {
		t.Errorf("protocol = %q, want HTTP/2.0", body)
	}
}

func TestListenWrapsErrors(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	if _, err := Listen(t.Context(), ln.Addr().String()); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("busy port: got %v, want an error wrapping EADDRINUSE", err)
	}
	_, err := Listen(t.Context(), "no-port-here")
	var addrErr *net.AddrError
	if !errors.As(err, &addrErr) {
		t.Errorf("malformed address: got %v, want a *net.AddrError", err)
	}
}

func TestRecoverKeepsServing(t *testing.T) {
	h := Recover(discard(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestLevelFor(t *testing.T) {
	for _, tt := range []struct {
		code connect.Code
		err  error
		want slog.Level
	}{
		{connect.CodeInternal, nil, slog.LevelError},
		{connect.CodeUnimplemented, nil, slog.LevelError},
		{connect.CodeInvalidArgument, nil, slog.LevelInfo},
		{connect.CodeNotFound, nil, slog.LevelInfo},
		{connect.CodeCanceled, context.Canceled, slog.LevelDebug},
		{connect.CodeDeadlineExceeded, context.DeadlineExceeded, slog.LevelInfo},
	} {
		if got := levelFor(tt.code, tt.err); got != tt.want {
			t.Errorf("levelFor(%v) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

// Once requests are cancelled, whatever stays open holds nothing worth
// waiting for: a handler that ignores cancellation, or a connection that
// began just as shutdown did. It is closed soon after, not waited on until
// the timeout.
func TestShutdownDoesntWaitOnWhatIgnoresCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-release // ignores the request's context
	})
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, h, Options{Name: "test", Logger: discard(), StreamGrace: 50 * time.Millisecond, ShutdownTimeout: 10 * time.Second})
	}()
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/stream", http.NoBody)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	<-started

	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want a clean shutdown", err)
		}
		if took := time.Since(begin); took > 3*time.Second {
			t.Errorf("shutdown took %v; it should close what's left soon after cancelling requests", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Serve waited on a handler that ignores cancellation")
	}
}

// logged is what the interceptor logged, by level and message.
type logged struct {
	mu    sync.Mutex
	lines []string
}

func (l *logged) Enabled(context.Context, slog.Level) bool { return true }
func (l *logged) Handle(_ context.Context, r slog.Record) error {
	var code string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "code" {
			code = a.Value.String()
		}
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, r.Level.String()+" "+r.Message+" "+code)
	return nil
}
func (l *logged) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logged) WithGroup(string) slog.Handler      { return l }

func (l *logged) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// A call its caller left isn't a failure, so it logs at debug, and a bare
// context.Canceled answers canceled, not unknown. Only the request's own context
// says the caller left: a handler's own deadline or cancel, a fault wrapping one,
// and a fault of the server's own while the caller left all log as they would
// (#112).
func TestACanceledCallIsNoFailure(t *testing.T) {
	logs := &logged{}
	opts := connect.WithInterceptors(logInterceptor{logger: slog.New(logs)})
	// A call whose name starts with Left waits until the test ends its request's
	// context, as the caller going away does, then answers as it says.
	var mu sync.Mutex
	leave := map[string]context.CancelFunc{}
	entered := make(chan string, 1)
	mux := http.NewServeMux()
	unary := func(name string, answer func(ctx context.Context) error) {
		mux.Handle("/test.v1.Test/"+name, connect.NewUnaryHandler("/test.v1.Test/"+name,
			func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
				if strings.HasPrefix(name, "Left") {
					entered <- name
					<-ctx.Done()
				}
				return nil, answer(ctx)
			}, opts))
	}
	unary("LeftBare", func(context.Context) error { return fmt.Errorf("can't list services: %w", context.Canceled) })
	unary("LeftFault", func(context.Context) error { return errors.New("the store is broken") })
	unary("OwnDeadline", func(ctx context.Context) error {
		c, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		<-c.Done()
		return fmt.Errorf("the agent didn't answer: %w", c.Err())
	})
	unary("OwnCancel", func(ctx context.Context) error {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return fmt.Errorf("a worker stopped: %w", c.Err())
	})
	unary("InternalWrapsCanceled", func(context.Context) error {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("store: %w", context.Canceled))
	})
	unary("UnavailableWrapsCanceled", func(context.Context) error {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("agent lost: %w", context.Canceled))
	})
	// A log view's stream, which reading fails with canceled as its client resets
	// it, wrapping nothing of context's.
	mux.Handle("/test.v1.Test/LeftStream", connect.NewClientStreamHandler("/test.v1.Test/LeftStream",
		func(ctx context.Context, s *connect.ClientStream[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			if s.Receive() {
				entered <- "LeftStream"
			}
			<-ctx.Done()
			return nil, connect.NewError(connect.CodeCanceled, errors.New("stream reset by the client"))
		}, opts))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		mu.Lock()
		leave[strings.TrimPrefix(r.URL.Path, "/test.v1.Test/")] = cancel
		mu.Unlock()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	call := func(name string) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+"/test.v1.Test/"+name).
				CallUnary(t.Context(), connect.NewRequest(&emptypb.Empty{}))
		}()
		if strings.HasPrefix(name, "Left") {
			<-entered
			mu.Lock()
			leave[name]()
			mu.Unlock()
		}
		<-done
	}

	cases := []struct{ name, want string }{
		{"LeftBare", "DEBUG rpc canceled canceled"},
		{"LeftFault", "ERROR rpc failed unknown"},
		{"OwnDeadline", "ERROR rpc failed unknown"},
		{"OwnCancel", "ERROR rpc failed unknown"},
		{"InternalWrapsCanceled", "ERROR rpc failed internal"},
		{"UnavailableWrapsCanceled", "INFO rpc failed unavailable"},
	}
	var want []string
	for _, c := range cases {
		call(c.name)
		want = append(want, c.want)
	}
	stream := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+"/test.v1.Test/LeftStream").CallClientStream(t.Context())
	if err := stream.Send(&emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	mu.Lock()
	leave["LeftStream"]()
	mu.Unlock()
	_, _ = stream.CloseAndReceive()
	want = append(want, "DEBUG rpc canceled canceled")

	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(logs.all(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logs.all()
	for i, c := range cases {
		if i >= len(got) || got[i] != c.want {
			t.Errorf("%s logged %q, want %q", c.name, at(got, i), c.want)
		}
	}
	if at(got, len(cases)) != want[len(cases)] {
		t.Errorf("a stream its client reset logged %q, want %q", at(got, len(cases)), want[len(cases)])
	}
}

func at(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return ""
}
