// SPDX-License-Identifier: AGPL-3.0-only

// Package ghtest is a stand-in for GitHub, its pages and its API at one address,
// for the panel's end-to-end test of connecting GitHub as an App (ADR 0098), which
// reaches no GitHub (DOCKWELL_TEST_GITHUB). It makes the App a manifest asks for,
// installs it on acme, and serves acme's repositories from a Git server it is
// pointed at, to the installation's token alone, as a private repository is. It
// checks every JWT against the App's key, and sends a push to the App's webhook,
// signed with its secret, when a test asks. It is never part of a release.
package ghtest

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// What the stand-in makes: one App, on one account, with one token.
const (
	appID        = 42
	installation = 7
	account      = "acme"
	token        = "ghs_e2e"
)

// Repository is one of acme's repositories, served from the Git server.
type Repository struct {
	// FullName is as GitHub names it, such as "acme/web".
	FullName string `json:"full_name"`
	// Path is where the Git server serves it, such as "web.git".
	Path    string `json:"path"`
	Private bool   `json:"private"`
	// DefaultBranch is its default branch, such as "main".
	DefaultBranch string `json:"default_branch"`
}

// Server is the stand-in.
type Server struct {
	key *rsa.PrivateKey

	mu sync.Mutex
	// git is the Git server acme's repositories are served from.
	git   *url.URL
	repos []Repository
	// codes are the manifests GitHub was posted, by the code it sent back.
	codes map[string]manifest
	// app is the App made, once a code is traded for it.
	app *manifest
	// secret is the App's webhook secret.
	secret string
}

// manifest is what GitHub reads of a manifest posted to it.
type manifest struct {
	Name     string `json:"name"`
	Redirect string `json:"redirect_url"`
	Setup    string `json:"setup_url"`
	Hook     struct {
		URL string `json:"url"`
	} `json:"hook_attributes"`
	owner string
}

// New makes the stand-in, with the App's key made already.
func New() (*Server, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Server{key: k, codes: map[string]manifest{}, secret: "e2e-webhook-secret"}, nil //nolint:gosec // a test's made-up secret
}

