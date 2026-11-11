// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/cli/apicli"
)

type stubBuildDetection struct {
	dockwellv1connect.UnimplementedBuildServiceHandler
	requests chan *dockwellv1.DetectBuildRequest
	branches chan *dockwellv1.ListRepositoryBranchesRequest
}

func (s *stubBuildDetection) DetectBuild(_ context.Context, req *connect.Request[dockwellv1.DetectBuildRequest]) (*connect.Response[dockwellv1.DetectBuildResponse], error) {
	s.requests <- req.Msg
	return connect.NewResponse(&dockwellv1.DetectBuildResponse{Commit: strings.Repeat("a", 40)}), nil
}

func (s *stubBuildDetection) ListRepositoryBranches(_ context.Context, req *connect.Request[dockwellv1.ListRepositoryBranchesRequest]) (*connect.Response[dockwellv1.ListRepositoryBranchesResponse], error) {
	s.branches <- req.Msg
	return connect.NewResponse(&dockwellv1.ListRepositoryBranchesResponse{Branches: []string{"main", "next"}, DefaultBranch: "main"}), nil
}

// A repository's URL goes to the API as --repository, as --url is the control
// plane's own address, which the same command still takes.
func TestBranchesTakeTheRepositoryAsItsOwnFlag(t *testing.T) {
	stub := &stubBuildDetection{branches: make(chan *dockwellv1.ListRepositoryBranchesRequest, 1)}
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewBuildServiceHandler(stub))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	env := map[string]string{apicli.EnvToken: wellFormedToken(), "XDG_CONFIG_HOME": t.TempDir()}
	r := run(t, env, "builds", "branches", "--url", server.URL, "--repository", "github.com/acme/web", "--json")
	if r.code != exitOK {
		t.Fatalf("builds branches = %d:\n%s%s", r.code, r.out, r.err)
	}
	select {
	case got := <-stub.branches:
		if got.GetUrl() != "github.com/acme/web" {
			t.Errorf("builds branches asked for %q, want github.com/acme/web", got.GetUrl())
		}
	default:
		t.Fatal("builds branches never reached the API")
	}
	if !strings.Contains(r.out, `"next"`) {
		t.Errorf("builds branches printed %s", r.out)
	}
}

func TestBuildDetectionTakesAnOptionalServiceArgument(t *testing.T) {
	stub := &stubBuildDetection{requests: make(chan *dockwellv1.DetectBuildRequest, 1)}
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewBuildServiceHandler(stub))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	env := map[string]string{apicli.EnvURL: server.URL, apicli.EnvToken: wellFormedToken(), "XDG_CONFIG_HOME": t.TempDir()}

	for _, tt := range []struct {
		name string
		args []string
		want *dockwellv1.DetectBuildRequest
	}{
		{
			name: "settings before the app exists",
			args: []string{"--server", "e2e-agent", "--build", `{"git":{"url":"https://example.com/web.git","ref":"node"}}`},
			want: &dockwellv1.DetectBuildRequest{Server: "e2e-agent", Build: &resourcev1.ImageBuild{
				Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: "https://example.com/web.git", Ref: "node"}},
			}},
		},
		{
			name: "an existing service by ID",
			args: []string{"svc_01JD6Z3R8M9QF2K7T4XW5BNC0V"},
			want: &dockwellv1.DetectBuildRequest{Id: "svc_01JD6Z3R8M9QF2K7T4XW5BNC0V"},
		},
		{
			name: "an existing service by name and scope",
			args: []string{"web", "--project", "storefront", "--environment", "production"},
			want: &dockwellv1.DetectBuildRequest{Id: "web", Project: "storefront", Environment: "production"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"builds", "detect", "--json"}, tt.args...)
			r := run(t, env, args...)
			if r.code != exitOK {
				t.Fatalf("builds detect = %d:\n%s%s", r.code, r.out, r.err)
			}
			select {
			case got := <-stub.requests:
				if !proto.Equal(got, tt.want) {
					t.Errorf("builds detect sent %v, want %v", got, tt.want)
				}
			default:
				t.Fatal("builds detect never reached the API")
			}
		})
	}

	r := run(t, env, "builds", "detect", "--help")
	if r.code != exitOK || !strings.Contains(r.out, "builds detect [<id>]") {
		t.Errorf("help doesn't describe the optional service:\n%s%s", r.out, r.err)
	}
}

func TestGeneratedCommandsKeepTheirArgumentChecks(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"builds", "detect", "web", "another"}, "takes at most one argument"},
		{[]string{"builds", "get"}, "takes one argument"},
		{[]string{"builds", "get", "bld_one", "bld_two"}, "takes one argument"},
		{[]string{"servers", "list", "unexpected"}, "takes no arguments"},
	} {
		r := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, tt.args...)
		if r.code != exitUsage || !strings.Contains(r.err, tt.want) {
			t.Errorf("%v = %d, want a usage error with %q:\n%s%s", tt.args, r.code, tt.want, r.out, r.err)
		}
	}
}
