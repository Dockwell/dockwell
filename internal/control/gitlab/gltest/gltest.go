// SPDX-License-Identifier: AGPL-3.0-only

// Package gltest is a stand-in for GitLab's REST API v4 used by control-plane
// and panel end-to-end tests (DOCKWELL_TEST_GITLAB). It validates tokens,
// checks scopes on /api/v4/personal_access_tokens/self, records posted commit
// statuses, and lets tests inspect them via /_statuses. Never part of a release.
package gltest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
}

// Server is the fake GitLab API server.
type Server struct {
	mu       sync.Mutex
	tokens   map[string]Token
	projects []Project
	statuses []RecordedStatus
	nextID   int64
	failNext int
}

// Default test tokens registered by New().
const (
	TokenAPI      = "glpat-e2e-api"  //nolint:gosec // test fixture token
	TokenReadOnly = "glpat-e2e-read" //nolint:gosec // test fixture token
)

// New creates a stand-in with a default working token ("glpat-e2e-api") that
// has the api scope, plus a read-only token ("glpat-e2e-read") for testing
// missing-scope handling.
func New() *Server {
	return &Server{
		tokens: map[string]Token{
			TokenAPI:      {Value: TokenAPI, Name: "dockwell-ci", Scopes: []string{"api"}, Active: true},
			TokenReadOnly: {Value: TokenReadOnly, Name: "read-only", Scopes: []string{"read_api"}, Active: true},
		},
		projects: []Project{
			{PathWithNamespace: "acme/web", WebURL: "https://gitlab.example.com/acme/web"},
		},
		nextID: 1000,
	}
}

// SetProjects replaces the projects returned by GET /api/v4/projects.
func (s *Server) SetProjects(projects []Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects = slices.Clone(projects)
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

// Statuses returns a snapshot of every posted commit status.
func (s *Server) Statuses() []RecordedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses)
}

// ServeHTTP answers GitLab v4 API calls and test control endpoints.
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
	case p == "/api/v4/projects" && r.Method == http.MethodGet:
		tok, ok := s.auth(r)
		if !ok || !tok.Active || tok.Revoked {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
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
