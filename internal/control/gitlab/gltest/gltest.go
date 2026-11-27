// SPDX-License-Identifier: AGPL-3.0-only

// Package gltest is a stand-in for GitLab's REST API v4 and Git smart HTTP
// endpoint used by control-plane and panel end-to-end tests
// (DOCKWELL_TEST_GITLAB). It validates tokens, checks scopes on
// /api/v4/personal_access_tokens/self, records posted commit statuses, serves
// private Git repositories over smart HTTP with Basic oauth2:<token> auth, and
// lets tests inspect them via /_statuses. Never part of a release.
package gltest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi" //nolint:gosec // test stand-in only, running git http-backend on Go 1.27
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Token configures one accepted GitLab access token in the stand-in.
type Token struct {
	Value   string   `json:"value"`
	Name    string   `json:"name"`
	Scopes  []string `json:"scopes"`
	Active  bool     `json:"active"`
	Revoked bool     `json:"revoked"`
}

// RecordedStatus is one commit status posted to the stand-in.
type RecordedStatus struct {
	ID          int64  `json:"id"`
	Project     string `json:"project"`
	SHA         string `json:"sha"`
	State       string `json:"state"`
	Name        string `json:"name"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
	Ref         string `json:"ref"`
}

// Project is one GitLab project returned by GET /api/v4/projects in the stand-in.
type Project struct {
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch,omitempty"`
}

// Server is the fake GitLab API and Git smart HTTP server.
type Server struct {
	mu            sync.Mutex
	tokens        map[string]Token
	projects      []Project
	repos         map[string]string // lowercase projectPath -> bare repo dir
	statuses      []RecordedStatus
	nextID        int64
	failNext      int
	projectsCalls int
	gitCalls      int
	tempDirs      []string
}

// Default test tokens registered by New().
const (
	TokenAPI      = "glpat-e2e-api"       //nolint:gosec // test fixture token
	TokenReadOnly = "glpat-e2e-read"      //nolint:gosec // test fixture token
	TokenReadRepo = "glpat-e2e-repo"      //nolint:gosec // test fixture token
	TokenRepoOnly = "glpat-e2e-repo-only" //nolint:gosec // test fixture token
)

// New creates a stand-in with a default working token ("glpat-e2e-api") that
// has the api scope, a read-only API token ("glpat-e2e-read", read_api only),
// a repo+picker token ("glpat-e2e-repo", read_api + read_repository), and a
// repo-only token ("glpat-e2e-repo-only", read_repository only).
func New() *Server {
	return &Server{
		tokens: map[string]Token{
			TokenAPI:      {Value: TokenAPI, Name: "dockwell-ci", Scopes: []string{"api"}, Active: true},
			TokenReadOnly: {Value: TokenReadOnly, Name: "read-only", Scopes: []string{"read_api"}, Active: true},
			TokenReadRepo: {Value: TokenReadRepo, Name: "dockwell-build", Scopes: []string{"read_api", "read_repository"}, Active: true},
			TokenRepoOnly: {Value: TokenRepoOnly, Name: "repo-only", Scopes: []string{"read_repository"}, Active: true},
		},
		projects: []Project{
			{PathWithNamespace: "acme/web", WebURL: "https://gitlab.example.com/acme/web", DefaultBranch: "main"},
		},
		repos:  map[string]string{},
		nextID: 1000,
	}
}

// Close removes any temporary bare repositories created via /_git-repo.
func (s *Server) Close() {
	s.mu.Lock()
	dirs := s.tempDirs
	s.tempDirs = nil
	s.mu.Unlock()
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// SetProjects replaces the projects returned by GET /api/v4/projects.
func (s *Server) SetProjects(projects []Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects = slices.Clone(projects)
}

// SetRepo registers a bare Git repository on disk to serve over smart HTTP at
// /<projectPath>(.git)?/info/refs and /<projectPath>(.git)?/git-upload-pack.
func (s *Server) SetRepo(projectPath, bareDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repos == nil {
		s.repos = map[string]string{}
	}
	key := strings.ToLower(strings.TrimSuffix(strings.Trim(projectPath, "/"), ".git"))
	s.repos[key] = bareDir
}

// SetToken registers or updates an accepted token.
func (s *Server) SetToken(tok Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]Token{}
	}
	s.tokens[tok.Value] = tok
}

// FailNext causes the next n commit status posts to answer 503.
func (s *Server) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// ProjectsCalls returns how many GET /api/v4/projects requests the stand-in received.
func (s *Server) ProjectsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectsCalls
}

// GitCalls returns how many authenticated Git smart-HTTP requests the stand-in served.
func (s *Server) GitCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gitCalls
}

// Statuses returns a snapshot of every posted commit status.
func (s *Server) Statuses() []RecordedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses)
}

