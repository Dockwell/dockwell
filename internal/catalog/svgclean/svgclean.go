// SPDX-License-Identifier: AGPL-3.0-only

// Package svgclean makes an SVG image from a stranger's catalog one the control
// plane can serve as an image: a new document, written from an allowlist of what
// draws, with nothing in it that runs, links, loads another file or multiplies
// itself. Nothing of the file reaches what's written but the names of elements and
// attributes on the allowlist, and their values once checked, so a construct the
// list doesn't know is left out rather than passed on. See
// docs/adr/0083-a-stores-own-logos-and-words-and-stores-one-click-away.md and
// docs/security/catalog-sources.md.
package svgclean

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Bounds on what an image may hold, beside the size of its file, which the caller
// bounds. The largest logo of the three stores Dockwell reads, on 12 Nov 2026, had
// 2,400 elements, nested 14 deep.
const (
	// MaxElements is the most elements an image may have, those left out included.
	MaxElements = 10_000
	// MaxDepth is how deep its elements may nest.
	MaxDepth = 64
	// MaxDrawn is the most elements it may draw once each reference stands for what it
	// names, each time it's named: a <use>, and a mask, clip path, filter or paint
	// named by url(#id). References to references draw exponentially often: four
	// levels of masks of 4.7 KB blocked Chromium's main thread for seconds. The most
	// any of the stores' 538 logos that pass drew so on 13 Nov 2026 was 1,110.
	MaxDrawn = 4_000
)

const (
	svgNS   = "http://www.w3.org/2000/svg"
	xlinkNS = "http://www.w3.org/1999/xlink"
	xmlNS   = "http://www.w3.org/XML/1998/namespace"
)

// elements are those an image keeps: shapes, text, gradients, clipping, masking and
// the filter primitives that draw. Animation, scripts, links, foreign content, fonts
// and anything else are left out with what they hold, but a link's drawing, which is
// kept as a group. A marker, drawn at every point of a path, and a pattern, drawn
// again in every tile it fills, multiply what they hold in a way no count of
// elements bounds, so they go too, as no store's logo needs either.
var elements = set(
	"svg", "g", "defs", "symbol", "use", "title", "desc", "style",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan", "textPath",
	"linearGradient", "radialGradient", "stop",
	"clipPath", "mask", "image",
	"filter", "feBlend", "feColorMatrix", "feComponentTransfer", "feComposite", "feDropShadow",
	"feFlood", "feFuncA", "feFuncB", "feFuncG", "feFuncR", "feGaussianBlur", "feMerge",
	"feMergeNode", "feMorphology", "feOffset",
)

// texts are the elements whose text is kept: what text draws, an image's title and
// description, and a stylesheet.
var texts = set("text", "tspan", "textPath", "title", "desc", "style")

// attributes are those an element keeps, unprefixed, whichever element has them:
// what places, sizes, paints, clips and filters a shape, and the names a style
// sheet or a reference finds it by. An attribute an element doesn't use draws
// nothing. Event handlers, and anything else, are left out.
var attributes = set(
	"id", "class", "style", "transform", "viewBox", "preserveAspectRatio", "version", "baseProfile",
	"width", "height", "x", "y", "x1", "y1", "x2", "y2", "cx", "cy", "r", "rx", "ry", "fx", "fy", "fr",
	"d", "points", "pathLength",
	"fill", "fill-opacity", "fill-rule", "stroke", "stroke-width", "stroke-linecap", "stroke-linejoin",
	"stroke-miterlimit", "stroke-dasharray", "stroke-dashoffset", "stroke-opacity", "opacity", "color",
	"display", "visibility", "overflow", "clip-path", "clip-rule", "clipPathUnits", "mask", "maskUnits",
	"maskContentUnits", "filter", "filterUnits", "primitiveUnits", "gradientUnits", "gradientTransform",
	"spreadMethod", "offset", "stop-color", "stop-opacity", "refX", "refY",
	"font-family", "font-size", "font-weight", "font-style", "font-variant", "font-stretch",
	"text-anchor", "dominant-baseline", "alignment-baseline", "baseline-shift", "letter-spacing",
	"word-spacing", "text-decoration", "dx", "dy", "rotate", "textLength", "lengthAdjust", "startOffset",
	"method", "spacing", "side", "writing-mode", "direction", "unicode-bidi",
	"isolation", "mix-blend-mode", "paint-order", "vector-effect", "shape-rendering", "text-rendering",
	"image-rendering", "color-interpolation", "color-interpolation-filters", "flood-color",
	"flood-opacity", "lighting-color", "enable-background",
	"in", "in2", "result", "stdDeviation", "edgeMode", "mode", "operator", "k1", "k2", "k3", "k4",
	"type", "values", "tableValues", "slope", "intercept", "amplitude", "exponent", "radius",
)

