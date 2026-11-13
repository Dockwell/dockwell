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
			seen := map[xml.Name]bool{}
			for _, a := range start.Attr {
				if seen[a.Name] {
					t.Fatalf("Clean kept %s twice on an element:\n%s", a.Name.Local, out)
				}
				seen[a.Name] = true
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

// levels writes a document of depth levels, each of width elements that each name the
// level below by the reference ref writes, such as a mask or a clip path, and a rect
// at the top that names the deepest.
func levels(kind, attr string, width, depth int) string {
	var s strings.Builder
	s.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><defs>`)
	for l := 0; l <= depth; l++ {
		fmt.Fprintf(&s, `<%s id="k%d">`, kind, l)
		for i := range width {
			ref := ""
			if l > 0 {
				ref = fmt.Sprintf(` %s="url(#k%d)"`, attr, l-1)
			}
			fmt.Fprintf(&s, `<rect x="%d" width="50" height="50" fill="#fff"%s/>`, i, ref)
		}
		fmt.Fprintf(&s, `</%s>`, kind)
	}
	fmt.Fprintf(&s, `</defs><rect width="100" height="100" %s="url(#k%d)"/></svg>`, attr, depth)
	return s.String()
}

// What a reference names is drawn each time it's named, so masks of masks and clip
// paths of clip paths count as uses of uses do, and four levels of each, a few KiB,
// are refused. Two levels draw as they did.
func TestCleanCountsEveryReference(t *testing.T) {
	for _, c := range []struct{ kind, attr string }{{"mask", "mask"}, {"clipPath", "clip-path"}} {
		if out, err := Clean([]byte(levels(c.kind, c.attr, 8, 4))); err == nil {
			t.Errorf("four levels of %s were kept:\n%.300s", c.kind, out)
		}
		if _, err := Clean([]byte(levels(c.kind, c.attr, 8, 2))); err != nil {
			t.Errorf("two levels of %s were refused: %v", c.kind, err)
		}
	}
	// A reference in a style attribute counts as one in its own attribute does.
	styled := strings.ReplaceAll(levels("mask", "mask", 8, 4), `mask="url(`, `style="mask:url(`)
	if _, err := Clean([]byte(styled)); err == nil {
		t.Error("four levels of masks named in style attributes were kept")
	}
	cycle := svg(`<clipPath id="c"><rect clip-path="url(#c)" width="1"/></clipPath><rect clip-path="url(#c)"/>`)
	if _, err := Clean([]byte(cycle)); err == nil {
		t.Error("a clip path that names itself was kept")
	}
}

// A marker draws at every point of a path, and nests multiplicatively, and a pattern
// draws again in each tile it fills: neither is kept, nor what names them, so nested
// markers draw nothing at all.
func TestCleanFlattensMarkersAndPatterns(t *testing.T) {
	var s strings.Builder
	s.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><defs>`)
	for l := range 4 {
		ref := ""
		if l > 0 {
			ref = fmt.Sprintf(` marker-start="url(#m%d)" marker-mid="url(#m%d)" marker-end="url(#m%d)"`, l-1, l-1, l-1)
		}
		fmt.Fprintf(&s, `<marker id="m%d" markerWidth="10" markerHeight="10" orient="auto"><path d="M0 0L1 1L2 0L3 1L4 0L5 1L6 0L7 1"%s/></marker>`, l, ref)
	}
	s.WriteString(`<pattern id="p" width="1" height="1"><rect width="1" height="1"/></pattern></defs>`)
	s.WriteString(`<path d="M0 0L10 10L20 0L30 10" marker-mid="url(#m3)"/><rect width="9" height="9" fill="url(#p)"/></svg>`)
	out, err := Clean([]byte(s.String()))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"marker", "pattern", "orient"} {
		if strings.Contains(string(out), gone) {
			t.Errorf("Clean kept %s:\n%s", gone, out)
		}
	}
}

// A style sheet reaches elements by selectors, so what it names can't be counted: it
// may name a gradient, and nothing else.
func TestCleanKeepsStyleSheetsToGradients(t *testing.T) {
	gradient := svg(`<linearGradient id="g"><stop offset="0"/></linearGradient><style>.a{fill:url(#g)}</style><rect class="a"/>`)
	if _, err := Clean([]byte(gradient)); err != nil {
		t.Errorf("a style sheet that names a gradient was refused: %v", err)
	}
	for name, body := range map[string]string{
		"a mask":       `<mask id="m"><rect/></mask><style>.a{mask:url(#m)}</style><rect class="a"/>`,
		"a clip path":  `<clipPath id="c"><rect/></clipPath><style>.a{clip-path:url(#c)}</style><rect class="a"/>`,
		"nothing":      `<style>.a{fill:url(#missing)}</style><rect class="a"/>`,
		"a later mask": `<style>.a{mask:url(#m)}</style><rect class="a"/><mask id="m"><rect/></mask>`,
	} {
		if out, err := Clean([]byte(svg(body))); err == nil {
			t.Errorf("a style sheet that names %s was kept:\n%s", name, out)
		}
	}
}

// href and xlink:href both become href, and an element with an attribute twice is one
// a browser won't draw, so the first is kept.
func TestCleanKeepsTheFirstOfAnAttribute(t *testing.T) {
	out, err := Clean([]byte(svg(`<g id="a"/><g id="b"/><use href="#a" xlink:href="#b"/>`)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `<use href="#a"/>`) {
		t.Errorf("Clean wrote:\n%s", out)
	}
}
