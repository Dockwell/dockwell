// SPDX-License-Identifier: AGPL-3.0-only

// Package gittest serves Git repositories over Git's smart HTTP protocol, with the
// real git http-backend, for tests of what fetches from one (ADR 0046). A test
// without git on its machine skips.
package gittest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/cgi" //nolint:gosec // a test's own Git server, on a Go long past the CVE
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Server serves the repositories under a directory, each at /<name>.git.
type Server struct {
	// URL is where it listens, such as http://127.0.0.1:41234.
	URL string
	t   testing.TB
	dir string
	srv *httptest.Server
	// Requests counts what it was asked, by path.
	Requests []string
}

// Start serves an empty directory of repositories, until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("this test needs git, which this machine hasn't")
	}
	out, err := exec.CommandContext(context.Background(), git, "--exec-path").Output() //nolint:gosec // git, from PATH
	if err != nil {
		t.Skipf("git doesn't say where its programs are: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("this test needs git http-backend, which this machine's git hasn't")
	}
	s := &Server{t: t, dir: t.TempDir()}
	h := &cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + s.dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + s.dir},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Requests = append(s.Requests, r.URL.Path)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL
	return s
}

// Repo is a repository a Server serves.
type Repo struct {
	s    *Server
	name string
	work string
	// URL is where it is, such as http://127.0.0.1:41234/web.git.
	URL string
}

// Repo makes a repository called name, with one branch, main, and nothing on it.
func (s *Server) Repo(name string) *Repo {
	s.t.Helper()
	bare := filepath.Join(s.dir, name+".git")
	r := &Repo{s: s, name: name, work: s.t.TempDir(), URL: s.URL + "/" + name + ".git"}
	r.git(s.dir, "init", "-q", "--bare", "-b", "main", bare)
	r.git(bare, "config", "http.receivepack", "true")
	r.git(bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	r.git(r.work, "init", "-q", "-b", "main")
	r.git(r.work, "remote", "add", "origin", bare)
	return r
}

// Commit writes files to the branch, by path, as one commit with subject, by the
// author "Leo Park", pushes it, and says what the commit is.
func (r *Repo) Commit(branch, subject string, files map[string]string) string {
	r.s.t.Helper()
	if out := r.gitOut(r.work, "branch", "--show-current"); out != branch {
		if r.gitOut(r.work, "rev-parse", "--verify", "-q", "HEAD") == "" {
			r.git(r.work, "checkout", "-q", "-b", branch)
		} else {
			r.git(r.work, "checkout", "-q", "-B", branch)
		}
	}
	for p, body := range files {
		full := filepath.Join(r.work, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			r.s.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			r.s.t.Fatal(err)
		}
	}
	r.git(r.work, "add", "-A")
	r.git(r.work, "-c", "user.name=Leo Park", "-c", "user.email=leo@example.com", "commit", "-q", "--allow-empty", "-m", subject)
	r.git(r.work, "push", "-q", "-f", "origin", branch)
	return r.gitOut(r.work, "rev-parse", "HEAD")
}

// Tag tags commit as name, with a message, as an annotated tag, and pushes it.
func (r *Repo) Tag(name, commit string) {
	r.s.t.Helper()
	r.git(r.work, "-c", "user.name=Leo Park", "-c", "user.email=leo@example.com", "tag", "-a", "-m", name, name, commit)
	r.git(r.work, "push", "-q", "origin", name)
}

// Ref points the full name ref, such as refs/pull/7/head, at commit, as a host
// keeps a pull request's head beside the branches.
func (r *Repo) Ref(name, commit string) {
	r.s.t.Helper()
	r.git(r.work, "push", "-q", "-f", "origin", commit+":"+name)
}

func (r *Repo) git(dir string, args ...string) {
	r.s.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // a test's own repository
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+r.s.dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.s.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (r *Repo) gitOut(dir string, args ...string) string {
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // as above
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+r.s.dir)
	out, _ := cmd.Output()
	return string(bytes.TrimSpace(out))
}
