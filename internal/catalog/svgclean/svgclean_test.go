// SPDX-License-Identifier: AGPL-3.0-only

package svgclean

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

const head = `<?xml version="1.0" encoding="UTF-8"?>`

func svg(body string) string {
	return head + `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 24 24">` + body + `</svg>`
}

func TestCleanKeepsWhatDraws(t *testing.T) {
	in := svg(`<defs><linearGradient id="g" x1="0" x2="1"><stop offset="0" stop-color="#fff"/></linearGradient>` +
		`<clipPath id="c"><rect width="24" height="24" rx="4"/></clipPath></defs>` +
		`<style>.a{fill:url(#g)} @media (prefers-color-scheme: dark){.a{fill:#000}}</style>` +
		`<g clip-path="url(#c)"><path class="a" d="M0 0h24v24H0z" fill="url('#g')"/><use xlink:href="#p"/>` +
		`<text x="2" y="20" font-family="'Open Sans'" xml:space="preserve">Hi &amp; bye</text></g>` +
		`<path id="p" d="M1 1"/>`)
	out, err := Clean([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">`,
		`<linearGradient id="g" x1="0" x2="1">`,
		`<style>.a{fill:url(#g)} @media (prefers-color-scheme: dark){.a{fill:#000}}</style>`,
		`fill="url(&#39;#g&#39;)"`,
		`<use href="#p"/>`,
		`xml:space="preserve">Hi &amp; bye</text>`,
		`font-family="&#39;Open Sans&#39;"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("Clean left out %s, writing:\n%s", want, out)
		}
	}
}

func TestCleanLeavesOutWhatRunsOrLoads(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n0000"))
	svgData := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	cases := map[string]struct{ in, gone, kept string }{
		"a script":                {`<script>alert(1)</script><rect width="1"/>`, "script", "<rect"},
		"an empty script":         {`<script xmlns=""/><rect width="1"/>`, "script", "<rect"},
		"an event handler":        {`<rect width="1" onload="alert(1)" onclick="x()"/>`, "on", `width="1"`},
		"foreign content":         {`<foreignObject><div xmlns="http://www.w3.org/1999/xhtml">hi</div></foreignObject><rect/>`, "foreign", "<rect"},
		"animation":               {`<rect><set attributeName="href" to="javascript:alert(1)"/><animate/></rect>`, "set", "<rect"},
		"a link":                  {`<a href="javascript:alert(1)"><rect width="2"/></a>`, "javascript", `<g><rect width="2"/></g>`},
		"another file's element":  {`<use href="https://example.com/x.svg#a"/>`, "example.com", "<use/>"},
		"a scheme in a reference": {`<use xlink:href="javascript:alert(1)"/>`, "javascript", "<use/>"},
		"a file to draw":          {`<image href="https://example.com/a.png" width="4"/>`, "image", "<svg"},
		"an SVG to draw":          {`<image href="data:image/svg+xml;base64,` + svgData + `"/>`, "image", "<svg"},
		"a paint from a file":     {`<rect fill="url(https://example.com/a.svg#g)" width="3"/>`, "fill", `width="3"`},
		"a style that loads":      {`<rect style="background:url(https://example.com/a.png)" width="3"/>`, "style", `width="3"`},
		"an editor's markup":      {`<sodipodi:namedview xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"/><rect inkscape:label="x" xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape"/>`, "sodipodi", "<rect/>"},
		"a style sheet's PI":      {`<rect/>`, "stylesheet", "<rect/>"},
		"a raster image":          {`<image href="data:image/png;base64,` + png + `" width="4"/>`, "https", `<image href="data:image/png;base64,` + png + `" width="4"/>`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := svg(c.in)
			if name == "a style sheet's PI" {
				in = `<?xml-stylesheet href="https://example.com/a.css"?>` + in
			}
			out, err := Clean([]byte(in))
			if err != nil {
				t.Fatal(err)
			}
			body := strings.TrimPrefix(string(out), `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">`)
			if strings.Contains(strings.ToLower(body), c.gone) {
				t.Errorf("Clean kept %s:\n%s", c.gone, out)
			}
			if !strings.Contains(string(out), c.kept) {
				t.Errorf("Clean left out %s:\n%s", c.kept, out)
			}
		})
	}
}

