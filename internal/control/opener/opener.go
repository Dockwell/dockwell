// SPDX-License-Identifier: AGPL-3.0-only

// Package opener opens the web UI a service publishes on its server through the
// panel, signed in as the panel is, on an origin of the service's own, a subdomain of
// the panel's host (ADR 0061): http://<label>.localhost:<port> while the panel is on
// localhost. A one-time ticket the API hands the panel becomes a cookie of that
// origin alone, which lasts as long as the panel session that asked, and requests
// with it go to the service through the agent of the server it runs on.
package opener

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/store"
)

// Path is where a ticket becomes a cookie, on the service's origin. It's under a
// directory no app's own pages are likely to use.
const Path = "/.dockwell/open"

// CookieName is the cookie that holds an open session, on the service's origin alone.
const CookieName = "dockwell_open"

const (
	// ticketLife is how long a ticket works, once.
	ticketLife = time.Minute
	// recheck is how often an open session checks the panel session and the role
	// behind it, as a terminal's stream does.
	recheck = 30 * time.Second
	// longest is how long an open session lasts at most, whatever its panel session.
	longest = 12 * time.Hour
	// labelLength is how many characters of base32 a label has.
	labelLength = 10
)

// Target is a service's web UI: where it runs, and the port in its container.
type Target struct {
	ServiceID, Service string
	// Project is the service's Compose project, as its containers' labels name it,
	// and ProjectID the Dockwell project it's in, whose roles decide who opens it.
	Project, ProjectID string
	// Server is the server it runs on, by ID.
	Server string
	Port   uint32
}

// Opener issues tickets, and serves what they open.
type Opener struct {
	// Dial connects to a target's port, through its server's agent.
	Dial func(ctx context.Context, t Target) (net.Conn, error)
	// Live reports whether a panel session still works on its site.
	Live func(ctx context.Context, client auth.Client, sessionID string) bool
	// Role is the role a user holds on a project, which must stay a developer's or
	// more for what they opened to stay open.
	Role func(ctx context.Context, userID, projectID string) (string, error)
	// Key makes each service's label, which stays the same for a service.
	Key    []byte
	Logger *slog.Logger
	// Now is the time. Nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	tickets  map[string]*ticket
	sessions map[string]*session
}

// ticket is what a one-time address stands for.
type ticket struct {
	target  Target
	label   string
	user    string
	session string
	site    auth.Site
	expires time.Time
}

// session is an app opened in a browser.
type session struct {
	ticket
	started time.Time
	checked time.Time
	proxy   *httputil.ReverseProxy
}

func (o *Opener) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

var label32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Label is a service's label: ten characters of base32 from an HMAC of its ID, which
// stays the same for it, can't be guessed, and says nothing of the app.
func (o *Opener) Label(serviceID string) string {
	mac := hmac.New(sha256.New, o.Key)
	mac.Write([]byte("dockwell open label " + serviceID))
	return strings.ToLower(label32.EncodeToString(mac.Sum(nil)))[:labelLength]
}

// labelHost is a label's host on localhost, with the panel's port.
var labelHost = regexp.MustCompile(`^([a-z2-7]{10})\.localhost(?::\d+)?$`)

// ErrNotLocalhost is a ticket asked for on a panel reached other than on localhost,
// which this slice of ADR 0061 doesn't open apps on yet.
var ErrNotLocalhost = errors.New("an app opens through the panel on localhost for now: open the panel through an SSH tunnel to its port, then open the app again")

// Ticket makes a one-time address that opens t for the user of a panel session on
// site, and says where, the origin the app answers on, and until when the address
// works.
func (o *Opener) Ticket(userID, sessionID string, site auth.Site, t Target) (url, origin string, expires time.Time, err error) {
	host, port := splitHost(strings.TrimPrefix(site.Origin, "http://"))
	if site.RPID != "localhost" || site.Secure || host != "localhost" {
		return "", "", time.Time{}, ErrNotLocalhost
	}
	token, err := random()
	if err != nil {
		return "", "", time.Time{}, err
	}
	label := o.Label(t.ServiceID)
	origin = "http://" + label + ".localhost"
	if port != "" {
		origin += ":" + port
	}
	expires = o.now().Add(ticketLife)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sweep()
	if o.tickets == nil {
		o.tickets = map[string]*ticket{}
	}
	o.tickets[token] = &ticket{target: t, label: label, user: userID, session: sessionID, site: site, expires: expires}
	return origin + Path + "?ticket=" + token, origin, expires, nil
}

// sweep forgets tickets past their time, and sessions past theirs. Callers hold mu.
func (o *Opener) sweep() {
	now := o.now()
	for k, t := range o.tickets {
		if now.After(t.expires) {
			delete(o.tickets, k)
		}
	}
	for k, s := range o.sessions {
		if now.Sub(s.started) > longest {
			delete(o.sessions, k)
		}
	}
}

// Serves reports a request for a service's origin, which only a browser on this
// machine, or at the end of a tunnel to it, sends.
func (o *Opener) Serves(r *http.Request) bool {
	return labelHost.MatchString(strings.ToLower(r.Host)) && fromLoopback(r.RemoteAddr)
}

