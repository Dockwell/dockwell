// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
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

// updatesRegistry stands in for Docker Hub, its API and ghcr.io: postgres's
// tags on Docker Hub, with their digests and push times, and acme/app's on
// another registry, by name alone. It counts what it's asked.
type updatesRegistry struct {
	srv                   *httptest.Server
	heads, gets, hubLists atomic.Int32
	filter                atomic.Pointer[string]
	limited               atomic.Bool
}

func fakeDigest(s string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(s))) }

func newUpdatesRegistry(t *testing.T) *updatesRegistry {
	f := &updatesRegistry{}
	pushed := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.limited.Load() {
			http.Error(w, "too many", http.StatusTooManyRequests)
			return
		}
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/v2/namespaces/library/repositories/postgres/tags"):
			f.hubLists.Add(1)
			filter := r.URL.Query().Get("name")
			f.filter.Store(&filter)
			var results []string
			for _, tag := range []string{"16.3", "16.4", "16", "17.2", "17", "18.0-rc1", "16.4-alpine", "latest"} {
				if filter != "" && !strings.Contains(tag, filter) {
					continue
				}
				results = append(results, fmt.Sprintf(`{"name":%q,"digest":%q,"tag_last_pushed":%q}`, tag, fakeDigest("postgres:"+tag), pushed.Format(time.RFC3339)))
			}
			_, _ = fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(results, ","))
		case path == "/v2/acme/app/tags/list":
			_, _ = w.Write([]byte(`{"name":"acme/app","tags":["v1.2.3","v1.2.4","v1.3.0","v2.0.0-beta1","main"]}`))
		case strings.Contains(path, "/manifests/"):
			if r.Method == http.MethodHead {
				f.heads.Add(1)
			} else {
				f.gets.Add(1)
			}
			before, _, _ := strings.Cut(path, "/manifests/")
			repo := strings.TrimPrefix(before, "/v2/")
			tag := path[strings.LastIndex(path, "/")+1:]
			if repo == "library/gone" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", fakeDigest(strings.TrimPrefix(repo, "library/")+":"+tag))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newUpdatesImages(t *testing.T, f *updatesRegistry) (*images, *dockertest.Engine) {
	t.Helper()
	e := dockertest.New(t)
	base, _ := url.Parse(f.srv.URL)
	im := newImages(e.Client(), ImageReads{}, nil)
	im.newClient = func(o registry.Options) *registry.Client {
		stand := to{base, nil}
		o.Secure, o.Plain, o.Public = stand, stand, stand
		return registry.New(o)
	}
	return im, e
}

