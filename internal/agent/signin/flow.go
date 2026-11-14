// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
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
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Server answers Caddy's forward_auth for the routes that sign people in: 200 with
// who it is from a cookie it signed, a redirect to the provider for a navigation,
// 401 for anything else, and the callback and sign-out under the route's path.
type Server struct {
	// Routes gives the route Caddy names, by ID.
	Routes func(id string) (Route, bool)
	// Client reads the provider's discovery, keys and tokens, from this server.
	Client Doer
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	issuers map[string]*issuerState
	limits  map[string]*limit
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
	// discoveryFailFor is how long a provider that couldn't be read stays so, so a
	// crowd of visitors waits on one read, not one each (#598's review, L4).
	discoveryFailFor = 30 * time.Second
	discoveryFor     = time.Hour
	keysAgainAfter   = time.Minute
	// MaxAnswer caps what a provider's answer reads.
	MaxAnswer = 256 << 10
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
		s.signOut(w, route)
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
func (r Route) vouched(c Claims) string {
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
	doc, err := s.discover(ctx, route.Issuer)
	if err != nil {
		unreachable(route, uri, err).write(w)
		return
	}
	state, nonce, verifier := random(), random(), random()
	_, flowName := Names(route.ID)
	value, err := SealFlow(route.Key, route.Generation, Flow{Route: route.ID, State: state, Nonce: nonce, Verifier: verifier, Back: route.back(uri), Ends: s.now().Add(flowFor)})
	if err != nil {
		failed(route, uri, "Dockwell can't keep this sign-in in a cookie.").write(w)
		return
	}
	setCookie(w, flowName, value, int(flowFor/time.Second))
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {route.ClientID},
		"redirect_uri":          {route.redirectURI()},
		"scope":                 {strings.Join(append(append([]string{}, "openid", "email", "profile"), route.Scopes...), " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	target := doc.AuthorizationEndpoint
	if strings.Contains(target, "?") {
		target += "&" + q.Encode()
	} else {
		target += "?" + q.Encode()
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// callback finishes a sign-in: the state matches the flow cookie, the code becomes
// tokens at the provider, the ID token checks out, and the route lets them in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request, route Route, rawQuery string) {
	_, flowName := Names(route.ID)
	q, _ := url.ParseQuery(rawQuery)
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
	claims, err := s.redeem(r.Context(), route, q.Get("code"), flow)
	release()
	if err != nil {
		var down *unreachableError
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

// signOut clears the route's cookies, here alone, and goes to the route's own path,
// whatever the link says, as anyone can make a link to it (#598's review, M1).
func (s *Server) signOut(w http.ResponseWriter, route Route) {
	sessionName, flowName := Names(route.ID)
	setCookie(w, sessionName, "", -1)
	setCookie(w, flowName, "", -1)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", route.back(""))
	w.WriteHeader(http.StatusFound)
}

// setCookie sets a __Host- cookie, which only this host may set, sent over HTTPS
// alone, to every path, and not on another site's requests but a link's.
func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// redeem exchanges a code for tokens, and checks the ID token.
func (s *Server) redeem(ctx context.Context, route Route, code string, flow Flow) (Claims, error) {
	if code == "" {
		return Claims{}, errors.New("the provider sent no code")
	}
	doc, err := s.discover(ctx, route.Issuer)
	if err != nil {
		return Claims{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {route.redirectURI()}, "code_verifier": {flow.Verifier}}
	basic := false
	switch {
	case route.ClientSecret == "":
		form.Set("client_id", route.ClientID)
	case slices.Contains(doc.AuthMethods, "client_secret_post") && !slices.Contains(doc.AuthMethods, "client_secret_basic"):
		form.Set("client_id", route.ClientID)
		form.Set("client_secret", route.ClientSecret)
	default:
		basic = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, err
	}
	if basic {
		req.SetBasicAuth(url.QueryEscape(route.ClientID), url.QueryEscape(route.ClientSecret))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := s.Client.Do(req)
	if err != nil {
		return Claims{}, &unreachableError{host: hostOf(doc.TokenEndpoint), err: err}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxAnswer))
	if err != nil {
		return Claims{}, &unreachableError{host: hostOf(doc.TokenEndpoint), err: err}
	}
	var tokens struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &tokens)
	switch {
	case res.StatusCode != http.StatusOK && tokens.Error == "invalid_client":
		return Claims{}, errors.New("the provider refused Dockwell's client. Check its client ID and secret in Settings")
	case res.StatusCode != http.StatusOK:
		return Claims{}, fmt.Errorf("the provider answered %d for the code", res.StatusCode)
	case tokens.IDToken == "":
		return Claims{}, errors.New("the provider sent no ID token. Check that its client asks for openid")
	}
	want := Want{Issuer: route.Issuer, ClientID: route.ClientID, Nonce: flow.Nonce, GroupsClaim: route.GroupsClaim}
	keys, err := s.keys(ctx, route.Issuer, doc, false)
	if err != nil {
		return Claims{}, err
	}
	claims, err := Verify(tokens.IDToken, keys, want, s.now())
	if errors.Is(err, ErrUnknownKey) {
		// The provider may have rotated its keys: read them again, at most once a minute.
		if keys, err = s.keys(ctx, route.Issuer, doc, true); err != nil {
			return Claims{}, err
		}
		claims, err = Verify(tokens.IDToken, keys, want, s.now())
	}
	return claims, err
}

// discovery is what sign-in needs of an issuer's discovery document.
type discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
}

// issuerState is what's kept of an issuer: its document, or why it couldn't be read,
// and its keys, each with the read under way, if one is.
type issuerState struct {
	mu        sync.Mutex
	doc       *discovery
	docAt     time.Time
	docFailed error
	failedAt  time.Time
	keys      []Key
	keysAt    time.Time
	reading   map[string]chan struct{}
}

func (s *Server) issuer(issuer string) *issuerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.issuers == nil {
		s.issuers = map[string]*issuerState{}
	}
	st, ok := s.issuers[issuer]
	if !ok {
		st = &issuerState{reading: map[string]chan struct{}{}}
		s.issuers[issuer] = st
	}
	return st
}

// once has one read of what's named under way at a time. kept says, with st.mu held,
// whether what's kept will do; otherwise the caller reads, or waits for the read under
// way as long as its own request lasts and then looks again. The read goes on for
// those still waiting, bounded by the client's own timeout, whoever leaves. A caller
// that read takes what its read left, and doesn't read again.
func (s *Server) once(ctx context.Context, st *issuerState, what string, kept func() bool, read func(context.Context)) error {
	for {
		st.mu.Lock()
		if kept() {
			st.mu.Unlock()
			return nil
		}
		if wait, ok := st.reading[what]; ok {
			st.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		st.reading[what] = done
		st.mu.Unlock()
		read(context.WithoutCancel(ctx))
		st.mu.Lock()
		delete(st.reading, what)
		kept()
		st.mu.Unlock()
		close(done)
		return nil
	}
}

// discover reads an issuer's discovery document, kept for an hour, or what kept it
// from being read, kept for 30 seconds.
func (s *Server) discover(ctx context.Context, issuer string) (*discovery, error) {
	st := s.issuer(issuer)
	var doc *discovery
	var failed error
	kept := func() bool {
		switch {
		case st.doc != nil && s.now().Sub(st.docAt) < discoveryFor:
			doc = st.doc
			return true
		case st.docFailed != nil && s.now().Sub(st.failedAt) < discoveryFailFor:
			failed = st.docFailed
			return true
		}
		return false
	}
	err := s.once(ctx, st, "discovery", kept, func(ctx context.Context) {
		read, err := s.readDiscovery(ctx, issuer)
		st.mu.Lock()
		defer st.mu.Unlock()
		if err != nil {
			st.docFailed, st.failedAt = err, s.now()
			return
		}
		st.doc, st.docAt, st.docFailed = read, s.now(), nil
	})
	switch {
	case err != nil:
		return nil, err
	case doc == nil && failed == nil:
		return nil, &unreachableError{host: hostOf(issuer), err: errors.New("its discovery document couldn't be read")}
	}
	return doc, failed
}

// readDiscovery reads an issuer's discovery document, and checks it.
func (s *Server) readDiscovery(ctx context.Context, issuer string) (*discovery, error) {
	var doc discovery
	if err := s.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, err
	}
	for _, endpoint := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.JWKSURI} {
		if u, err := url.Parse(endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errors.New("the provider's discovery document has an endpoint that isn't HTTPS")
		}
	}
	if doc.Issuer != issuer {
		return nil, errors.New("the provider's discovery document names another issuer than the one saved")
	}
	return &doc, nil
}

// keys reads an issuer's key set, kept until a token names a key it doesn't hold,
// and then read again at most once a minute.
func (s *Server) keys(ctx context.Context, issuer string, doc *discovery, again bool) ([]Key, error) {
	st := s.issuer(issuer)
	var keys []Key
	var failed error
	kept := func() bool {
		if st.keys != nil && (!again || s.now().Sub(st.keysAt) < keysAgainAfter) {
			keys = st.keys
			return true
		}
		return false
	}
	err := s.once(ctx, st, "keys", kept, func(ctx context.Context) {
		var raw json.RawMessage
		if failed = s.getJSON(ctx, doc.JWKSURI, &raw); failed != nil {
			return
		}
		read, err := ParseKeys(raw)
		if failed = err; err != nil {
			return
		}
		keys = read
		st.mu.Lock()
		st.keys, st.keysAt = read, s.now()
		st.mu.Unlock()
	})
	switch {
	case err != nil:
		return nil, err
	case keys == nil:
		return nil, failed
	}
	return keys, nil
}

// getJSON reads a provider's document.
func (s *Server) getJSON(ctx context.Context, target string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.Client.Do(req)
	if err != nil {
		return &unreachableError{host: hostOf(target), err: err}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return &unreachableError{host: hostOf(target), err: fmt.Errorf("it answered %d", res.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxAnswer))
	if err != nil {
		return &unreachableError{host: hostOf(target), err: err}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("what %s sent isn't JSON", hostOf(target))
	}
	return nil
}

// unreachableError is a provider this server couldn't reach.
type unreachableError struct {
	host string
	err  error
}

func (e *unreachableError) Error() string { return "can't reach " + e.host + ": " + e.err.Error() }

func (e *unreachableError) Unwrap() error { return e.err }

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
	host := hostOf(route.Issuer)
	var down *unreachableError
	if errors.As(err, &down) {
		host = down.host
	}
	return end{Status: http.StatusBadGateway, Title: "Sign-in can't reach " + host + " right now", Action: "Try again", Href: route.back(back)}
}

// random is 32 random bytes in base64url, for state, nonce and the PKCE verifier.
func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hostOf(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return "the provider"
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