// referring are the elements an href is kept on, as a reference to another element
// of the image, and image, as a raster image written into the file.
var referring = set("use", "textPath", "linearGradient", "radialGradient", "filter", "image")

func set(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// fragment is a reference to an element of the same image, by its ID.
var fragment = regexp.MustCompile(`^#[A-Za-z_][A-Za-z0-9_.:-]{0,255}$`)

// rasterData is a raster image written into the file, which an image may draw.
var rasterData = regexp.MustCompile(`^data:image/(png|jpeg|webp);base64,([A-Za-z0-9+/=\s]+)$`)

// urlRef is a CSS url() that names an element of the image, with or without quotes.
var urlRef = regexp.MustCompile(`^url\(\s*(['"]?)#[A-Za-z_][A-Za-z0-9_.:-]*(['"]?)\s*\)`)

// node is an element kept, or text, in the document written out.
type node struct {
	name  string
	attrs []xml.Attr
	kids  []*node
	text  string
	leaf  bool // it's text
}

// Clean reads an SVG image and writes it anew from what's on the allowlist, or says
// why it can't be shown at all: it isn't XML, or isn't an SVG image, declares
// entities, has a style sheet that could load something, or holds more than the
// bounds above. What's left out of a file that can be shown is left out silently, as
// it never drew anything a browser shows an image with. Clean of what Clean wrote
// gives the same bytes.
func Clean(data []byte) ([]byte, error) {
	// A byte order mark is UTF-8's own, and says nothing an image needs.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	// Entities other than XML's own aren't expanded, and a declared encoding other
	// than UTF-8 isn't read: both leave the file refused.
	var (
		root     *node
		stack    []*node
		elements int
		done     bool
	)
	for !done {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("it isn't XML a browser reads as an image: %w", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			if bytes.Contains(bytes.ToUpper(t), []byte("ENTITY")) {
				return nil, errors.New("it declares entities, which an image doesn't need")
			}
		case xml.StartElement:
			elements++
			if elements > MaxElements {
				return nil, fmt.Errorf("it has over %d elements, more than a logo needs", MaxElements)
			}
			if len(stack) >= MaxDepth {
				return nil, fmt.Errorf("it nests over %d levels deep", MaxDepth)
			}
			if root == nil {
				if !svgElement(t.Name) || t.Name.Local != "svg" {
					return nil, errors.New("its root isn't an SVG image's svg element")
				}
			}
			n, keep := element(t)
			if !keep {
				if err := skip(d, &elements); err != nil {
					return nil, err
				}
				continue
			}
			if root == nil {
				root = n
			} else {
				parent := stack[len(stack)-1]
				parent.kids = append(parent.kids, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n.name == "style" {
				if err := styleSheet(n); err != nil {
					return nil, err
				}
			}
			if len(stack) == 0 {
				done = true
			}
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			parent := stack[len(stack)-1]
			if texts[parent.name] {
				parent.kids = append(parent.kids, &node{leaf: true, text: string(t)})
			}
		}
	}
	if root == nil {
		return nil, errors.New("it has no svg element")
	}
	if !done {
		return nil, errors.New("its svg element never ends")
	}
	if err := sheetsPaintOnly(root); err != nil {
		return nil, err
	}
	if err := drawnWithin(root); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	write(&out, root, true)
	return out.Bytes(), nil
}

// svgElement says an element's name is in SVG's namespace, or in none, as a file
// that never declares it is read by a browser that's told it's SVG.
func svgElement(name xml.Name) bool {
	return name.Space == svgNS || name.Space == ""
}

// element is an element as it's kept, with its attributes checked, and whether it's
// kept at all. A link is kept as a group, without where it leads.
func element(t xml.StartElement) (*node, bool) {
	if !svgElement(t.Name) {
		return nil, false
	}
	name := t.Name.Local
	if name == "a" {
		name = "g"
	}
	if !elements[name] {
		return nil, false
	}
	n := &node{name: name}
	// The first of each name is kept: href and xlink:href both become href, and an
	// element with an attribute twice is one a browser won't draw.
	seen := map[xml.Name]bool{}
	for _, a := range t.Attr {
		kept, ok := attribute(name, a)
		if ok && !seen[kept.Name] {
			seen[kept.Name] = true
			n.attrs = append(n.attrs, kept)
		}
	}
	// An image draws only a raster image written into the file, never a file of its
	// own or another host's.
	if name == "image" && !hasAttr(n, "href") {
		return nil, false
	}
	return n, true
}

func hasAttr(n *node, local string) bool {
	for _, a := range n.attrs {
		if a.Name.Space == "" && a.Name.Local == local {
			return true
		}
	}
	return false
}

// attribute is an attribute as it's kept, and whether it is: an unprefixed one on
// the allowlist, with a value that loads nothing; an href, in either namespace, that
// names an element of the image, or a raster image written into it; and xml:space
// and xml:lang.
func attribute(element string, a xml.Attr) (xml.Attr, bool) {
	value := a.Value
	if !plainText(value) {
		return xml.Attr{}, false
	}
	switch {
	case a.Name.Space == "" && a.Name.Local == "href", (a.Name.Space == xlinkNS || a.Name.Space == "xlink") && a.Name.Local == "href":
		if !referring[element] {
			return xml.Attr{}, false
		}
		v := strings.TrimSpace(value)
		if element == "image" {
			if !rasterImage(v) {
				return xml.Attr{}, false
			}
			return xml.Attr{Name: xml.Name{Local: "href"}, Value: strings.Join(strings.Fields(v), "")}, true
		}
		if !fragment.MatchString(v) {
			return xml.Attr{}, false
		}
		return xml.Attr{Name: xml.Name{Local: "href"}, Value: v}, true
	case (a.Name.Space == xmlNS || a.Name.Space == "xml") && (a.Name.Local == "space" || a.Name.Local == "lang"):
		return xml.Attr{Name: xml.Name{Space: "xml", Local: a.Name.Local}, Value: value}, true
	case a.Name.Space != "":
		return xml.Attr{}, false
	case !attributes[a.Name.Local]:
		return xml.Attr{}, false
	case a.Name.Local == "style":
		if safeCSS(value) != nil {
			return xml.Attr{}, false
		}
	case !inertValue(value):
		return xml.Attr{}, false
	}
	return xml.Attr{Name: xml.Name{Local: a.Name.Local}, Value: value}, true
}

// plainText says a value has no control character but tab and the line breaks.
func plainText(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0x7f {
			return false
		}
	}
	return true
}

// rasterImage says a data URL is a PNG, JPEG or WebP image, whose bytes say so.
func rasterImage(v string) bool {
	m := rasterData.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m[2]), ""))
	if err != nil {
		return false
	}
	switch m[1] {
	case "png":
		return bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n"))
	case "jpeg":
		return bytes.HasPrefix(raw, []byte{0xff, 0xd8, 0xff})
	default:
		return len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP"))
	}
}

