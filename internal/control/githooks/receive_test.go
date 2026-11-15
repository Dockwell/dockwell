// SPDX-License-Identifier: AGPL-3.0-only

package githooks

import "testing"

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
		{"http://10.0.0.5:3000/acme/web.git", []string{"http://10.0.0.5:3000/acme/web.git"}, true},
		{"http://10.0.0.5:3000/acme/web.git", []string{"http://10.0.0.5/acme/web.git"}, false},
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
