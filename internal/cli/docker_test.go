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

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/cli/apicli"
)

type dockerCLI struct {
	dockwellv1connect.UnimplementedDockerServiceHandler
	dockwellv1connect.UnimplementedServerServiceHandler
	servers []*v1.Server
}

func (d *dockerCLI) ListServers(context.Context, *connect.Request[v1.ListServersRequest]) (*connect.Response[v1.ListServersResponse], error) {
	return connect.NewResponse(&v1.ListServersResponse{Servers: d.servers}), nil
}

func (*dockerCLI) GetDockerSettings(_ context.Context, req *connect.Request[v1.GetDockerSettingsRequest]) (*connect.Response[v1.GetDockerSettingsResponse], error) {
	return connect.NewResponse(&v1.GetDockerSettingsResponse{Settings: &v1.DockerSettings{EngineVersion: req.Msg.GetServerId()}}), nil
}

func (*dockerCLI) WatchDockerObjects(_ context.Context, req *connect.Request[v1.WatchDockerObjectsRequest], stream *connect.ServerStream[v1.WatchDockerObjectsResponse]) error {
	for _, id := range req.Msg.GetSelection().GetServerIds() {
		if err := stream.Send(&v1.WatchDockerObjectsResponse{WatchStatus: &v1.DockerWatchStatus{ServerId: id}}); err != nil {
			return err
		}
	}
	return nil
}

func TestDockerCLISelectsTheOnlyServerOrListsChoices(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		stub := &dockerCLI{servers: []*v1.Server{{Id: "srv_fixture_one", Name: "example-one"}}}
		if multiple {
			stub.servers = append(stub.servers, &v1.Server{Id: "srv_fixture_two", Name: "example-two"})
		}
		mux := http.NewServeMux()
		mux.Handle(dockwellv1connect.NewServerServiceHandler(stub))
		mux.Handle(dockwellv1connect.NewDockerServiceHandler(stub))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		env := map[string]string{apicli.EnvURL: srv.URL, apicli.EnvToken: wellFormedToken(), "XDG_CONFIG_HOME": t.TempDir()}
		for _, verb := range []string{"get-settings", "watch"} {
			got := run(t, env, "docker", verb, "--json")
			if multiple {
				if got.code == exitOK || !strings.Contains(got.err, "Available servers: example-one, example-two") {
					t.Errorf("%s with two servers: %+v", verb, got)
				}
			} else if got.code != exitOK || !strings.Contains(got.out, "srv_fixture_one") {
				t.Errorf("%s with one server: %+v", verb, got)
			}
		}
		got := run(t, env, "docker", "get-settings", "--server-id", "example-two", "--json")
		if got.code != exitOK || !strings.Contains(got.out, "example-two") {
			t.Errorf("explicit server: %+v", got)
		}
	}
}
