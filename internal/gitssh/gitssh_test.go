// SPDX-License-Identifier: AGPL-3.0-only

package gitssh_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/gitssh"
	"github.com/dockwell/dockwell/internal/gitssh/gitsshtest"
	"github.com/dockwell/dockwell/internal/netguard"
)

func initTestRepo(t *testing.T) (repoDir, firstCommit, secondCommit string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37\nCMD [\"echo\", \"v1\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "Dockerfile")
	run("commit", "-q", "-m", "first commit")
	firstCommit = run("rev-parse", "HEAD")

	run("checkout", "-q", "-b", "release")
	if err := os.WriteFile(filepath.Join(dir, "release.txt"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "release.txt")
	run("commit", "-q", "-m", "release branch")
	run("checkout", "-q", "main")

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37\nCMD [\"echo\", \"v2\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "Dockerfile")
	run("commit", "-q", "-m", "second commit")
	secondCommit = run("rev-parse", "HEAD")
	return dir, firstCommit, secondCommit
}

func TestParseAndDeployKeysPage(t *testing.T) {
	ep, err := gitssh.ParseSSH("git@github.com:acme/web.git")
	if err != nil {
		t.Fatalf("ParseSSH scp: %v", err)
	}
	if ep.CanonicalURL != "git@github.com:acme/web.git" || ep.RepoPath != "acme/web.git" || ep.Port != 22 || ep.User != "git" {
		t.Fatalf("unexpected relative SCP endpoint: %+v", ep)
	}

	epAbsSCP, err := gitssh.ParseSSH("git@git.example.com:/srv/git/app.git")
	if err != nil {
		t.Fatalf("ParseSSH abs scp: %v", err)
	}
	if epAbsSCP.CanonicalURL != "ssh://git@git.example.com/srv/git/app.git" || epAbsSCP.RepoPath != "/srv/git/app.git" {
		t.Fatalf("unexpected absolute SCP endpoint: %+v", epAbsSCP)
	}

	ep2, err := gitssh.ParseSSH("ssh://deploy@git.example.com:2222/srv/git/app.git")
	if err != nil {
		t.Fatalf("ParseSSH ssh://: %v", err)
	}
	if ep2.Port != 2222 || ep2.User != "deploy" || ep2.RepoPath != "/srv/git/app.git" || ep2.CanonicalURL != "ssh://deploy@git.example.com:2222/srv/git/app.git" {
		t.Fatalf("unexpected absolute ssh:// endpoint: %+v", ep2)
	}

	epRelSSH, err := gitssh.ParseSSH("ssh://deploy@git.example.com:2222/~/team/app.git")
	if err != nil {
		t.Fatalf("ParseSSH ssh:// relative: %v", err)
	}
	if epRelSSH.RepoPath != "team/app.git" || epRelSSH.CanonicalURL != "ssh://deploy@git.example.com:2222/~/team/app.git" {
		t.Fatalf("unexpected relative ssh:// endpoint: %+v", epRelSSH)
	}

	if !gitssh.SameRepo("git@github.com:acme/web.git", "ssh://git@github.com/~/acme/web") {
		t.Fatal("expected SameRepo to match relative SCP and ssh://~/ forms")
	}
	if !gitssh.SameRepo("git@git.example.com:/srv/git/app.git", "ssh://git@git.example.com/srv/git/app") {
		t.Fatal("expected SameRepo to match absolute SCP and ssh:// forms")
	}
	if gitssh.SameRepo("git@git.example.com:/srv/git/app.git", "git@git.example.com:srv/git/app.git") {
		t.Fatal("expected SameRepo to keep absolute /srv/git/app.git distinct from relative srv/git/app.git (N12)")
	}
	if gitssh.SameRepo("git@github.com:acme/web.git", "ssh://git@github.com/acme/web") {
		t.Fatal("expected SameRepo to keep relative acme/web.git distinct from absolute /acme/web (N12)")
	}

	for _, bad := range []string{
		"ssh://git:secret@example.com/acme/web.git",
		"git@example.com:../etc/passwd",
		"git@example.com:-x/repo.git",
		"git@example.com:/-x/repo.git",
		"ssh://git@example.com/--upload-pack=x",
		"ssh://git@example.com/~/-x/repo.git",
	} {
		if _, err := gitssh.ParseSSH(bad); err == nil {
			t.Fatalf("expected %q to be refused", bad)
		}
	}

	conv, err := gitssh.ParseOrConvertSSH("https://gitlab.com/acme/web")
	if err != nil || conv.CanonicalURL != "ssh://git@gitlab.com/acme/web.git" {
		t.Fatalf("ParseOrConvertSSH: %+v, %v", conv, err)
	}

	cases := []struct {
		repo, wantURL, wantHost string
	}{
		{"git@github.com:acme/web.git", "https://github.com/acme/web/settings/keys", "GitHub"},
		{"ssh://git@gitlab.com/acme/sub/web.git", "https://gitlab.com/acme/sub/web/-/settings/repository#js-deploy-keys-settings", "GitLab"},
		{"git@gitlab.example.com:team/app.git", "https://gitlab.example.com/team/app/-/settings/repository#js-deploy-keys-settings", "GitLab"},
		{"git@bitbucket.org:acme/web.git", "https://bitbucket.org/acme/web/admin/access-keys", "Bitbucket"},
		{"git@codeberg.org:acme/web.git", "https://codeberg.org/acme/web/settings/keys", "Codeberg"},
		{"git@gitea.example.com:acme/web.git", "https://gitea.example.com/acme/web/settings/keys", "Gitea"},
		{"git@forgejo.example.com:acme/web.git", "https://forgejo.example.com/acme/web/settings/keys", "Forgejo"},
		{"git@git.example.com:acme/web.git", "", ""},
	}
	for _, tc := range cases {
		gotURL, gotHost := gitssh.DeployKeysPage(tc.repo)
		if gotURL != tc.wantURL || gotHost != tc.wantHost {
			t.Errorf("DeployKeysPage(%q) = (%q, %q), want (%q, %q)", tc.repo, gotURL, gotHost, tc.wantURL, tc.wantHost)
		}
	}
}

func TestSSHGitFetchPinMismatchWrongKeyRotateBoundsAndNetguard(t *testing.T) {
	repoDir, firstCommit, secondCommit := initTestRepo(t)
	srv, err := gitsshtest.NewServer("127.0.0.1:0", "acme/web.git", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ep, err := gitssh.ParseSSH(srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	key1, err := gitssh.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.SetAuthorizedKeys(key1.PublicKey); err != nil {
		t.Fatal(err)
	}
	hostAlgo, hostFP := srv.HostKey()

	// 1. Netguard refuses loopback when Local is false (production default).
	_, _, err = gitssh.List(t.Context(), gitssh.Config{
		Endpoint:      ep,
		PrivateKeyPEM: key1.PrivateKeyPEM,
		Local:         false,
	}, nil)
	if !errors.Is(err, netguard.ErrForbidden) {
		t.Fatalf("expected netguard refusal for loopback, got %v", err)
	}

	// Netguard refuses cloud metadata address even with Local=true.
	metaEP, _ := gitssh.ParseSSH("ssh://git@169.254.169.254/acme/web.git")
	_, _, err = gitssh.List(t.Context(), gitssh.Config{
		Endpoint:      metaEP,
		PrivateKeyPEM: key1.PrivateKeyPEM,
		Local:         true,
	}, nil)
	if !errors.Is(err, netguard.ErrForbidden) {
		t.Fatalf("expected netguard refusal for metadata, got %v", err)
	}

	// 2. Unpinned first connection reports host key and aborts before auth.
	_, hs, err := gitssh.List(t.Context(), gitssh.Config{
		Endpoint:      ep,
		PrivateKeyPEM: key1.PrivateKeyPEM,
		Local:         true,
	}, nil)
	if !errors.Is(err, gitssh.ErrHostKeyUnverified) {
		t.Fatalf("expected ErrHostKeyUnverified, got %v", err)
	}
	if !hs.Reached || hs.Verified || hs.SignedIn || hs.Fingerprint != hostFP || hs.HostKeyAlgorithm != hostAlgo {
		t.Fatalf("unexpected unpinned handshake: %+v (want %s %s)", hs, hostAlgo, hostFP)
	}

	// 3. Pinned connection with wrong key fails auth.
	wrongKey, err := gitssh.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, hs, err = gitssh.List(t.Context(), gitssh.Config{
		Endpoint:         ep,
		HostKeyAlgorithm: hostAlgo,
		HostKeyPin:       hostFP,
		PrivateKeyPEM:    wrongKey.PrivateKeyPEM,
		Local:            true,
	}, nil)
	if !errors.Is(err, gitssh.ErrAuth) || !hs.Verified || hs.SignedIn {
		t.Fatalf("expected ErrAuth with verified host key, got err=%v hs=%+v", err, hs)
	}

	// 4. Pinned connection with authorized key lists branches and fetches pack (v2 and v1).
	cfg := gitssh.Config{
		Endpoint:         ep,
		HostKeyAlgorithm: hostAlgo,
		HostKeyPin:       hostFP,
		PrivateKeyPEM:    key1.PrivateKeyPEM,
		Local:            true,
	}
	for _, v1Only := range []bool{false, true} {
		srv.SetV1Only(v1Only)
		head, branches, hs, err := gitssh.Branches(t.Context(), cfg, nil)
		if err != nil {
			t.Fatalf("Branches (v1Only=%v): %v", v1Only, err)
		}
		if !hs.SignedIn || head != "main" || !slices.Equal(branches, []string{"main", "release"}) {
			t.Fatalf("unexpected branches (v1Only=%v): head=%q branches=%v hs=%+v", v1Only, head, branches, hs)
		}
		gotCommit, fullRef, err := gitssh.Resolve(t.Context(), cfg, nil, "main")
		if err != nil || gotCommit != secondCommit || fullRef != "refs/heads/main" {
			t.Fatalf("Resolve (v1Only=%v): commit=%s ref=%s err=%v", v1Only, gotCommit, fullRef, err)
		}
		// Fetch tip commit (depth 1).
		f, size, err := gitssh.FetchPack(t.Context(), cfg, nil, "refs/heads/main", secondCommit, gitssh.DefaultPackLimits())
		if err != nil {
			t.Fatalf("FetchPack tip (v1Only=%v): %v", v1Only, err)
		}
		if size <= 0 {
			t.Fatalf("expected non-empty pack, got %d", size)
		}
		_ = f.Close()
		_ = os.Remove(f.Name())

		// Fetch ancestor commit (verifies ancestry on branch).
		f2, _, err := gitssh.FetchPack(t.Context(), cfg, nil, "refs/heads/main", firstCommit, gitssh.DefaultPackLimits())
		if err != nil {
			t.Fatalf("FetchPack ancestor (v1Only=%v): %v", v1Only, err)
		}
		_ = f2.Close()
		_ = os.Remove(f2.Name())
	}
	srv.SetV1Only(false)

	// Verify only "session" channels and no forwarding/pty requests were ever sent.
	for _, ch := range srv.SeenChannels() {
		if ch != "session" {
			t.Fatalf("unexpected SSH channel type: %s", ch)
		}
	}
	for _, rq := range srv.SeenRequests() {
		if rq != "env" && rq != "exec" {
			t.Fatalf("unexpected SSH session request type: %s", rq)
		}
	}

	// 5. Repository unavailable / no access after auth.
	srv.SetDenyRepo(true)
	_, _, err = gitssh.List(t.Context(), cfg, nil)
	if !errors.Is(err, gitssh.ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	srv.SetDenyRepo(false)

	// 6. Rotation: add key2, test passes with key2, remove key1 -> key1 now fails auth.
	key2, err := gitssh.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.SetAuthorizedKeys(key1.PublicKey, key2.PublicKey); err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.PrivateKeyPEM = key2.PrivateKeyPEM
	if _, _, _, err := gitssh.Branches(t.Context(), cfg2, nil); err != nil {
		t.Fatalf("Branches with rotated key2: %v", err)
	}
	if err := srv.SetAuthorizedKeys(key2.PublicKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitssh.List(t.Context(), cfg, nil); !errors.Is(err, gitssh.ErrAuth) {
		t.Fatalf("expected old key1 to fail auth after switch, got %v", err)
	}

	// 7. Size bound enforced on oversized pack.
	srv.SetExtraBytes(256 << 10)
	_, _, err = gitssh.FetchPack(t.Context(), cfg2, nil, "", secondCommit, gitssh.PackLimits{Pack: 32 << 10, MaxDepth: 1})
	if !errors.Is(err, gitssh.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge on oversized pack, got %v", err)
	}
	srv.SetExtraBytes(0)

	// 8. Timeout bound enforced on slow host.
	srv.SetDelay(400 * time.Millisecond)
	slowCfg := cfg2
	slowCfg.ListTimeout = 100 * time.Millisecond
	_, _, err = gitssh.List(t.Context(), slowCfg, nil)
	if !errors.Is(err, gitssh.ErrTimeout) {
		t.Fatalf("expected ErrTimeout on slow host, got %v", err)
	}
	srv.SetDelay(0)

	// 9. Host key mismatch (same algorithm, new key) stops connection and reports observed key.
	newAlgo, newFP, err := srv.RotateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = gitssh.List(t.Context(), cfg2, nil)
	var changed *gitssh.HostKeyChange
	if !errors.As(err, &changed) || !errors.Is(err, gitssh.ErrHostKeyChanged) {
		t.Fatalf("expected HostKeyChange, got %v", err)
	}
	if changed.Algorithm != newAlgo || changed.Fingerprint != newFP {
		t.Fatalf("unexpected changed key: %+v (want %s %s)", changed, newAlgo, newFP)
	}

	// 10. Host key algorithm change (Ed25519 -> ECDSA) probes unauthenticated and reports new key.
	ecdsaAlgo, ecdsaFP, err := srv.RotateHostKeyECDSA()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = gitssh.List(t.Context(), cfg2, nil)
	var algoChanged *gitssh.HostKeyChange
	if !errors.As(err, &algoChanged) || !errors.Is(err, gitssh.ErrHostKeyChanged) {
		t.Fatalf("expected HostKeyChange on algorithm switch, got %v", err)
	}
	if algoChanged.Algorithm != ecdsaAlgo || algoChanged.Fingerprint != ecdsaFP {
		t.Fatalf("unexpected algorithm-changed key: %+v (want %s %s)", algoChanged, ecdsaAlgo, ecdsaFP)
	}
}

func TestAbsoluteGitShellPathListFloodAndPackTempDir(t *testing.T) {
	repoDir, _, secondCommit := initTestRepo(t)

	// 1. Strict git-shell-like server requiring exact absolute path "/srv/git/app.git".
	absSrv, err := gitsshtest.NewServer("127.0.0.1:0", "/srv/git/app.git", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer absSrv.Close()

	key, err := gitssh.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := absSrv.SetAuthorizedKeys(key.PublicKey); err != nil {
		t.Fatal(err)
	}
	algo, fp := absSrv.HostKey()

	absEP, err := gitssh.ParseSSH(absSrv.URL())
	if err != nil {
		t.Fatalf("ParseSSH(%q): %v", absSrv.URL(), err)
	}
	if absEP.RepoPath != "/srv/git/app.git" {
		t.Fatalf("RepoPath = %q, want /srv/git/app.git", absEP.RepoPath)
	}
	packDir := t.TempDir()
	cfg := gitssh.Config{
		Endpoint:         absEP,
		HostKeyAlgorithm: algo,
		HostKeyPin:       fp,
		PrivateKeyPEM:    key.PrivateKeyPEM,
		Local:            true,
		PackTempDir:      packDir,
	}
	head, branches, _, err := gitssh.Branches(t.Context(), cfg, nil)
	if err != nil || head != "main" || !slices.Contains(branches, "main") {
		t.Fatalf("Branches against absolute /srv/git/app.git: head=%q branches=%v err=%v", head, branches, err)
	}

	// Verify temporary pack is created inside cfg.PackTempDir.
	f, size, err := gitssh.FetchPack(t.Context(), cfg, nil, "refs/heads/main", secondCommit, gitssh.DefaultPackLimits())
	if err != nil {
		t.Fatalf("FetchPack against absolute /srv/git/app.git: %v", err)
	}
	if size <= 0 || filepath.Dir(f.Name()) != packDir {
		t.Fatalf("pack file = %q (size %d), want inside %q", f.Name(), size, packDir)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())

	// 2. Oversized ref advertisement (> 2 MiB) is cut off with ErrTooLarge.
	absSrv.SetListFloodBytes(4 << 20)
	if _, _, err := gitssh.List(t.Context(), cfg, nil); !errors.Is(err, gitssh.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge on 4 MiB ref flood, got %v", err)
	}
	absSrv.SetListFloodBytes(0)

	// 3. Strict relative path server requiring exact "acme/web.git" (no leading "/").
	relSrv, err := gitsshtest.NewServer("127.0.0.1:0", "acme/web.git", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer relSrv.Close()
	relSrv.SetStrictPath(true)
	if err := relSrv.SetAuthorizedKeys(key.PublicKey); err != nil {
		t.Fatal(err)
	}
	relAlgo, relFP := relSrv.HostKey()
	relEP, err := gitssh.ParseSSH("ssh://git@" + relSrv.Addr + "/~/acme/web.git")
	if err != nil {
		t.Fatal(err)
	}
	if relEP.RepoPath != "acme/web.git" {
		t.Fatalf("relative RepoPath = %q, want acme/web.git", relEP.RepoPath)
	}
	relCfg := gitssh.Config{
		Endpoint:         relEP,
		HostKeyAlgorithm: relAlgo,
		HostKeyPin:       relFP,
		PrivateKeyPEM:    key.PrivateKeyPEM,
		Local:            true,
	}
	if _, _, _, err := gitssh.Branches(t.Context(), relCfg, nil); err != nil {
		t.Fatalf("Branches against strict relative acme/web.git: %v", err)
	}

	// 4. Protocol v1 server advertising 40,000 refs/pull/* lines (> 3 MiB wire)
	// streams and drops them in O(1) memory without hitting the 2 MiB kept cap (N14).
	absSrv.SetV1Only(true)
	absSrv.SetV1ExtraRefs(40_000)
	head, branches, _, err = gitssh.Branches(t.Context(), cfg, nil)
	if err != nil || head != "main" || !slices.Equal(branches, []string{"main", "release"}) {
		t.Fatalf("Branches against v1 server with 40k refs/pull/*: head=%q branches=%v err=%v", head, branches, err)
	}
	fV1, sizeV1, err := gitssh.FetchPack(t.Context(), cfg, nil, "refs/heads/main", secondCommit, gitssh.DefaultPackLimits())
	if err != nil || sizeV1 <= 0 {
		t.Fatalf("FetchPack against v1 server with 40k refs/pull/*: size=%d err=%v", sizeV1, err)
	}
	_ = fV1.Close()
	_ = os.Remove(fV1.Name())
	absSrv.SetV1ExtraRefs(0)
	absSrv.SetV1Only(false)
}
