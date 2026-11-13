// SPDX-License-Identifier: AGPL-3.0-only

package folder

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"strings"
	"testing"
)

// onePixel is a whole PNG image of one pixel.
const onePixel = "\x89\x50\x4e\x47\x0d\x0a\x1a\x0a\x00\x00\x00\x0d\x49\x48\x44\x52\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\x0b\x49\x44\x41\x54\x78\x9c\x63\x60\x00\x02\x00\x00\x05\x00\x01\x7a\x5e\xab\x3f\x00\x00\x00\x00\x49\x45\x4e\x44\xae\x42\x60\x82"

// pngSaying is the start of a PNG image whose header says it's w by h pixels, all a
// reading of its size needs.
func pngSaying(w, h uint32) []byte {
	ihdr := append([]byte("IHDR"), binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, w), h)...)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	out := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 13)
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}

// A raster image is as large as its header says, and a browser decodes it whole, so
// a small file that says 30000 by 30000 pixels is refused: 4096 a side and 16 MP at
// most, and a logo 1024 a side (the review of #589, M3).
func TestImageBoundsItsPixels(t *testing.T) {
	for name, tc := range map[string]struct {
		w, h uint32
		want string
	}{
		"one pixel":       {1, 1, ""},
		"a wide screen":   {3840, 2160, ""},
		"16 MP":           {4000, 4000, ""},
		"a side too long": {30000, 30000, "over the 4096 a side"},
		"a long strip":    {4097, 1, "over the 4096 a side"},
		"too many pixels": {4096, 4096, "over the 16 MP"},
		"no pixels":       {0, 10, "Save it again"},
	} {
		_, err := Image("shot.png", pngSaying(tc.w, tc.h))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if err := Within("logo.png", pngSaying(1024, 1024), LogoPixels); err != nil {
		t.Errorf("a logo of 1024 pixels: %v", err)
	}
	if err := Within("logo.png", pngSaying(2000, 2000), LogoPixels); err == nil || !strings.Contains(err.Error(), "over the 1024 a side") {
		t.Errorf("a logo of 2000 pixels: %v", err)
	}
	if _, err := Image("shot.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")); err == nil {
		t.Error("an image whose size can't be read passes")
	}
	if _, err := Prepare("shot.png", pngSaying(30000, 30000)); err == nil {
		t.Error("Prepare keeps an image of 30000 by 30000 pixels")
	}
}

// An image is what its first bytes say, WebP, PNG or JPEG alone, and its name's
// extension must say the same, so nothing a browser would run passes for one.
func TestSniff(t *testing.T) {
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
		got, err := Sniff(tc.file, tc.head)
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
	png := []byte(onePixel)
	if kept, err := Prepare("logo.png", png); err != nil || !bytes.Equal(kept, png) {
		t.Errorf("Prepare(logo.png) = %q, %v", kept, err)
	}
	if _, err := Prepare("logo.png", []byte("<svg/>")); err == nil {
		t.Error("Prepare took an SVG image named as a PNG one")
	}
}

// An entry whose Compose file wasn't read as the read was cut short, or the reader
// busy, makes the check unsettled, so nobody keeps what it says of that entry.
func TestACheckCutShortIsUnsettled(t *testing.T) {
	own, err := os.ReadFile("../snapshot/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := Check(ctx, "community", map[string][]byte{"memos.yaml": own}, map[string]bool{}, map[string]Taken{}, Options{})
	if !got.Unsettled || len(got.Entries) != 0 {
		t.Errorf("a check cut short: unsettled %t, %d entries", got.Unsettled, len(got.Entries))
	}
	if got := Check(t.Context(), "community", map[string][]byte{"memos.yaml": own}, map[string]bool{}, map[string]Taken{}, Options{}); got.Unsettled {
		t.Errorf("a whole check is unsettled: %v", got.Refused)
	}
}