// ServeHTTP answers as GitHub's pages and API would.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/_repos":
		s.setRepos(w, r)
	case r.Method == http.MethodPost && p == "/_push":
		s.push(w, r)
	case r.Method == http.MethodPost && (p == "/settings/apps/new" || strings.HasPrefix(p, "/organizations/") && strings.HasSuffix(p, "/settings/apps/new")):
		s.made(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/apps/") && strings.HasSuffix(p, "/installations/new"):
		s.installed(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/app-manifests/") && strings.HasSuffix(p, "/conversions"):
		s.convert(w, strings.TrimSuffix(strings.TrimPrefix(p, "/app-manifests/"), "/conversions"))
	case p == "/installation/repositories":
		if !s.byToken(r) {
			refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		s.listRepos(w)
	case strings.HasPrefix(p, "/app"):
		if !s.asApp(r) {
			refuse(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		s.asTheApp(w, r)
	default:
		s.gitProxy(w, r)
	}
}

func refuse(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}

func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// setRepos points the stand-in at a Git server and the repositories it serves:
// {"git": "http://127.0.0.1:41234", "repositories": [...]}.
func (s *Server) setRepos(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Git          string       `json:"git"`
		Repositories []Repository `json:"repositories"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, err := url.Parse(body.Git)
	if err != nil || u.Host == "" {
		http.Error(w, "git names no server", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.git, s.repos = u, body.Repositories
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// made is GitHub's page that makes an App from a manifest: it sends the browser
// back to the manifest's redirect_url with a code and the state it came with, as
// pressing Create on GitHub's page does.
func (s *Server) made(w http.ResponseWriter, r *http.Request) {
	var m manifest
	if err := json.Unmarshal([]byte(r.FormValue("manifest")), &m); err != nil || m.Redirect == "" {
		http.Error(w, "the manifest can't be read", http.StatusUnprocessableEntity)
		return
	}
	m.owner = account
	code := rand.Text()
	s.mu.Lock()
	s.codes[code] = m
	s.mu.Unlock()
	back := m.Redirect + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(r.URL.Query().Get("state"))
	http.Redirect(w, r, back, http.StatusFound)
}

// installed is GitHub's page that installs the App: it installs it on acme, on the
// repositories the stand-in serves, and sends the browser to the App's setup_url.
func (s *Server) installed(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	app := s.app
	s.mu.Unlock()
	if app == nil {
		http.Error(w, "no App was made", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s?installation_id=%d&setup_action=install", app.Setup, installation), http.StatusFound)
}

// convert trades a code for the App, once.
func (s *Server) convert(w http.ResponseWriter, code string) {
	s.mu.Lock()
	m, ok := s.codes[code]
	delete(s.codes, code)
	if ok {
		s.app = &m
	}
	secret := s.secret
	s.mu.Unlock()
	if !ok {
		refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.key)})
	write(w, map[string]any{
		"id": appID, "slug": "dockwell-e2e", "client_id": "Iv1.e2e", "client_secret": "never-kept",
		"webhook_secret": secret, "pem": string(key), "owner": map[string]string{"login": m.owner, "type": "Organization"},
	})
}

// asApp checks a request's JWT is signed with the App's key.
func (s *Server) asApp(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, sum[:], sig) == nil
}

// byToken checks a request carries the installation's token.
func (s *Server) byToken(r *http.Request) bool {
	a := r.Header.Get("Authorization")
	return a == "Bearer "+token || a == "token "+token
}

// asTheApp answers the App's own calls, made with its JWT.
func (s *Server) asTheApp(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	in := fmt.Sprintf("/app/installations/%d", installation)
	switch {
	case p == "/app":
		write(w, map[string]any{"id": appID, "slug": "dockwell-e2e"})
	case p == "/app/hook/deliveries":
		// Nothing undelivered: the panel reaches as far as GitHub needs.
		write(w, []any{})
	case p == "/app/hook/config" && r.Method == http.MethodPatch:
		var body struct {
			Secret string `json:"secret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Secret != "" {
			s.mu.Lock()
			s.secret = body.Secret
			s.mu.Unlock()
		}
		write(w, map[string]any{})
	case p == in && r.Method == http.MethodGet:
		write(w, map[string]any{
			"id": installation, "app_id": appID, "repository_selection": "selected",
			"account": map[string]string{"login": account, "type": "Organization"},
		})
	case p == in && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case p == in+"/access_tokens" && r.Method == http.MethodPost:
		write(w, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	default:
		refuse(w, http.StatusNotFound, "Not Found")
	}
}

// listRepos lists acme's repositories the installation reaches.
func (s *Server) listRepos(w http.ResponseWriter) {
	s.mu.Lock()
	repos := s.repos
	s.mu.Unlock()
	list := make([]map[string]any, 0, len(repos))
	for i, r := range repos {
		list = append(list, map[string]any{
			"id": 100 + i, "full_name": r.FullName, "private": r.Private, "default_branch": r.DefaultBranch,
		})
	}
	write(w, map[string]any{"total_count": len(list), "repositories": list})
}

// gitProxy serves acme's repositories over Git's smart HTTP from the Git server, a
// private one only with the installation's token, as Basic x-access-token.
func (s *Server) gitProxy(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	git, repos := s.git, s.repos
	s.mu.Unlock()
	for _, repo := range repos {
		rest, ok := strings.CutPrefix(r.URL.Path, "/"+repo.FullName+".git")
		if !ok {
			rest, ok = strings.CutPrefix(r.URL.Path, "/"+repo.FullName)
		}
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) || git == nil {
			continue
		}
		if repo.Private {
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
			if r.Header.Get("Authorization") != want {
				w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
				http.Error(w, "Repository not found.", http.StatusUnauthorized)
				return
			}
		}
		proxy := httputil.NewSingleHostReverseProxy(git)
		r.URL.Path = "/" + repo.Path + rest
		r.Header.Del("Authorization")
		proxy.ServeHTTP(w, r)
		return
	}
	refuse(w, http.StatusNotFound, "Not Found")
}

// push sends the App's webhook a push to repo's ref, which now names after, as
// GitHub does: /_push?repo=acme/web&ref=refs/heads/main&after=<commit>. It answers
// with the status the webhook gave.
func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	app, secret := s.app, s.secret
	s.mu.Unlock()
	if app == nil {
		http.Error(w, "no App was made", http.StatusNotFound)
		return
	}
	web := "http://" + r.Host + "/" + q.Get("repo")
	body, _ := json.Marshal(map[string]any{
		"ref": q.Get("ref"), "after": q.Get("after"),
		"repository":   map[string]any{"full_name": q.Get("repo"), "html_url": web, "clone_url": web + ".git"},
		"installation": map[string]any{"id": installation},
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, app.Hook.URL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", rand.Text())
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	w.WriteHeader(res.StatusCode)
}
