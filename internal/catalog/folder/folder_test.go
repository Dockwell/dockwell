// SPDX-License-Identifier: AGPL-3.0-only

package folder

import (
	"bytes"
	"strings"
	"testing"
)

// An image is what its first bytes say, WebP, PNG or JPEG alone, and its name's
// extension must say the same, so nothing a browser would run passes for one.
func TestImage(t *testing.T) {
	webp := []byte("RIFF\x10\x00\x00\x00WEBPVP8 ")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\r")
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01")
	for name, tc := range map[string]struct {
		file string
		head []byte
		want string
	}{
		"webp":        {"logo.webp", webp, "image/webp"},
		"png":         {"logo.png", png, "image/png"},
		"jpeg":        {"photo.jpeg", jpeg, "image/jpeg"},
		"jpg":         {"photo.jpg", jpeg, "image/jpeg"},
		"html":        {"logo.png", []byte("<html><script>"), "isn't a WebP, PNG or JPEG"},
		"svg":         {"logo.png", []byte("<svg onload=x>"), "isn't a WebP, PNG or JPEG"},
		"gif":         {"logo.png", []byte("GIF89a\x01\x00\x01\x00"), "isn't a WebP, PNG or JPEG"},
		"riff no web": {"logo.webp", []byte("RIFF\x10\x00\x00\x00WAVEfmt "), "isn't a WebP, PNG or JPEG"},
		"too short":   {"logo.webp", []byte("RIFF"), "isn't a WebP, PNG or JPEG"},
		"misnamed":    {"logo.webp", png, "it's a PNG image, named as a WEBP one. Name it .png"},
		"an SVG":      {"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x()</script><rect/></svg>`), "image/svg+xml"},
		"not an SVG":  {"logo.svg", png, "can't be shown as an SVG image"},
		"HTML as SVG": {"logo.svg", []byte("<html><script>x()</script></html>"), "can't be shown as an SVG image"},
	} {
		got, err := Image(tc.file, tc.head)
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// An entry names an image in its folder media by its name alone, one level down, in
// a raster format or SVG.
func TestMediaFile(t *testing.T) {
	for path, want := range map[string]string{
		"media/ghost-icon.webp": "ghost-icon.webp",
		"media/a_b-1.jpg":       "a_b-1.jpg",
		"media/logo.svg":        "logo.svg",
		"media/logo.SVG":        "",
		"media/sub/logo.png":    "",
		"media/../logo.png":     "",
		"/catalog-media/x.png":  "",
		"logo.png":              "",
		"media/Logo.png":        "",
		"media/.png":            "",
	} {
		got, ok := MediaFile(path)
		if ok != (want != "") || got != want && ok {
			t.Errorf("%q: %q, %t, want %q", path, got, ok, want)
		}
	}
}

// What's kept of an SVG image is what svgclean writes anew, never the file as it came,
// and a raster image is kept as it is.
func TestPrepare(t *testing.T) {
	kept, err := Prepare("logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="x()"><script>x()</script><rect width="1"/></svg>`))
	if err != nil || string(kept) != `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1"/></svg>` {
		t.Errorf("Prepare(logo.svg) = %q, %v", kept, err)
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if kept, err := Prepare("logo.png", png); err != nil || !bytes.Equal(kept, png) {
		t.Errorf("Prepare(logo.png) = %q, %v", kept, err)
	}
	if _, err := Prepare("logo.png", []byte("<svg/>")); err == nil {
		t.Error("Prepare took an SVG image named as a PNG one")
	}
}
