// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/signin"
	"github.com/dockwell/dockwell/internal/ingress"
)

// On real Caddy, a held route with a setup link asks its agent before each request:
// the link's key, once, leaves the browser a cookie through forward_auth and a page
// that goes on, with nothing sent to the app; that browser then reaches the app,
// without Dockwell's cookie, and everyone else, the key again included, is told
// the app isn't open yet (ADR 0105).
func TestASetupLinkGoesThroughTheAgentOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	keep := &signin.Keep{Dir: t.TempDir()}
	const domain = "blog.example.net"
	route := ingress.Route{ID: "dockwell.blog.www", Domain: domain, Upstreams: []string{seenApp(t)}, Held: true, Setup: c.SignInDial()}
	sum := sha256.Sum256([]byte("the-key"))
	if err := keep.PutSetup(route.ID, domain, "", &resourcev1.RouteSetup{KeyHash: hex.EncodeToString(sum[:]), Until: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", SignInSocket(c.Dir))
	if err != nil {
		t.Fatal(err)
	}
	answer := &signin.Server{Routes: keep.Route, Setups: keep.Setup}
	go func() { _ = signin.Serve(ln, answer, 10*time.Second, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() { _ = ln.Close() })
	put(t, c, route)
	if got, err := c.Routes(t.Context()); err != nil || len(got) != 1 || !got[0].Held || got[0].Setup != route.Setup || got[0].Edited {
		t.Fatalf("the route reads back as %+v, %v", got, err)
	}
	b := c.browser(t, domain)
	home := "https://" + domain + "/"
	var got visited
	for range 100 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, home, http.NoBody)
		if res, err := b.Do(req); err == nil {
			_ = res.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got = visit(t, b, http.MethodGet, home, nil); got.status != http.StatusServiceUnavailable || !strings.Contains(got.page, "open yet") {
		t.Fatalf("without the link, it answers %d %q", got.status, got.page)
	}
	got = visit(t, b, http.MethodGet, home+".well-known/dockwell-setup?key=the-key", nil)
	if got.seen != nil || got.status < 300 || got.status < 400 && got.status >= 300 || !strings.Contains(got.page, "url=/") {
		t.Fatalf("the link answers %d, the app saw %v, page %q", got.status, got.seen, got.page)
	}
	if len(got.set) != 1 || !strings.HasPrefix(got.set[0], signin.SetupName(route.ID)+"=") || !strings.Contains(got.set[0], "SameSite=Strict") {
		t.Errorf("the link sets %v", got.set)
	}
	if got = visit(t, b, http.MethodGet, home+"wp-admin/", map[string]string{"Cookie": "theme=dark"}); got.status != http.StatusOK || got.seen["path"] != "/wp-admin/" {
		t.Fatalf("with the cookie, it answers %d, the app saw %v", got.status, got.seen)
	}
	if cookie := got.seen["cookie"]; strings.Contains(cookie, "__Host-dw_") {
		t.Errorf("the app gets the cookies %q", cookie)
	}
	other := c.browser(t, domain)
	if got = visit(t, other, http.MethodGet, home+".well-known/dockwell-setup?key=the-key", nil); got.status != http.StatusForbidden || len(got.set) != 0 {
		t.Errorf("the key again, from another browser: %d, sets %v", got.status, got.set)
	}
	if got = visit(t, other, http.MethodGet, home, nil); got.status != http.StatusServiceUnavailable {
		t.Errorf("another browser gets %d", got.status)
	}
}

// On real Caddy, a setup link's request that the agent doesn't answer is a 502, and
// what Caddy logs of it holds no key, nor a sign-in's code or state (the review's M2).
func TestCaddyLogsNoSetupKeyOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	const domain = "blog.example.net"
	// No agent answers on the sign-in socket.
	route := ingress.Route{ID: "dockwell.blog.www", Domain: domain, Upstreams: []string{seenApp(t)}, Held: true, Setup: c.SignInDial()}
	put(t, c, route)
	b := c.browser(t, domain)
	link := "https://" + domain + "/.well-known/dockwell-setup?key=SECRETKEY123&code=SECRETCODE456&state=SECRETSTATE789&keep=this"
	var got visited
	for range 100 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, link, http.NoBody)
		if res, err := b.Do(req); err == nil {
			_ = res.Body.Close()
			got.status = res.StatusCode
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.status != http.StatusBadGateway {
		t.Fatalf("with no agent, the link answers %d, want 502", got.status)
	}
	var out []byte
	for range 50 {
		out, _ = os.ReadFile(filepath.Join(c.Dir, "caddy.out"))
		if strings.Contains(string(out), "dockwell-setup") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(string(out), "dockwell-setup") || !strings.Contains(string(out), "keep=this") {
		t.Fatalf("Caddy logged nothing of the request:\n%s", out)
	}
	for _, secret := range []string{"SECRETKEY123", "SECRETCODE456", "SECRETSTATE789"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("Caddy's log holds %s:\n%s", secret, out)
		}
	}
}
