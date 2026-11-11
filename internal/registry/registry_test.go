// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in                          string
		registry, repo, tag, digest string
		name, reference, stringBack string
	}{
		{"nginx", Hub, "library/nginx", "", "", "nginx", "latest", "nginx"},
		{"nginx:1.27", Hub, "library/nginx", "1.27", "", "nginx", "1.27", "nginx:1.27"},
		{"bitnami/redis:7", Hub, "bitnami/redis", "7", "", "bitnami/redis", "7", "bitnami/redis:7"},
		{"docker.io/library/postgres:16", Hub, "library/postgres", "16", "", "postgres", "16", "postgres:16"},
		{"ghcr.io/acme/web:1.4.2", "ghcr.io", "acme/web", "1.4.2", "", "ghcr.io/acme/web", "1.4.2", "ghcr.io/acme/web:1.4.2"},
		{"localhost:5000/app", "localhost:5000", "app", "", "", "localhost:5000/app", "latest", "localhost:5000/app"},
		{"localhost/app:dev", "localhost", "app", "dev", "", "localhost/app", "dev", "localhost/app:dev"},
		{"[::1]:5000/app:1", "[::1]:5000", "app", "1", "", "[::1]:5000/app", "1", "[::1]:5000/app:1"},
		{"GHCR.IO/acme/web", "ghcr.io", "acme/web", "", "", "ghcr.io/acme/web", "latest", "ghcr.io/acme/web"},
		{
			"nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Hub, "library/nginx", "",
			"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "nginx",
			"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	} {
		r, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if r.Registry != c.registry || r.Repository != c.repo || r.Tag != c.tag || r.Digest != c.digest {
			t.Errorf("Parse(%q) = %+v", c.in, r)
		}
		if r.Name() != c.name || r.Reference() != c.reference || r.String() != c.stringBack {
			t.Errorf("Parse(%q): name %q, reference %q, string %q", c.in, r.Name(), r.Reference(), r.String())
		}
	}
	// A registry's part is a host, so nothing in it can move the path into a query or a fragment.
	bad := []string{
		"", "Nginx", "nginx:", "two words", "nginx@sha256:short", "ghcr.io/Acme/web", "a//b",
		"127.0.0.1:8080?x=/acme/web", "evil.example#/acme/web", "user@evil.example/acme/web", "evil.example:http/acme/web",
	}
	for _, bad := range bad {
		if _, err := Parse(bad); !errors.Is(err, ErrBadRef) {
			t.Errorf("Parse(%q) = %v, want ErrBadRef", bad, err)
		}
	}
}

func TestSortTags(t *testing.T) {
	tags := []string{"latest", "1.27-alpine", "1.9", "1.27", "1.28.0-rc1", "1.28", "edge", "v2.0", "1.27.3", "mainline"}
	SortTags(tags)
	want := []string{"v2.0", "1.28", "1.28.0-rc1", "1.27.3", "1.27", "1.27-alpine", "1.9", "edge", "latest", "mainline"}
	if !slices.Equal(tags, want) {
		t.Errorf("SortTags = %v, want %v", tags, want)
	}
}

// fakeRegistry is a registry that hands out a token as Docker Hub does, keeps a
// multi-platform image and a private one, and its blobs elsewhere.
type fakeRegistry struct {
	srv       *httptest.Server
	gets      atomic.Int32 // manifest GETs, which Docker Hub counts
	heads     atomic.Int32
	tokens    atomic.Int32
	limitHead bool
}

