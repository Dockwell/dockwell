// SPDX-License-Identifier: AGPL-3.0-only

// Package panel serves the web panel built into the binary.
//
// `make web` builds web/ into dist/app, which is embedded here. The handler
// loads the build once, then serves it with the headers a production web app
// needs: precompressed Brotli or gzip variants, immutable caching for
// content-hashed assets, ETag revalidation for everything else, a
// single-page-app fallback for client-side routes, and strict security
// headers on HTML.
package panel

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed all:dist
var embedded embed.FS

// Embedded returns the panel build compiled into the binary, rooted at its
// index.html. It may be empty when the binary was built without `make web`.
func Embedded() fs.FS {
	sub, err := fs.Sub(embedded, "dist/app")
	if err != nil {
		// fs.Sub only fails on an invalid path, and "dist/app" is valid.
		panic(err)
	}
	return sub
}

// contentSecurityPolicy allows nothing the panel doesn't load from its own
// origin: no inline scripts or styles, no framing, no plugins.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; manifest-src 'self'; worker-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

const immutableAssets = "/assets/"

// apiPrefixes are request paths that belong to the API. They never fall back
// to the panel, so a mistyped procedure returns 404 rather than HTML.
var apiPrefixes = []string{"/dockwell.", "/grpc."}

type asset struct {
	contentType string
	etag        string
	body        []byte
	brotli      []byte
	gzip        []byte
	immutable   bool
}

// Handler serves a panel build.
type Handler struct {
	assets map[string]*asset
	index  *asset
}

// New loads the build in fsys. A build without index.html is allowed: the
// handler then explains how to build the panel instead of serving it.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{assets: map[string]*asset{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(name, ".br") || strings.HasSuffix(name, ".gz") {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		a := &asset{
			contentType: contentType(name),
			etag:        etag(body),
			body:        body,
			immutable:   strings.HasPrefix("/"+name, immutableAssets),
		}
		a.brotli = readVariant(fsys, name+".br", len(body))
		a.gzip = readVariant(fsys, name+".gz", len(body))
		h.assets["/"+name] = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("can't load the panel build: %w", err)
	}
	h.index = h.assets["/index.html"]
	return h, nil
}

// Built reports whether the handler has a panel to serve.
func (h *Handler) Built() bool { return h.index != nil }

// ServeHTTP serves the asset at the request path, falls back to index.html
// for page navigations, and answers 404 for anything else.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := path.Clean("/" + r.URL.Path)
	a, ok := h.assets[name]
	if !ok {
		if !isNavigation(r, name) {
			http.NotFound(w, r)
			return
		}
		if h.index == nil {
			notBuilt(w)
			return
		}
		a = h.index
	}
	serve(w, r, name, a)
}

func serve(w http.ResponseWriter, r *http.Request, name string, a *asset) {
	header := w.Header()
	header.Set("Content-Type", a.contentType)
	if a.immutable {
		header.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		header.Set("Cache-Control", "no-cache")
	}
	if a.contentType == htmlType {
		setDocumentHeaders(header)
	}

	body, encoding := a.body, ""
	if a.brotli != nil || a.gzip != nil {
		header.Add("Vary", "Accept-Encoding")
		switch accepts := acceptedEncodings(r.Header.Get("Accept-Encoding")); {
		case a.brotli != nil && accepts["br"]:
			body, encoding = a.brotli, "br"
		case a.gzip != nil && accepts["gzip"]:
			body, encoding = a.gzip, "gzip"
		}
	}
	if encoding != "" {
		header.Set("Content-Encoding", encoding)
		header.Set("ETag", strconv.Quote(a.etag+"-"+encoding))
	} else {
		header.Set("ETag", strconv.Quote(a.etag))
	}
	// ServeContent handles If-None-Match, Range and HEAD. The zero modtime
	// leaves validation to the ETag.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

func setDocumentHeaders(h http.Header) {
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
}

// isNavigation reports whether a request for a missing file is a page load
// that the panel's client-side router should handle.
func isNavigation(r *http.Request, name string) bool {
	for _, prefix := range apiPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	if path.Ext(name) != "" {
		return false
	}
	accept := r.Header.Get("Accept")
	return accept == "" || strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*")
}

// acceptedEncodings parses Accept-Encoding, honouring q=0 as a refusal.
func acceptedEncodings(header string) map[string]bool {
	accepted := map[string]bool{}
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding == "" {
			continue
		}
		q := 1.0
		for p := range strings.SplitSeq(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "q") {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					q = f
				}
			}
		}
		accepted[coding] = q > 0
	}
	return accepted
}

const htmlType = "text/html; charset=utf-8"

func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return htmlType
	case ".webmanifest":
		return "application/manifest+json"
	case ".woff2":
		return "font/woff2"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func etag(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:12])
}

// readVariant returns a precompressed variant if it exists and is smaller.
func readVariant(fsys fs.FS, name string, size int) []byte {
	b, err := fs.ReadFile(fsys, name)
	if err != nil || len(b) >= size {
		return nil
	}
	return b
}

// notBuilt explains a binary built without the panel. It is a 404: the panel
// isn't here, but the API is.
func notBuilt(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", htmlType)
	h.Set("Cache-Control", "no-store")
	setDocumentHeaders(h)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notBuiltPage))
}

const notBuiltPage = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Dockwell</title>
<body>
<h1>The panel isn't built into this binary</h1>
<p>The API is running. To add the panel, run <code>make build</code> in a Dockwell checkout, which builds the panel and embeds it.</p>
</body>
</html>
`
