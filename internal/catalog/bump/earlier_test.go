// SPDX-License-Identifier: AGPL-3.0-only

package bump_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/catalog/bump"
)

// history is a catalog repository whose entry shop went through versions, one
// commit each, and its commits by version.
func history(t *testing.T, versions ...string) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0o750); err != nil {
		t.Fatal(err)
	}
	commits := map[string]string{}
	for i, v := range versions {
		text := "id: shop\nversion: \"" + v + "\"\nrelease: " + string(rune('1'+i)) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "entries", "shop.yaml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		// Another entry's change between, which isn't shop's history.
		if err := os.WriteFile(filepath.Join(dir, "entries", "other.yaml"), []byte("id: other\nversion: \""+v+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", "-A")
		run("commit", "-q", "-m", v)
		commits[v] = run("rev-parse", "HEAD")
	}
	return dir, commits
}

// The release before comes from the pull request's base, or without one from the
// history; the first of the current major from the history, where it's neither.
func TestEarlierReleasesComeFromTheCatalogsHistory(t *testing.T) {
	dir, c := history(t, "1.9.0", "2.0.0", "2.1.0", "2.1.1", "2.2.0")

	got, err := bump.EarlierReleases(t.Context(), dir, "shop", "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Version != "2.1.1" || got.Before.Commit != c["2.1.1"] {
		t.Errorf("without a base, the release before is %+v, want 2.1.1", got.Before)
	}
	if got.First == nil || got.First.Version != "2.0.0" || got.First.Commit != c["2.0.0"] {
		t.Errorf("the first of the current major is %+v, want 2.0.0", got.First)
	}

	// A pull request's base is what it upgrades from, whatever the history says.
	got, err = bump.EarlierReleases(t.Context(), dir, "shop", "HEAD", c["2.1.0"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Version != "2.1.0" || got.First == nil || got.First.Version != "2.0.0" {
		t.Errorf("from a base at 2.1.0: %+v, %+v", got.Before, got.First)
	}

	// The first of the major that's the release before isn't proven twice.
	got, err = bump.EarlierReleases(t.Context(), dir, "shop", c["2.1.0"], c["2.0.0"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Version != "2.0.0" || got.First != nil {
		t.Errorf("at 2.1.0 from 2.0.0: %+v, %+v; want no first, as it's the one before", got.Before, got.First)
	}

	// A new major's first release has no first of its major before it.
	got, err = bump.EarlierReleases(t.Context(), dir, "shop", c["2.0.0"], "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Version != "1.9.0" || got.First != nil {
		t.Errorf("at 2.0.0: %+v, %+v", got.Before, got.First)
	}

	// A new entry upgrades from nothing.
	got, err = bump.EarlierReleases(t.Context(), dir, "shop", c["1.9.0"], "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Before != nil || got.First != nil {
		t.Errorf("a new entry: %+v, %+v", got.Before, got.First)
	}
	if _, err := bump.EarlierReleases(t.Context(), dir, "nothing", "HEAD", ""); err == nil {
		t.Error("an entry the commit hasn't reads")
	}
}

// A release that changes how an entry is put together, keeping its version, as
// Dockwell/catalog#4 gave WordPress and Ghost a database need at 7.1.3 and 6.72.0,
// is the release an install upgrades from (#726's slice 2 review, M1).
func TestAReleaseThatKeepsItsVersionIsUpgradedFrom(t *testing.T) {
	dir, c := history(t, "7.1.3", "7.1.3")
	got, err := bump.EarlierReleases(t.Context(), dir, "shop", "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Commit == c["7.1.3"] {
		t.Fatalf("release 2 of 7.1.3 upgrades from %+v; want release 1", got.Before)
	}
}

// A pull request that changes only checks/<id>.sh leaves the entry as its base has
// it. Its check's "upgraded" half still needs an upgrade to run on: the history's
// release before (#726's slice 2 review, L3).
func TestACheckOnlyChangeStillUpgrades(t *testing.T) {
	dir, c := history(t, "1.0.0", "1.1.0")
	got, err := bump.EarlierReleases(t.Context(), dir, "shop", "HEAD", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Commit != c["1.0.0"] {
		t.Fatalf("a base whose entry is the head's gives %+v; want the history's 1.0.0", got.Before)
	}
}

// Proven at a pull request's merge commit, the history holds the pull request's own
// commits. The first release of a new major must be one main had, not a commit
// that only lived on the branch (#726's slice 2 review, L2).
func TestTheFirstOfAMajorIsOneMainHad(t *testing.T) {
	dir, _ := history(t, "1.9.0")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(v, r string) {
		if err := os.WriteFile(filepath.Join(dir, "entries", "shop.yaml"), []byte("id: shop\nversion: \""+v+"\"\nrelease: "+r+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("checkout", "-q", "-b", "pr")
	write("2.0.0", "2")
	run("commit", "-qam", "a first try, never on main")
	write("2.0.1", "2")
	run("commit", "-qam", "2.0.1")
	run("checkout", "-q", "main")
	// main moves on elsewhere, so the merge is a real merge commit, as refs/pull/N/merge is.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-qm", "elsewhere")
	base := run("rev-parse", "HEAD")
	run("merge", "-q", "--no-ff", "-m", "merge", "pr")
	got, err := bump.EarlierReleases(t.Context(), dir, "shop", "HEAD", base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before == nil || got.Before.Version != "1.9.0" || got.Before.Commit != base {
		t.Errorf("before is %+v, want the base's 1.9.0", got.Before)
	}
	if got.First != nil {
		t.Errorf("the first of major 2 is %+v, a commit only the pull request's branch had; want none", got.First)
	}
}
