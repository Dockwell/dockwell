// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
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
	k := &Keep{Dir: t.TempDir()}
	sum := sha256.Sum256([]byte(key))
	if err := k.PutSetup("rte_blog", "blog.example.com", "", &resourcev1.RouteSetup{KeyHash: hex.EncodeToString(sum[:]), Until: until.Unix()}); err != nil {
		t.Fatal(err)
	}
	return &Server{
		Routes: func(string) (Route, bool) { return Route{}, false },
		Setups: k.Setup,
		Now:    func() time.Time { return *now },
	}
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
	// The hour over, the cookie opens nothing.
	now = now.Add(time.Hour)
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

// A link that says it works for more than an hour works for an hour.
func TestASetupLinkWorksAnHourAtMost(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := setupStand(t, "the-key", now.Add(24*time.Hour), &now)
	c := cookie(t, askSetup(s, SetupPath+"?key=the-key", html), SetupName("rte_blog"))
	if c.MaxAge > int(time.Hour.Seconds())+1 {
		t.Errorf("the cookie lasts %d s", c.MaxAge)
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
