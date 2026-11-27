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
	"slices"
	"strconv"
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

// CheckRun is a check run recorded by the stand-in.
type CheckRun struct {
	ID         int64  `json:"id"`
	Repo       string `json:"repo"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	DetailsURL string `json:"details_url"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	HTMLURL    string `json:"html_url"`
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
	// permissions granted to the App and installation.
	permissions map[string]string
	// checkRuns recorded via POST/PATCH /repos/:owner/:repo/check-runs.
	checkRuns []CheckRun
	nextCheck int64
	// dropNextCheckResponse creates the check run on POST and then answers 503
	// so tests can verify duplicate-free recovery after a lost response.
	dropNextCheckResponse bool
	// repoCheckErrors simulates repo-specific check-run errors (e.g. archived 403)
	// while the installation still holds checks: write.
	repoCheckErrors map[string]repoCheckErr
}

type repoCheckErr struct {
	status  int
	message string
}

// manifest is what GitHub reads of a manifest posted to it.
type manifest struct {
	Name        string            `json:"name"`
	Redirect    string            `json:"redirect_url"`
	Setup       string            `json:"setup_url"`
	Permissions map[string]string `json:"default_permissions"`
	Hook        struct {
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
	return &Server{ //nolint:gosec // a test's made-up secret
		key:         k,
		codes:       map[string]manifest{},
		secret:      "e2e-webhook-secret",
		permissions: map[string]string{"contents": "read", "metadata": "read", "checks": "write"},
		nextCheck:   500,
	}, nil
}

// SetPermissions replaces the permissions granted to the App and installation.
func (s *Server) SetPermissions(perms map[string]string) {
	s.mu.Lock()
	s.permissions = mapsCopy(perms)
	s.mu.Unlock()
}

// DropNextCheckRunResponse causes the next POST /check-runs to record the check
// run and then answer 503, simulating a lost response or restart before recording.
func (s *Server) DropNextCheckRunResponse() {
	s.mu.Lock()
	s.dropNextCheckResponse = true
	s.mu.Unlock()
}

// SetRepoCheckRunError forces POST /repos/:repo/check-runs to fail with status
// and message (pass status 0 to clear).
func (s *Server) SetRepoCheckRunError(repo string, status int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repoCheckErrors == nil {
		s.repoCheckErrors = map[string]repoCheckErr{}
	}
	if status == 0 {
		delete(s.repoCheckErrors, repo)
		return
	}
	s.repoCheckErrors[repo] = repoCheckErr{status: status, message: message}
}

// SetRepos replaces the repositories listed by installation 7.
func (s *Server) SetRepos(repos []Repository) {
	s.mu.Lock()
	s.repos = slices.Clone(repos)
	s.mu.Unlock()
}

// SeedCode registers a manifest conversion code directly for Go tests.
func (s *Server) SeedCode(code, slug string) {
	s.mu.Lock()
	s.codes[code] = manifest{Name: slug, owner: account, Permissions: mapsCopy(s.permissions)}
	s.mu.Unlock()
}

// CheckRuns returns a snapshot of all recorded check runs.
func (s *Server) CheckRuns() []CheckRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.checkRuns)
}

