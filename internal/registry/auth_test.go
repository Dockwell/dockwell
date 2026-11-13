// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	privateDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	// The made-up login the tests sign in with.
	testUser     = "ci"
	testPassword = "s3cret-read-token"
)

// basicRegistry is a registry that signs in itself, with Basic, and keeps acme/app
// private.
func basicRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testPassword))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Www-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/":
		case "/v2/acme/app/manifests/1.0":
			w.Header().Set("Docker-Content-Digest", privateDigest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testClient(srv *httptest.Server) *Client {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return New(Options{Test: srv.URL, Secure: c, Plain: c, Public: c})
}

// A registry that signs in itself reads a private image with its login, and none
// without, and says a wrong password is a refusal, not a missing image.
func TestALoginSignsInWithBasic(t *testing.T) {
	srv := basicRegistry(t)
	c := testClient(srv)
	ref := Ref{Registry: "registry.example.com", Repository: "acme/app", Tag: "1.0"}
	login := Auth{Name: "example-ci", Registry: "registry.example.com", Username: testUser, Password: testPassword}

	if _, err := c.Head(t.Context(), ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("a private image without a login = %v, want ErrNotFound", err)
	}
	got, err := c.Head(WithAuth(t.Context(), login), ref)
	if err != nil || got != privateDigest {
		t.Fatalf("with its login, Head = %q, %v", got, err)
	}
	wrong := login
	wrong.Password = "nope"
	if _, err := c.Head(WithAuth(t.Context(), wrong), ref); !errors.Is(err, ErrSignIn) {
		t.Errorf("a wrong password = %v, want ErrSignIn", err)
	}
	if host, err := c.SignIn(t.Context(), login, true); err != nil || host != "" {
		t.Errorf("SignIn = %q, %v, want signed in at the registry itself", host, err)
	}
	if _, err := c.SignIn(t.Context(), wrong, true); !errors.Is(err, ErrSignIn) {
		t.Errorf("SignIn with a wrong password = %v, want ErrSignIn", err)
	}
	// What the login read stays its own: a read without it is refused again.
	if _, err := c.Head(t.Context(), ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after a read with a login, one without = %v, want ErrNotFound", err)
	}
}

// tokenRegistry is a registry whose tokens come from a service on another host,
// auth.example.com, as Docker Hub's come from auth.docker.io. In the test, both are
// the test's server, which logs what the token service was sent.
type tokenRegistry struct {
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
}

func newTokenRegistry(t *testing.T) *tokenRegistry {
	t.Helper()
	tr := &tokenRegistry{}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tr.mu.Lock()
			tr.asked = append(tr.asked, r.Header.Get("Authorization"))
			tr.mu.Unlock()
			user, password, ok := r.BasicAuth()
			if !ok || user != testUser || password != testPassword {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "granted", "expires_in": 300})
			return
		}
		if r.Header.Get("Authorization") != "Bearer granted" {
			w.Header().Set("Www-Authenticate", `Bearer realm="https://auth.example.com/token",service="registry.example.com"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/":
		case "/v2/acme/app/manifests/1.0":
			w.Header().Set("Docker-Content-Digest", privateDigest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *tokenRegistry) sent() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.asked...)
}

// A login goes to the token service it was saved with, and to no other: a
// registry that names another sends it nowhere, and says so.
func TestALoginGoesOnlyToItsTokenService(t *testing.T) {
	tr := newTokenRegistry(t)
	c := testClient(tr.srv)
	ref := Ref{Registry: "registry.example.com", Repository: "acme/app", Tag: "1.0"}

	saved := Auth{Name: "example-ci", Registry: "registry.example.com", Username: testUser, Password: testPassword, TokenHost: "auth.example.com"}
	got, err := c.Head(WithAuth(t.Context(), saved), ref)
	if err != nil || got != privateDigest {
		t.Fatalf("with the token service it was saved with, Head = %q, %v", got, err)
	}
	if sent := tr.sent(); len(sent) != 1 || !strings.HasPrefix(sent[0], "Basic ") {
		t.Fatalf("the token service was sent %q, want the login once", sent)
	}

	elsewhere := saved
	elsewhere.Name, elsewhere.TokenHost = "moved", "tokens.example.net"
	if _, err := c.Head(WithAuth(t.Context(), elsewhere), ref); !errors.Is(err, ErrMoved) {
		t.Errorf("a token service other than the one saved = %v, want ErrMoved", err)
	}
	if found, err := c.SignIn(t.Context(), elsewhere, false); !errors.Is(err, ErrMoved) || found != "auth.example.com" {
		t.Errorf("SignIn of a saved login that moved = %q, %v, want the host it names now and ErrMoved", found, err)
	}
	if sent := tr.sent(); len(sent) != 1 {
		t.Errorf("a login whose registry moved its token service was sent there: %q", sent)
	}

	// A login checked for the first time takes the token service its registry
	// names, and says which, for the plan to save.
	first := saved
	first.TokenHost = ""
	if found, err := c.SignIn(t.Context(), first, true); err != nil || found != "auth.example.com" {
		t.Errorf("a first SignIn = %q, %v, want the token service's host", found, err)
	}
	wrong := first
	wrong.Password = "nope"
	if _, err := c.SignIn(t.Context(), wrong, true); !errors.Is(err, ErrSignIn) {
		t.Errorf("a first SignIn with a wrong password = %v, want ErrSignIn", err)
	}
}

// Before Docker is handed a login, the registry's challenge must name no token
// service but the one saved, as Docker follows any realm a registry names.
func TestAChallengeRefusesAMovedTokenService(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="https://tokens.example.net/token",service="registry.example.com"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	c := New(Options{Secure: srv.Client(), Plain: srv.Client(), Public: srv.Client()})
	saved := Auth{Name: "example-ci", Registry: host, Username: testUser, Password: testPassword, TokenHost: "auth.example.com"}
	if err := c.Challenge(t.Context(), saved); !errors.Is(err, ErrMoved) {
		t.Errorf("Challenge of a token service other than the one saved = %v, want ErrMoved", err)
	}
	saved.TokenHost = "tokens.example.net"
	if err := c.Challenge(t.Context(), saved); err != nil {
		t.Errorf("Challenge of the token service saved = %v", err)
	}
}

// A login goes to the registry itself, never to Docker Hub's mirrors, which read
// Docker Hub's public images for anyone.
func TestALoginNeverGoesToAMirror(t *testing.T) {
	var mu sync.Mutex
	var mirrored, hub []string
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mirrored = append(mirrored, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(mirror.Close)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testPassword))
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hub = append(hub, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Www-Authenticate", `Basic realm="hub"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", privateDigest)
	}))
	t.Cleanup(registry.Close)
	// Docker Hub's registry is the test's, by the address its name would lead to.
	to, _ := url.Parse(registry.URL)
	secure := doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "registry-1.docker.io" {
			t.Errorf("Docker Hub's read went to %s", r.URL.Host)
		}
		r.URL.Scheme, r.URL.Host = to.Scheme, to.Host
		return registry.Client().Do(r)
	})
	c := New(Options{Mirrors: []string{mirror.URL}, Insecure: func(host string) bool { return strings.HasPrefix(host, "127.0.0.1") }, Secure: secure, Plain: mirror.Client(), Public: secure})
	ref := Ref{Registry: Hub, Repository: "acme/private", Tag: "1.0"}
	login := Auth{Name: "hub-ci", Registry: Hub, Username: testUser, Password: testPassword}
	got, err := c.Head(WithAuth(t.Context(), login), ref)
	if err != nil || got != privateDigest {
		t.Fatalf("Docker Hub with a login = %q, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mirrored) > 0 {
		t.Errorf("a read with a login went to a mirror: %q", mirrored)
	}
	if len(hub) == 0 {
		t.Error("Docker Hub's registry wasn't asked")
	}
}