// inertValue says an attribute's value names nothing outside the image: every
// url() in it names an element of the image, and it names no scheme.
func inertValue(v string) bool {
	lower := strings.ToLower(v)
	for _, bad := range []string{"javascript:", "vbscript:", "data:", "expression(", "\\"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return urlsInside(lower)
}

// urlsInside says each url( in a lowercased value names an element of the image.
func urlsInside(lower string) bool {
	for i := strings.Index(lower, "url("); i >= 0; {
		if !urlRef.MatchString(lower[i:]) {
			return false
		}
		next := strings.Index(lower[i+4:], "url(")
		if next < 0 {
			break
		}
		i += 4 + next
	}
	return true
}

// cssFetches are what CSS loads another file with, or runs, beside url().
var cssFetches = []string{
	"@", "\\", "<", "expression", "javascript:", "vbscript:", "behavior", "-moz-binding",
	"image-set", "image(", "cross-fade", "element(", "src(", "data:",
}

// safeCSS says what's wrong with a style sheet or a style attribute, if anything: an
// at-rule other than @media, an escape, which could spell any of the rest, a
// function that loads a file, or a url() that names anything but an element of the
// image.
func safeCSS(css string) error {
	lower := strings.ToLower(css)
	for _, bad := range cssFetches {
		if bad == "@" {
			for i := strings.Index(lower, "@"); i >= 0; {
				if !strings.HasPrefix(lower[i:], "@media") {
					return errors.New("its style sheet has an at-rule other than @media, which could load a file")
				}
				next := strings.Index(lower[i+1:], "@")
				if next < 0 {
					break
				}
				i += 1 + next
			}
			continue
		}
		if strings.Contains(lower, bad) {
			return fmt.Errorf("its style sheet has %q, which could load a file or run", bad)
		}
	}
	if !urlsInside(lower) {
		return errors.New("its style sheet names a file by url(), where an image's styles name only its own elements")
	}
	return nil
}

// styleSheet checks a style element's text as one style sheet, and keeps it as one
// piece of text, with no attributes, which a browser reads as CSS.
func styleSheet(n *node) error {
	var css strings.Builder
	for _, k := range n.kids {
		if k.leaf {
			css.WriteString(k.text)
		}
	}
	if err := safeCSS(css.String()); err != nil {
		return err
	}
	n.attrs = nil
	n.kids = []*node{{leaf: true, text: css.String()}}
	return nil
}

// skip reads past the element just started, and everything inside it, counting its
// elements against MaxElements.
func skip(d *xml.Decoder, elements *int) error {
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("it isn't XML a browser reads as an image: %w", err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
			*elements++
			if *elements > MaxElements {
				return fmt.Errorf("it has over %d elements, more than a logo needs", MaxElements)
			}
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// refID is the ID each url(#id) in a value names, as a reference to an element of
// the image.
var refID = regexp.MustCompile(`url\(\s*['"]?#([A-Za-z_][A-Za-z0-9_.:-]*)['"]?\s*\)`)

// gradients are the elements a style sheet may name: a paint whose cost is the area
// it fills, whatever names it.
var gradients = set("linearGradient", "radialGradient")

// indexIDs maps each ID to the first element that has it, as a browser finds it.
func indexIDs(root *node) map[string]*node {
	ids := map[string]*node{}
	var index func(n *node)
	index = func(n *node) {
		for _, a := range n.attrs {
			if a.Name.Space == "" && a.Name.Local == "id" {
				if _, taken := ids[a.Value]; !taken {
					ids[a.Value] = n
				}
			}
		}
		for _, k := range n.kids {
			index(k)
		}
	}
	index(root)
	return ids
}

// sheetsPaintOnly refuses a style sheet that names anything but a gradient by url():
// a sheet's rules reach elements by selectors, so what a mask or a clip path it names
// would multiply can't be counted, where a gradient only paints.
func sheetsPaintOnly(root *node) error {
	ids := indexIDs(root)
	var walk func(n *node) error
	walk = func(n *node) error {
		if n.name == "style" {
			for _, k := range n.kids {
				for _, m := range refID.FindAllStringSubmatch(k.text, -1) {
					if target := ids[m[1]]; target == nil || !gradients[target.name] {
						return errors.New("its style sheet names something other than a gradient by url(), whose cost no count of the image can bound")
					}
				}
			}
		}
		for _, k := range n.kids {
			if err := walk(k); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}

// references are the elements a node names, each time it names one: a <use>'s, a
// gradient's or a filter's href, and every url(#id) in its attributes, its style
// attribute included, as fill, stroke, mask, clip-path and filter name them.
func references(n *node, ids map[string]*node) []*node {
	var out []*node
	for _, a := range n.attrs {
		if a.Name.Space != "" {
			continue
		}
		if a.Name.Local == "href" {
			if target := ids[strings.TrimPrefix(a.Value, "#")]; target != nil {
				out = append(out, target)
			}
			continue
		}
		for _, m := range refID.FindAllStringSubmatch(a.Value, -1) {
			if target := ids[m[1]]; target != nil {
				out = append(out, target)
			}
		}
	}
	return out
}

// drawnWithin says the image draws no more than MaxDrawn elements once each reference
// stands for the element it names, each time it names it, and that nothing names
// itself, through others. Masks of masks, clip paths of clip paths and uses of uses
// each draw what they hold as often as they're named, so they're counted so.
func drawnWithin(root *node) error {
	ids := indexIDs(root)
	const (
		visiting = 1
		visited  = 2
	)
	state := map[*node]int{}
	drawn := map[*node]int{}
	tooMany := fmt.Errorf("it draws over %d elements once its references are counted, more than a logo needs", MaxDrawn)
	var count func(n *node) (int, error)
	count = func(n *node) (int, error) {
		switch state[n] {
		case visiting:
			return 0, errors.New("an element of it names itself, through others")
		case visited:
			return drawn[n], nil
		}
		state[n] = visiting
		total := 1
		for _, k := range n.kids {
			if k.leaf {
				continue
			}
			c, err := count(k)
			if err != nil {
				return 0, err
			}
			if total += c; total > MaxDrawn {
				return 0, tooMany
			}
		}
		for _, target := range references(n, ids) {
			c, err := count(target)
			if err != nil {
				return 0, err
			}
			if total += c; total > MaxDrawn {
				return 0, tooMany
			}
		}
		state[n], drawn[n] = visited, total
		return total, nil
	}
	_, err := count(root)
	return err
}

// write writes an element and what's inside it, escaped, the root declaring SVG's
// namespace.
func write(w *bytes.Buffer, n *node, root bool) {
	if n.leaf {
		_ = xml.EscapeText(w, []byte(n.text))
		return
	}
	w.WriteString("<")
	w.WriteString(n.name)
	if root {
		w.WriteString(` xmlns="` + svgNS + `"`)
	}
	for _, a := range n.attrs {
		w.WriteString(" ")
		if a.Name.Space != "" {
			w.WriteString(a.Name.Space)
			w.WriteString(":")
		}
		w.WriteString(a.Name.Local)
		w.WriteString(`="`)
		_ = xml.EscapeText(w, []byte(a.Value))
		w.WriteString(`"`)
	}
	if len(n.kids) == 0 {
		w.WriteString("/>")
		return
	}
	w.WriteString(">")
	for _, k := range n.kids {
		write(w, k, false)
	}
	w.WriteString("</")
	w.WriteString(n.name)
	w.WriteString(">")
}
