// SPDX-License-Identifier: AGPL-3.0-only

package gitref

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/gittest"
)

func TestResolveReadsWhatABranchOrTagNamesNow(t *testing.T) {
	s := gittest.Start(t)
	repo := s.Repo("web")
	first := repo.Commit("main", "First", map[string]string{"a": "1"})
	repo.Tag("v1.0.0", first)
	second := repo.Commit("main", "Second", map[string]string{"a": "2"})
	dev := repo.Commit("dev", "On dev", map[string]string{"b": "1"})
	r := &Resolver{Local: true}
	// A tag with a branch's name: the branch comes first, by its full name.
	repo.Ref("refs/tags/dev", first)
	for _, c := range []struct{ ref, want, branch string }{
		{"main", second, "refs/heads/main"},
		{"dev", dev, "refs/heads/dev"},
		{"refs/tags/dev", first, "refs/tags/dev"},
		{"v1.0.0", first, "refs/tags/v1.0.0"},
		{"refs/heads/main", second, "refs/heads/main"},
		{"", second, "refs/heads/main"},
		{strings.ToUpper(first), first, ""},
	} {
		got, branch, err := r.Resolve(t.Context(), repo.URL, c.ref)
		if err != nil {
			t.Fatalf("%q: %v", c.ref, err)
		}
		if got != c.want || branch != c.branch {
			t.Errorf("%q names %s on %q, want %s on %q", c.ref, got, branch, c.want, c.branch)
		}
	}
}

func TestResolveSaysWhatBranchesThereAre(t *testing.T) {
	s := gittest.Start(t)
	repo := s.Repo("web")
	repo.Commit("main", "First", map[string]string{"a": "1"})
	repo.Commit("trunk", "Other", map[string]string{"a": "2"})
	_, _, err := (&Resolver{Local: true}).Resolve(t.Context(), repo.URL, "mian")
	if err == nil || !strings.Contains(err.Error(), "has no branch or tag mian. Its branches are main and trunk") {
		t.Errorf("a branch that isn't there says %v", err)
	}
}

func TestResolveSaysWhatIsWrongWithTheRepository(t *testing.T) {
	s := gittest.Start(t)
	_, _, err := (&Resolver{Local: true}).Resolve(t.Context(), s.URL+"/nothing.git", "main")
	if err == nil || !strings.Contains(err.Error(), "no repository at") && !strings.Contains(err.Error(), "wants credentials") {
		t.Errorf("a repository that isn't there says %v", err)
	}
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer private.Close()
	_, _, err = (&Resolver{Local: true}).Resolve(t.Context(), private.URL+"/acme/secret", "main")
	if err == nil || !strings.Contains(err.Error(), "wants credentials, so it's private") {
		t.Errorf("a private repository says %v", err)
	}
	notGit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer notGit.Close()
	_, _, err = (&Resolver{Local: true}).Resolve(t.Context(), notGit.URL+"/x", "main")
	if err == nil || !strings.Contains(err.Error(), "isn't a Git repository's branches") {
		t.Errorf("something that isn't Git says %v", err)
	}
}

func TestAnEmptyRepositoryHasNoDefaultBranch(t *testing.T) {
	s := gittest.Start(t)
	repo := s.Repo("empty")
	_, _, err := (&Resolver{Local: true}).Resolve(t.Context(), repo.URL, "")
	if err == nil || !strings.Contains(err.Error(), "doesn't say what its default branch is") {
		t.Errorf("an empty repository says %v", err)
	}
}

// A lookup never reaches this machine, a link-local address or, over plain HTTP,
// anything but a private network, unless it's a test's.
func TestALookupDialsOnlyWhereARepositoryCanBe(t *testing.T) {
	for _, c := range []struct {
		local, plain bool
		address      string
		ok           bool
	}{
		{false, false, "127.0.0.1:80", false},
		{false, false, "[::1]:443", false},
		{false, false, "169.254.169.254:80", false},
		{false, false, "0.0.0.0:443", false},
		{false, false, "203.0.113.7:443", true},
		{false, true, "203.0.113.7:80", false},
		{false, true, "10.0.0.5:80", true},
		{false, true, "[fd00::5]:80", true},
		{true, false, "127.0.0.1:80", true},
		{true, true, "127.0.0.1:80", true},
	} {
		err := Guard(c.local, c.plain)("tcp", c.address, nil)
		if (err == nil) != c.ok {
			t.Errorf("Guard(local %t, plain %t) %s = %v", c.local, c.plain, c.address, err)
		}
	}
	// And so a lookup of this machine is refused before it asks.
	repo := gittest.Start(t).Repo("web")
	repo.Commit("main", "First", map[string]string{"a": "1"})
	if _, _, err := (&Resolver{}).Resolve(t.Context(), repo.URL, "main"); err == nil || !strings.Contains(err.Error(), "is this server, or link-local") {
		t.Errorf("a lookup of this machine = %v", err)
	}
}

// What a repository advertises beyond its branches and tags, such as a pull
// request's head, which anyone with a fork pushes, names nothing a build follows.
func TestResolveFollowsOnlyBranchesAndTags(t *testing.T) {
	s := gittest.Start(t)
	repo := s.Repo("web")
	repo.Commit("main", "First", map[string]string{"a": "1"})
	fork := repo.Commit("fork", "Evil", map[string]string{"a": "evil"})
	repo.Ref("refs/pull/7/head", fork)
	r := &Resolver{Local: true}
	for _, ref := range []string{"refs/pull/7/head", "pull/7/head"} {
		if got, _, err := r.Resolve(t.Context(), repo.URL, ref); err == nil {
			t.Errorf("%q names %s, a pull request's head", ref, got)
		}
	}
}
