// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Signing in to a registry (ADR 0086): a login goes to its registry's own host,
// and to the token service it was saved with, and nowhere else. A read with a login
// never goes through a mirror, and nothing it reads is kept for a read without it.

// Auth is a registry login as a call opens it: never logged, never kept past the
// call.
type Auth struct {
	// Name is the login's name, as Dockwell keeps it, for words alone.
	Name string
	// Registry is the host it signs in to, such as "ghcr.io"; "docker.io" for
	// Docker Hub.
	Registry string
	Username string
	Password string
	// TokenHost is the host the registry sent clients to for a token when the
	// login was saved, or empty for a registry that signs in itself.
	TokenHost string
	// Version says which revision of which saved login it is, as the control plane
	// keeps it, for a plan's fingerprint. It's never sent.
	Version string
}

// The errors a read with a login can end in, beside a read's own.
var (
	// ErrSignIn is a registry that refused the login.
	ErrSignIn = errors.New("the registry refused the login")
	// ErrMoved is a registry that sends clients to a token service other than the
	// one the login was saved with, where Dockwell sends nothing.
	ErrMoved = errors.New("the registry sends clients to another host for its tokens, so Dockwell won't sign in there")
)

// Values is a login as values sealed to an agent stream carry it.
func (a Auth) Values() map[string]string {
	return map[string]string{"name": a.Name, "registry": a.Registry, "username": a.Username, "password": a.Password, "token_host": a.TokenHost}
}

// AuthFrom reads a login from the values an agent opened.
func AuthFrom(v map[string]string) (Auth, error) {
	a := Auth{Name: v["name"], Registry: v["registry"], Username: v["username"], Password: v["password"], TokenHost: v["token_host"]}
	if a.Registry == "" || a.Password == "" {
		return Auth{}, errors.New("the registry login the control plane sent is empty")
	}
	return a, nil
}

// Header is the login as Docker's X-Registry-Auth takes it, for one pull.
func (a Auth) Header() string {
	server := a.Registry
	if server == Hub {
		// What Docker names Docker Hub's sign-in by.
		server = "https://index.docker.io/v1/"
	}
	b, _ := json.Marshal(map[string]string{"username": a.Username, "password": a.Password, "serveraddress": server})
	return base64.URLEncoding.EncodeToString(b)
}

// key tells the tokens one login gets from those another, or none, gets.
func (a Auth) key() string {
	if a.Registry == "" {
		return ""
	}
	return " as " + a.Name + "/" + a.Username
}

type authKey struct{}

// WithAuth is ctx carrying a login for the reads made with it.
func WithAuth(ctx context.Context, a Auth) context.Context {
	return context.WithValue(ctx, authKey{}, a)
}

// AuthOf is the login ctx carries, if any.
func AuthOf(ctx context.Context) (Auth, bool) {
	a, ok := ctx.Value(authKey{}).(Auth)
	return a, ok && a.Registry != ""
}

// authFor is the login a read of host signs in with: the one ctx carries, when it's
// for that registry.
func authFor(ctx context.Context, host string) (Auth, bool) {
	a, ok := AuthOf(ctx)
	if !ok || !strings.EqualFold(a.Registry, host) {
		return Auth{}, false
	}
	return a, true
}

// realmAllowed says whether a token's realm is one a login may go to: on the
// registry's own host, or on the host the login was saved with. With none saved,
// as a login checked for the first time, any realm a read may reach is allowed,
// and found.
func realmAllowed(a Auth, base string, realm *url.URL, first bool) bool {
	if first {
		return ownHostRealm(base, realm) || realm.Scheme == "https"
	}
	return tokenServiceAllowed(a, base, realm)
}

// ownHostRealm says whether a realm is on the registry's own host, over a scheme
// as safe as the one the registry is reached by: https for a registry reached over
// https, and http only for one reached over plain http.
func ownHostRealm(base string, realm *url.URL) bool {
	b, err := url.Parse(base)
	if err != nil || !strings.EqualFold(b.Host, realm.Host) {
		return false
	}
	return realm.Scheme == b.Scheme || realm.Scheme == "https"
}

