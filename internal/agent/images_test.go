// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/registry"
)

// imageRegistry stands in for every registry: it has library/nginx and
// acme/web, and counts the manifest GETs Docker Hub would count as pulls.
type imageRegistry struct {
	srv  *httptest.Server
	gets atomic.Int32
	down atomic.Bool
}

func newImageRegistry(t *testing.T) *imageRegistry {
	f := &imageRegistry{}
	config := `{"os":"linux","architecture":"amd64","config":{"ExposedPorts":{"80/tcp":{}},"Env":["NGINX_VERSION=1.27.3"],"Volumes":{"/data":{}}}}`
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			time.Sleep(50 * time.Millisecond)
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/manifests/") && (strings.Contains(path, "/library/nginx/") || strings.Contains(path, "/acme/web/")):
			ref := path[strings.LastIndex(path, "/")+1:]
			if ref == "nope" {
				http.NotFound(w, r)
				return
			}
			// Each tag is an image of its own, so each config is read once.
			w.Header().Set("Docker-Content-Digest", fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(path))))
			if r.Method == http.MethodGet {
				f.gets.Add(1)
				_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%064d","size":%d}}`, 1, len(config))
			}
		case strings.Contains(path, "/blobs/"):
			_, _ = w.Write([]byte(config))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// to sends every request to the stand-in, whatever registry it was for, as a
// server's network would take it to the registry it names.
type to struct{ base *url.URL }

func (d to) Do(req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = d.base.Scheme, d.base.Host
	return http.DefaultClient.Do(req)
}

func newTestImages(t *testing.T, f *imageRegistry, now func() time.Time) (*images, *dockertest.Engine) {
	t.Helper()
	e := dockertest.New(t)
	base, _ := url.Parse(f.srv.URL)
	im := newImages(e.Client(), ImageReads{}, now)
	// Made as ready makes it, with every request sent to the stand-in.
	im.ready(context.Background())
	im.client = registry.New(registry.Options{Secure: to{base}, Plain: to{base}, Now: now})
	return im, e
}

func imageCall(ref string) *agentv1.ImageCall {
	return &agentv1.ImageCall{Ask: &agentv1.ImageCall_Image{Image: &dockwellv1.GetImageRequest{Reference: ref}}}
}

func TestImagesReadAnImageOnceAndKeepIt(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	ctx := context.Background()
	got, err := im.read(ctx, imageCall("ghcr.io/acme/web:1.2"))
	if err != nil {
		t.Fatal(err)
	}
	res := got.GetImage()
	if res.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || res.GetDigest() == "" {
		t.Fatalf("answer %v, digest %q", res.GetAnswer(), res.GetDigest())
	}
	c := res.GetConfig()
	if len(c.GetPorts()) != 1 || c.GetPorts()[0].GetPort() != 80 || len(c.GetVolumes()) != 1 || c.GetEnv()[0].GetName() != "NGINX_VERSION" {
		t.Errorf("config = %v", c)
	}
	if _, err := im.read(ctx, imageCall("ghcr.io/acme/web:1.2")); err != nil || f.gets.Load() != 1 {
		t.Errorf("a second read made %d manifest GETs in all, want 1 (its config is kept): %v", f.gets.Load(), err)
	}
	got, _ = im.read(ctx, imageCall("ghcr.io/acme/web:nope"))
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND {
		t.Errorf("a missing tag reads %v", got.GetImage().GetAnswer())
	}
}

func TestImagesReadWhatTheServerHasWithoutTheNetwork(t *testing.T) {
	f := newImageRegistry(t)
	im, e := newTestImages(t, f, nil)
	e.Registry = map[string]dockertest.Image{"redis:7.4": {Config: docker.Config{
		ExposedPorts: map[string]struct{}{"6379/tcp": {}}, Volumes: map[string]struct{}{"/data": {}},
	}}}
	if err := e.Client().PullImage(context.Background(), "redis:7.4", nil); err != nil {
		t.Fatal(err)
	}
	got, err := im.read(context.Background(), imageCall("redis:7.4"))
	if err != nil {
		t.Fatal(err)
	}
	c := got.GetImage().GetConfig()
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || c.GetPorts()[0].GetPort() != 6379 || c.GetVolumes()[0] != "/data" {
		t.Errorf("an image the server has reads %v", got.GetImage())
	}
	if f.gets.Load() != 0 {
		t.Errorf("an image the server has made %d manifest GETs", f.gets.Load())
	}
}