// ServeHTTP answers GitLab v4 API calls, Git smart HTTP calls, and test control endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/_statuses" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Statuses())
	case p == "/_tokens" && r.Method == http.MethodPost:
		var tok Token
		if err := json.NewDecoder(r.Body).Decode(&tok); err != nil || tok.Value == "" {
			http.Error(w, "invalid token payload", http.StatusBadRequest)
			return
		}
		s.SetToken(tok)
		w.WriteHeader(http.StatusNoContent)
	case p == "/_projects" && r.Method == http.MethodPost:
		var projects []Project
		if err := json.NewDecoder(r.Body).Decode(&projects); err != nil {
			http.Error(w, "invalid projects payload", http.StatusBadRequest)
			return
		}
		s.SetProjects(projects)
		w.WriteHeader(http.StatusNoContent)
	case p == "/_git-repo" && r.Method == http.MethodPost:
		s.handleSeedGitRepo(w, r)
	case p == "/api/v4/projects" && r.Method == http.MethodGet:
		s.mu.Lock()
		s.projectsCalls++
		s.mu.Unlock()
		tok, ok := s.auth(r)
		if !ok || !tok.Active || tok.Revoked {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
			return
		}
		if !slices.Contains(tok.Scopes, "api") && !slices.Contains(tok.Scopes, "read_api") {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "403 Forbidden - insufficient_scope"})
			return
		}
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
		s.mu.Lock()
		var out []Project
		for _, proj := range s.projects {
			if q == "" || strings.Contains(strings.ToLower(proj.PathWithNamespace), q) {
				out = append(out, proj)
			}
		}
		s.mu.Unlock()
		if len(out) > 100 {
			w.Header().Set("X-Next-Page", "2")
			out = out[:100]
		}
		if out == nil {
			out = []Project{}
		}
		writeJSON(w, http.StatusOK, out)
	case p == "/api/v4/personal_access_tokens/self" && r.Method == http.MethodGet:
		tok, ok := s.auth(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    tok.Name,
			"scopes":  tok.Scopes,
			"active":  tok.Active,
			"revoked": tok.Revoked,
		})
	case strings.HasPrefix(p, "/api/v4/projects/") && (strings.Contains(p, "/statuses/") || strings.HasSuffix(p, "/statuses")):
		s.handleStatus(w, r)
	case strings.HasSuffix(p, "/info/refs") || strings.HasSuffix(p, "/git-upload-pack"):
		s.handleGitSmartHTTP(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not Found"})
	}
}

func (s *Server) auth(r *http.Request) (Token, bool) {
	val := r.Header.Get("PRIVATE-TOKEN")
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.tokens[val]
	return tok, ok
}

