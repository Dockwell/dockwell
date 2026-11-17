// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// setupStand is an agent keeping one held route's setup link, at a clock a test
// moves.
func setupStand(t *testing.T, key string, until time.Time, now *time.Time) *Server {
	t.Helper()
	return setupAgent(t, t.TempDir(), key, until, now, "rte_blog")
}

// setupAgent is an agent keeping its routes' setup links in dir, as one that starts
// again finds them, each route of blog.example.com, the first at /, the next at /api.
func setupAgent(t *testing.T, dir, key string, until time.Time, now *time.Time, routes ...string) *Server {
	t.Helper()
	k := &Keep{Dir: dir, Now: func() time.Time { return *now }}
	sum := sha256.Sum256([]byte(key))
	for i, id := range routes {
		path := ""
		if i > 0 {
			path = "/api"
		}
		if err := k.PutSetup(id, "blog.example.com", path, &resourcev1.RouteSetup{KeyHash: hex.EncodeToString(sum[:]), Until: until.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{
		Routes: func(string) (Route, bool) { return Route{}, false },
		Setups: k.Setup, SetupsOn: k.SetupsOn, UseSetup: k.UseSetupKey,
		Now: func() time.Time { return *now },
	}
}

// askRoute is askSetup for another route.
func askRoute(s *Server, route, uri string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/setup/"+route, http.NoBody)
	r.Header.Set("X-Forwarded-Method", http.MethodGet)
	r.Header.Set("X-Forwarded-Uri", uri)
	r.Header.Set("Accept", html)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// askSetup is forward_auth for the held route, as Caddy sends it, with cookies.
func askSetup(s *Server, uri, accept string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/setup/rte_blog", http.NoBody)
	r.Header.Set("X-Forwarded-Method", http.MethodGet)
	r.Header.Set("X-Forwarded-Uri", uri)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// The setup link's key, once, sets a Strict __Host- cookie and answers with a page
// that goes on by itself, never a 2xx that would send the key on to the app, nor a
// redirect, and with no referrer. That browser then gets through the hold; anyone
// else, and that key again, is told the app isn't open yet (ADR 0105).
func TestASetupLinkLetsOneBrowserThroughTheHold(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := setupStand(t, "the-key", now.Add(time.Hour), &now)
	if w := askSetup(s, "/", html); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "open yet") {
		t.Fatalf("without the link: %d %s", w.Code, w.Body)
	}
	if w := askSetup(s, "/", "*/*"); w.Code != http.StatusServiceUnavailable || w.Body.String() != HeldBody {
		t.Errorf("a script without the link: %d %q", w.Code, w.Body)
	}
	if w := askSetup(s, SetupPath+"?key=not-the-key", html); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Errorf("a wrong key: %d, cookies %v", w.Code, w.Result().Cookies())
	}
	w := askSetup(s, SetupPath+"?key=the-key", html)
	if w.Code < 300 || (w.Code >= 300 && w.Code < 400) {
		t.Errorf("the link answered %d, which sends it on or redirects", w.Code)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy is %q", got)
	}
	if !strings.Contains(w.Body.String(), `http-equiv="refresh"`) || !strings.Contains(w.Body.String(), "url=/") {
		t.Errorf("the page doesn't go on by itself:\n%s", w.Body)
	}
	c := cookie(t, w, SetupName("rte_blog"))
	if !strings.HasPrefix(c.Name, "__Host-dw_") || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("the cookie is %+v", c)
	}
	if strings.Contains(c.Value, "the-key") {
		t.Error("the cookie holds the key")
	}
	if w := askSetup(s, "/wp-admin/install.php", html, c); w.Code != http.StatusOK {
		t.Errorf("with the cookie: %d", w.Code)
	}
	if w := askSetup(s, SetupPath+"?key=the-key", html); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Errorf("the key a second time: %d, cookies %v", w.Code, w.Result().Cookies())
	}
	// The hour over, and the minute's leeway, the cookie opens nothing.
	now = now.Add(time.Hour + setupSkew)
	if w := askSetup(s, "/", html, c); w.Code != http.StatusServiceUnavailable {
		t.Errorf("after the hour, with the cookie: %d", w.Code)
	}
}

// A cookie opens only the route, domain and link it was made for: another route's,
// or one from an earlier link of the same route, is told it isn't open yet.
func TestASetupCookieOpensOnlyItsOwnLink(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := setupStand(t, "first", now.Add(time.Hour), &now)
	c := cookie(t, askSetup(s, SetupPath+"?key=first", html), SetupName("rte_blog"))
	// A new link for the same route: the old cookie is for another hour.
	k := &Keep{Dir: t.TempDir()}
	sum := sha256.Sum256([]byte("second"))
	if err := k.PutSetup("rte_blog", "blog.example.com", "", &resourcev1.RouteSetup{KeyHash: hex.EncodeToString(sum[:]), Until: now.Add(50 * time.Minute).Unix()}); err != nil {
		t.Fatal(err)
	}
	s.Setups = k.Setup
	if w := askSetup(s, "/", html, c); w.Code != http.StatusServiceUnavailable {
		t.Errorf("an earlier link's cookie: %d", w.Code)
	}
	// Forged: the value of a cookie for another route.
	other := &http.Cookie{Name: SetupName("rte_blog"), Value: strings.Repeat("x", 40) + "." + strings.Repeat("y", 43)}
	if w := askSetup(s, "/", html, other); w.Code != http.StatusServiceUnavailable {
		t.Errorf("a made-up cookie: %d", w.Code)
	}
}

// A link that says it works for a day works for an hour from when the agent was
// given it: its key sets no cookie two hours on, and a cookie it set lasts the hour
// (the review's M3).
func TestASetupLinkWorksAnHourAtMost(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := setupStand(t, "the-key", now.Add(24*time.Hour), &now)
	c := cookie(t, askSetup(s, SetupPath+"?key=the-key", html), SetupName("rte_blog"))
	if c.MaxAge > int((time.Hour + setupSkew).Seconds()) {
		t.Errorf("the cookie lasts %d s", c.MaxAge)
	}
	later := setupStand(t, "another", now.Add(24*time.Hour), &now)
	now = now.Add(2 * time.Hour)
	if w := askSetup(later, SetupPath+"?key=another", html); len(w.Result().Cookies()) != 0 || w.Code != http.StatusServiceUnavailable {
		t.Errorf("a day-long link two hours on: %d, cookies %v", w.Code, w.Result().Cookies())
	}
	if w := askSetup(s, "/", html, c); w.Code != http.StatusServiceUnavailable {
		t.Errorf("its cookie two hours on: %d", w.Code)
	}
}

// An agent whose clock is behind the control plane's keeps the link working: the
// cookie the link set lets the browser in on its next request (the review's M3).
func TestASetupLinkWorksOnAnAgentWhoseClockIsBehind(t *testing.T) {
	made := time.Unix(1_800_000_000, 0)
	now := made.Add(-30 * time.Second)
	s := setupStand(t, "the-key", made.Add(time.Hour), &now)
	c := cookie(t, askSetup(s, SetupPath+"?key=the-key", html), SetupName("rte_blog"))
	now = now.Add(time.Second)
	if w := askSetup(s, "/", html, c); w.Code != http.StatusOK {
		t.Errorf("a second on, with the cookie: %d", w.Code)
	}
	now = now.Add(50 * time.Minute)
	if w := askSetup(s, "/", html, c); w.Code != http.StatusOK {
		t.Errorf("50 minutes on, with the cookie: %d", w.Code)
	}
}

// A key used once stays used though the agent starts again within the hour, with
// what it keeps on disk (the review's M1).
func TestAUsedSetupKeyStaysUsedAcrossARestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dir := t.TempDir()
	first := setupAgent(t, dir, "the-key", now.Add(time.Hour), &now, "rte_blog")
	cookie(t, askSetup(first, SetupPath+"?key=the-key", html), SetupName("rte_blog"))
	now = now.Add(20 * time.Minute)
	again := setupAgent(t, dir, "the-key", now.Add(40*time.Minute), &now, "rte_blog")
	if w := askSetup(again, SetupPath+"?key=the-key", html); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Errorf("the key after a restart: %d, cookies %v", w.Code, w.Result().Cookies())
	}
	// What's kept of it goes once its link ends.
	now = now.Add(time.Hour)
	other := setupAgent(t, dir, "other", now.Add(time.Hour), &now, "rte_blog")
	askSetup(other, SetupPath+"?key=other", html)
	sum := sha256.Sum256([]byte("the-key"))
	if _, err := os.Stat(filepath.Join(dir, "setup-used", hex.EncodeToString(sum[:]))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a used key past its link's end is still kept: %v", err)
	}
}

