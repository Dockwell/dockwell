// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"fmt"
	htmlesc "html"
	"net/http"
	"slices"
	"strings"
)

// brand are the tokens an end-state page draws with, light then dark, as
// design/system/tokens.json has them; a test holds them to it.
var brand = map[string][2]string{
	"canvas":    {"#f3f4f1", "#0f1110"},
	"surface":   {"#ffffff", "#161917"},
	"line":      {"#dfe2dd", "#272c29"},
	"ink":       {"#191c1a", "#e8ece8"},
	"ink-muted": {"#575e58", "#9aa39c"},
	"action":    {"#191c1a", "#e8ece8"},
	"on-action": {"#ffffff", "#121413"},
	"critical":  {"#c0390e", "#ff7d5a"},
	"cobalt":    {"#2450d6", "#809dff"},
}

// fonts are the families tokens.json names, sans then mono.
const fonts = `"Host Grotesk", "PingFang SC", "Microsoft YaHei", "Noto Sans SC", system-ui, sans-serif`

// style is the page's CSS: the tokens as properties, each theme's, then the card.
func style() string {
	var light, dark strings.Builder
	names := make([]string, 0, len(brand))
	for name := range brand {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&light, "--%s:%s;", name, brand[name][0])
		fmt.Fprintf(&dark, "--%s:%s;", name, brand[name][1])
	}
	css := ":root{" + light.String() + "color-scheme:light dark}" +
		"@media (prefers-color-scheme:dark){:root{" + dark.String() + "}}" +
		"body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--canvas);color:var(--ink);font:15px/1.5 " + fonts + "}" +
		"main{box-sizing:border-box;width:min(420px,calc(100vw - 32px));padding:24px;background:var(--surface);border:1px solid var(--line);border-radius:14px}" +
		"h1{margin:0 0 4px;font-size:18px;font-weight:600;overflow-wrap:anywhere}" +
		"p{margin:0 0 20px;color:var(--ink-muted);overflow-wrap:anywhere}" +
		"main.down h1::before{content:'';display:inline-block;width:9px;height:9px;margin-right:8px;background:var(--critical);vertical-align:middle}" +
		"a.act{display:inline-block;padding:8px 14px;border-radius:6px;background:var(--action);color:var(--on-action);text-decoration:none;font-weight:600}" +
		"a.act:focus-visible{outline:2px solid var(--cobalt);outline-offset:2px}"
	return css
}

// end is a page that ends a sign-in: a state, a line at most, and the action that
// resolves it.
type end struct {
	Status       int
	Title, Said  string
	Action, Href string
	Tone         string
}

// write sends the page, which no cache keeps. It's written with html.EscapeString,
// not html/template, which finds methods by name as it runs, so the linker would
// keep every exported method the agent build has (#622). Its link stays on this
// site.
func (e end) write(w http.ResponseWriter) {
	if e.Tone == "" {
		e.Tone = "down"
	}
	if !strings.HasPrefix(e.Href, "/") || strings.HasPrefix(e.Href, "//") || strings.HasPrefix(e.Href, "/\\") {
		e.Href = "/"
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(e.Status)
	title := htmlesc.EscapeString(e.Title)
	var b strings.Builder
	b.WriteString("<!doctype html>\n" +
		`<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` + "\n" +
		`<meta name="robots" content="noindex"><title>` + title + "</title><style>" + style() + "</style></head>\n" +
		`<body><main class="` + htmlesc.EscapeString(e.Tone) + `"><h1>` + title + "</h1>")
	if e.Said != "" {
		b.WriteString("<p>" + htmlesc.EscapeString(e.Said) + "</p>")
	}
	b.WriteString(`<a class="act" href="` + htmlesc.EscapeString(e.Href) + `">` + htmlesc.EscapeString(e.Action) + "</a></main></body></html>\n")
	_, _ = w.Write([]byte(b.String()))
}