// Docker's X-Registry-Auth names Docker Hub by its old sign-in address, as
// docker login does, and any other registry by its host.
func TestAHeaderIsWhatDockerTakes(t *testing.T) {
	for registry, server := range map[string]string{Hub: "https://index.docker.io/v1/", "ghcr.io": "ghcr.io"} {
		raw, err := base64.URLEncoding.DecodeString(Auth{Registry: registry, Username: testUser, Password: testPassword}.Header())
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got["serveraddress"] != server || got["username"] != testUser || got["password"] != testPassword {
			t.Errorf("%s's header holds %v", registry, got)
		}
	}
}

// A login sent sealed reads back whole, and an empty one is refused.
func TestALoginReadsBackFromItsValues(t *testing.T) {
	a := Auth{Name: "example-ci", Registry: "registry.example.com", Username: testUser, Password: testPassword, TokenHost: "auth.example.com"}
	got, err := AuthFrom(a.Values())
	if err != nil || got != a {
		t.Errorf("AuthFrom(Values) = %+v, %v", got, err)
	}
	if _, err := AuthFrom(map[string]string{"registry": "ghcr.io"}); err == nil {
		t.Error("a login with no password was taken")
	}
	if _, ok := AuthOf(context.Background()); ok {
		t.Error("a context with no login says it has one")
	}
}