func TestImagesSpendDockerHubsAllowanceOnlyOnWhatNoCacheHad(t *testing.T) {
	f := newImageRegistry(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	im, _ := newTestImages(t, f, clock)
	ctx := context.Background()
	for i := range hubReads {
		got, err := im.read(ctx, imageCall(fmt.Sprintf("nginx:1.%d", i)))
		if err != nil || got.GetImage().GetConfig() == nil {
			t.Fatalf("read %d: %v, %v", i, got, err)
		}
	}
	// Read again, it's kept, and costs nothing.
	if got, _ := im.read(ctx, imageCall("nginx:1.0")); got.GetImage().GetConfig() == nil {
		t.Errorf("a kept config wasn't read from what's kept")
	}
	got, _ := im.read(ctx, imageCall("nginx:2.0"))
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || got.GetImage().GetUnread() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED || got.GetImage().GetConfig() != nil {
		t.Errorf("past the allowance, a read says %v", got.GetImage())
	}
	if f.gets.Load() != hubReads {
		t.Errorf("Docker Hub saw %d counted GETs, want %d", f.gets.Load(), hubReads)
	}
	// Six hours on, there's an allowance again.
	now = now.Add(hubReadWindow)
	if got, _ := im.read(ctx, imageCall("nginx:2.0")); got.GetImage().GetConfig() == nil {
		t.Errorf("after six hours, a read still says %v", got.GetImage())
	}
}

func TestImagesLeaveASilentRegistryAloneForAWhile(t *testing.T) {
	f := newImageRegistry(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	im, _ := newTestImages(t, f, func() time.Time { return now })
	f.down.Store(true)
	got, _ := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2"))
	if got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Fatalf("a registry that's down reads %v", got.GetImage().GetAnswer())
	}
	f.down.Store(false)
	if got, _ := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE {
		t.Errorf("a registry that didn't answer was asked again at once")
	}
	now = now.Add(silentImagesFor)
	if got, _ := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); got.GetImage().GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND {
		t.Errorf("after a while, it reads %v", got.GetImage().GetAnswer())
	}
}

func TestImagesSearchDockerHubThroughTheEngine(t *testing.T) {
	f := newImageRegistry(t)
	im, e := newTestImages(t, f, nil)
	e.Hub = []docker.Found{{Name: "bitnami/postgresql", Stars: 300}, {Name: "postgres", Official: true, Stars: 14000}, {Name: "nginx", Official: true}}
	got, err := im.read(context.Background(), &agentv1.ImageCall{Ask: &agentv1.ImageCall_Search{Search: &dockwellv1.SearchImagesRequest{Term: "POST"}}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got.GetSearch().GetMatches() {
		names = append(names, m.GetName())
	}
	if strings.Join(names, ",") != "postgres,bitnami/postgresql" {
		t.Errorf("search found %v, want the official one first", names)
	}
}

func TestImagesForgetEverythingWhenIdle(t *testing.T) {
	f := newImageRegistry(t)
	im, _ := newTestImages(t, f, nil)
	if _, err := im.read(context.Background(), imageCall("ghcr.io/acme/web:1.2")); err != nil {
		t.Fatal(err)
	}
	im.forget()
	im.mu.Lock()
	kept := len(im.configs) + len(im.digests)
	im.mu.Unlock()
	if kept != 0 || im.client != nil {
		t.Errorf("an idle reader keeps %d answers", kept)
	}
}

func TestImagesSayWhyTheyAreOff(t *testing.T) {
	im := newImages(nil, ImageReads{Off: "this is a test, so Dockwell reads no image from a registry here"}, nil)
	if _, err := im.read(context.Background(), imageCall("nginx")); err == nil || !strings.Contains(err.Error(), "this is a test") {
		t.Errorf("a reader that's off says %v", err)
	}
}
