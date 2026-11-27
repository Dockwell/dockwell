// SPDX-License-Identifier: AGPL-3.0-only

package gitlabconnection

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestValidateAndNormalize(t *testing.T) {
	t.Parallel()
	ty := Type{}

	spec := &resourcev1.GitLabConnection{
		InstanceUrl: "https://gitlab.example.com/",
		Token:       "glpat-secret",
		PathPrefix:  "/acme/platform/",
	}
	if err := ty.Validate(spec); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	gotURL, err := NormalizeURL("https://GitLab.Example.COM/sub/")
	if err != nil || gotURL != "https://gitlab.example.com/sub" {
		t.Fatalf("NormalizeURL = %q, %v", gotURL, err)
	}
	if _, err := NormalizeURL("http://gitlab.example.com"); err == nil {
		t.Error("plain http URL accepted")
	}
	if _, err := NormalizeURL("https://user:pass@gitlab.example.com"); err == nil {
		t.Error("URL with userinfo accepted")
	}
	if _, err := NormalizeURL("https://gitlab.example.com?a=1"); err == nil {
		t.Error("URL with query accepted")
	}

	gotPrefix, err := NormalizePathPrefix("/acme/web/")
	if err != nil || gotPrefix != "acme/web" {
		t.Fatalf("NormalizePathPrefix = %q, %v", gotPrefix, err)
	}
	if _, err := NormalizePathPrefix("acme/../other"); err == nil {
		t.Error("path prefix with .. accepted")
	}
}

func TestCheckNewAndKeep(t *testing.T) {
	t.Parallel()
	ty := Type{}

	if err := ty.CheckNew(&resourcev1.GitLabConnection{InstanceUrl: "https://gitlab.com"}); err == nil {
		t.Error("new connection without token accepted")
	}
	if err := ty.CheckNew(&resourcev1.GitLabConnection{InstanceUrl: "https://gitlab.com", Token: "glpat-1"}); err != nil {
		t.Fatalf("new connection with token rejected: %v", err)
	}

	prev := &resourcev1.GitLabConnection{InstanceUrl: "https://gitlab.example.com", Token: "glpat-keep"}
	sameURL := &resourcev1.GitLabConnection{InstanceUrl: "https://gitlab.example.com", PathPrefix: "acme"}
	kept, err := ty.Keep(prev, sameURL)
	if err != nil {
		t.Fatalf("Keep on same URL failed: %v", err)
	}
	if sameURL.GetToken() != "glpat-keep" || kept.Reason != "kept_token" {
		t.Fatalf("Keep did not preserve token: got %q (%+v)", sameURL.GetToken(), kept)
	}

	diffURL := &resourcev1.GitLabConnection{InstanceUrl: "https://other.example.com"}
	if _, err := ty.Keep(prev, diffURL); err == nil {
		t.Error("Keep allowed reusing saved token across different URLs")
	}
}

func TestReaches(t *testing.T) {
	t.Parallel()
	every := &resourcev1.GitLabConnection{InstanceUrl: "https://gitlab.com"}
	if !Reaches(every, "prj_1") || !Reaches(every, "prj_2") {
		t.Error("empty projects should reach every project")
	}
	narrow := &resourcev1.GitLabConnection{InstanceUrl: "https://gitlab.com", Projects: []string{"prj_1"}}
	if !Reaches(narrow, "prj_1") || Reaches(narrow, "prj_2") {
		t.Error("narrowed projects reach mismatch")
	}
}