// Docker reads every challenge in every WWW-Authenticate header, tries Bearer first,
// and sends a login to the realm it names: so before Docker is handed one, every
// challenge is read, and one Bearer realm anywhere that isn't the registry's own
// host, or the saved token service over HTTPS, refuses it. One that can't be read
// refuses it too. (Security review of #584, H1.)
func TestAChallengeIsReadAsDockerReadsIt(t *testing.T) {
	saved := Auth{Name: "example-ci", Username: testUser, Password: testPassword, TokenHost: "auth.example.com"}
	for _, c := range []struct {
		name    string
		headers []string
		want    error
	}{
		{"Basic, then a second header's Bearer to another host", []string{`Basic realm="r"`, `Bearer realm="https://attacker.example/token"`}, ErrMoved},
		{"Basic and Bearer in one header", []string{`Basic realm="r", Bearer realm="https://attacker.example/token"`}, ErrMoved},
		{"a tab after Bearer", []string{"Bearer\trealm=\"https://attacker.example/token\""}, ErrMoved},
		{"Bearer in capitals", []string{`BEARER realm="https://attacker.example/token"`}, ErrMoved},
		{"the saved token service over plain HTTP", []string{`Bearer realm="http://auth.example.com/token"`}, ErrMoved},
		{"a realm that isn't a URL", []string{`Bearer realm="::not a url"`}, ErrUnreadChallenge},
		{"a challenge that can't be read", []string{`Bearer realm="https://auth.example.com/token`}, ErrUnreadChallenge},
		{"a realm given twice", []string{`Bearer realm="https://auth.example.com/token",realm="https://attacker.example/token"`}, ErrUnreadChallenge},
		{"a 401 that names no challenge", nil, ErrUnreadChallenge},
		{"Bearer with an empty realm", []string{`Bearer realm=`}, ErrUnreadChallenge},
		{"the saved token service over HTTPS", []string{`Bearer realm="https://auth.example.com/token",service="registry"`}, nil},
		{"Basic alone", []string{`Basic realm="r"`}, nil},
		{"Basic with Bearer on the saved token service", []string{`Basic realm="r"`, `Bearer realm="https://auth.example.com/token"`}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, h := range c.headers {
					w.Header().Add("Www-Authenticate", h)
				}
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(srv.Close)
			a := saved
			a.Registry = strings.TrimPrefix(srv.URL, "https://")
			cl := New(Options{Secure: srv.Client(), Plain: srv.Client(), Public: srv.Client()})
			err := cl.Challenge(t.Context(), a)
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("Challenge = %v, want %v", err, c.want)
			}
		})
	}
}

