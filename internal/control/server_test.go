// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/buildinfo"
)

var testBuild = buildinfo.Info{
	Version:    "v0.4.0",
	Commit:     "3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b",
	CommitTime: time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC),
	GoVersion:  "go1.27.3",
	Platform:   "linux/amd64",
}

// startServer runs a Server on a loopback port and returns its base URL. The
// server stops when the test ends.
func startServer(t *testing.T, logs io.Writer) string {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	srv, err := New(Config{Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Build: testBuild})
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v after cancel, want nil", err)
		}
	})
	return "http://" + ln.Addr().String()
}

// h2cClient speaks unencrypted HTTP/2, as gRPC clients do on loopback.
func h2cClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p}}
}

func TestGetVersionOverEveryProtocol(t *testing.T) {
	base := startServer(t, nil)
	clients := map[string]dockwellv1connect.SystemServiceClient{
		"connect json":   dockwellv1connect.NewSystemServiceClient(http.DefaultClient, base, connect.WithProtoJSON()),
		"connect binary": dockwellv1connect.NewSystemServiceClient(http.DefaultClient, base),
		"connect get":    dockwellv1connect.NewSystemServiceClient(http.DefaultClient, base, connect.WithHTTPGet()),
		"grpc over h2c":  dockwellv1connect.NewSystemServiceClient(h2cClient(), base, connect.WithGRPC()),
		"grpc-web":       dockwellv1connect.NewSystemServiceClient(http.DefaultClient, base, connect.WithGRPCWeb()),
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			res, err := client.GetVersion(t.Context(), connect.NewRequest(&dockwellv1.GetVersionRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			b := res.Msg.GetBuild()
			if b.GetVersion() != "v0.4.0" || b.GetCommit() != testBuild.Commit || b.GetPlatform() != "linux/amd64" {
				t.Errorf("unexpected build: %v", b)
			}
			if !b.GetCommitTime().AsTime().Equal(testBuild.CommitTime) {
				t.Errorf("commit time = %v, want %v", b.GetCommitTime().AsTime(), testBuild.CommitTime)
			}
		})
	}
}

func TestGetVersionAsPlainJSON(t *testing.T) {
	base := startServer(t, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+dockwellv1connect.SystemServiceGetVersionProcedure, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{`"version":"v0.4.0"`, `"goVersion":"go1.27.3"`, `"commitTime":"2026-11-03T21:40:00Z"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body %s missing %s", body, want)
		}
	}
}

func TestHealth(t *testing.T) {
	base := startServer(t, nil)

	t.Run("healthz", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", http.NoBody)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || string(body) != "ok\n" {
			t.Errorf("GET /healthz = %d %q", res.StatusCode, body)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	})

	t.Run("grpc health", func(t *testing.T) {
		// grpchealth ships no client, so check over the Connect protocol with JSON.
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
			base+"/"+grpchealth.HealthV1ServiceName+"/Check", strings.NewReader(`{"service":"`+dockwellv1connect.SystemServiceName+`"}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "SERVING") {
			t.Errorf("health check = %d %s", res.StatusCode, body)
		}
	})
}

func TestUnknownRoute(t *testing.T) {
	base := startServer(t, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/nope", http.NoBody)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", res.StatusCode)
	}
}

// syncWriter lets the server's goroutines and the test share a log buffer.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestRPCsAreLogged(t *testing.T) {
	logs := &syncWriter{}
	base := startServer(t, logs)
	client := dockwellv1connect.NewSystemServiceClient(http.DefaultClient, base)
	if _, err := client.GetVersion(t.Context(), connect.NewRequest(&dockwellv1.GetVersionRequest{})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "procedure="+dockwellv1connect.SystemServiceGetVersionProcedure) {
		t.Errorf("logs missing the RPC:\n%s", logs.String())
	}
}

func TestNewRequiresLogger(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without a logger succeeded")
	}
}

func TestListenAndServeReportsBusyAddress(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv, err := New(Config{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	err = srv.ListenAndServe(t.Context(), ln.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "pass a different --listen address") {
		t.Fatalf("ListenAndServe on a busy port = %v", err)
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
