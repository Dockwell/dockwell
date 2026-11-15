// SPDX-License-Identifier: AGPL-3.0-only

package githooks

import (
	"slices"
	"testing"
)

func TestAPushMatchesTheRefABuildFollows(t *testing.T) {
	for _, tc := range []struct {
		built, pushed string
		want          bool
	}{
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/main", "refs/heads/main-2", false},
		{"refs/heads/main", "refs/tags/main", false},
		{"main", "refs/heads/main", true},
		{"v1.2", "refs/tags/v1.2", true},
		{"main", "refs/pull/1/head", false},
		{"", "refs/heads/trunk", true},
		{"", "refs/heads/main", false},
	} {
		if got := sameRef(tc.built, tc.pushed, "trunk"); got != tc.want {
			t.Errorf("a build of %q, a push to %q: %t, want %t", tc.built, tc.pushed, got, tc.want)
		}
	}
	if sameRef("", "refs/heads/main", "") {
		t.Error("a push matched a default branch the payload doesn't name")
	}
}

func TestAPushMatchesTheRepositoryABuildFetches(t *testing.T) {
	for _, tc := range []struct {
		built  string
		pushed []string
		want   bool
	}{
		{"https://github.com/acme/web", []string{"https://github.com/acme/web.git"}, true},
		{"https://github.com/acme/web.git", []string{"", "https://github.com/acme/web"}, true},
		{"https://GitHub.com/Acme/Web", []string{"https://github.com/acme/web.git"}, true},
		{"https://git.example.com:443/acme/web", []string{"https://git.example.com/acme/web.git"}, true},
		{"http://192.0.2.10:3000/acme/web.git", []string{"http://192.0.2.10:3000/acme/web.git"}, true},
		{"http://192.0.2.10:3000/acme/web.git", []string{"http://192.0.2.10/acme/web.git"}, false},
		{"https://github.com/acme/web", []string{"https://github.com/acme/web-fork.git"}, false},
		{"https://github.com/acme/web", []string{"https://example.com/acme/web.git"}, false},
		{"https://github.com/acme/web", nil, false},
		{"", []string{""}, false},
	} {
		if got := sameRepository(tc.built, tc.pushed...); got != tc.want {
			t.Errorf("a build from %q, a push to %q: %t, want %t", tc.built, tc.pushed, got, tc.want)
		}
	}
}

func TestADeliveryIDIsShortAndPlain(t *testing.T) {
	for id, want := range map[string]bool{
		"72d3162e-cc78-11e3-81ab-4c9367dc0958": true,
		"":                                     false,
		"a b":                                  false,
		"x\ny":                                 false,
		string(make([]byte, 101)):              false,
	} {
		if got := deliveryID(id); got != want {
			t.Errorf("deliveryID(%q) = %t, want %t", id, got, want)
		}
	}
}

// Each provider's push reads as the refs it moved, the refs it deleted, and the
// repository's URLs.
func TestEachProvidersPushReadsTheSame(t *testing.T) {
	for _, tc := range []struct {
		what          string
		read          func([]byte) (pushed, error)
		body          string
		refs, gone    []string
		repos         []string
		defaultBranch string
	}{
		{
			"GitHub's", readGitHub,
			`{"ref":"refs/heads/main","after":"9fceb02d0ae598e95dc970b74767f19372d61af8","repository":{"clone_url":"https://github.com/acme/web.git","html_url":"https://github.com/acme/web","default_branch":"main"}}`,
			[]string{"refs/heads/main"},
			nil,
			[]string{"https://github.com/acme/web.git", "https://github.com/acme/web"},
			"main",
		},
		{
			"GitHub's deleting a branch", readGitHub,
			`{"ref":"refs/heads/old","after":"0000000000000000000000000000000000000000","deleted":true,"repository":{}}`,
			nil,
			[]string{"refs/heads/old"},
			[]string{"", ""},
			"",
		},
		{
			"GitLab's", readGitLab,
			`{"object_kind":"push","ref":"refs/heads/main","after":"9fceb02d0ae598e95dc970b74767f19372d61af8","project":{"git_http_url":"https://gitlab.example.com/acme/web.git","web_url":"https://gitlab.example.com/acme/web","default_branch":"main"}}`,
			[]string{"refs/heads/main"},
			nil,
			[]string{"https://gitlab.example.com/acme/web.git", "https://gitlab.example.com/acme/web"},
			"main",
		},
		{
			"GitLab's of a tag", readGitLab,
			`{"object_kind":"tag_push","ref":"refs/tags/v1.2","after":"9fceb02d0ae598e95dc970b74767f19372d61af8","project":{"web_url":"https://gitlab.example.com/acme/web"}}`,
			[]string{"refs/tags/v1.2"},
			nil,
			[]string{"", "https://gitlab.example.com/acme/web"},
			"",
		},
		{
			"GitLab's deleting a branch", readGitLab,
			`{"object_kind":"push","ref":"refs/heads/old","after":"0000000000000000000000000000000000000000","project":{}}`,
			nil,
			[]string{"refs/heads/old"},
			[]string{"", ""},
			"",
		},
		{
			"Bitbucket's, of two branches, a tag, and one deleted", readBitbucket,
			`{"push":{"changes":[
				{"new":{"type":"branch","name":"dev","target":{"hash":"9fceb02d"}},"old":{"type":"branch","name":"dev"}},
				{"new":{"type":"branch","name":"main"},"old":null,"created":true},
				{"new":{"type":"annotated_tag","name":"v1.2"}},
				{"new":null,"old":{"type":"branch","name":"old"},"closed":true}
			]},"repository":{"full_name":"acme/web","links":{"html":{"href":"https://bitbucket.org/acme/web"}}}}`,
			[]string{"refs/heads/dev", "refs/heads/main", "refs/tags/v1.2"},
			[]string{"refs/heads/old"},
			[]string{"https://bitbucket.org/acme/web"},
			"",
		},
	} {
		got, err := tc.read([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.what, err)
			continue
		}
		if !slices.Equal(got.refs, tc.refs) || !slices.Equal(got.gone, tc.gone) || !slices.Equal(got.repos, tc.repos) || got.defaultBranch != tc.defaultBranch {
			t.Errorf("%s read as %+v", tc.what, got)
		}
	}
	for _, read := range []func([]byte) (pushed, error){readGitHub, readGitLab, readBitbucket} {
		if _, err := read([]byte(`not JSON`)); err == nil {
			t.Error("a payload that isn't JSON read as a push")
		}
	}
}
