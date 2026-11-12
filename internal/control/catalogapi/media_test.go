// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/gitfetch"
)

// A source's image is served from the commit kept for a source applied, as what its
// first bytes say it is, never sniffed, cached for good as its path names its commit,
// and to whoever may read the catalog alone. A file that isn't an image, a commit
// read only for a plan, and any other path answer that there's nothing there.
func TestASourcesImagesAreServed(t *testing.T) {
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{
			"notes.yaml":       []byte("id: notes\n"),
			"media/logo.png":   png,
			"media/page.png":   []byte("<html><script>alert(1)</script></html>"),
			"media/photo.jpeg": []byte("\xff\xd8\xff\xe0\x00\x10JFIF"),
		}, nil
	}
	s := &Sources{Dir: t.TempDir(), Fetch: fetch}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("d", 40)}
	if _, err := s.files(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	signedIn := true
	h := s.Media(func(*http.Request) error {
		if !signedIn {
			return errors.New("no session")
		}
		return nil
	})
	mux := http.NewServeMux()
	mux.Handle(MediaRoute, h)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return w
	}
	logo := MediaPath(spec, "logo.png")
	if w := get(logo); w.Code != http.StatusNotFound {
		t.Errorf("a commit read only for a plan served its image: %d", w.Code)
	}
	s.tidy([]stored{{name: "community", spec: spec}})
	w := get(logo)
	if w.Code != http.StatusOK || w.Body.String() != string(png) {
		t.Fatalf("the logo: %d %q", w.Code, w.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Cache-Control":           "private, max-age=31536000, immutable",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}
	if w := get(MediaPath(spec, "photo.jpeg")); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("a JPEG: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, path := range []string{
		MediaPath(spec, "page.png"),
		MediaPath(spec, "missing.png"),
		MediaPrefix + keyOf(spec) + "/" + spec.GetCommit() + "/..%2fnotes.yaml",
		MediaPrefix + keyOf(spec) + "/" + spec.GetCommit() + "/notes.yaml",
		MediaPrefix + "..%2f..%2fkept/" + spec.GetCommit() + "/logo.png",
		MediaPrefix + strings.ToUpper(keyOf(spec)) + "/" + spec.GetCommit() + "/logo.png",
		MediaPrefix + keyOf(spec) + "/" + strings.Repeat("e", 40) + "/logo.png",
		MediaPrefix + keyOf(spec) + "/" + spec.GetCommit() + "/logo.svg",
	} {
		if w := get(path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
	signedIn = false
	if w := get(logo); w.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", w.Code)
	}
	if w := httptest.NewRecorder(); true {
		(&Sources{}).Media(nil).ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, logo, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("with nothing kept: %d", w.Code)
		}
	}
}
