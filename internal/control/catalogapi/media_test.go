// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// A source's SVG image is served as svgclean writes it, whatever was kept, and so
// that a browser draws it as an image alone: opened on its own it's a download, and
// were it shown, a sandboxed page that runs nothing and loads nothing (ADR 0083).
func TestASourcesSVGIsServedAsAnImageAlone(t *testing.T) {
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{
			"notes.yaml":     []byte("id: notes\n"),
			"media/logo.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script><rect width="1" onclick="x()"/></svg>`),
			"media/bad.svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>@import url(https://example.com/a.css);</style></svg>`),
		}, nil
	}
	s := &Sources{Dir: t.TempDir(), Fetch: fetch}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("d", 40)}
	if _, err := s.files(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	s.tidy([]stored{{name: "community", spec: spec}})
	mux := http.NewServeMux()
	mux.Handle(MediaRoute, s.Media(nil))
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return w
	}
	w := get(MediaPath(spec, "logo.svg"))
	if w.Code != http.StatusOK || w.Body.String() != `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1"/></svg>` {
		t.Fatalf("the logo: %d %q", w.Code, w.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":                 "image/svg+xml",
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox",
		"Content-Disposition":          "attachment",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}
	// Cleaned once, and served again from what was kept, however often it's asked for.
	if again := get(MediaPath(spec, "logo.svg")); again.Body.String() != w.Body.String() || len(s.images) != 1 || s.imageBytes != w.Body.Len() {
		t.Errorf("the logo again: %q, %d kept of %d bytes", again.Body.String(), len(s.images), s.imageBytes)
	}
	if w := get(MediaPath(spec, "bad.svg")); w.Code != http.StatusNotFound {
		t.Errorf("an SVG image that can't be shown: %d, want 404", w.Code)
	}
	// What's on disk is served only once it passes again, as a file kept by an
	// older Dockwell, or changed on disk, may not have.
	kept, _ := s.folders(spec)
	if err := os.WriteFile(filepath.Join(kept, "media", "logo.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := get(MediaPath(spec, "logo.svg")); strings.Contains(w.Body.String(), "script") {
		t.Errorf("an SVG image on disk was served as it was: %q", w.Body.String())
	}
}
