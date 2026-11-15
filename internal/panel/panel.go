// SPDX-License-Identifier: AGPL-3.0-only

// Package panel serves the web panel built into the binary.
//
// `make web` builds web/ into dist/app, which is embedded here. The handler
// indexes the build's names once and then serves it with the headers a
// production web app needs: precompressed Brotli or gzip variants, immutable
// caching for content-hashed assets, ETag revalidation for everything else, a
// client-route fallback for page loads, and strict security headers on HTML.
// Starting reads no file's contents (#619): an asset's name is its ETag, the
// few other files are hashed on their first request, and the build writes a
// Brotli or gzip variant only where it's smaller. File contents are read from
// the embedded filesystem per request, so the build isn't copied onto the heap.
package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:dist
var embedded embed.FS

// Embedded returns the panel build compiled into the binary, rooted at its
// index.html. The directory is absent when the binary was built without
// `make web`, which New treats as no panel.
func Embedded() fs.FS {
	sub, err := fs.Sub(embedded, "dist/app")
	if err != nil {
		// fs.Sub only fails on an invalid path, and "dist/app" is valid.
		panic(err)
	}
	return sub
}

// contentSecurityPolicy allows nothing the panel doesn't load from its own
// origin: no inline scripts or styles, no framing, no plugins. Its forms post
// to the panel alone, unless PostsTo names another origin.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; manifest-src 'self'; worker-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// assetDir holds the build's content-hashed files. They never change, so
// they are cached for a year, and a missing one is a 404, never the index.
const assetDir = "/assets/"

const htmlType = "text/html; charset=utf-8"

type asset struct {
	file        string // path within the build
	contentType string
	brotli      bool
	gzip        bool
	immutable   bool

	// The ETag: an asset's content-hashed name, and for the rest a hash of the
	// file, made on its first request and kept.
	tagOnce sync.Once
	tag     string
	tagErr  error
}

// etag is the asset's ETag, without an encoding's suffix. A content-hashed
// asset's is its name, which changes with every byte; another file is read
// once, on its first request, rather than as the control plane starts.
func (a *asset) etag(fsys fs.FS) (string, error) {
	a.tagOnce.Do(func() {
		if a.immutable {
			a.tag = path.Base(a.file)
			return
		}
		a.tag, a.tagErr = hashFile(fsys, a.file)
	})
	return a.tag, a.tagErr
}

// Handler serves a panel build.
type Handler struct {
	fsys   fs.FS
	assets map[string]*asset
	index  *asset
	// csp is the Content-Security-Policy its pages are served with.
	csp string
}

// PostsTo lets the panel's forms post to origins too, as connecting GitHub posts
// the App's manifest to GitHub's page that makes it (ADR 0098). Each is a scheme
// and host, with a port where it has one; nothing else widens. Call it before
// the handler serves.
func (h *Handler) PostsTo(origins ...string) {
	allowed := []string{"form-action 'self'"}
	for _, o := range origins {
		if o != "" && !strings.ContainsAny(o, " ;,'\"") {
			allowed = append(allowed, o)
		}
	}
	h.csp = strings.Replace(contentSecurityPolicy, "form-action 'self'", strings.Join(allowed, " "), 1)
}

// New indexes the build in fsys by its names alone, reading no file. A
// missing or empty build is allowed: the handler then explains how to build
// the panel instead of serving it.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{fsys: fsys, assets: map[string]*asset{}, csp: contentSecurityPolicy}
	var names []string
	variants := map[string]bool{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || d.IsDir():
			return err
		case strings.HasSuffix(name, ".br") || strings.HasSuffix(name, ".gz"):
			variants[name] = true
		default:
			names = append(names, name)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) && len(names) == 0 {
		return h, nil // no build at all
	}
	if err != nil {
		return nil, fmt.Errorf("can't load the panel build: %w", err)
	}
	for _, name := range names {
		// The build writes a variant only where it's smaller (web/vite.config.ts), so
		// one that's there is served.
		h.assets["/"+name] = &asset{
			file:        name,
			contentType: contentType(name),
			brotli:      variants[name+".br"],
			gzip:        variants[name+".gz"],
			immutable:   strings.HasPrefix("/"+name, assetDir),
		}
	}
	h.index = h.assets["/index.html"]
	return h, nil
}

// Built reports whether the handler has a panel to serve.
func (h *Handler) Built() bool { return h.index != nil }

// ServeHTTP serves the file at the request path. A page load of any other
// path gets index.html, so the panel's router can handle it; everything else
// is a 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name := path.Clean("/" + r.URL.Path)
	a, ok := h.assets[name]
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead

	switch {
	case ok && !isRead:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case ok:
		h.serve(w, r, a)
	case !isRead:
		// Not ours: most likely a mistyped API procedure, which clients should
		// see as unimplemented (404), not as a method error.
		notFound(w)
	default:
		// The answer depends on Accept, so caches must key on it.
		w.Header().Add("Vary", "Accept")
		switch {
		case !isPageLoad(r, name):
			notFound(w)
		case h.index == nil:
			notBuilt(w)
		default:
			h.serve(w, r, h.index)
		}
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, a *asset) {
	header := w.Header()
	header.Set("Content-Type", a.contentType)
	if a.immutable {
		header.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		header.Set("Cache-Control", "no-cache")
	}
	if a.contentType == htmlType {
		setDocumentHeaders(header, h.csp)
	}

	file, encoding := a.file, ""
	if a.brotli || a.gzip {
		header.Add("Vary", "Accept-Encoding")
		switch accepts := acceptedEncodings(r.Header.Get("Accept-Encoding")); {
		case a.brotli && accepts["br"]:
			file, encoding = a.file+".br", "br"
		case a.gzip && accepts["gzip"]:
			file, encoding = a.file+".gz", "gzip"
		}
	}
	etag, err := a.etag(h.fsys)
	if err != nil {
		http.Error(w, "the panel build changed while running; restart dockwell", http.StatusInternalServerError)
		return
	}
	if encoding != "" {
		header.Set("Content-Encoding", encoding)
		etag += "-" + encoding
	}
	header.Set("ETag", strconv.Quote(etag))

	f, err := h.fsys.Open(file)
	if err != nil {
		http.Error(w, "the panel build changed while running; restart dockwell", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	content, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "the panel build can't be served from this filesystem", http.StatusInternalServerError)
		return
	}
	// ServeContent handles If-None-Match, Range and HEAD. The zero modtime
	// leaves validation to the ETag.
	http.ServeContent(w, r, a.file, time.Time{}, content)
}

func setDocumentHeaders(h http.Header, csp string) {
	h.Set("Content-Security-Policy", csp)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
}

// isPageLoad reports whether a request for a missing path is a browser
// loading a page, which the panel's router should answer. Browsers always
// send text/html in Accept for a navigation; API clients and asset requests
// never do. Client routes may contain dots (/servers/web-1.example.com), so
// the path's extension decides nothing outside /assets/.
func isPageLoad(r *http.Request, name string) bool {
	return !strings.HasPrefix(name, assetDir) && strings.Contains(r.Header.Get("Accept"), "text/html")
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

// hashFile returns a short content hash, reading the file once without
// keeping it.
func hashFile(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), nil
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "404 page not found", http.StatusNotFound)
}

// notBuilt explains a binary built without the panel. It is a 404: the panel
// isn't here, but the API is.
func notBuilt(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", htmlType)
	h.Set("Cache-Control", "no-store")
	setDocumentHeaders(h, contentSecurityPolicy)
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, notBuiltPage)
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
