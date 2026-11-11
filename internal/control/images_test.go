// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
)

// testRegistry stands in for every registry a test's agent reads images from:
// it has nginx, at 1.27 and latest, which expose port 80.
func testRegistry(t *testing.T) string {
	t.Helper()
	config := `{"os":"linux","architecture":"amd64","config":{"ExposedPorts":{"80/tcp":{}},"Volumes":{"/usr/share/nginx/html":{}}}}`
	const digest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/v2/library/nginx/manifests/1.27" || p == "/v2/library/nginx/manifests/latest" || p == "/v2/library/nginx/manifests/"+digest:
			w.Header().Set("Docker-Content-Digest", digest)
			if r.Method == http.MethodGet {
				_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%064d","size":%d}}`, 8, len(config))
			}
		case strings.HasPrefix(p, "/v2/library/nginx/blobs/"):
			_, _ = w.Write([]byte(config))
		case p == "/v2/namespaces/library/repositories/nginx/tags":
			_, _ = fmt.Fprintf(w, `{"results":[{"name":"latest","digest":%q,"tag_last_pushed":"2026-11-01T00:00:00Z"},{"name":"1.27","digest":%q,"tag_last_pushed":"2026-11-01T00:00:00Z"}]}`, digest, digest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A form reads an image through the agent on the server the app will run on: what
// it declares, its tags, and Docker Hub's search, for anyone who may propose an
// app, and nothing for a viewer (ADR 0062).
func TestImagesAreReadThroughTheServersAgent(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Hub = []docker.Found{{Name: "nginx", Description: "Official build of Nginx.", Official: true, Stars: 20000}}
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: testRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	ic := dockwellv1connect.NewImageServiceClient(b, b.origin, connect.WithProtoJSON())

	got, err := ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx:1.27"}))
	if err != nil {
		t.Fatal(err)
	}
	c := got.Msg.GetConfig()
	if got.Msg.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || len(c.GetPorts()) != 1 || c.GetPorts()[0].GetPort() != 80 || c.GetVolumes()[0] != "/usr/share/nginx/html" {
		t.Errorf("nginx:1.27 reads %v", got.Msg)
	}
	missing, err := ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx:nope", Server: srv.id.ServerID}))
	if err != nil || missing.Msg.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND {
		t.Errorf("a missing tag reads %v, %v", missing.Msg, err)
	}
	tags, err := ic.ListImageTags(t.Context(), connect.NewRequest(&dockwellv1.ListImageTagsRequest{Repository: "nginx"}))
	if err != nil || len(tags.Msg.GetTags()) != 2 || !tags.Msg.GetTags()[0].GetLatest() {
		t.Errorf("nginx's tags = %v, %v", tags.Msg, err)
	}
	found, err := ic.SearchImages(t.Context(), connect.NewRequest(&dockwellv1.SearchImagesRequest{Term: "ngin"}))
	if err != nil || len(found.Msg.GetMatches()) != 1 || !found.Msg.GetMatches()[0].GetOfficial() {
		t.Errorf("a search found %v, %v", found.Msg, err)
	}

	_, err = ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "Not An Image"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Write it as nginx:1.27") {
		t.Errorf("a reference that isn't one: %v", err)
	}
	// Only Docker Hub is searched: the engine would search a registry a term names.
	_, err = ic.SearchImages(t.Context(), connect.NewRequest(&dockwellv1.SearchImagesRequest{Term: "127.0.0.1:2375/x"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "only Docker Hub is searched") {
		t.Errorf("a search naming a registry: %v", err)
	}
	_, err = ic.ListImageTags(t.Context(), connect.NewRequest(&dockwellv1.ListImageTagsRequest{Repository: "nginx:1.27"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Name its repository alone") {
		t.Errorf("a repository with a tag: %v", err)
	}
	_, err = ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx", Server: "srv_nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v, want NotFound", err)
	}

	hold := aMember(t, f)
	hold(store.Grant{ProjectID: "prj_somewhere", Role: store.RoleDeveloper})
	if _, err := ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx:1.27", Server: srv.id.ServerID})); err != nil {
		t.Errorf("a developer reading an image: %v", err)
	}
	hold(onInstall(store.RoleViewer))
	_, err = ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx:1.27"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a viewer reading an image: %v, want PermissionDenied", err)
	}
	hold(onInstall(store.RoleAdmin))

	// With two servers, a read names the one the app will run on.
	f.addServerNamed(t, "hz-2", nil, nil)
	_, err = ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx:1.27"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "say which one the app will run on") {
		t.Errorf("two servers and none named: %v", err)
	}

	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	_, err = ic.GetImage(t.Context(), connect.NewRequest(&dockwellv1.GetImageRequest{Reference: "nginx:1.27", Server: srv.id.ServerID}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("a server whose agent has gone: %v, want Unavailable, saying why", err)
	}
}