// ServeHTTP answers as GitHub's pages and API would.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/_repos":
		s.setRepos(w, r)
	case r.Method == http.MethodPost && p == "/_push":
		s.push(w, r)
	case r.Method == http.MethodPost && p == "/_permissions":
		s.setPermissions(w, r)
	case r.Method == http.MethodGet && p == "/_check-runs":
		s.mu.Lock()
		runs := slices.Clone(s.checkRuns)
		s.mu.Unlock()
		write(w, runs)
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
	case strings.HasPrefix(p, "/repos/") && strings.Contains(p, "/check-runs"):
		if !s.byToken(r) {
			refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		s.handleCheckRuns(w, r)
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
		s.mu.Lock()
		perms := mapsCopy(s.permissions)
		s.mu.Unlock()
		write(w, map[string]any{"id": appID, "slug": "dockwell-e2e", "permissions": perms})
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
		s.mu.Lock()
		perms := mapsCopy(s.permissions)
		s.mu.Unlock()
		write(w, map[string]any{
			"id": installation, "app_id": appID, "repository_selection": "selected",
			"permissions": perms,
			"account":     map[string]string{"login": account, "type": "Organization"},
		})
	case p == in && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case p == in+"/access_tokens" && r.Method == http.MethodPost:
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		perms := mapsCopy(s.permissions)
		s.mu.Unlock()
		for k, want := range body.Permissions {
			if perms[k] != want && (want != "read" || perms[k] != "write") {
				refuse(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
				return
			}
		}
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

func mapsCopy(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Server) setPermissions(w http.ResponseWriter, r *http.Request) {
	var perms map[string]string
	if err := json.NewDecoder(r.Body).Decode(&perms); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.permissions = perms
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCheckRuns(w http.ResponseWriter, r *http.Request) {
	// Paths:
	//   POST  /repos/{owner}/{repo}/check-runs
	//   PATCH /repos/{owner}/{repo}/check-runs/{id}
	//   GET   /repos/{owner}/{repo}/commits/{sha}/check-runs
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if len(parts) < 3 {
		refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := parts[0] + "/" + parts[1]
	s.mu.Lock()
	hasChecks := s.permissions["checks"] == "write"
	s.mu.Unlock()
	if !hasChecks {
		refuse(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 5 && parts[2] == "commits" && parts[4] == "check-runs":
		sha := parts[3]
		name := r.URL.Query().Get("check_name")
		s.mu.Lock()
		var matching []map[string]any
		for i := range s.checkRuns {
			cr := &s.checkRuns[i]
			if strings.EqualFold(cr.Repo, repo) && cr.HeadSHA == sha && (name == "" || cr.Name == name) {
				matching = append(matching, map[string]any{
					"id": cr.ID, "name": cr.Name, "external_id": cr.ExternalID,
					"html_url": cr.HTMLURL, "status": cr.Status, "conclusion": cr.Conclusion,
				})
			}
		}
		s.mu.Unlock()
		write(w, map[string]any{"total_count": len(matching), "check_runs": matching})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "check-runs":
		var body struct {
			Name       string `json:"name"`
			HeadSHA    string `json:"head_sha"`
			DetailsURL string `json:"details_url"`
			ExternalID string `json:"external_id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Output     struct {
				Title   string `json:"title"`
				Summary string `json:"summary"`
			} `json:"output"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.mu.Lock()
		if repoErr, ok := s.repoCheckErrors[repo]; ok {
			s.mu.Unlock()
			refuse(w, repoErr.status, repoErr.message)
			return
		}
		s.nextCheck++
		cr := CheckRun{
			ID: s.nextCheck, Repo: repo, Name: body.Name, HeadSHA: body.HeadSHA,
			ExternalID: body.ExternalID, DetailsURL: body.DetailsURL,
			Status: body.Status, Conclusion: body.Conclusion,
			Title: body.Output.Title, Summary: body.Output.Summary,
			HTMLURL: fmt.Sprintf("https://github.com/%s/runs/%d", repo, s.nextCheck),
		}
		s.checkRuns = append(s.checkRuns, cr)
		drop := s.dropNextCheckResponse
		s.dropNextCheckResponse = false
		s.mu.Unlock()
		if drop {
			refuse(w, http.StatusServiceUnavailable, "Service Unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": cr.ID, "name": cr.Name, "external_id": cr.ExternalID,
			"html_url": cr.HTMLURL, "status": cr.Status, "conclusion": cr.Conclusion,
		})
	case r.Method == http.MethodPatch && len(parts) == 4 && parts[2] == "check-runs":
		idNum, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		var body struct {
			DetailsURL string `json:"details_url"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Output     struct {
				Title   string `json:"title"`
				Summary string `json:"summary"`
			} `json:"output"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.checkRuns {
			if s.checkRuns[i].ID != idNum {
				continue
			}
			if body.DetailsURL != "" {
				s.checkRuns[i].DetailsURL = body.DetailsURL
			}
			if body.Status != "" {
				s.checkRuns[i].Status = body.Status
			}
			if body.Conclusion != "" {
				s.checkRuns[i].Conclusion = body.Conclusion
			}
			if body.Output.Title != "" {
				s.checkRuns[i].Title = body.Output.Title
			}
			if body.Output.Summary != "" {
				s.checkRuns[i].Summary = body.Output.Summary
			}
			cr := &s.checkRuns[i]
			write(w, map[string]any{
				"id": cr.ID, "name": cr.Name, "external_id": cr.ExternalID,
				"html_url": cr.HTMLURL, "status": cr.Status, "conclusion": cr.Conclusion,
			})
			return
		}
		refuse(w, http.StatusNotFound, "Not Found")
	default:
		refuse(w, http.StatusNotFound, "Not Found")
	}
}