func basic(a Auth) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password))
}

// hostOf is the registry a base URL reads: Docker Hub for its own host, and in a
// test, where every registry is the test's, the one a login names.
func hostOf(ctx context.Context, base, test string) string {
	if test != "" {
		if a, ok := AuthOf(ctx); ok {
			return a.Registry
		}
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	if u.Host == "registry-1.docker.io" {
		return Hub
	}
	return u.Host
}

// SignIn signs in at a login's registry, as a pull there would, and keeps nothing:
// a Basic sign-in at /v2/, or a token from the realm the registry names, with no
// repository in its scope. It says the token's host when it's another than the
// registry's own. first takes a realm on any host a read may reach, for a login
// not yet saved; otherwise only the host the login was saved with, and on ErrMoved
// it says the host the registry names now, where nothing was sent.
func (c *Client) SignIn(ctx context.Context, a Auth, first bool) (string, error) {
	bases := c.bases(a.Registry)
	base := bases[len(bases)-1]
	ctx = WithAuth(ctx, a)
	resp, err := c.do(ctx, http.MethodGet, base+"/v2/", "", "", false)
	if err != nil {
		return "", err
	}
	values := resp.Header.Values("Www-Authenticate")
	code := resp.StatusCode
	drain(resp)
	if code == http.StatusOK {
		// A registry that takes anyone takes this login too.
		return "", nil
	}
	if code != http.StatusUnauthorized {
		return "", fmt.Errorf("%w: it answered %d", ErrUnreachable, code)
	}
	challenges, err := parseChallenges(values)
	if err != nil || len(challenges) == 0 {
		return "", fmt.Errorf("%w: it asks for a sign-in Dockwell can't read", ErrUnreachable)
	}
	// As Docker does: a Bearer challenge first, and every one of them checked.
	bearer, hasBasic, err := pickBearer(challenges)
	if err != nil {
		return "", fmt.Errorf("%w: it names two token services", ErrUnreachable)
	}
	if bearer == nil {
		if !hasBasic {
			return "", fmt.Errorf("%w: it asks for a sign-in Dockwell doesn't speak", ErrUnreachable)
		}
		resp, err := c.do(ctx, http.MethodGet, base+"/v2/", "", basic(a), false)
		if err != nil {
			return "", err
		}
		code := resp.StatusCode
		drain(resp)
		switch code {
		case http.StatusOK:
			return "", nil
		case http.StatusUnauthorized, http.StatusForbidden:
			return "", ErrSignIn
		}
		return "", fmt.Errorf("%w: it answered %d", ErrUnreachable, code)
	}
	params := bearer.params
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || realm.User != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
		return "", fmt.Errorf("%w: its token service can't be read", ErrUnreachable)
	}
	if !realmAllowed(a, base, realm, first) {
		return realm.Host, ErrMoved
	}
	found := ""
	if !sameHost(base, realm) {
		found = realm.Host
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	realm.RawQuery = q.Encode()
	if c.o.Test != "" {
		test, _ := url.Parse(c.o.Test)
		realm.Scheme, realm.Host = test.Scheme, test.Host
	}
	away, err := c.elsewhere(base, realm, realm.Path)
	if err != nil {
		return "", err
	}
	resp, err = c.do(ctx, http.MethodGet, realm.String(), "application/json", basic(a), away)
	if err != nil {
		return "", err
	}
	code = resp.StatusCode
	drain(resp)
	switch code {
	case http.StatusOK:
		return found, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrSignIn
	}
	return "", fmt.Errorf("%w: its token service answered %d", ErrUnreachable, code)
}

// Challenge reads what a login's registry asks of a client, and refuses one that
// would send it to a token service other than the one it was saved with, before
// Docker is handed it for a pull, as Docker follows whatever realm a registry names.
// It reads every challenge in every WWW-Authenticate header, as Docker does, and
// tries a Bearer one before Basic: one Bearer realm anywhere that isn't the
// registry's own host, or the saved token service over HTTPS, refuses the login
// (ErrMoved), and so does a challenge it can't read (ErrUnreadChallenge).
func (c *Client) Challenge(ctx context.Context, a Auth) error {
	bases := c.bases(a.Registry)
	base := bases[len(bases)-1]
	resp, err := c.do(ctx, http.MethodGet, base+"/v2/", "", "", false)
	if err != nil {
		return err
	}
	values := resp.Header.Values("Www-Authenticate")
	code := resp.StatusCode
	drain(resp)
	if len(values) == 0 {
		if code == http.StatusOK {
			// A registry that takes anyone asks for no login.
			return nil
		}
		return ErrUnreadChallenge
	}
	challenges, err := parseChallenges(values)
	if err != nil {
		return ErrUnreadChallenge
	}
	for _, ch := range challenges {
		switch ch.scheme {
		case "basic":
			// Sent to the registry's own host, where it asked for it.
		case "bearer":
			realm, err := url.Parse(ch.params["realm"])
			if err != nil || realm.Host == "" || realm.User != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
				return ErrUnreadChallenge
			}
			if !tokenServiceAllowed(a, base, realm) {
				return ErrMoved
			}
		default:
			// A scheme Docker doesn't speak: it sends nothing for it.
		}
	}
	return nil
}