func TestCleanRefuses(t *testing.T) {
	var uses strings.Builder
	uses.WriteString(`<g id="a0"><path d="M0 0"/></g>`)
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&uses, `<g id="a%d">%s</g>`, i, strings.Repeat(fmt.Sprintf(`<use href="#a%d"/>`, i-1), 10))
	}
	use := uses.String()
	deep := strings.Repeat("<g>", MaxDepth+1) + strings.Repeat("</g>", MaxDepth+1)
	many := strings.Repeat("<desc/>", MaxElements)
	cases := map[string]string{
		"not XML":            `<svg xmlns="http://www.w3.org/2000/svg"><rect></svg>`,
		"not an SVG image":   `<html><body/></html>`,
		"entities":           `<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;">]><svg xmlns="http://www.w3.org/2000/svg"><text>&b;</text></svg>`,
		"an unknown entity":  `<svg xmlns="http://www.w3.org/2000/svg"><text>&nbsp;</text></svg>`,
		"another encoding":   `<?xml version="1.0" encoding="ISO-8859-1"?><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"an @import":         svg(`<style>@import url(https://example.com/a.css);</style>`),
		"a font to load":     svg(`<style>@font-face{font-family:x;src:url(https://example.com/f.woff)}</style>`),
		"a url in a sheet":   svg(`<style>.a{fill:url(https://example.com/a.svg#g)}</style>`),
		"an escape":          svg(`<style>.a{fill:\75 rl(https://example.com)}</style>`),
		"an image-set":       svg(`<style>.a{background:image-set("https://example.com/a.png" 1x)}</style>`),
		"uses of uses":       svg(`<defs>` + use + `</defs><use href="#a6"/>`),
		"a use of itself":    svg(`<g id="loop"><use href="#loop"/></g>`),
		"too deep":           svg(deep),
		"too many elements":  svg(many),
		"no end":             `<svg xmlns="http://www.w3.org/2000/svg"><rect/>`,
		"nothing":            ``,
		"another namespace":  `<svg xmlns="http://example.com/not-svg"/>`,
		"a root that isn't":  `<g xmlns="http://www.w3.org/2000/svg"/>`,
		"a control in a CSS": svg("<style>.a{fill:red}</style><rect style=\"fill:red\x01\"/>") + "\x00",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if out, err := Clean([]byte(in)); err == nil {
				t.Errorf("Clean took it, writing:\n%.300s", out)
			}
		})
	}
}

func TestCleanTakesAFileThatNeverDeclaresItsNamespace(t *testing.T) {
	out, err := Clean([]byte("\xef\xbb\xbf" + `<svg viewBox="0 0 2 2"><rect width="2" height="2"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 2 2"><rect width="2" height="2"/></svg>`; string(out) != want {
		t.Errorf("Clean wrote\n%s\nwant\n%s", out, want)
	}
}

func TestCleanIsStable(t *testing.T) {
	in := svg(`<title>A logo, &lt;made&gt;</title><style></style><text>a
b	c</text><g><rect fill="#fff"/></g>`)
	once, err := Clean([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Clean(once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("Clean of what it wrote changed it:\n%s\n%s", once, twice)
	}
}

// FuzzClean holds what Clean writes to what a browser reads as an image and
// nothing more: XML that parses, from the allowlist alone, the same when cleaned
// again.
func FuzzClean(f *testing.F) {
	for _, seed := range []string{
		svg(`<rect width="1" onload="x()"/><script>alert(1)</script>`),
		svg(`<style>.a{fill:url(#g)}</style><use href="#a"/><g id="a"><path d="M0 0"/></g>`),
		svg(`<text xml:space="preserve">a &amp; b</text><a href="https://example.com"><circle r="1"/></a>`),
		`<svg><image href="data:image/png;base64,iVBORw0KGgowMDAw"/></svg>`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := Clean(in)
		if err != nil {
			return
		}
		again, err := Clean(out)
		if err != nil {
			t.Fatalf("Clean refused what it wrote: %v\n%s", err, out)
		}
		if !bytes.Equal(out, again) {
			t.Fatalf("Clean of what it wrote changed it:\n%s\n%s", out, again)
		}
		d := xml.NewDecoder(bytes.NewReader(out))
		d.Strict = true
		for {
			tok, err := d.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("what Clean wrote isn't XML: %v\n%s", err, out)
			}
			start, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if !elements[start.Name.Local] {
				t.Fatalf("Clean kept the element %s:\n%s", start.Name.Local, out)
			}
			for _, a := range start.Attr {
				name := a.Name.Local
				switch {
				case a.Name.Space == "" && name == "xmlns", a.Name.Space == xmlNS:
				case a.Name.Space != "":
					t.Fatalf("Clean kept the attribute %s:%s:\n%s", a.Name.Space, name, out)
				case name == "href":
					if !fragment.MatchString(a.Value) && !rasterImage(a.Value) {
						t.Fatalf("Clean kept the href %q:\n%s", a.Value, out)
					}
				case !attributes[name]:
					t.Fatalf("Clean kept the attribute %s:\n%s", name, out)
				case strings.Contains(strings.ToLower(a.Value), "javascript:"):
					t.Fatalf("Clean kept %q:\n%s", a.Value, out)
				}
			}
		}
	})
}
