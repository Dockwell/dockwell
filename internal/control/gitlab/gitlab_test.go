// SPDX-License-Identifier: AGPL-3.0-only

package gitlab_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
	"github.com/dockwell/dockwell/internal/netguard"
)

func TestNetguardBlocksLoopbackAndMetadata(t *testing.T) {
	t.Parallel()
	// Default Client uses netguard and refuses loopback and cloud metadata.
	client := &gitlab.Client{}
	for _, blocked := range []string{"https://127.0.0.1", "https://localhost", "https://169.254.169.254"} {
		_, err := client.CheckToken(t.Context(), blocked, "glpat-secret")
		if !errors.Is(err, netguard.ErrForbidden) {
			t.Errorf("CheckToken(%s) = %v, want netguard.ErrForbidden", blocked, err)
		}
	}
}

func TestCheckTokenAndPostCommitStatus(t *testing.T) {
	t.Parallel()
	gl := gltest.New()
	fake := httptest.NewServer(gl)
	t.Cleanup(fake.Close)
	client := &gitlab.Client{HTTP: fake.Client(), TestBase: fake.URL}

	info, err := client.CheckToken(t.Context(), "https://gitlab.example.com", gltest.TokenAPI)
	if err != nil {
		t.Fatalf("CheckToken(api) failed: %v", err)
	}
	if !info.HasAPIScope() || !info.Active {
		t.Fatalf("expected api scope & active: %+v", info)
	}

	readInfo, err := client.CheckToken(t.Context(), "https://gitlab.example.com", gltest.TokenReadOnly)
	if err != nil {
		t.Fatalf("CheckToken(read_api) failed: %v", err)
	}
	if readInfo.HasAPIScope() {
		t.Fatalf("read_api token should not have api scope: %+v", readInfo)
	}

	if _, err := client.CheckToken(t.Context(), "https://gitlab.example.com", "glpat-bad"); err == nil {
		t.Fatal("invalid token succeeded")
	}

	sha := "0123456789abcdef0123456789abcdef01234567"
	got, err := client.PostCommitStatus(t.Context(), "https://gitlab.example.com", gltest.TokenAPI, "acme/web", sha, gitlab.CommitStatusRequest{
		State:       "pending",
		Name:        "Dockwell / web",
		TargetURL:   "https://panel.example.com/changes/chg_1",
		Description: "Deploy queued for web",
		Ref:         "main",
	})
	if err != nil || got.ID == 0 || got.Status != "pending" {
		t.Fatalf("PostCommitStatus = %+v, %v", got, err)
	}

	// Idempotent duplicate transition (same state) returns existing status instead of error.
	again, err := client.PostCommitStatus(t.Context(), "https://gitlab.example.com", gltest.TokenAPI, "acme/web", sha, gitlab.CommitStatusRequest{
		State:       "pending",
		Name:        "Dockwell / web",
		TargetURL:   "https://panel.example.com/changes/chg_1",
		Description: "Deploy queued for web",
	})
	if err != nil || again.ID != got.ID {
		t.Fatalf("duplicate PostCommitStatus = %+v, %v (want id %d)", again, err, got.ID)
	}
}

func TestCheckTokenFallbackToUser(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/personal_access_tokens/self":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
		case "/api/v4/user":
			if r.Header.Get("PRIVATE-TOKEN") != "glpat-legacy" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"username":"ci-bot"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client := &gitlab.Client{HTTP: srv.Client(), TestBase: srv.URL}
	info, err := client.CheckToken(t.Context(), "https://gitlab.example.com", "glpat-legacy")
	if err != nil {
		t.Fatalf("fallback CheckToken failed: %v", err)
	}
	if info.ScopesKnown || !info.HasAPIScope() || info.Name != "ci-bot" {
		t.Fatalf("unexpected fallback info: %+v", info)
	}
}

func TestProjectPathAndMatchesPrefix(t *testing.T) {
	t.Parallel()
	if got := gitlab.ProjectPath("https://gitlab.example.com", "https://gitlab.example.com/acme/sub/web.git"); got != "acme/sub/web" {
		t.Fatalf("ProjectPath = %q", got)
	}
	if got := gitlab.ProjectPath("https://gitlab.example.com/gl", "https://gitlab.example.com/gl/acme/web"); got != "acme/web" {
		t.Fatalf("ProjectPath with subpath = %q", got)
	}
	for _, bad := range []string{
		"https://other.example.com/acme/web",
		"https://gitlab.example.com/acme",
		"https://gitlab.example.com/acme/../other",
		"https://gitlab.example.com/acme/./web",
		"https://gitlab.example.com/acme//web",
		"https://gitlab.example.com/acme/web?x=1",
		"https://gitlab.example.com/acme/web#frag",
		"https://user:pass@gitlab.example.com/acme/web",
	} {
		if got := gitlab.ProjectPath("https://gitlab.example.com", bad); got != "" {
			t.Errorf("ProjectPath(%q) = %q, want empty", bad, got)
		}
	}
	if got := gitlab.CommitStatusURL("https://gitlab.example.com", "acme/my group/web", "abc123", 42); got != "https://gitlab.example.com/acme/my%20group/web/-/pipelines/42" {
		t.Fatalf("CommitStatusURL(pipeline) = %q", got)
	}
	if got := gitlab.CommitStatusURL("https://gitlab.example.com", "acme/web", "abc123", 0); got != "https://gitlab.example.com/acme/web/-/commit/abc123" {
		t.Fatalf("CommitStatusURL(commit) = %q", got)
	}
	if !gitlab.MatchesPrefix("acme/sub/web", "acme") || !gitlab.MatchesPrefix("acme/web", "acme/web") || gitlab.MatchesPrefix("acme2/web", "acme") {
		t.Fatal("MatchesPrefix mismatch")
	}
}

func TestListProjects(t *testing.T) {
	t.Parallel()
	gl := gltest.New()
	gl.SetProjects([]gltest.Project{
		{PathWithNamespace: "acme/web", WebURL: "https://gitlab.example.com/acme/web"},
		{PathWithNamespace: "acme/api", WebURL: "https://gitlab.example.com/acme/api"},
	})
	fake := httptest.NewServer(gl)
	t.Cleanup(fake.Close)
	client := &gitlab.Client{HTTP: fake.Client(), TestBase: fake.URL}

	all, err := client.ListProjects(t.Context(), "https://gitlab.example.com", gltest.TokenAPI, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("ListProjects() = %+v, %v", all, err)
	}
	filtered, err := client.ListProjects(t.Context(), "https://gitlab.example.com", gltest.TokenAPI, "web")
	if err != nil || len(filtered) != 1 || filtered[0].PathWithNamespace != "acme/web" {
		t.Fatalf("ListProjects(web) = %+v, %v", filtered, err)
	}
}
