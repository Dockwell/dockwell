// SPDX-License-Identifier: AGPL-3.0-only

// Package signin is a private route's sign-in with an OpenID Connect provider, on
// the route's agent (ADR 0087): it answers Caddy's forward_auth before each request,
// from a cookie it signed, and talks to the provider only as someone signs in,
// through internal/oidc.
package signin

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/oidc"
)

// Route is a private route that signs people in, as the agent keeps it.
type Route struct {
	ID, Domain, Path string
	// The provider, as the control plane filled it into the route's step.
	Issuer, ClientID, ClientSecret string
	Scopes                         []string
	GroupsClaim                    string
	AnyAccount                     bool
	// Who may sign in: by email, by domain, by group.
	Emails, EmailDomains, Groups []string
	SessionHours                 int
	Generation                   uint64
	// Key is the route's cookie key, which never leaves the server.
	Key []byte
}

// Doer sends a request, as netguard's client does.
type Doer = oidc.Doer

// Server answers Caddy's forward_auth for the routes that sign people in: 200 with
// who it is from a cookie it signed, a redirect to the provider for a navigation,
// 401 for anything else, and the callback and sign-out under the route's path.
type Server struct {
	// Routes gives the route Caddy names, by ID.
	Routes func(id string) (Route, bool)
	// Setups gives a held route's setup link, by ID (ADR 0105); SetupsOn every held
	// route of a domain with a key's hash; and UseSetup says whether a key is used for
	// the first time, keeping that past a restart.
	Setups   func(id string) (Setup, bool)
	SetupsOn func(domain, keyHash string) []Setup
	UseSetup func(hash string, until time.Time) (bool, error)
	// Client reads the provider's discovery, keys and tokens, from this server.
	Client Doer
	// RateLimits enforces per-route visitor rate limits; if nil, Server uses its own.
	RateLimits *RateLimiter
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu            sync.Mutex
	provider      *oidc.Client
	limits        map[string]*limit
	ownRateLimits *RateLimiter
	// used are setup links' keys opened already, by hash, until their links end.
	used map[string]time.Time
}

// oidc is what reads the providers, kept between sign-ins.
func (s *Server) oidc() *oidc.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider == nil {
		// The server's clock as it is at each read, as a test moves it.
		s.provider = &oidc.Client{HTTP: s.Client, Now: s.now}
	}
	return s.provider
}

const (
	// Prefix is where under a route's path sign-in answers itself.
	Prefix = "/.dockwell/signin/"
	// UserHeader tells the app the email of who signed in.
	UserHeader = "X-Dockwell-User"
	// SubjectHeader tells the app who signed in as the provider names them.
	SubjectHeader = "X-Dockwell-Subject"
	// ClientHeader is who the visitor is, as Caddy writes it for the agent alone:
	// the peer, or what a proxy the route names says.
	ClientHeader = "X-Dockwell-Client"
	// exchangesAtOnce, exchangesAMinute and exchangesAVisitor bound the codes a route
	// exchanges, and maxVisitors and maxStates what it keeps to tell them apart.
	exchangesAtOnce   = 4
	exchangesAMinute  = 30
	exchangesAVisitor = 5
	maxVisitors       = 4096
	maxStates         = 8192
	// MaxAnswer caps what a provider's answer reads.
	MaxAnswer = oidc.MaxAnswer
)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ServeHTTP answers forward_auth for /route/<id>, with the visitor's method and URI
// in X-Forwarded-Method and X-Forwarded-Uri.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if id, ok := strings.CutPrefix(r.URL.Path, RateLimitPrefix); ok {
		s.serveRateLimit(w, r, id)
		return
	}
	// A held route's hold, which its setup link lifts for one browser, comes first
	// (ADR 0105).
	if id, ok := strings.CutPrefix(r.URL.Path, "/setup/"); ok {
		s.serveSetup(w, r, id)
		return
	}
	route, ok := s.Routes(strings.TrimPrefix(r.URL.Path, "/route/"))
	if !ok {
		// Closed, never open: a route Caddy names that this agent doesn't sign in for.
		end{Status: http.StatusBadGateway, Title: "Sign-in isn't ready here", Action: "Try again", Href: "/"}.write(w)
		return
	}
	uri := r.Header.Get("X-Forwarded-Uri")
	path, query, _ := strings.Cut(uri, "?")
	base := strings.TrimSuffix(route.Path, "/")
	switch path {
	case base + Prefix + "callback":
		s.callback(w, r, route, query)
		return
	case base + Prefix + "out":
		s.signOut(r.Context(), w, route)
		return
	}
	sessionName, _ := Names(route.ID)
	if c, err := r.Cookie(sessionName); err == nil {
		if sess, err := OpenSession(route.Key, route.ID, route.Generation, c.Value, s.now()); err == nil && route.letsIn(sess.Email, sess.Domain, sess.Groups) {
			// Only an address the provider verified is kept, and only that is said.
			if sess.Email != "" {
				w.Header().Set(UserHeader, sess.Email)
			}
			w.Header().Set(SubjectHeader, sess.Subject)
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	method := r.Header.Get("X-Forwarded-Method")
	if (method != http.MethodGet && method != http.MethodHead) || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		// A script fails plainly, rather than following a redirect to a page.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.begin(r.Context(), w, route, uri)
}

// letsIn says whether a route lets in someone, by email or domain, read on every
// request so taking someone off works at once, or by the groups they signed in with.
// A domain is the one the provider vouches for (vouched), never one read off an
// address alone.
func (r Route) letsIn(email, domain string, groups []string) bool {
	if r.AnyAccount && len(r.Emails)+len(r.EmailDomains)+len(r.Groups) == 0 {
		return true
	}
	if email != "" && slices.Contains(r.Emails, email) {
		return true
	}
	if domain != "" && slices.Contains(r.EmailDomains, domain) {
		return true
	}
	return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(r.Groups, g) })
}