// One link opens every held route of its domain: the app at / and its API at /api
// both let in the browser that opened it (the review's M5).
func TestOneSetupLinkOpensEveryHeldRouteOfItsDomain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := setupAgent(t, t.TempDir(), "the-key", now.Add(time.Hour), &now, "rte_blog", "rte_blog_api")
	w := askSetup(s, SetupPath+"?key=the-key", html)
	web, api := cookie(t, w, SetupName("rte_blog")), cookie(t, w, SetupName("rte_blog_api"))
	if got := askRoute(s, "rte_blog_api", "/api/v1/setup", web, api); got.Code != http.StatusOK {
		t.Errorf("the API route with the link's cookies: %d", got.Code)
	}
	if got := askRoute(s, "rte_blog_api", "/api/v1/setup"); got.Code != http.StatusServiceUnavailable {
		t.Errorf("the API route without them: %d", got.Code)
	}
}

// A setup cookie's name has a sign-in cookie's shape, which every route's proxy takes
// out of what reaches the app, and isn't the route's sign-in's.
func TestASetupCookieIsNamedAsTheProxyStripsIt(t *testing.T) {
	session, flow := Names("rte_blog")
	name := SetupName("rte_blog")
	if !regexp.MustCompile(`^__Host-dw_[0-9a-f]{12}$`).MatchString(name) || name == session || name == flow {
		t.Errorf("the setup cookie is %s, beside %s and %s", name, session, flow)
	}
}