func (s *Server) gitAuth(r *http.Request) (Token, bool) {
	authz := r.Header.Get("Authorization")
	raw, ok := strings.CutPrefix(authz, "Basic ")
	if !ok {
		return Token{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return Token{}, false
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok || user == "" || pass == "" {
		return Token{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, found := s.tokens[pass]
	return tok, found
}

func (s *Server) handleGitSmartHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	var suffix string
	switch {
	case strings.HasSuffix(p, "/info/refs"):
		suffix = "/info/refs"
	case strings.HasSuffix(p, "/git-upload-pack"):
		suffix = "/git-upload-pack"
	default:
		http.NotFound(w, r)
		return
	}
	repoPath := strings.TrimSuffix(strings.Trim(strings.TrimSuffix(p, suffix), "/"), ".git")
	key := strings.ToLower(repoPath)

	s.mu.Lock()
	bareDir := s.repos[key]
	s.mu.Unlock()
	if bareDir == "" {
		http.NotFound(w, r)
		return
	}

	tok, ok := s.gitAuth(r)
	if !ok || !tok.Active || tok.Revoked {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitLab"`)
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return
	}
	if !slices.Contains(tok.Scopes, "api") && !slices.Contains(tok.Scopes, "read_repository") && !slices.Contains(tok.Scopes, "write_repository") {
		http.Error(w, "403 Forbidden - insufficient_scope", http.StatusForbidden)
		return
	}

	s.mu.Lock()
	s.gitCalls++
	s.mu.Unlock()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		http.Error(w, "git binary not found", http.StatusInternalServerError)
		return
	}
	env := []string{
		"GIT_PROJECT_ROOT=" + filepath.Dir(bareDir),
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/" + filepath.Base(bareDir) + suffix,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"PATH=" + os.Getenv("PATH"),
	}
	if proto := r.Header.Get("Git-Protocol"); proto != "" {
		env = append(env, "HTTP_GIT_PROTOCOL="+proto)
	}
	h := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  env,
	}
	h.ServeHTTP(w, r)
}

func (s *Server) handleSeedGitRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Project       string            `json:"project"`
		WebURL        string            `json:"web_url"`
		Branch        string            `json:"branch"`
		ExtraBranches []string          `json:"extra_branches"`
		Files         map[string]string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Project) == "" {
		http.Error(w, "invalid git repo seed payload", http.StatusBadRequest)
		return
	}
	branch := strings.TrimSpace(body.Branch)
	if branch == "" {
		branch = "main"
	}
	if len(body.Files) == 0 {
		body.Files = map[string]string{
			"Dockerfile": "FROM busybox:1.37\nCMD [\"sh\", \"-c\", \"sleep 3600\"]\n",
		}
	}
	root, err := os.MkdirTemp("", "dockwell-gltest-repo-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bareDir := filepath.Join(root, "repo.git")
	workDir := filepath.Join(root, "work")
	runGit := func(dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(r.Context(), "git", args...) //nolint:gosec // test stand-in git invocation
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Fixture",
			"GIT_AUTHOR_EMAIL=fixture@example.com",
			"GIT_COMMITTER_NAME=Fixture",
			"GIT_COMMITTER_EMAIL=fixture@example.com",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if err := os.MkdirAll(bareDir, 0o700); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := runGit(bareDir, "init", "--bare", "--initial-branch="+branch); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := runGit(workDir, "init", "--initial-branch="+branch); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for name, content := range body.Files {
		dst := filepath.Join(workDir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			_ = os.RemoveAll(root)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(dst, []byte(content), 0o600); err != nil {
			_ = os.RemoveAll(root)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if _, err := runGit(workDir, "add", "."); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := runGit(workDir, "commit", "-m", "Initial GitLab private commit"); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	commit, err := runGit(workDir, "rev-parse", "HEAD")
	if err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := runGit(workDir, "push", bareDir, "HEAD:refs/heads/"+branch); err != nil {
		_ = os.RemoveAll(root)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, extra := range body.ExtraBranches {
		extra = strings.TrimSpace(extra)
		if extra == "" || extra == branch {
			continue
		}
		if _, err := runGit(workDir, "push", bareDir, "HEAD:refs/heads/"+extra); err != nil {
			_ = os.RemoveAll(root)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	_, _ = runGit(bareDir, "symbolic-ref", "HEAD", "refs/heads/"+branch)

	webURL := strings.TrimSpace(body.WebURL)
	if webURL == "" {
		webURL = "https://gitlab.example.com/" + strings.Trim(body.Project, "/")
	}
	s.mu.Lock()
	s.tempDirs = append(s.tempDirs, root)
	key := strings.ToLower(strings.TrimSuffix(strings.Trim(body.Project, "/"), ".git"))
	s.repos[key] = bareDir
	found := false
	for i := range s.projects {
		if strings.EqualFold(s.projects[i].PathWithNamespace, body.Project) {
			s.projects[i].WebURL = webURL
			s.projects[i].DefaultBranch = branch
			found = true
			break
		}
	}
	if !found {
		s.projects = append(s.projects, Project{
			PathWithNamespace: strings.Trim(body.Project, "/"),
			WebURL:            webURL,
			DefaultBranch:     branch,
		})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"project": strings.Trim(body.Project, "/"),
		"url":     webURL,
		"branch":  branch,
		"commit":  commit,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.auth(r)
	if !ok || !tok.Active || tok.Revoked {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		return
	}
	if !slices.Contains(tok.Scopes, "api") {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "403 Forbidden - insufficient_scope"})
		return
	}
	rawPath := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(rawPath, "/api/v4/projects/")
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	if r.Method == http.MethodGet && strings.Contains(rest, "/repository/commits/") {
		projEsc, tail, found := strings.Cut(rest, "/repository/commits/")
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not Found"})
			return
		}
		sha := strings.TrimSuffix(tail, "/statuses")
		proj, _ := url.PathUnescape(projEsc)
		name := r.URL.Query().Get("name")
		s.mu.Lock()
		var matching []map[string]any
		for _, st := range s.statuses {
			if st.Project == proj && st.SHA == sha && (name == "" || st.Name == name) {
				matching = append(matching, map[string]any{
					"id": st.ID, "status": st.State, "name": st.Name, "target_url": st.TargetURL, "pipeline_id": st.ID,
				})
			}
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, matching)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "405 Method Not Allowed"})
		return
	}
	projEsc, sha, found := strings.Cut(rest, "/statuses/")
	if !found || projEsc == "" || sha == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	proj, err := url.PathUnescape(projEsc)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid project path"})
		return
	}
	var body struct {
		State       string `json:"state"`
		Name        string `json:"name"`
		TargetURL   string `json:"target_url"`
		Description string `json:"description"`
		Ref         string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	s.mu.Lock()
	if s.failNext > 0 {
		s.failNext--
		s.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "503 Service Unavailable"})
		return
	}
	for i := len(s.statuses) - 1; i >= 0; i-- {
		st := s.statuses[i]
		if st.Project == proj && st.SHA == sha && st.Name == body.Name {
			if st.State == body.State {
				s.mu.Unlock()
				writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Cannot transition status via :enqueue from :" + body.State})
				return
			}
			break
		}
	}
	s.nextID++
	rec := RecordedStatus{
		ID: s.nextID, Project: proj, SHA: sha,
		State: body.State, Name: body.Name, TargetURL: body.TargetURL,
		Description: body.Description, Ref: body.Ref,
	}
	s.statuses = append(s.statuses, rec)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": rec.ID, "status": rec.State, "name": rec.Name,
		"target_url": rec.TargetURL, "pipeline_id": rec.ID,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_, _ = fmt.Fprintf(w, `{"message":%q}`, err.Error())
	}
}
