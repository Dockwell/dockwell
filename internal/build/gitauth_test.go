// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

const testGitAuth = "Basic eC1hY2Nlc3MtdG9rZW46Z2hzX3MzY3JldA=="

// A private repository's token reaches the reading and the builder as a file in
// memory, mounted read-only, and never their environment, command or log; it's gone
// once the build ends (ADR 0098).
func TestAPrivateRepositorysTokenIsAFileTheFetchReads(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Private", map[string]string{"Dockerfile": "FROM alpine:3.23\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Builder, req.GitAuth = resource.BuilderAuto, testGitAuth
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	made := append(b.made("dockwell-detect"), b.made("dockwell-build")...)
	if len(made) < 2 {
		t.Fatalf("%d containers ran, not a reading and a builder", len(made))
	}
	for _, c := range made {
		if !slices.ContainsFunc(c.HostConfig.Mounts, func(m docker.Mount) bool { return m.Target == gitAuthMount && m.ReadOnly && m.Type == "bind" }) {
			t.Errorf("a container doesn't mount the token read-only: %+v", c.HostConfig.Mounts)
		}
		said := strings.Join(append(append([]string{}, c.Env...), c.Cmd...), " ")
		if strings.Contains(said, "ghs_s3cret") || strings.Contains(said, testGitAuth) {
			t.Errorf("the token is in a container's environment or command")
		}
	}
	for _, l := range b.lines() {
		if strings.Contains(l, testGitAuth) {
			t.Fatalf("the build's log holds the token: %q", l)
		}
	}
	entries, err := os.ReadDir(b.builder.SecretsDir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the token is still on the server once the build ended: %v %v", entries, err)
	}
}

// git sends the token for the repository's URL alone, from its file, and for no
// other URL, as a redirect elsewhere would ask.
func TestGitSendsTheTokenToTheRepositoryAlone(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("this test needs git")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authorization"), []byte(testGitAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(gitAuthScript, gitAuthMount, dir) + `
"$GIT" config --get-urlmatch http.extraHeader "$DW_URL"
echo ---
"$GIT" config --get-urlmatch http.extraHeader https://elsewhere.example.com/acme/web.git || echo none
`
	cmd := exec.CommandContext(t.Context(), "sh", "-eu", "-c", script)
	cmd.Env = []string{"DW_URL=https://github.com/acme/web.git", "HOME=" + t.TempDir(), "GIT=" + git, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "Authorization: "+testGitAuth+"\n---\nnone" {
		t.Errorf("git sends %q", got)
	}
}