const (
	indexDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	armDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	amdDigest    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	configDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	oldDigest    = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
)

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokens.Add(1)
		if !strings.Contains(r.URL.Query().Get("scope"), "repository:library/nginx:pull") &&
			!strings.Contains(r.URL.Query().Get("scope"), "repository:acme/web:pull") {
			// Any client gets a token, but one that reads nothing private.
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "nothing", "expires_in": 300})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "pull", "expires_in": 300})
	})
	auth := func(w http.ResponseWriter, r *http.Request, repo string) bool {
		if r.Header.Get("Authorization") == "Bearer pull" && repo != "acme/private" {
			return true
		}
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:%s:pull"`, f.srv.URL, repo))
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	mux.HandleFunc("/v2/{ns}/{name}/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("ns") + "/" + r.PathValue("name")
		if !auth(w, r, repo) {
			return
		}
		if r.Method == http.MethodHead {
			f.heads.Add(1)
			if f.limitHead {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		} else {
			f.gets.Add(1)
		}
		ref := r.PathValue("ref")
		switch {
		case repo == "library/nginx" && (ref == "1.27" || ref == "latest" || ref == indexDigest):
			w.Header().Set("Docker-Content-Digest", indexDigest)
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			_, _ = fmt.Fprintf(w, `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
				{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}},
				{"digest":%q,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}`, amdDigest, armDigest)
		case repo == "library/nginx" && ref == "1.26":
			w.Header().Set("Docker-Content-Digest", oldDigest)
		case repo == "library/nginx" && (ref == armDigest || ref == amdDigest):
			w.Header().Set("Docker-Content-Digest", ref)
			_, _ = fmt.Fprintf(w, `{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":300}}`, configDigest)
		case repo == "acme/web" && (ref == "1.2" || ref == "latest"):
			w.Header().Set("Docker-Content-Digest", amdDigest)
		case repo == "acme/web" && ref == "1.1":
			w.Header().Set("Docker-Content-Digest", oldDigest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// Blobs are kept on another host, as Docker Hub's are.
	elsewhere := http.NewServeMux()
	cdn := httptest.NewServer(elsewhere)
	t.Cleanup(cdn.Close)
	mux.HandleFunc("/v2/{ns}/{name}/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r, r.PathValue("ns")+"/"+r.PathValue("name")) {
			return
		}
		http.Redirect(w, r, cdn.URL+"/elsewhere/"+r.PathValue("digest"), http.StatusTemporaryRedirect)
	})
	elsewhere.HandleFunc("/elsewhere/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("a blob kept elsewhere was sent the registry's token")
		}
		_, _ = w.Write([]byte(`{"os":"linux","architecture":"arm64","variant":"v8","config":{
			"ExposedPorts":{"80/tcp":{},"443/tcp":{},"443/udp":{}},
			"Env":["PATH=/usr/sbin:/usr/bin","NGINX_VERSION=1.27.3","EMPTY="],
			"Volumes":{"/var/cache/nginx":{},"/data":{}},
			"Cmd":["nginx","-g","daemon off;"],"Entrypoint":["/docker-entrypoint.sh"],
			"Healthcheck":{"Test":["CMD","curl","-f","http://localhost"]}}}`))
	})
	mux.HandleFunc("/v2/{ns}/{name}/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r, r.PathValue("ns")+"/"+r.PathValue("name")) {
			return
		}
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/acme/web/tags/list?last=1.1&n=1000>; rel="next"`)
			_, _ = w.Write([]byte(`{"tags":["1.1","latest"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"tags":["1.2","1.2-rc1"]}`))
	})
	mux.HandleFunc("/v2/namespaces/library/repositories/nginx/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ordering") != "last_updated" {
			t.Errorf("Docker Hub's tags were asked for unordered")
		}
		_, _ = fmt.Fprintf(w, `{"results":[
			{"name":"1.26","digest":%q,"tag_last_pushed":"2025-01-01T00:00:00Z"},
			{"name":"latest","digest":%q,"tag_last_pushed":"2026-10-01T00:00:00Z"},
			{"name":"1.27","digest":%q,"tag_last_pushed":"2026-10-01T00:00:01Z"}]}`, oldDigest, indexDigest, indexDigest)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client reads every registry from the fake, as a test's does, and follows no
// redirect itself, as netguard's client doesn't.
func (f *fakeRegistry) client() *Client {
	c := f.srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return New(Options{Test: f.srv.URL, Secure: c, Plain: c, Public: c})
}

func TestHead(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client()
	ctx := context.Background()
	ref, _ := Parse("nginx:1.27")
	digest, err := c.Head(ctx, ref)
	if err != nil || digest != indexDigest {
		t.Fatalf("Head = %q, %v", digest, err)
	}
	// The token is kept: a second HEAD asks for none.
	if _, err := c.Head(ctx, ref); err != nil || f.tokens.Load() != 1 {
		t.Errorf("a second HEAD asked for a token again: %d tokens, %v", f.tokens.Load(), err)
	}
	if f.gets.Load() != 0 {
		t.Errorf("Head made %d manifest GETs, which Docker Hub counts as pulls", f.gets.Load())
	}
	for _, missing := range []string{"nginx:nope", "acme/private:1"} {
		r, _ := Parse(missing)
		if _, err := c.Head(ctx, r); !errors.Is(err, ErrNotFound) {
			t.Errorf("Head(%s) = %v, want ErrNotFound", missing, err)
		}
	}
	f.limitHead = true
	if _, err := c.Head(ctx, ref); !errors.Is(err, ErrLimited) {
		t.Errorf("Head of a limiting registry = %v, want ErrLimited", err)
	}
}

func TestConfig(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client()
	ref, _ := Parse("nginx:1.27")
	got, digest, err := c.Config(context.Background(), ref, Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatal(err)
	}
	if digest != indexDigest {
		t.Errorf("digest = %q, want the index's", digest)
	}
	if want := []Port{{80, false}, {443, false}, {443, true}}; !slices.Equal(got.Ports, want) {
		t.Errorf("ports = %v, want %v", got.Ports, want)
	}
	if want := []string{"/data", "/var/cache/nginx"}; !slices.Equal(got.Volumes, want) {
		t.Errorf("volumes = %v, want %v", got.Volumes, want)
	}
	if len(got.Env) != 3 || got.Env[1] != (Var{"NGINX_VERSION", "1.27.3"}) || got.Env[2] != (Var{"EMPTY", ""}) {
		t.Errorf("env = %v", got.Env)
	}
	if got.Platform.String() != "linux/arm64/v8" || !slices.Equal(got.Healthcheck, []string{"CMD", "curl", "-f", "http://localhost"}) {
		t.Errorf("platform %s, healthcheck %v", got.Platform, got.Healthcheck)
	}
	// The index, then the platform's manifest: two counted GETs, and nothing more.
	if f.gets.Load() != 2 {
		t.Errorf("Config made %d manifest GETs, want 2", f.gets.Load())
	}
	if _, _, err := c.Config(context.Background(), ref, Platform{OS: "linux", Architecture: "riscv64"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Config for a platform it hasn't = %v, want ErrNotFound", err)
	}
}

func TestTags(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client()
	ctx := context.Background()

	hub, _ := Parse("nginx")
	tags, err := c.Tags(ctx, hub)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var latest []string
	for _, tag := range tags {
		names = append(names, tag.Name)
		if tag.Latest {
			latest = append(latest, tag.Name)
		}
	}
	if !slices.Equal(names, []string{"1.27", "latest", "1.26"}) || !slices.Equal(latest, []string{"1.27", "latest"}) {
		t.Errorf("Docker Hub's tags = %v, latest %v", names, latest)
	}
	if tags[0].Pushed.IsZero() || !tags[0].Pushed.After(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a tag's push time = %v", tags[0].Pushed)
	}

	other, _ := Parse("ghcr.io/acme/web")
	tags, err = c.Tags(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	names, latest = nil, nil
	for _, tag := range tags {
		names = append(names, tag.Name)
		if tag.Latest {
			latest = append(latest, tag.Name)
		}
	}
	// Both pages, by version, and which point where latest does by their digests.
	if !slices.Equal(names, []string{"1.2", "1.2-rc1", "1.1", "latest"}) || !slices.Equal(latest, []string{"1.2", "latest"}) {
		t.Errorf("another registry's tags = %v, latest %v", names, latest)
	}
	if f.gets.Load() != 0 {
		t.Errorf("listing tags made %d manifest GETs, which Docker Hub counts as pulls", f.gets.Load())
	}
}

func TestChallengeParams(t *testing.T) {
	got := challengeParams(`realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull,push"`)
	if got["realm"] != "https://auth.docker.io/token" || got["service"] != "registry.docker.io" || got["scope"] != "repository:library/nginx:pull,push" {
		t.Errorf("challengeParams = %v", got)
	}
}

func TestNextPage(t *testing.T) {
	if got := nextPage(`</v2/acme/web/tags/list?last=1.1&n=1000>; rel="next"`); got != "/v2/acme/web/tags/list?last=1.1&n=1000" {
		t.Errorf("nextPage = %q", got)
	}
	// A link elsewhere is no page of this registry's.
	if got := nextPage(`<https://elsewhere.example.com/x>; rel="next"`); got != "" {
		t.Errorf("nextPage elsewhere = %q", got)
	}
}