// ErrUnreadChallenge is a registry whose WWW-Authenticate can't be read, or names a
// realm that isn't a URL, so nothing says where a client would send a login, and
// Dockwell hands Docker none.
var ErrUnreadChallenge = errors.New("the registry asks for a sign-in in a way Dockwell can't read, so Dockwell won't hand Docker the login")

// tokenServiceAllowed says whether a Bearer realm is one a saved login may go to:
// the registry's own host, as its base is reached, or the token service the login
// was saved with, over HTTPS alone.
func tokenServiceAllowed(a Auth, base string, realm *url.URL) bool {
	if ownHostRealm(base, realm) {
		return true
	}
	return realm.Scheme == "https" && a.TokenHost != "" && strings.EqualFold(realm.Host, a.TokenHost)
}

// pickBearer is the Bearer challenge a client answers before any Basic one, as
// Docker does, and whether there's a Basic one. Two Bearer challenges that name
// different realms are a challenge Dockwell can't read, as clients differ in which
// they take.
func pickBearer(challenges []challenge) (*challenge, bool, error) {
	var bearer *challenge
	basic := false
	for i := range challenges {
		switch challenges[i].scheme {
		case "basic":
			basic = true
		case "bearer":
			if bearer != nil && bearer.params["realm"] != challenges[i].params["realm"] {
				return nil, false, ErrUnreadChallenge
			}
			bearer = &challenges[i]
		}
	}
	return bearer, basic, nil
}

// Probe asks a registry's /v2/ with no login, and sends nothing that signs in:
// whether it asks for a sign-in, and the host of the token service it names when
// that's another than its own. One that takes anyone asks for none.
func (c *Client) Probe(ctx context.Context, host string) (asks bool, tokenHost string, err error) {
	bases := c.bases(host)
	base := bases[len(bases)-1]
	resp, err := c.do(ctx, http.MethodGet, base+"/v2/", "", "", false)
	if err != nil {
		return false, "", err
	}
	values := resp.Header.Values("Www-Authenticate")
	code := resp.StatusCode
	drain(resp)
	switch code {
	case http.StatusOK:
		return false, "", nil
	case http.StatusUnauthorized:
	default:
		return false, "", fmt.Errorf("%w: it answered %d, as no registry does", ErrUnreachable, code)
	}
	challenges, err := parseChallenges(values)
	if err != nil || len(challenges) == 0 {
		return false, "", ErrUnreadChallenge
	}
	for _, ch := range challenges {
		if ch.scheme != "bearer" {
			continue
		}
		realm, err := url.Parse(ch.params["realm"])
		if err != nil || realm.Host == "" {
			return false, "", ErrUnreadChallenge
		}
		if !sameHost(base, realm) {
			if tokenHost != "" && !strings.EqualFold(tokenHost, realm.Host) {
				return false, "", ErrUnreadChallenge
			}
			tokenHost = realm.Host
		}
	}
	return true, tokenHost, nil
}

