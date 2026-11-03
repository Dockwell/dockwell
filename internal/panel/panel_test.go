// SPDX-License-Identifier: AGPL-3.0-only

package panel

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var (
	indexHTML = []byte(`<!doctype html><html><head><script type="module" src="/assets/app-3f2a9c.js"></script></head><body><div id="root"></div></body></html>`)
	appJS     = bytes.Repeat([]byte("console.log('dockwell');\n"), 200)
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(fstest.MapFS{
		"index.html":           {Data: indexHTML},
		"favicon.svg":          {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"manifest.webmanifest": {Data: []byte(`{"name":"Dockwell"}`)},
		"assets/app-3f2a9c.js": {Data: appJS},
		// A stand-in for Brotli: the handler serves variants as they are.
		"assets/app-3f2a9c.js.br": {Data: []byte("brotli bytes")},
		"assets/app-3f2a9c.js.gz": {Data: gz(t, appJS)},
		"assets/font-b9c1.woff2":  {Data: []byte("wOF2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(t *testing.T, h http.Handler, method, target string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, http.NoBody)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIndexAndClientRoutes(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{"/", "/index.html", "/servers/hz-1", "/apps/api/logs?follow=1"} {
		t.Run(target, func(t *testing.T) {
			res := get(t, h, http.MethodGet, target, map[string]string{"Accept": "text/html,application/xhtml+xml"})
			if res.StatusCode != http.StatusOK || body(t, res) != string(indexHTML) {
				t.Fatalf("GET %s = %d, want the index", target, res.StatusCode)
			}
			for header, want := range map[string]string{
				"Content-Type":            "text/html; charset=utf-8",
				"Cache-Control":           "no-cache",
				"X-Content-Type-Options":  "nosniff",
				"X-Frame-Options":         "DENY",
				"Referrer-Policy":         "same-origin",
				"Content-Security-Policy": contentSecurityPolicy,
			} {
				if got := res.Header.Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

func TestCSPAllowsNoInlineCode(t *testing.T) {
	for _, directive := range []string{"script-src 'self';", "style-src 'self';", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(contentSecurityPolicy, directive) {
			t.Errorf("CSP lacks %q", directive)
		}
	}
	if strings.Contains(contentSecurityPolicy, "unsafe-") {
		t.Error("CSP must not allow unsafe-inline or unsafe-eval")
	}
}

func TestAssetsArePrecompressedAndImmutable(t *testing.T) {
	h := newTestHandler(t)
	tests := []struct {
		acceptEncoding, wantEncoding string
	}{
		{"gzip, deflate, br, zstd", "br"},
		{"br;q=0, gzip", "gzip"},
		{"gzip;q=0.5", "gzip"},
		{"", ""},
		{"identity", ""},
	}
	for _, tt := range tests {
		t.Run(tt.acceptEncoding, func(t *testing.T) {
			res := get(t, h, http.MethodGet, "/assets/app-3f2a9c.js", map[string]string{"Accept-Encoding": tt.acceptEncoding})
			got := body(t, res)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d", res.StatusCode)
			}
			if enc := res.Header.Get("Content-Encoding"); enc != tt.wantEncoding {
				t.Errorf("Content-Encoding = %q, want %q", enc, tt.wantEncoding)
			}
			switch tt.wantEncoding {
			case "br":
				if got != "brotli bytes" {
					t.Errorf("body = %q, want the .br variant", got)
				}
			case "":
				if got != string(appJS) {
					t.Error("body isn't the uncompressed asset")
				}
			}
			if cc := res.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q", cc)
			}
			if vary := res.Header.Get("Vary"); vary != "Accept-Encoding" {
				t.Errorf("Vary = %q", vary)
			}
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

func TestContentTypes(t *testing.T) {
	h := newTestHandler(t)
	for target, want := range map[string]string{
		"/favicon.svg":            "image/svg+xml",
		"/manifest.webmanifest":   "application/manifest+json",
		"/assets/font-b9c1.woff2": "font/woff2",
		"/assets/app-3f2a9c.js":   "text/javascript; charset=utf-8",
	} {
		res := get(t, h, http.MethodGet, target, nil)
		_ = body(t, res)
		if got := res.Header.Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type = %q, want %q", target, got, want)
		}
		if res.Header.Get("Content-Security-Policy") != "" {
			t.Errorf("%s: CSP belongs on documents only", target)
		}
	}
}

func TestRevalidation(t *testing.T) {
	h := newTestHandler(t)
	first := get(t, h, http.MethodGet, "/favicon.svg", nil)
	_ = body(t, first)
	tag := first.Header.Get("ETag")
	if tag == "" {
		t.Fatal("no ETag")
	}
	again := get(t, h, http.MethodGet, "/favicon.svg", map[string]string{"If-None-Match": tag})
	_ = body(t, again)
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304", again.StatusCode)
	}

	br := get(t, h, http.MethodGet, "/assets/app-3f2a9c.js", map[string]string{"Accept-Encoding": "br"})
	plain := get(t, h, http.MethodGet, "/assets/app-3f2a9c.js", nil)
	_, _ = body(t, br), body(t, plain)
	if br.Header.Get("ETag") == plain.Header.Get("ETag") {
		t.Error("each encoding needs its own ETag")
	}
}

func TestNotFound(t *testing.T) {
	h := newTestHandler(t)
	tests := []struct {
		name, target, accept string
	}{
		{"missing file", "/assets/gone-123.js", "*/*"},
		{"API path", "/dockwell.v1.NopeService/Get", "text/html"},
		{"health path", "/grpc.health.v1.Health/Watch", "text/html"},
		{"not a page load", "/settings", "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := get(t, h, http.MethodGet, tt.target, map[string]string{"Accept": tt.accept})
			_ = body(t, res)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", tt.target, res.StatusCode)
			}
		})
	}
}

func TestMethods(t *testing.T) {
	h := newTestHandler(t)
	head := get(t, h, http.MethodHead, "/", map[string]string{"Accept": "text/html"})
	if head.StatusCode != http.StatusOK || body(t, head) != "" {
		t.Errorf("HEAD / = %d with a body", head.StatusCode)
	}
	post := get(t, h, http.MethodPost, "/", nil)
	_ = body(t, post)
	if post.StatusCode != http.StatusMethodNotAllowed || post.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST / = %d, Allow %q", post.StatusCode, post.Header.Get("Allow"))
	}
}

func TestNotBuilt(t *testing.T) {
	h, err := New(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Built() {
		t.Fatal("an empty build reports Built")
	}
	res := get(t, h, http.MethodGet, "/", map[string]string{"Accept": "text/html"})
	text := body(t, res)
	if res.StatusCode != http.StatusNotFound || !strings.Contains(text, "make build") {
		t.Errorf("GET / without a build = %d %q", res.StatusCode, text)
	}
}

func TestAcceptedEncodings(t *testing.T) {
	got := acceptedEncodings("gzip;q=1.0, br;q=0, *;q=0.1, DEFLATE")
	want := map[string]bool{"gzip": true, "br": false, "*": true, "deflate": true}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

func TestEmbeddedLoads(t *testing.T) {
	// Whether or not `make web` ran, the embedded build must load.
	if _, err := New(Embedded()); err != nil {
		t.Fatal(err)
	}
}