// Dispatch sends a request for a service's origin to o, and everything else to next,
// which is the panel and its API behind their guard.
func Dispatch(o *Opener, next http.Handler) http.Handler {
	if o == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.Serves(r) {
			o.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP turns a ticket into a cookie of the service's origin, and sends what
// comes with that cookie to the service.
func (o *Opener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	label := labelHost.FindStringSubmatch(strings.ToLower(r.Host))[1]
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == Path {
		o.open(w, r, label)
		return
	}
	s, why := o.sessionFor(r, label)
	if s == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintln(w, why)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

// open takes a ticket, once, and sets the cookie it stands for.
func (o *Opener) open(w http.ResponseWriter, r *http.Request, label string) {
	refuse := func(why string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, why)
	}
	if r.Method != http.MethodGet {
		refuse("Open the app from the panel.")
		return
	}
	token := r.URL.Query().Get("ticket")
	o.mu.Lock()
	t := o.tickets[token]
	delete(o.tickets, token)
	o.mu.Unlock()
	switch {
	case t == nil || o.now().After(t.expires):
		refuse("This address has been used, or its minute is up. Open the app from the panel again.")
		return
	case t.label != label:
		refuse("This address is for another app. Open the app from the panel again.")
		return
	}
	cookie, err := random()
	if err != nil {
		refuse("Dockwell can't make a session for the app. Try again.")
		return
	}
	now := o.now()
	s := &session{ticket: *t, started: now, checked: now}
	s.proxy = o.proxyFor(s)
	o.mu.Lock()
	if o.sessions == nil {
		o.sessions = map[string]*session{}
	}
	o.sessions[cookie] = s
	o.mu.Unlock()
	o.Logger.InfoContext(r.Context(), "opened an app through the panel",
		slog.String("service", t.target.ServiceID), slog.Uint64("port", uint64(t.target.Port)), slog.String("user", t.user))
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // localhost alone, which plain HTTP never leaves, as the panel's own cookie there
		Name: CookieName, Value: cookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(longest.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sessionFor finds the open session a request's cookie names, for this label, while
// its panel session works and its user may still open the app, or says why not.
func (o *Opener) sessionFor(r *http.Request, label string) (*session, string) {
	const again = "Open the app from the panel again."
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil, "This app opens from the panel. " + again
	}
	o.mu.Lock()
	s := o.sessions[c.Value]
	o.mu.Unlock()
	now := o.now()
	switch {
	case s == nil || s.label != label:
		return nil, "This app's session has ended. " + again
	case now.Sub(s.started) > longest:
		o.end(c.Value)
		return nil, "This app's session has lasted its longest. " + again
	}
	o.mu.Lock()
	due := now.Sub(s.checked) >= recheck
	if due {
		s.checked = now
	}
	o.mu.Unlock()
	if due {
		client := auth.Client{Site: s.site, Addr: r.RemoteAddr, UserAgent: r.UserAgent()}
		if !o.Live(r.Context(), client, s.session) {
			o.end(c.Value)
			return nil, "Your panel session has ended, and the app's with it. Sign in to the panel, then " + strings.ToLower(again[:1]) + again[1:]
		}
		role, err := o.Role(r.Context(), s.user, s.target.ProjectID)
		if err == nil && store.RoleRank(role) < store.RoleRank(store.RoleDeveloper) {
			o.end(c.Value)
			return nil, "You no longer hold a role that opens this app."
		}
	}
	return s, ""
}

func (o *Opener) end(cookie string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.sessions, cookie)
}

// proxyFor sends a session's requests to its target, over connections its agent
// carries, as a reverse proxy in front of the app would: with its Host, and the
// X-Forwarded headers. The panel's own cookies never reach the app, and the app sets
// none past its own origin.
func (o *Opener) proxyFor(s *session) *httputil.ReverseProxy {
	target := s.target
	transport := &http.Transport{
		DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return o.Dial(ctx, target) },
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = pr.In.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			keepAppCookies(pr.Out)
		},
		Transport:      transport,
		ModifyResponse: func(resp *http.Response) error { keepToItsOrigin(resp.Header); return nil },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			o.Logger.WarnContext(r.Context(), "can't reach an opened app", slog.String("service", target.ServiceID), slog.String("error", err.Error()))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, "Dockwell can't reach %s: %v\n", target.Service, err)
		},
	}
}

// ours reports a cookie of Dockwell's, which no app gets or sets.
func ours(name string) bool {
	return name == CookieName || name == auth.LoopbackCookieName || name == auth.SecureCookieName
}

// keepAppCookies takes Dockwell's cookies off a request for an app.
func keepAppCookies(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if !ours(c.Name) {
			r.AddCookie(c)
		}
	}
}

// keepToItsOrigin drops a cookie an app sets for more than its own origin, or by
// a name of Dockwell's, so no app's cookie reaches the panel or another app.
func keepToItsOrigin(h http.Header) {
	set := h.Values("Set-Cookie")
	h.Del("Set-Cookie")
	for _, line := range set {
		c, err := http.ParseSetCookie(line)
		if err != nil || c.Domain != "" || ours(c.Name) || strings.HasPrefix(c.Name, "__Host-dockwell") {
			continue
		}
		h.Add("Set-Cookie", line)
	}
}

// LoadKey reads the key labels are made with from path, making it on first use, so a
// service's label stays the same from one start to the next.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the control plane's own data directory
	if err == nil && len(b) == 32 {
		return b, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("can't read the key apps are opened with, %s: %w", path, err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("making the key apps are opened with: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("can't keep the key apps are opened with in %s: %w", path, err)
	}
	return key, nil
}

// random is 32 random bytes, for a ticket or a cookie.
func random() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("making a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func splitHost(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h), p
	}
	return strings.ToLower(hostport), ""
}

func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
