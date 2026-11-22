// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublisherScopesRepositoryAndRechecksDigest(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("1", 64)
	different := "sha256:" + strings.Repeat("2", 64)
	var blobExists, moved, mismatched atomic.Bool
	var writes, blobWrites, tokens atomic.Int32
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			if r.Header.Get("Authorization") != "" {
				t.Error("challenge sent a credential")
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fixture",scope="registry:catalog:*"`, base))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/token" {
			tokens.Add(1)
			u, p, ok := r.BasicAuth()
			if !ok || u != "fixture" || p != "fixture-password" ||
				r.URL.Query().Get("scope") != "repository:team/image:pull,push" || r.URL.Query().Get("service") != "fixture" {
				t.Error("token exchange did not use the approved login and repository scope")
			}
			_, _ = io.WriteString(w, `{"token":"fixture-token","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("registry API received the wrong authentication")
		}
		switch {
		case r.URL.Path == "/v2/team/image/blobs/uploads/" && r.Method == http.MethodPost:
			w.Header().Set("Location", "/v2/team/image/blobs/uploads/fixture?_state=fixture-opaque")
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/blobs/uploads/fixture") && r.Method == http.MethodPut:
			if r.URL.Query().Get("digest") != digest || r.URL.Query().Get("_state") != "fixture-opaque" {
				t.Error("upload URL lost its digest or state")
			}
			data, _ := io.ReadAll(r.Body)
			if string(data) != "blob" {
				t.Error("blob contents changed")
			}
			blobExists.Store(true)
			blobWrites.Add(1)
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/v2/team/image/blobs/"+digest && r.Method == http.MethodHead:
			if !blobExists.Load() {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.URL.Path == "/v2/team/image/manifests/review" && r.Method == http.MethodHead:
			switch {
			case moved.Load(), mismatched.Load() && writes.Load() > 0:
				w.Header().Set("Docker-Content-Digest", different)
			case writes.Load() > 0:
				w.Header().Set("Docker-Content-Digest", digest)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case r.URL.Path == "/v2/team/image/manifests/review" && r.Method == http.MethodPut:
			writes.Add(1)
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("an unexpected request was sent: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	base = server.URL
	r, err := Parse(strings.TrimPrefix(base, "https://") + "/team/image:review")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{Secure: server.Client()}).PublisherFor(r, Auth{
		Registry: r.Registry, Username: "fixture", Password: "fixture-password",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.PutBlob(t.Context(), digest, 4, strings.NewReader("blob")); err != nil {
			t.Fatal(err)
		}
	}
	if blobWrites.Load() != 1 || tokens.Load() != 1 {
		t.Fatalf("sent blob %d times and got %d tokens, want one each", blobWrites.Load(), tokens.Load())
	}
	if time.Until(p.authUntil) < 50*time.Minute {
		t.Fatalf("publisher discarded the advertised token lifetime")
	}
	p.authUntil = time.Now().Add(-time.Second)
	if err := p.PutBlob(t.Context(), digest, 4, strings.NewReader("blob")); err != nil || tokens.Load() != 2 {
		t.Fatalf("publisher did not renew an expired token: %v", err)
	}

	before := func() error { return nil }
	moved.Store(true)
	if err := p.PutManifest(t.Context(), "review", digest, strings.NewReader("manifest"), 8, before); !errors.Is(err, ErrPushConflict) || writes.Load() != 0 {
		t.Fatalf("a moved tag accepted a write: %v (%d writes)", err, writes.Load())
	}
	moved.Store(false)
	mismatched.Store(true)
	if err := p.PutManifest(t.Context(), "review", digest, strings.NewReader("manifest"), 8, before); !errors.Is(err, ErrPushUnknown) || writes.Load() != 1 {
		t.Fatalf("different read-back = %v (%d writes), want unknown and one write", err, writes.Load())
	}
}

func TestPublisherRefusesMovedRealmAndUploadOrigin(t *testing.T) {
	t.Parallel()
	leaked := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("registry redirected a request off origin")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer leaked.Close()
	var moved atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			if moved.Load() {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token"`, leaked.URL))
			} else {
				w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/v2/team/image/blobs/uploads/":
			w.Header().Set("Location", leaked.URL+"/v2/team/image/blobs/uploads/fixture")
			w.WriteHeader(http.StatusAccepted)
		default:
			if moved.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	r, err := Parse(strings.TrimPrefix(server.URL, "https://") + "/team/image:review")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{Secure: server.Client(), Public: leaked.Client()}).PublisherFor(r, Auth{
		Registry: r.Registry, Username: "fixture", Password: "fixture-password",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := p.PutBlob(context.Background(), digest, 4, strings.NewReader("blob")); !errors.Is(err, ErrPushRefused) {
		t.Fatalf("foreign upload location = %v", err)
	}
	moved.Store(true)
	if _, err := p.ManifestDigest(t.Context(), r.Tag); !errors.Is(err, ErrMoved) {
		t.Fatalf("foreign token realm = %v", err)
	}
}

type pushTestDoer func(*http.Request) (*http.Response, error)

func (f pushTestDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublisherSharesBlobsThroughStorageRedirects(t *testing.T) {
	t.Parallel()
	const storageHost = "storage.example.test"
	digest := "sha256:" + strings.Repeat("a", 64)
	newBlob := "sha256:" + strings.Repeat("b", 64)
	var posts, storageReads, storageWrites atomic.Int32
	var blobWritten, published atomic.Bool
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+digest) || blobWritten.Load() && strings.HasSuffix(r.URL.Path, "/blobs/"+newBlob):
			w.Header().Set("Location", "https://"+storageHost+"/saved?signature=fixture")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/blobs/"+newBlob):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads/"):
			posts.Add(1)
			w.Header().Set("Location", base+"/v2/team/image/blobs/uploads/fixture")
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/v2/team/image/manifests/next":
			switch {
			case r.Method == http.MethodPut && blobWritten.Load():
				published.Store(true)
				w.Header().Set("Docker-Content-Digest", newBlob)
				w.WriteHeader(http.StatusCreated)
			case r.Method == http.MethodHead && published.Load():
				w.Header().Set("Docker-Content-Digest", newBlob)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/uploads/fixture"):
			// Consume the body before redirecting. The destination must get the same bytes.
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Location", "https://"+storageHost+"/upload?signature=fixture")
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	base = server.URL
	storage := pushTestDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != storageHost || r.Header.Get("Authorization") != "" || r.URL.Query().Get("signature") != "fixture" {
			t.Error("redirect lost its signature or sent credentials off origin")
		}
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			if string(body) != "new layer" {
				t.Error("redirected upload did not replay the exact bytes")
			}
			_ = r.Body.Close()
			blobWritten.Store(true)
			storageWrites.Add(1)
		} else {
			if r.Method != http.MethodHead {
				t.Error("a shared blob was downloaded")
			}
			storageReads.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	r, _ := Parse(strings.TrimPrefix(base, "https://") + "/team/image:next")
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	publisher, err := New(Options{Secure: client, Storage: storage}).PublisherFor(r, Auth{
		Registry: r.Registry, Username: "fixture", Password: "fixture",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.PutBlob(t.Context(), digest, 10, strings.NewReader("base layer")); err != nil || posts.Load() != 0 || storageReads.Load() != 1 {
		t.Fatalf("shared layer was not reused: %v", err)
	}
	if err := publisher.PutBlob(t.Context(), newBlob, 9, strings.NewReader("new layer")); err != nil ||
		posts.Load() != 1 || storageWrites.Load() != 1 || storageReads.Load() != 2 {
		t.Fatalf("redirected upload not confirmed: %v", err)
	}
	if err := publisher.PutManifest(t.Context(), r.Tag, newBlob, strings.NewReader("manifest"), 8, func() error { return nil }); err != nil || !published.Load() {
		t.Fatalf("push with a shared base did not publish the tag: %v", err)
	}
}

func TestPublisherRefusesPullOnlyJWTAndChecksEveryManifestType(t *testing.T) {
	t.Parallel()
	var grantPush atomic.Bool
	var manifestReads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			actions := `"pull"`
			if grantPush.Load() {
				actions += `,"push"`
			}
			payload := base64.RawURLEncoding.EncodeToString([]byte(`{"access":[{"type":"repository","name":"team/image","actions":[` + actions + `]}]}`))
			_, _ = fmt.Fprintf(w, `{"token":"header.%s.signature"}`, payload)
		default:
			manifestReads.Add(1)
			for _, media := range []string{
				OCIManifest, "application/vnd.oci.image.index.v1+json",
				"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json",
				"application/vnd.docker.distribution.manifest.v1+prettyjws", "application/vnd.docker.distribution.manifest.v1+json",
				"application/vnd.oci.artifact.manifest.v1+json",
			} {
				if !strings.Contains(r.Header.Get("Accept"), media) {
					t.Errorf("manifest check omitted %s", media)
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
		}
	}))
	defer server.Close()
	r, _ := Parse(strings.TrimPrefix(server.URL, "https://") + "/team/image:review")
	p, err := New(Options{Secure: server.Client()}).PublisherFor(r, Auth{Registry: r.Registry, Password: "synthetic"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ManifestDigest(t.Context(), r.Tag); !errors.Is(err, ErrSignIn) || manifestReads.Load() != 0 {
		t.Fatalf("pull-only scope reached the manifest: %v", err)
	}
	grantPush.Store(true)
	if got, err := p.ManifestDigest(t.Context(), r.Tag); err != nil || got == "" {
		t.Fatalf("existing manifest read as missing: %v", err)
	}
}
