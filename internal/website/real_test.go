// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
)

// needCaddy is the Caddy binary DOCKWELL_TEST_CADDY names, of the pinned version,
// as CI's Go on Caddy job sets it.
func needCaddy(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("DOCKWELL_TEST_CADDY")
	if bin == "" {
		t.Skipf("DOCKWELL_TEST_CADDY isn't set, so this doesn't run against real Caddy. Set it to a Caddy %s binary to run it", caddy.Tag)
	}
	return bin
}

// runs starts what the web service's container runs, the same script on the same
// config, with its paths moved into a directory of the test's own and its port
// to a free one. It returns where it listens.
func runs(t *testing.T, bin, config string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, port, _ := net.SplitHostPort(addr)
	dir := t.TempDir()
	config = strings.Replace(config, fmt.Sprintf("\n:%d {", Port), "\n:"+port+" {", 1)
	if !strings.Contains(config, ":"+port+" {") {
		t.Fatalf("the config has no site on :%d:\n%s", Port, config)
	}
	moved := strings.ReplaceAll(script, "/tmp/Caddyfile", filepath.Join(dir, "Caddyfile"))
	moved = strings.Replace(moved, "exec caddy ", "exec "+bin+" ", 1)
	cmd := exec.Command("/bin/sh", "-c", moved, "web", config)
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("Caddy said:\n%s", out.String())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("Caddy didn't listen on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// heard is what the upstream was sent.
type heard struct {
	host, path, query, forwardedFor, proto, forwardedHost string
}

func upstream(t *testing.T) (*httptest.Server, chan heard) {
	t.Helper()
	got := make(chan heard, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- heard{host: r.Host, path: r.URL.Path, query: r.URL.RawQuery, forwardedFor: r.Header.Get("X-Forwarded-For"), proto: r.Header.Get("X-Forwarded-Proto"), forwardedHost: r.Header.Get("X-Forwarded-Host")}
		_, _ = w.Write([]byte("from the upstream"))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// asTheIngress sends what the ingress sends: the website's domain, and its word on
// the visitor and how they came.
func asTheIngress(t *testing.T, addr, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.example.com"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "app.example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body strings.Builder
	if _, err := fmt.Fprint(&body, res.StatusCode); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

// The config a proxy to a URL runs is real Caddy's, through the script its container
// runs: a request reaches the URL under its path, with the website's domain as Host,
// or the URL's own when asked, and with the visitor's address and https as the ingress
// said them, so an app behind it sees who came, and how.
func TestAProxyToAURLOnRealCaddy(t *testing.T) {
	bin := needCaddy(t)
	srv, got := upstream(t)

	under := &resourcev1.WebsiteProxy{Url: srv.URL + "/app"}
	addr := runs(t, bin, Config(&resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: under}}))
	if status := asTheIngress(t, addr, "/hello?x=1"); status != "200" {
		t.Fatalf("a request answered %s", status)
	}
	h := <-got
	if h.path != "/app/hello" || h.query != "x=1" {
		t.Errorf("the upstream was asked for %s?%s, want /app/hello?x=1", h.path, h.query)
	}
	if h.host != "app.example.com" {
		t.Errorf("the upstream was sent Host %q, want the website's domain", h.host)
	}
	if !strings.HasPrefix(h.forwardedFor, "203.0.113.7") || h.proto != "https" || h.forwardedHost != "app.example.com" {
		t.Errorf("the upstream heard the visitor as %q over %q, for %q, want 203.0.113.7 over https, for app.example.com", h.forwardedFor, h.proto, h.forwardedHost)
	}

	own := &resourcev1.WebsiteProxy{Url: srv.URL, SendTargetHost: true}
	addr = runs(t, bin, Config(&resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: own}}))
	if status := asTheIngress(t, addr, "/"); status != "200" {
		t.Fatalf("a request answered %s", status)
	}
	h = <-got
	if want := strings.TrimPrefix(srv.URL, "http://"); h.host != want || h.path != "/" {
		t.Errorf("the upstream was sent Host %q for %s, want its own, %s, for /", h.host, h.path, want)
	}
}
