// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/revision"
)

type stubSources struct {
	gotLogin string
	gotScope string
	gotURL   string
	commit   string
	ref      string
	err      error
}

func (s *stubSources) Resolve(ctx context.Context, url, _ string) (string, string, error) {
	s.gotLogin = revision.GitLoginOf(ctx)
	s.gotScope = revision.GitScopeOf(ctx)
	s.gotURL = url
	return s.commit, s.ref, s.err
}

type stubGitLogins struct {
	pinned string
	err    error
}

func (s *stubGitLogins) PinGitLogin(_ context.Context, _, _, _ string) (string, error) {
	return s.pinned, s.err
}

func TestResolveDockerGitRequiresProjectForSSHAndGitLabLogins(t *testing.T) {
	h := &Handler{
		sources:   &stubSources{},
		gitLogins: &stubGitLogins{},
	}
	cases := []struct {
		name string
		in   *v1.DockerImageGitSource
		code connect.Code
		want string
	}{
		{
			name: "ssh scp url without project",
			in:   &v1.DockerImageGitSource{Repository: "git@gitlab.com:acme/web.git", Ref: "main"},
			code: connect.CodeFailedPrecondition,
			want: "project",
		},
		{
			name: "ssh scheme url without project",
			in:   &v1.DockerImageGitSource{Repository: "ssh://git@gitlab.com/acme/web.git", Ref: "main"},
			code: connect.CodeFailedPrecondition,
			want: "project",
		},
		{
			name: "ssh deploy key login without project",
			in:   &v1.DockerImageGitSource{Repository: "https://gitlab.com/acme/web.git", Ref: "main", GitLogin: "ssh:acme-web"},
			code: connect.CodeInvalidArgument,
			want: "project",
		},
		{
			name: "gitlab connection login without project",
			in:   &v1.DockerImageGitSource{Repository: "https://gitlab.com/acme/web.git", Ref: "main", GitLogin: "gitlab:corp"},
			code: connect.CodeInvalidArgument,
			want: "project",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h.ResolveDockerGit(t.Context(), tc.in, "")
			if connect.CodeOf(err) != tc.code {
				t.Fatalf("ResolveDockerGit = %v (code %v), want %v", err, connect.CodeOf(err), tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestResolveDockerGitPropagatesSourcesError(t *testing.T) {
	src := &stubSources{err: errors.New("host key changed")}
	h := &Handler{
		sources:   src,
		gitLogins: &stubGitLogins{},
	}
	_, _, err := h.ResolveDockerGit(t.Context(), &v1.DockerImageGitSource{
		Repository: "https://github.com/acme/web.git",
		Ref:        "main",
	}, "")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("ResolveDockerGit = %v (code %v), want FailedPrecondition", err, connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("error = %q, want underlying source error", err.Error())
	}
}
