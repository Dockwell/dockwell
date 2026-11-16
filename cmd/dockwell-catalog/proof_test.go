// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/catalog/bump/bumptest"
)

// scopeRepo is a catalog repository with a cache entry on Redis 7.4.1 at its base,
// and returns it with the base commit.
func scopeRepo(t *testing.T) (dir, base string, git func(...string) string) {
	t.Helper()
	dir = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	write(t, dir, "entries/cache.yaml", cacheEntry("7.4.1", 2))
	write(t, dir, "updates/cache.yaml", "services:\n  cache:\n    level: patch\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	return dir, git("rev-parse", "HEAD"), git
}

func cacheEntry(version string, release int) string {
	return `id: cache
name: Cache
summary: A cache
category: database
version: "` + version + `"
release: ` + string(rune('0'+release)) + `
license: AGPL-3.0-only
appLicense: BSD-3-Clause
appSource: https://github.com/redis/redis
compose: |
  services:
    cache:
      image: redis:` + version + `@` + bumptest.Digest("library/redis", bumptest.Tag{Name: version}) + `
`
}

func write(t *testing.T, dir, file, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bump scope reads a pull request's diff in a checkout of its base, with the head's
// commit there and never checked out, and prints the status it earns (ADR 0100).
func TestScopeIsACommitStatusFromTheDiff(t *testing.T) {
	s := bumptest.New(t)
	s.Images["library/redis"] = []bumptest.Tag{{Name: "7.4.1", Pushed: time.Now().Add(-90 * 24 * time.Hour)}, {Name: "7.4.2", Pushed: time.Now().Add(-5 * 24 * time.Hour)}}
	s.Hub["library/redis"] = true
	s.Releases["redis/redis@7.4.2"] = bumptest.Release{}
	dir, base, git := scopeRepo(t)
	deps := &scopeDeps{sources: s.Live(), now: time.Now()}
	scope := func(head string, args ...string) scopeOutput {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := scopeCommand(t.Context(), append([]string{"--catalog", dir, "--base", base, "--head", head}, args...), &stdout, &stderr, deps); err != nil {
			t.Fatalf("scope: %v\n%s", err, stderr.String())
		}
		var out scopeOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	commit := func(files map[string]string) string {
		t.Helper()
		git("checkout", "-q", "-B", "head", base)
		for f, text := range files {
			write(t, dir, f, text)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "head")
		head := git("rev-parse", "HEAD")
		// The checkout stays at the base: the head is objects alone, as the workflow fetches it.
		git("checkout", "-q", base)
		return head
	}

	inScope := commit(map[string]string{"entries/cache.yaml": cacheEntry("7.4.2", 3)})
	if got := scope(inScope, "--author", "app/dockwell-catalog-bump", "--branch", "bump/cache"); got.State != "success" {
		t.Errorf("the bump's own step reads %+v", got)
	}
	withPolicy := commit(map[string]string{"entries/cache.yaml": cacheEntry("7.4.2", 3), "updates/cache.yaml": "services:\n  cache:\n    level: minor\n"})
	if got := scope(withPolicy, "--author", "app/dockwell-catalog-bump", "--branch", "bump/cache"); got.State != "failure" || !strings.Contains(got.Description, "changes 2 files") {
		t.Errorf("a bump that changes its policy reads %+v", got)
	}
	if got := scope(withPolicy, "--author", "mira", "--branch", "move-cache"); got.State != "success" || !strings.Contains(got.Description, "review rule") {
		t.Errorf("a person's pull request reads %+v", got)
	}
	if got := scope(withPolicy, "--author", "app/dockwell-catalog-bump", "--branch", "bump/cache", "--approved"); got.State != "success" {
		t.Errorf("an approved bump reads %+v", got)
	}
	// Pushed a day ago, 7.4.2 is within a patch's three days.
	s.Images["library/redis"][1].Pushed = time.Now().Add(-24 * time.Hour)
	if got := scope(inScope, "--author", "app/dockwell-catalog-bump", "--branch", "bump/cache"); got.State != "failure" || !strings.Contains(got.Description, "within its wait") {
		t.Errorf("a step within its wait reads %+v", got)
	}
	if got := scope(inScope, "--author", "app/dockwell-catalog-bump", "--branch", "bump/cache"); len(got.Description) > 140 {
		t.Errorf("a description of %d characters", len(got.Description))
	}
}

// earlier prints the releases an upgrade is proven from, as JSON.
func TestEarlierPrintsTheReleasesBefore(t *testing.T) {
	dir, base, git := scopeRepo(t)
	write(t, dir, "entries/cache.yaml", cacheEntry("7.4.2", 3))
	git("commit", "-q", "-am", "7.4.2")
	var stdout, stderr bytes.Buffer
	if err := earlierCommand(t.Context(), []string{"--catalog", dir, "cache"}, &stdout, &stderr); err != nil {
		t.Fatalf("earlier: %v\n%s", err, stderr.String())
	}
	if want := `{"before":{"commit":"` + base + `","version":"7.4.1"}}`; strings.TrimSpace(stdout.String()) != want {
		t.Errorf("earlier prints %s, want %s", stdout.String(), want)
	}
}
