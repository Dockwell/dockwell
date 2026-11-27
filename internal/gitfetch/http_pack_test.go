// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch_test

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
	"github.com/dockwell/dockwell/internal/gitfetch"
)

func seedBareRepo(t *testing.T) (bareDir, commit1, commit2, otherBranchCommit string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "web.git")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(bareDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
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
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(bareDir, "init", "--bare", "--initial-branch=main")
	runGit(workDir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(workDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", ".")
	runGit(workDir, "commit", "-m", "first")
	commit1 = runGit(workDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", ".")
	runGit(workDir, "commit", "-m", "second")
	commit2 = runGit(workDir, "rev-parse", "HEAD")
	runGit(workDir, "push", bareDir, "HEAD:refs/heads/main")

	runGit(workDir, "checkout", "-b", "other", commit1)
	if err := os.WriteFile(filepath.Join(workDir, "other.txt"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", ".")
	runGit(workDir, "commit", "-m", "other")
	otherBranchCommit = runGit(workDir, "rev-parse", "HEAD")
	runGit(workDir, "push", bareDir, "HEAD:refs/heads/other")
	runGit(bareDir, "symbolic-ref", "HEAD", "refs/heads/main")
	return bareDir, commit1, commit2, otherBranchCommit
}

func basicOAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
}

func TestFetchVerifiedPackOverSmartHTTP(t *testing.T) {
	t.Parallel()
	bareDir, commit1, commit2, otherCommit := seedBareRepo(t)
	gl := gltest.New()
	gl.SetRepo("acme/web", bareDir)
	srv := httptest.NewServer(gl)
	t.Cleanup(srv.Close)

	packDir := filepath.Join(t.TempDir(), "packs")

	// 1. Tip commit with read_repository token succeeds (read_repository is enough).
	f, size, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:     srv.URL + "/acme/web",
		Ref:         "refs/heads/main",
		Commit:      commit2,
		AuthHeader:  basicOAuth(gltest.TokenRepoOnly),
		Client:      srv.Client(),
		PackTempDir: packDir,
	})
	if err != nil {
		t.Fatalf("FetchVerifiedPack(tip): %v", err)
	}
	if size <= 0 {
		t.Fatalf("pack size = %d, want > 0", size)
	}
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || string(head[:]) != "PACK" {
		t.Fatalf("pack header = %q, err=%v; want PACK", head[:], err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())

	// 2. Older commit on main verifies ancestry over shallow depth > 1.
	f2, size2, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:     srv.URL + "/acme/web",
		Ref:         "refs/heads/main",
		Commit:      commit1,
		AuthHeader:  basicOAuth(gltest.TokenReadRepo),
		Client:      srv.Client(),
		PackTempDir: packDir,
	})
	if err != nil {
		t.Fatalf("FetchVerifiedPack(older ancestor): %v", err)
	}
	if size2 <= 0 {
		t.Fatalf("older pack size = %d, want > 0", size2)
	}
	_ = f2.Close()
	_ = os.Remove(f2.Name())

	// 3. Commit on another branch is refused when asking for main.
	if _, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:     srv.URL + "/acme/web",
		Ref:         "refs/heads/main",
		Commit:      otherCommit,
		AuthHeader:  basicOAuth(gltest.TokenReadRepo),
		Client:      srv.Client(),
		PackTempDir: packDir,
	}); err == nil || (!strings.Contains(err.Error(), "isn't on") && !strings.Contains(err.Error(), "holds no commit")) {
		t.Fatalf("FetchVerifiedPack(commit not on ref) = %v, want not-on-ref refusal", err)
	}

	// 4. Token with only read_api (no read_repository) is refused with 403.
	if _, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:     srv.URL + "/acme/web",
		Ref:         "refs/heads/main",
		Commit:      commit2,
		AuthHeader:  basicOAuth(gltest.TokenReadOnly),
		Client:      srv.Client(),
		PackTempDir: packDir,
	}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("FetchVerifiedPack(read_api only) = %v, want 403 refusal", err)
	}
}

func TestFetchVerifiedPackRefusesCrossHostRedirectAndFollowsSameOrigin(t *testing.T) {
	t.Parallel()
	bareDir, _, commit2, _ := seedBareRepo(t)
	gl := gltest.New()
	gl.SetRepo("acme/web", bareDir)

	var secondHostGotAuth bool
	otherHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			secondHostGotAuth = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(otherHost.Close)

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cross/") {
			http.Redirect(w, r, otherHost.URL+"/acme/web/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		}
		if r.URL.Path == "/alias/web/info/refs" {
			http.Redirect(w, r, "/acme/web.git/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		}
		gl.ServeHTTP(w, r)
	}))
	t.Cleanup(redirecting.Close)

	// Cross-host redirect must be refused and never send credentials to otherHost.
	_, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    redirecting.URL + "/cross/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
		Client:     redirecting.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("cross-host redirect = %v, want refusal", err)
	}
	if secondHostGotAuth {
		t.Fatal("Authorization header was leaked to another host after redirect")
	}

	// Same-origin redirect (/alias/web -> /acme/web.git) succeeds safely.
	f, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    redirecting.URL + "/alias/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
		Client:     redirecting.Client(),
	})
	if err != nil {
		t.Fatalf("same-origin redirect failed: %v", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())

	// Production default client (Client == nil) refuses cleartext http:// URLs.
	_, _, err = gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    "http://gitlab.example.com/acme/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("cleartext HTTP without custom client = %v, want HTTPS refusal", err)
	}
	_ = errors.New("")
}
