// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveImageVerifiesIndexPlatformAndConfig(t *testing.T) {
	config := `{"os":"linux","architecture":"amd64","config":{"Env":["PASSWORD=fixture-only"]}}`
	configID := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(config)))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":%q}}`, configID)
	manifestID := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))
	index := fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, manifestID)
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/example/fixture/manifests/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, index)
	})
	mux.HandleFunc("/v2/example/fixture/manifests/"+manifestID, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, manifest)
	})
	mux.HandleFunc("/v2/example/fixture/blobs/"+configID, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Bad") != "" {
			_, _ = fmt.Fprint(w, config+" ")
		} else {
			_, _ = fmt.Fprint(w, config)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := New(Options{Test: server.URL})
	ref, err := Parse("example/fixture:latest")
	if err != nil {
		t.Fatal(err)
	}
	platform := Platform{OS: "linux", Architecture: "amd64"}
	got, err := client.ResolveImage(t.Context(), ref, platform)
	if err != nil || got.Manifest != manifestID || got.Config != configID || got.Platform != platform ||
		got.Index != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(index))) {
		t.Fatalf("verified identity = %+v, %v", got, err)
	}
	if _, err := client.ResolveImage(t.Context(), ref, Platform{OS: "linux", Architecture: "arm64"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected platform accepted: %v", err)
	}

	altered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Fixture-Bad", "1")
		mux.ServeHTTP(w, r)
	}))
	defer altered.Close()
	if _, err := New(Options{Test: altered.URL}).ResolveImage(t.Context(), ref, platform); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("altered config accepted: %v", err)
	}
}
