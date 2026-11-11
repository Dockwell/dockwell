// SPDX-License-Identifier: AGPL-3.0-only

// Package gitfetchtest serves a Git repository for a test, as Git's own
// http-backend does, over HTTPS on loopback, at a name under example.com that
// Client reaches. Nothing in what's tested makes an exception for loopback: a test
// passes Client as the client to read with.
package gitfetchtest

import (
	"context"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // a test's Git server, on loopback, of Go far past 1.6.3
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a repository a test commits to, and its URL.
type Repo struct {
	URL  string
	t    testing.TB
	root string
	work string
}

// New makes an empty repository and serves it. A machine without git's
// http-backend skips the test.
func New(t testing.TB) *Repo {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Skip("git isn't here")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git http-backend isn't here")
	}
	r := &Repo{t: t, root: t.TempDir()}
	r.work = filepath.Join(r.root, "catalog")
	if err := os.MkdirAll(r.work, 0o750); err != nil {
		t.Fatal(err)
	}
	r.git("init", "-q")
	r.git("config", "http.uploadpack", "true")
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + r.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + r.root}}
	r.URL = Serve(t, h) + "/catalog/.git"
	return r
}

// Serve serves h over HTTPS for a test, and returns its URL, at a name under
// example.com that Client reaches.
func Serve(t testing.TB, h http.Handler) string {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return "https://catalog.example.com:" + port
}

// Client is a client for what Serve serves: it trusts the certificate httptest's
// servers share, which names *.example.com, and connects to any name on this
// machine, at the port the URL gives.
func Client() *http.Client {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	tr := srv.Client().Transport.(*http.Transport).Clone()
	srv.Close()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	return &http.Client{Transport: tr}
}

// Commit writes each file, by its path in the repository, removes each one whose
// content is empty, and commits, returning the commit.
func (r *Repo) Commit(files map[string]string) string {
	r.t.Helper()
	for name, body := range files {
		file := filepath.Join(r.work, filepath.FromSlash(name))
		if body == "" {
			if err := os.Remove(file); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil { //nolint:gosec // a test's repository
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "a commit")
	return r.git("rev-parse", "HEAD")
}

// Tag tags the newest commit, as an annotated tag, which names a tag object that
// names the commit.
func (r *Repo) Tag(name string) {
	r.t.Helper()
	r.git("tag", "-a", name, "-m", name)
}

func (r *Repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main", "-C", r.work}, args...)...) //nolint:gosec // a test's repository
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+r.root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