// matched are the route's groups someone is in, which is all the cookie keeps.
func (r Route) matched(groups []string) []string {
	var out []string
	for _, g := range groups {
		if slices.Contains(r.Groups, g) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// redirectURI is where the provider sends someone back to, for this route.
func (r Route) redirectURI() string {
	return "https://" + r.Domain + strings.TrimSuffix(r.Path, "/") + Prefix + "callback"
}

// back is a path on this origin to come back to: what was asked for, or the route's
// own path. It's a path and nothing else: no scheme, no host, and, as written or once
// decoded, no backslash, DEL or control character, which a browser drops or reads as
// a slash, so "/\t/evil.example" or "/\\evil.example" would read as another site
// (#598's review, M1).
func (r Route) back(uri string) string {
	home := r.Path
	if home == "" {
		home = "/"
	}
	odd := func(s string) bool {
		return strings.ContainsFunc(s, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' })
	}
	decoded, err := url.PathUnescape(uri)
	if err != nil || odd(uri) || odd(decoded) || !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") || strings.HasPrefix(decoded, "//") {
		return home
	}
	if u, err := url.Parse(uri); err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return home
	}
	return uri
}

// vouched is the domain a provider vouches an account is at: an address's own, where
// the provider verified it, or at Google its hosted domain alone, as a Google account
// can carry an address at a domain that doesn't manage it (#598's review, L2).
func (r Route) vouched(c oidc.Claims) string {
	if r.Issuer == googleIssuer {
		return c.HostedDomain
	}
	if _, domain, ok := strings.Cut(c.Email, "@"); ok && c.EmailVerified {
		return domain
	}
	return ""
}

// googleIssuer is Google's, whose accounts' addresses are vouched for by hd alone.
const googleIssuer = "https://accounts.google.com"

// begin sends a navigation to the provider, with a flow cookie for the callback.
func (s *Server) begin(ctx context.Context, w http.ResponseWriter, route Route, uri string) {
	doc, err := s.oidc().Discover(ctx, route.Issuer)
	if err != nil {
		unreachable(route, uri, err).write(w)
		return
	}
	b := oidc.Begin()
	_, flowName := Names(route.ID)
	value, err := SealFlow(route.Key, route.Generation, Flow{Route: route.ID, State: b.State, Nonce: b.Nonce, Verifier: b.Verifier, Back: route.back(uri), Ends: s.now().Add(flowFor)})
	if err != nil {
		failed(route, uri, "Dockwell can't keep this sign-in in a cookie.").write(w)
		return
	}
	setCookie(w, flowName, value, int(flowFor/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", b.Authorize(doc, route.ClientID, route.redirectURI(), route.Scopes))
	w.WriteHeader(http.StatusFound)
}

// callback finishes a sign-in: the state matches the flow cookie, the code becomes
// tokens at the provider, the ID token checks out, and the route lets them in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request, route Route, rawQuery string) {
	_, flowName := Names(route.ID)
	q, _ := url.ParseQuery(rawQuery)
	// The provider brings someone it signed out back here, to the address it knows
	// already, with no code and no error (ADR 0087).
	if q.Get("code") == "" && q.Get("error") == "" {
		signedOut(w, route)
		return
	}
	c, err := r.Cookie(flowName)
	if err != nil {
		failed(route, "", "It took longer than 10 minutes, or began in another browser.").write(w)
		return
	}
	flow, err := OpenFlow(route.Key, route.ID, route.Generation, c.Value, s.now())
	if err != nil || q.Get("state") == "" || q.Get("state") != flow.State {
		failed(route, "", "It took longer than 10 minutes, or began in another browser.").write(w)
		return
	}
	if q.Get("error") != "" {
		failed(route, flow.Back, "The provider said: "+clip(q.Get("error"), 80)+".").write(w)
		return
	}
	release, ok := s.exchange(route.ID, clientOf(r), flow.State)
	if !ok {
		failed(route, flow.Back, "Too many people are signing in to this route at once.").write(w)
		return
	}
	claims, err := s.oidc().Redeem(r.Context(), oidc.Exchange{
		Issuer: route.Issuer, ClientID: route.ClientID, ClientSecret: route.ClientSecret,
		Code: q.Get("code"), RedirectURI: route.redirectURI(), Verifier: flow.Verifier, Nonce: flow.Nonce, GroupsClaim: route.GroupsClaim,
	})
	release()
	if err != nil {
		var down *oidc.UnreachableError
		if errors.As(err, &down) {
			unreachable(route, flow.Back, err).write(w)
			return
		}
		failed(route, flow.Back, sentence(err)).write(w)
		return
	}
	// An address the provider hasn't verified names no one, whatever the route lists:
	// it's neither kept nor told to the app (#598's review, M2).
	email := claims.Email
	if !claims.EmailVerified {
		email = ""
	}
	domain := route.vouched(claims)
	if !route.letsIn(email, domain, claims.Groups) {
		who := email
		if who == "" {
			who = "This account"
		}
		end{
			Status: http.StatusForbidden, Title: who + " isn't let in to " + route.Domain + strings.TrimSuffix(route.Path, "/"),
			Action: "Sign in as someone else", Href: strings.TrimSuffix(route.Path, "/") + Prefix + "out",
		}.write(w)
		return
	}
	hours := route.SessionHours
	if hours <= 0 {
		hours = 24
	}
	now := s.now()
	value, err := SealSession(route.Key, Session{
		Route: route.ID, Generation: route.Generation, Subject: claims.Subject, Email: email, Domain: domain,
		Groups: route.matched(claims.Groups), Made: now, Ends: now.Add(time.Duration(hours) * time.Hour),
	})
	if err != nil {
		failed(route, flow.Back, sentence(err)).write(w)
		return
	}
	sessionName, _ := Names(route.ID)
	setCookie(w, sessionName, value, hours*3600)
	setCookie(w, flowName, "", -1)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", flow.Back)
	w.WriteHeader(http.StatusFound)
}

// signOut clears the route's cookies, and signs out at the provider too, where its
// discovery document names an end_session_endpoint: otherwise the provider would sign
// the same account in again at once, and "Sign in as someone else" couldn't. The
// provider brings them back to the redirect address, which is registered there
// already, and that says they're signed out. Where the provider names none, or can't
// be read, it says so here. Whatever a link says, nothing else is where it goes, as
// anyone can make a link to it (#598's review, M1).
func (s *Server) signOut(ctx context.Context, w http.ResponseWriter, route Route) {
	sessionName, flowName := Names(route.ID)
	setCookie(w, sessionName, "", -1)
	setCookie(w, flowName, "", -1)
	doc, err := s.oidc().Discover(ctx, route.Issuer)
	if err != nil || doc.EndSessionEndpoint == "" {
		signedOut(w, route)
		return
	}
	// No id_token_hint, as the session cookie doesn't keep the ID token, which would
	// take it past 4 KB: a provider may ask whether to sign out.
	q := url.Values{"client_id": {route.ClientID}, "post_logout_redirect_uri": {route.redirectURI()}}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", oidc.WithQuery(doc.EndSessionEndpoint, q))
	w.WriteHeader(http.StatusFound)
}

// signedOut clears the route's cookies and says they're signed out, with a way to
// sign in again, rather than going to the route, which would sign them in again at
// once where the provider still knows them. Its status isn't 2xx, which would let
// the request through to the app.
func signedOut(w http.ResponseWriter, route Route) {
	sessionName, flowName := Names(route.ID)
	setCookie(w, sessionName, "", -1)
	setCookie(w, flowName, "", -1)
	end{
		Status: http.StatusUnauthorized, Title: "You're signed out of " + route.Domain + strings.TrimSuffix(route.Path, "/"),
		Action: "Sign in again", Href: route.back(""), Tone: "out",
	}.write(w)
}

// setCookie sets a __Host- cookie, which only this host may set, sent over HTTPS
// alone, to every path, and not on another site's requests but a link's.
func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// limit bounds the codes a route exchanges: at once, in a minute, in a minute from
// one visitor, and once for each sign-in begun, so a visitor's made-up callbacks
// can't spend a route's sign-ins for everyone (#598's review, L3).
type limit struct {
	mu       sync.Mutex
	active   int
	recent   []time.Time
	byClient map[string][]time.Time
	states   map[string]time.Time
}

// exchange takes a place for one code's exchange, for a client and a sign-in's state,
// or says there's none.
func (s *Server) exchange(route, client, state string) (release func(), ok bool) {
	s.mu.Lock()
	if s.limits == nil {
		s.limits = map[string]*limit{}
	}
	l, found := s.limits[route]
	if !found {
		l = &limit{byClient: map[string][]time.Time{}, states: map[string]time.Time{}}
		s.limits[route] = l
	}
	s.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	now := s.now()
	old := func(t time.Time) bool { return now.Sub(t) >= time.Minute }
	l.recent = slices.DeleteFunc(l.recent, old)
	mine := slices.DeleteFunc(l.byClient[client], old)
	if len(l.byClient) > maxVisitors || len(l.states) > maxStates {
		for c, times := range l.byClient {
			if times = slices.DeleteFunc(times, old); len(times) == 0 {
				delete(l.byClient, c)
			}
		}
		for st, at := range l.states {
			if now.Sub(at) >= flowFor {
				delete(l.states, st)
			}
		}
	}
	_, used := l.states[state]
	if used || l.active >= exchangesAtOnce || len(l.recent) >= exchangesAMinute || len(mine) >= exchangesAVisitor ||
		len(l.byClient) > maxVisitors || len(l.states) > maxStates {
		return nil, false
	}
	l.active++
	l.recent = append(l.recent, now)
	l.byClient[client] = append(mine, now)
	l.states[state] = now
	return func() {
		l.mu.Lock()
		l.active--
		l.mu.Unlock()
	}, true
}

// clientOf is who a callback came from, as Caddy writes it in ClientHeader, and
// never what a visitor writes, as X-Forwarded-For's first address is. An IPv6 visitor
// counts by their /64, as one has a whole network of addresses (#598's review, L3).
func clientOf(r *http.Request) string {
	a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(ClientHeader)))
	if err != nil {
		return ""
	}
	if a = a.Unmap(); a.Is6() {
		p, _ := a.WithZone("").Prefix(64)
		return p.String()
	}
	return a.String()
}

// failed is the page for a sign-in that didn't finish, with how to try again.
func failed(route Route, back, said string) end {
	return end{Status: http.StatusBadRequest, Title: "Sign-in to " + route.Domain + " didn't finish", Said: said, Action: "Try again", Href: route.back(back)}
}

// unreachable is the page for a provider this server couldn't reach.
func unreachable(route Route, back string, err error) end {
	host := oidc.HostOf(route.Issuer)
	var down *oidc.UnreachableError
	if errors.As(err, &down) {
		host = down.Host
	}
	return end{Status: http.StatusBadGateway, Title: "Sign-in can't reach " + host + " right now", Action: "Try again", Href: route.back(back)}
}

// sentence is an error as a page's line: capitalized, with a full stop.
func sentence(err error) string {
	s := strings.TrimSpace(err.Error())
	if s == "" {
		return ""
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// clip keeps what a provider says to a length a page holds.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
