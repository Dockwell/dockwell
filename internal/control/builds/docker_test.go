// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func TestResolveDockerGitRefusesSSHAndDeployKeys(t *testing.T) {
	h := &Handler{}
	cases := []struct {
		name string
		in   *v1.DockerImageGitSource
	}{
		{
			name: "ssh url",
			in:   &v1.DockerImageGitSource{Repository: "git@gitlab.com:acme/web.git", Ref: "main"},
		},
		{
			name: "ssh scheme url",
			in:   &v1.DockerImageGitSource{Repository: "ssh://git@gitlab.com/acme/web.git", Ref: "main"},
		},
		{
			name: "ssh deploy key login on https url",
			in:   &v1.DockerImageGitSource{Repository: "https://gitlab.com/acme/web.git", Ref: "main", GitLogin: "ssh:acme-web"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h.ResolveDockerGit(t.Context(), tc.in, "")
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("ResolveDockerGit = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
			}
			if !strings.Contains(err.Error(), "HTTPS") || !strings.Contains(err.Error(), "app instead") {
				t.Fatalf("error %q should state fact, consequence and fix", err.Error())
			}
		})
	}
}
