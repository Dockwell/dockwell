// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// registry answers as a registry that signs nothing in does: a challenge first, then
// with the token it gave.
func registry(t *testing.T, built time.Time) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:example/app:pull" || r.URL.Query().Get("service") != "test-registry" {
			http.Error(w, "wrong scope", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "t0ken"})
	})
	mux.HandleFunc("/v2/example/app/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0ken" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test-registry",scope="repository:example/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		switch strings.TrimPrefix(r.URL.Path, "/v2/example/app/") {
		case "manifests/1.0":
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				http.Error(w, "asks for no index", http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:index")
		case "manifests/sha256:index":
			_, _ = w.Write([]byte(`{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`))
		case "manifests/sha256:amd":
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:config"}}`))
		case "blobs/sha256:config":
			_ = json.NewEncoder(w).Encode(map[string]any{"created": built})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/v2/namespaces/library/repositories/redis/tags/8.0", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_last_pushed": built, "last_updated": built.Add(time.Hour)})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &gets
}

// A tag pins to its index with a HEAD, which no registry counts, and its age is read
// from the amd64 build's config.
func TestOCIPinsATagAndReadsItsAge(t *testing.T) {
	built := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	srv, gets := registry(t, built)
	o := &OCI{Client: srv.Client()}
	image := strings.TrimPrefix(srv.URL, "http://") + "/example/app:1.0"
	digest, err := o.Pin(t.Context(), image)
	if err != nil || digest != "sha256:index" {
		t.Fatalf("Pin = %q, %v", digest, err)
	}
	if gets.Load() != 0 {
		t.Errorf("pinning a tag took %d GETs, want only a HEAD", gets.Load())
	}
	got, err := o.Built(t.Context(), image, digest)
	if err != nil || !got.Equal(built) {
		t.Errorf("Built = %v, %v, want %v", got, err, built)
	}
}

// Docker Hub's own API says when a tag was pushed, which it doesn't count as a pull.
func TestOCIAsksDockerHubsAPIForAnAge(t *testing.T) {
	built := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	srv, _ := registry(t, built)
	o := &OCI{Client: srv.Client(), HubAPI: srv.URL}
	got, err := o.Built(t.Context(), "redis:8.0", "sha256:anything")
	if err != nil || !got.Equal(built) {
		t.Errorf("Built = %v, %v, want %v", got, err, built)
	}
}

// Past its budget, a manifest GET of Docker Hub isn't asked at all.
func TestOCIKeepsWithinDockerHubsBudget(t *testing.T) {
	o := &OCI{HubGets: 0}
	r, err := parseRef("example/app:1.0")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := o.get(t.Context(), r, "manifests/sha256:x", manifestTypes)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrNotAsked) {
		t.Errorf("a GET past the budget = %v, want ErrNotAsked", err)
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]ref{
		"redis:8.0":                         {host: dockerHub, name: "library/redis", tag: "8.0"},
		"neosmemo/memos:0.31.0":             {host: dockerHub, name: "neosmemo/memos", tag: "0.31.0"},
		"ghcr.io/coder/coder:v2.38.6":       {host: "ghcr.io", name: "coder/coder", tag: "v2.38.6"},
		"localhost:5000/app:1":              {host: "localhost:5000", name: "app", tag: "1"},
		"index.docker.io/library/nginx:1.2": {host: dockerHub, name: "library/nginx", tag: "1.2"},
		"nginx:1.27@sha256:abc":             {host: dockerHub, name: "library/nginx", tag: "1.27", digest: "sha256:abc"},
	} {
		got, err := parseRef(in)
		if err != nil || got != want {
			t.Errorf("parseRef(%q) = %+v, %v, want %+v", in, got, err, want)
		}
	}
	if _, err := parseRef("example/app"); err == nil {
		t.Error("an image with no tag parsed")
	}
}
