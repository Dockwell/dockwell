// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type archiveTestTransport func(*http.Request) (*http.Response, error)

func (f archiveTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestImageArchiveSelectsARMManifestWithoutChangingOtherEngineAPIs(t *testing.T) {
	index := "sha256:" + strings.Repeat("a", 64)
	child := "sha256:" + strings.Repeat("b", 64)
	c := New("")
	c.http.Transport = archiveTestTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		var body any
		switch r.URL.Path {
		case "/version":
			body = map[string]string{"Version": "29.4.1", "ApiVersion": "1.53", "MinAPIVersion": "1.44"}
		case "/v1.49/images/" + index + "/json":
			if got := r.URL.Query().Get("platform"); got != `{"os":"linux","architecture":"arm64"}` {
				t.Errorf("inspect platform is %s", got)
			}
			body = &Image{ID: child, Descriptor: &ImageDescriptor{Digest: child, MediaType: "application/vnd.oci.image.manifest.v1+json"}}
		case "/v1.49/images/" + index + "/get":
			if got := r.URL.Query().Get("platform"); got != `{"os":"linux","architecture":"arm64"}` {
				t.Errorf("save platform is %s", got)
			}
		default:
			t.Errorf("unexpected Engine path %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
		if body != nil {
			if err := json.NewEncoder(w).Encode(body); err != nil {
				return nil, err
			}
		}
		return w.Result(), nil
	})
	ctx := t.Context()
	got, err := c.ImagePlatformManifest(ctx, &Image{ID: index}, "linux/arm64")
	if err != nil || got != child {
		t.Fatalf("manifest %q: %v", got, err)
	}
	saved, err := c.SaveImage(ctx, index, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	_ = saved.Close()
	if api, _ := c.Version(); api != MaxAPIVersion {
		t.Fatalf("base API changed to %s", api)
	}
}