// A version tag on Docker Hub gets its digest with a HEAD and its newer tags
// from one page of Docker Hub's API, which carries their digests, so nothing
// counted as a pull is asked, and the label of what runs says where its notes are.
func TestImageUpdatesOfAVersionTag(t *testing.T) {
	f := newUpdatesRegistry(t)
	im, e := newUpdatesImages(t, f)
	e.AddLocalImage("postgres:16.3", dockertest.Image{Config: docker.Config{Labels: map[string]string{sourceLabel: "https://github.com/docker-library/postgres"}}})
	got, err := im.imageUpdates(t.Context(), &agentv1.ImageUpdatesCall{Reference: "postgres:16.3", Tags: true, Running: "postgres:16.3"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND || got.GetDigest() != fakeDigest("postgres:16.3") {
		t.Errorf("answer %v, digest %s; want found, with 16.3's", got.GetAnswer(), got.GetDigest())
	}
	var said []string
	for _, n := range got.GetNewer() {
		said = append(said, fmt.Sprintf("%s %s %v", n.GetName(), n.GetKind(), n.GetDigest() == fakeDigest("postgres:"+n.GetName())))
		if n.GetPushTime() == nil {
			t.Errorf("%s has no push time, which Docker Hub said", n.GetName())
		}
	}
	if want := "16.4 UPDATE_KIND_PATCH true, 17.2 UPDATE_KIND_MAJOR true"; strings.Join(said, ", ") != want {
		t.Errorf("newer = %q, want %q", strings.Join(said, ", "), want)
	}
	if got.GetSource() != "https://github.com/docker-library/postgres" {
		t.Errorf("source = %q", got.GetSource())
	}
	if f.gets.Load() != 0 || f.heads.Load() != 1 || f.hubLists.Load() != 1 {
		t.Errorf("asked %d GETs, %d HEADs, %d lists; want 0, 1 and 1", f.gets.Load(), f.heads.Load(), f.hubLists.Load())
	}
}

// A suffix asks Docker Hub only for tags that hold its word.
func TestImageUpdatesAskForTheSuffixAlone(t *testing.T) {
	f := newUpdatesRegistry(t)
	im, _ := newUpdatesImages(t, f)
	got, err := im.imageUpdates(t.Context(), &agentv1.ImageUpdatesCall{Reference: "postgres:16.3-alpine", Tags: true})
	if err != nil {
		t.Fatal(err)
	}
	if filter := f.filter.Load(); filter == nil || *filter != "alpine" {
		t.Errorf("Docker Hub was asked for %v, want tags holding alpine", filter)
	}
	if len(got.GetNewer()) != 1 || got.GetNewer()[0].GetName() != "16.4-alpine" {
		t.Errorf("newer = %v, want 16.4-alpine alone", got.GetNewer())
	}
}

// Another registry's list gives names alone, so each newer tag's digest takes a
// HEAD, and a pre-release is left out.
func TestImageUpdatesOfAnotherRegistry(t *testing.T) {
	f := newUpdatesRegistry(t)
	im, _ := newUpdatesImages(t, f)
	got, err := im.imageUpdates(t.Context(), &agentv1.ImageUpdatesCall{Reference: "ghcr.io/acme/app:v1.2.3", Tags: true})
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, n := range got.GetNewer() {
		said = append(said, n.GetName())
		if n.GetDigest() != fakeDigest("acme/app:"+n.GetName()) {
			t.Errorf("%s's digest = %s", n.GetName(), n.GetDigest())
		}
	}
	if strings.Join(said, " ") != "v1.2.4 v1.3.0" {
		t.Errorf("newer = %v, want v1.2.4 and v1.3.0", said)
	}
	if f.heads.Load() != 3 {
		t.Errorf("asked %d HEADs, want one for the tag and one for each newer", f.heads.Load())
	}
}

// A tag that isn't a version gets its digest alone, and lists no tags.
func TestImageUpdatesOfLatest(t *testing.T) {
	f := newUpdatesRegistry(t)
	im, _ := newUpdatesImages(t, f)
	got, err := im.imageUpdates(t.Context(), &agentv1.ImageUpdatesCall{Reference: "postgres", Tags: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDigest() != fakeDigest("postgres:latest") || len(got.GetNewer()) != 0 || f.hubLists.Load() != 0 {
		t.Errorf("digest %s, newer %v, %d lists; want latest's digest alone", got.GetDigest(), got.GetNewer(), f.hubLists.Load())
	}
}

// What a registry says when it can't help is an answer, not a failed call, so
// the control plane keeps what it found before and says why.
func TestImageUpdatesSayWhyTheyCouldnt(t *testing.T) {
	f := newUpdatesRegistry(t)
	im, _ := newUpdatesImages(t, f)
	got, err := im.imageUpdates(t.Context(), &agentv1.ImageUpdatesCall{Reference: "gone:1.0", Tags: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_FOUND {
		t.Errorf("a missing image answered %v", got.GetAnswer())
	}
	f.limited.Store(true)
	got, err = im.imageUpdates(t.Context(), &agentv1.ImageUpdatesCall{Reference: "postgres:16.3", Tags: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetAnswer() != dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED {
		t.Errorf("a limited registry answered %v, want not asked", got.GetAnswer())
	}
}

func TestHTTPSOnly(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/app": "https://github.com/acme/app",
		"http://github.com/acme/app":  "",
		"git@github.com:acme/app.git": "",
		"https://user@example.com/x":  "",
		"javascript:alert(1)":         "",
		"":                            "",
	} {
		if got := httpsOnly(in); got != want {
			t.Errorf("httpsOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