// AuthsValues are several logins as values sealed to an agent stream carry them,
// as a build's are, numbered from 0.
func AuthsValues(auths []Auth) map[string]string {
	out := map[string]string{}
	for i, a := range auths {
		for k, v := range a.Values() {
			out[strconv.Itoa(i)+"/"+k] = v
		}
	}
	return out
}

// AuthsFrom reads several logins from the values an agent opened, in their order.
func AuthsFrom(v map[string]string) ([]Auth, error) {
	byIndex := map[int]map[string]string{}
	for k, val := range v {
		n, field, ok := strings.Cut(k, "/")
		i, err := strconv.Atoi(n)
		if !ok || err != nil || i < 0 || i >= maxAuths {
			return nil, errors.New("the registry logins the control plane sent can't be read")
		}
		if byIndex[i] == nil {
			byIndex[i] = map[string]string{}
		}
		byIndex[i][field] = val
	}
	out := make([]Auth, 0, len(byIndex))
	for i := range len(byIndex) {
		a, err := AuthFrom(byIndex[i])
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// maxAuths is the most logins one call carries: one for each registry a build may
// pull from.
const maxAuths = 32

type buildAuthsKey struct{}

// WithBuildAuths is ctx carrying the logins a build's base images pull with, one
// for each registry (ADR 0086).
func WithBuildAuths(ctx context.Context, auths []Auth) context.Context {
	return context.WithValue(ctx, buildAuthsKey{}, auths)
}

type buildRecheckKey struct{}

// WithBuildRecheck is ctx carrying how a build's logins are checked again, as it
// leaves the queue.
func WithBuildRecheck(ctx context.Context, fn func(context.Context, []Auth) ([]Auth, error)) context.Context {
	return context.WithValue(ctx, buildRecheckKey{}, fn)
}

// BuildRecheckOf is how ctx says a build's logins are checked again, or nil.
func BuildRecheckOf(ctx context.Context) func(context.Context, []Auth) ([]Auth, error) {
	fn, _ := ctx.Value(buildRecheckKey{}).(func(context.Context, []Auth) ([]Auth, error))
	return fn
}

// BuildAuthsOf are the logins ctx carries for a build's base images.
func BuildAuthsOf(ctx context.Context) []Auth {
	a, _ := ctx.Value(buildAuthsKey{}).([]Auth)
	return a
}

// DockerConfig is logins as Docker's config.json holds them, which BuildKit's
// client reads to answer the builder's ask for the login of the registry it pulls
// from, and of no other.
func DockerConfig(auths []Auth) ([]byte, error) {
	type entry struct {
		Auth string `json:"auth"`
	}
	all := map[string]entry{}
	for _, a := range auths {
		server := a.Registry
		if server == Hub {
			server = "https://index.docker.io/v1/"
		}
		all[server] = entry{Auth: base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))}
	}
	return json.Marshal(map[string]any{"auths": all})
}

// OnLoopback says whether a registry's host is the machine's own loopback, by its
// name or its address: localhost, 127.0.0.0/8, ::1 or an unspecified address. A
// build's steps share the builder's loopback, so a login for a registry there would
// go to whatever a step listens on (ADR 0086).
func OnLoopback(host string) bool {
	h := host
	if strings.HasPrefix(h, "[") {
		if end := strings.Index(h, "]"); end > 0 {
			h = h[1:end]
		}
	} else if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// lookupIPAddr resolves a name, as the builder would. Tests stand in for it.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// ResolvesToLoopback says whether a registry's host is on the loopback, or a name
// any of whose addresses is, as the builder would dial it. A name that can't be
// resolved reads as on it, as nothing says where it leads.
func ResolvesToLoopback(ctx context.Context, host string) bool {
	if OnLoopback(host) {
		return true
	}
	h := host
	if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	if net.ParseIP(h) != nil {
		return false
	}
	addrs, err := lookupIPAddr(ctx, h)
	if err != nil || len(addrs) == 0 {
		return true
	}
	for _, a := range addrs {
		if a.IP.IsLoopback() || a.IP.IsUnspecified() {
			return true
		}
	}
	return false
}
