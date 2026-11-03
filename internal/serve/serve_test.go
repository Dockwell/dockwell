// SPDX-License-Identifier: AGPL-3.0-only

package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

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

func TestSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{
		15 * time.Second:        "15 s",
		1500 * time.Millisecond: "1.5 s",
		0:                       "0 s",
	} {
		if got := Seconds(d); got != want {
			t.Errorf("Seconds(%v) = %q, want %q", d, got, want)
		}
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