// A Bearer realm on the registry's own host passes, and so does an open registry.
func TestAChallengeTakesTheRegistrysOwnHost(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Www-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	cl := New(Options{Secure: srv.Client(), Plain: srv.Client(), Public: srv.Client()})
	a := Auth{Name: "own", Registry: strings.TrimPrefix(srv.URL, "https://"), Username: testUser, Password: testPassword}
	if err := cl.Challenge(t.Context(), a); err != nil {
		t.Errorf("a realm on the registry's own host = %v", err)
	}
}

// The parser reads challenges as RFC 9110 writes them.
func TestChallengesAreParsedWhole(t *testing.T) {
	got, err := parseChallenges([]string{`Negotiate abc==, Basic realm="a \"quoted\" one", Bearer realm="https://x.example/t",service=reg,scope="repository:a:pull"`})
	if err != nil || len(got) != 3 {
		t.Fatalf("parsed %+v, %v", got, err)
	}
	if got[0].scheme != "negotiate" || got[0].params[""] != "abc==" {
		t.Errorf("Negotiate: %+v", got[0])
	}
	if got[1].scheme != "basic" || got[1].params["realm"] != `a "quoted" one` {
		t.Errorf("Basic: %+v", got[1])
	}
	if got[2].scheme != "bearer" || got[2].params["service"] != "reg" || got[2].params["scope"] != "repository:a:pull" {
		t.Errorf("Bearer: %+v", got[2])
	}
	for _, bad := range []string{`realm="x"`, `Bearer realm="x" junk`, `Bearer realm="x`} {
		if _, err := parseChallenges([]string{bad}); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
}

// A realm on the registry's own host takes a login over a scheme as safe as the one
// the registry is reached by: an https registry's realm over plain http is refused,
// as Docker would send the login in clear, and a registry reached over http may
// name an http realm on its own host. (Second look at #584's H1.)
func TestARealmOnTheRegistrysOwnHostKeepsItsScheme(t *testing.T) {
	saved := Auth{Name: "own", Username: testUser, Password: testPassword}

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="http://`+r.Host+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(secure.Close)
	a := saved
	a.Registry = strings.TrimPrefix(secure.URL, "https://")
	cl := New(Options{Secure: secure.Client(), Plain: secure.Client(), Public: secure.Client()})
	if err := cl.Challenge(t.Context(), a); !errors.Is(err, ErrMoved) {
		t.Errorf("an https registry's realm over http on its own host = %v, want ErrMoved", err)
	}
	if _, err := cl.Head(WithAuth(t.Context(), a), Ref{Registry: a.Registry, Repository: "acme/app", Tag: "1.0"}); !errors.Is(err, ErrMoved) {
		t.Errorf("Dockwell's own read there = %v, want ErrMoved", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="http://`+r.Host+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(plain.Close)
	b := saved
	b.Registry = strings.TrimPrefix(plain.URL, "http://")
	cl = New(Options{Insecure: func(string) bool { return true }, Secure: plain.Client(), Plain: plain.Client(), Public: plain.Client()})
	if err := cl.Challenge(t.Context(), b); err != nil {
		t.Errorf("a registry reached over http naming an http realm on its own host = %v", err)
	}
}

// Dockwell's own reads with a login read every challenge in every header, Bearer
// first, and check its realm as Docker's pull is checked: a Basic challenge first
// and a Bearer one to another host after it sends the login nowhere. (Second look
// at #584's H1.)
func TestDockwellsOwnReadsReadEveryChallenge(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Add("Www-Authenticate", `Basic realm="r"`)
		w.Header().Add("Www-Authenticate", `Bearer realm="https://attacker.example/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := testClient(srv)
	login := Auth{Name: "example-ci", Registry: "registry.example.com", Username: testUser, Password: testPassword, TokenHost: "auth.example.com"}
	_, err := c.Head(WithAuth(t.Context(), login), Ref{Registry: "registry.example.com", Repository: "acme/app", Tag: "1.0"})
	if !errors.Is(err, ErrMoved) {
		t.Errorf("a read with a login = %v, want ErrMoved", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range asked {
		if strings.Contains(a, "Basic ") || strings.Contains(a, "/token") {
			t.Errorf("the login went somewhere: %q", a)
		}
	}
}
