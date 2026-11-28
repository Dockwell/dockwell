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

	// 4. Token with only read_api (no read_repository) is refused with 403 and
	// uses the caller-supplied CredentialLabel (L1).
	if _, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:         srv.URL + "/acme/web",
		Ref:             "refs/heads/main",
		Commit:          commit2,
		AuthHeader:      basicOAuth(gltest.TokenReadOnly),
		CredentialLabel: "the GitLab connection corp's token",
		Client:          srv.Client(),
		PackTempDir:     packDir,
	}); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "the GitLab connection corp's token") {
		t.Fatalf("FetchVerifiedPack(read_api only) = %v, want 403 refusal naming CredentialLabel", err)
	}

	// 5. Missing branch error lists available branches (N3).
	if _, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:     srv.URL + "/acme/web",
		Ref:         "no-such-branch",
		Commit:      commit2,
		AuthHeader:  basicOAuth(gltest.TokenRepoOnly),
		Client:      srv.Client(),
		PackTempDir: packDir,
	}); err == nil || !strings.Contains(err.Error(), "Its branches are main, other") {
		t.Fatalf("FetchVerifiedPack(missing branch) = %v, want branch list in error", err)
	}

	// 6. Over-limit pack reports build wording, not catalog wording (L1).
	if _, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:     srv.URL + "/acme/web",
		Ref:         "refs/heads/main",
		Commit:      commit2,
		AuthHeader:  basicOAuth(gltest.TokenRepoOnly),
		Client:      srv.Client(),
		PackTempDir: packDir,
		MaxPack:     16,
	}); err == nil || !strings.Contains(err.Error(), "pack is over") || strings.Contains(err.Error(), "catalog") {
		t.Fatalf("FetchVerifiedPack(over MaxPack) = %v, want build pack-size error without catalog wording", err)
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
		switch {
		case strings.HasPrefix(r.URL.Path, "/cross/"):
			// Same hostname (127.0.0.1), different port -> must refuse (L5).
			http.Redirect(w, r, otherHost.URL+"/cross/web/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		case strings.HasPrefix(r.URL.Path, "/userinfo/"):
			// Redirect carrying userinfo -> must refuse (L5).
			u := strings.Replace(otherHost.URL, "://", "://attacker:pw@", 1)
			http.Redirect(w, r, u+"/userinfo/web/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		case r.URL.Path == "/alias/web/info/refs":
			// Path change to a different repository -> must refuse (N1).
			http.Redirect(w, r, "/acme/web.git/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		case r.URL.Path == "/acme/web/info/refs":
			// Same-repository .git suffix normalization -> allowed (N1).
			http.Redirect(w, r, "/acme/web.git/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		}
		gl.ServeHTTP(w, r)
	}))
	t.Cleanup(redirecting.Close)

	// Port-changing redirect must be refused and never send credentials to otherHost.
	_, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    redirecting.URL + "/cross/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
		Client:     redirecting.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "another host or port") {
		t.Fatalf("cross-port redirect = %v, want refusal", err)
	}
	if secondHostGotAuth {
		t.Fatal("Authorization header was leaked to another host/port after redirect")
	}

	// Redirect carrying userinfo must be refused (L5).
	_, _, err = gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    redirecting.URL + "/userinfo/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
		Client:     redirecting.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("userinfo redirect = %v, want refusal", err)
	}

	// Same-origin redirect changing the repository path (/alias/web -> /acme/web.git) must be refused (N1).
	_, _, err = gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    redirecting.URL + "/alias/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
		Client:     redirecting.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "another repository path") {
		t.Fatalf("path-changing redirect = %v, want refusal", err)
	}

	// Same-origin .git suffix redirect (/acme/web -> /acme/web.git) succeeds safely (N1).
	f, _, err := gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:    redirecting.URL + "/acme/web",
		Ref:        "refs/heads/main",
		Commit:     commit2,
		AuthHeader: basicOAuth(gltest.TokenReadRepo),
		Client:     redirecting.Client(),
	})
	if err != nil {
		t.Fatalf("same-origin .git redirect failed: %v", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())

	// Production default client (Client == nil) refuses cleartext http:// URLs (L5).
	_, _, err = gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:      "http://gitlab.example.com/acme/web",
		Ref:          "refs/heads/main",
		Commit:       commit2,
		AuthHeader:   basicOAuth(gltest.TokenReadRepo),
		AllowPrivate: true,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("cleartext HTTP without custom client = %v, want HTTPS refusal", err)
	}

	// Production default client (Client == nil) refuses netguard loopback even with AllowPrivate (L5).
	_, _, err = gitfetch.FetchVerifiedPack(t.Context(), gitfetch.PackFetchOptions{
		RepoURL:      "https://127.0.0.1/acme/web",
		Ref:          "refs/heads/main",
		Commit:       commit2,
		AuthHeader:   basicOAuth(gltest.TokenReadRepo),
		AllowPrivate: true,
	})
	if err == nil || !strings.Contains(err.Error(), "won't fetch there") {
		t.Fatalf("loopback HTTPS without custom client = %v, want netguard refusal", err)
	}
	_ = errors.New("")
}
