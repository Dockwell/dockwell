// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/gitfetch"
)

// A commit read once is kept, as a commit never changes, and read again from where
// it's kept, by another run too.
func TestASourcesCommitIsReadOnce(t *testing.T) {
	asked := 0
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		asked++
		return map[string][]byte{"notes.yaml": []byte("id: notes\n")}, nil
	}
	dir := t.TempDir()
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Path: "entries", Commit: "0123456789abcdef0123456789abcdef01234567"}
	for range 2 {
		s := &Sources{Dir: dir, Fetch: fetch}
		got, err := s.files(t.Context(), spec)
		if err != nil || string(got.entries["notes.yaml"]) != "id: notes\n" {
			t.Fatalf("files = %q, %v", got.entries, err)
		}
	}
	if asked != 1 {
		t.Errorf("the commit was fetched %d times", asked)
	}
	other := &resourcev1.CatalogSource{Repository: spec.GetRepository(), Path: spec.GetPath(), Commit: "fedcba9876543210fedcba9876543210fedcba98"}
	if _, err := (&Sources{Dir: dir, Fetch: fetch}).files(t.Context(), other); err != nil || asked != 2 {
		t.Errorf("another commit wasn't fetched: %v, %d", err, asked)
	}
}

// Only an entry's file, <id>.yaml, and an image named as one in the folder media, are
// read on and kept, inside the folder a commit is kept in, whatever names the fetch
// gives (the first review of #332, its H1).
func TestASourceKeepsOnlyItsEntries(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "planted.yaml")
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{
			"notes.yaml":             []byte("id: notes\n"),
			"README.md":              []byte("entries\n"),
			"../../../../" + outside: []byte("planted\n"),
			outside:                  []byte("planted\n"),
			"Upper.yaml":             []byte("id: Upper\n"),
			"sub/x.yaml":             []byte("id: x\n"),
			".yaml":                  []byte("id: \n"),
			"media/logo.png":         png,
			"media/../escape.png":    png,
			"media/sub/deep.png":     png,
			"media/page.svg":         []byte(`<svg onload="alert(1)"><script>alert(1)</script><rect/></svg>`),
			"media/README.md":        []byte("images\n"),
		}, nil
	}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: "0123456789abcdef0123456789abcdef01234567"}
	got, err := (&Sources{Dir: dir, Fetch: fetch}).files(t.Context(), spec)
	if err != nil || len(got.entries) != 1 || got.entries["notes.yaml"] == nil {
		t.Fatalf("files = %v, %v", slices.Sorted(maps.Keys(got.entries)), err)
	}
	if !maps.Equal(got.images(), map[string]bool{"logo.png": true, "page.svg": true}) {
		t.Errorf("images = %v", got.media)
	}
	if _, err := os.Stat(outside); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a name reached out of the folder: %v", err)
	}
	var kept []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			kept = append(kept, filepath.Base(filepath.Dir(rel))+"/"+filepath.Base(p))
		}
		return nil
	})
	slices.Sort(kept)
	if want := []string{"0123456789abcdef0123456789abcdef01234567/notes.yaml", "media/logo.png", "media/page.svg"}; !slices.Equal(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
	// An SVG image is kept as svgclean wrote it anew (ADR 0083).
	svg, _ := planSVG(dir, keyOf(spec), spec.GetCommit())
	if string(svg) != `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>` {
		t.Errorf("the SVG image kept is %q", svg)
	}
}

// A commit kept by a Dockwell from before sources held images kept its entries
// alone, so it's read again, as a whole, and kept with its images from then on.
func TestACommitKeptBeforeImagesIsReadAgain(t *testing.T) {
	asked := 0
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		asked++
		return map[string][]byte{"notes.yaml": []byte("id: notes\n"), "media/logo.png": png}, nil
	}
	dir := t.TempDir()
	s := &Sources{Dir: dir, Fetch: fetch}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("b", 40)}
	kept, _ := s.folders(spec)
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "notes.yaml"), []byte("id: notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.files(t.Context(), spec)
	if err != nil || asked != 1 || !got.images()["logo.png"] {
		t.Fatalf("a commit kept without images: %v, fetched %d times, images %v", err, asked, got.media)
	}
	s.tidy([]stored{{name: "community", spec: spec}})
	if got, err = s.files(t.Context(), spec); err != nil || asked != 1 || !got.images()["logo.png"] {
		t.Errorf("read again after it was kept with its images: %v, fetched %d times", err, asked)
	}
}

// png is a whole PNG image of one pixel, and jpeg a JPEG one, as an image's check
// reads its size from its header.
var (
	png  = []byte("\x89\x50\x4e\x47\x0d\x0a\x1a\x0a\x00\x00\x00\x0d\x49\x48\x44\x52\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\x0b\x49\x44\x41\x54\x78\x9c\x63\x60\x00\x02\x00\x00\x05\x00\x01\x7a\x5e\xab\x3f\x00\x00\x00\x00\x49\x45\x4e\x44\xae\x42\x60\x82")
	jpeg = []byte("\xff\xd8\xff\xdb\x00\x84\x00\x06\x04\x05\x06\x05\x04\x06\x06\x05\x06\x07\x07\x06\x08\x0a\x10\x0a\x0a\x09\x09\x0a\x14\x0e\x0f\x0c\x10\x17\x14\x18\x18\x17\x14\x16\x16\x1a\x1d\x25\x1f\x1a\x1b\x23\x1c\x16\x16\x20\x2c\x20\x23\x26\x27\x29\x2a\x29\x19\x1f\x2d\x30\x2d\x28\x30\x25\x28\x29\x28\x01\x07\x07\x07\x0a\x08\x0a\x13\x0a\x0a\x13\x28\x1a\x16\x1a\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\x28\xff\xc0\x00\x11\x08\x00\x01\x00\x01\x03\x01\x22\x00\x02\x11\x01\x03\x11\x01\xff\xc4\x01\xa2\x00\x00\x01\x05\x01\x01\x01\x01\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x10\x00\x02\x01\x03\x03\x02\x04\x03\x05\x05\x04\x04\x00\x00\x01\x7d\x01\x02\x03\x00\x04\x11\x05\x12\x21\x31\x41\x06\x13\x51\x61\x07\x22\x71\x14\x32\x81\x91\xa1\x08\x23\x42\xb1\xc1\x15\x52\xd1\xf0\x24\x33\x62\x72\x82\x09\x0a\x16\x17\x18\x19\x1a\x25\x26\x27\x28\x29\x2a\x34\x35\x36\x37\x38\x39\x3a\x43\x44\x45\x46\x47\x48\x49\x4a\x53\x54\x55\x56\x57\x58\x59\x5a\x63\x64\x65\x66\x67\x68\x69\x6a\x73\x74\x75\x76\x77\x78\x79\x7a\x83\x84\x85\x86\x87\x88\x89\x8a\x92\x93\x94\x95\x96\x97\x98\x99\x9a\xa2\xa3\xa4\xa5\xa6\xa7\xa8\xa9\xaa\xb2\xb3\xb4\xb5\xb6\xb7\xb8\xb9\xba\xc2\xc3\xc4\xc5\xc6\xc7\xc8\xc9\xca\xd2\xd3\xd4\xd5\xd6\xd7\xd8\xd9\xda\xe1\xe2\xe3\xe4\xe5\xe6\xe7\xe8\xe9\xea\xf1\xf2\xf3\xf4\xf5\xf6\xf7\xf8\xf9\xfa\x01\x00\x03\x01\x01\x01\x01\x01\x01\x01\x01\x01\x00\x00\x00\x00\x00\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x11\x00\x02\x01\x02\x04\x04\x03\x04\x07\x05\x04\x04\x00\x01\x02\x77\x00\x01\x02\x03\x11\x04\x05\x21\x31\x06\x12\x41\x51\x07\x61\x71\x13\x22\x32\x81\x08\x14\x42\x91\xa1\xb1\xc1\x09\x23\x33\x52\xf0\x15\x62\x72\xd1\x0a\x16\x24\x34\xe1\x25\xf1\x17\x18\x19\x1a\x26\x27\x28\x29\x2a\x35\x36\x37\x38\x39\x3a\x43\x44\x45\x46\x47\x48\x49\x4a\x53\x54\x55\x56\x57\x58\x59\x5a\x63\x64\x65\x66\x67\x68\x69\x6a\x73\x74\x75\x76\x77\x78\x79\x7a\x82\x83\x84\x85\x86\x87\x88\x89\x8a\x92\x93\x94\x95\x96\x97\x98\x99\x9a\xa2\xa3\xa4\xa5\xa6\xa7\xa8\xa9\xaa\xb2\xb3\xb4\xb5\xb6\xb7\xb8\xb9\xba\xc2\xc3\xc4\xc5\xc6\xc7\xc8\xc9\xca\xd2\xd3\xd4\xd5\xd6\xd7\xd8\xd9\xda\xe2\xe3\xe4\xe5\xe6\xe7\xe8\xe9\xea\xf2\xf3\xf4\xf5\xf6\xf7\xf8\xf9\xfa\xff\xda\x00\x0c\x03\x01\x00\x02\x11\x03\x11\x00\x3f\x00\xe0\xe8\xa2\x8a\xfb\x13\xe7\x0f\xff\xd9")
)

// A source's images are checked by their first bytes, kept beside its entries, and
// named in the API by the path the control plane serves them at. One that isn't what
// its name says is left out, and an entry that names it, or one the commit hasn't,
// or the panel's own artwork, shows none in its place, saying so.
func TestASourcesImagesAreItsOwn(t *testing.T) {
	own, err := os.ReadFile("../../catalog/snapshot/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// As an entry built into Dockwell named its artwork, under /catalog-media/.
	file := strings.ReplaceAll(strings.Replace(string(own), "id: memos\n", "id: memos-src\n", 1), ": media/", ": /catalog-media/")
	file = regexp.MustCompile(`(?m)^icon: .*$`).ReplaceAllString(file, "icon: media/memos-icon.png")
	shots := strings.Count(file, "  - path: /catalog-media/")
	file = strings.Replace(file, "  - path: /catalog-media/", "  - path: media/fake.png\n    caption: A page, not an image\n    source: https://example.com/fake.png\n  - path: media/missing.webp\n    caption: Not in the commit\n    source: https://example.com/missing.webp\n  - path: /catalog-media/", 1)
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{
			"memos-src.yaml":       []byte(file),
			"media/memos-icon.png": png,
			"media/fake.png":       []byte("<html><script>alert(1)</script></html>"),
			"media/named.jpg":      png,
		}, nil
	}
	dir := t.TempDir()
	s := &Sources{Dir: dir, Fetch: fetch}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Path: "entries", Commit: strings.Repeat("c", 40)}
	read := func() sourceRead {
		t.Helper()
		files, err := s.files(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return checkSource(t.Context(), "community", spec, files, claims{})
	}
	for _, got := range []sourceRead{read(), read()} { // fetched, then as kept
		if len(got.entries) != 1 {
			t.Fatalf("entries = %d, refused = %v", len(got.entries), got.refused)
		}
		e := got.entries[0]
		if want := "/catalog-media/" + keyOf(spec) + "/" + spec.GetCommit() + "/memos-icon.png"; e.GetIcon() != want {
			t.Errorf("icon %q, want %q", e.GetIcon(), want)
		}
		if len(e.GetScreenshots()) != 0 {
			t.Errorf("%d screenshots kept, none of which the commit holds as images", len(e.GetScreenshots()))
		}
		for file, want := range map[string]string{
			"media/fake.png":      "isn't a WebP, PNG or JPEG image",
			"media/named.jpg":     "it's a PNG image, named as a JPEG one",
			"media/missing.webp":  "has no such image",
			"/catalog-media/memo": "artwork Dockwell ships",
		} {
			if !slices.ContainsFunc(got.refused, func(r *dockwellv1.CatalogSourceRefusal) bool {
				return strings.HasPrefix(r.GetFile(), file) && strings.Contains(r.GetReason(), want)
			}) {
				t.Errorf("%s: no reason saying %q in %v", file, want, got.refused)
			}
		}
		if shots == 0 {
			t.Fatal("memos.yaml has no screenshots to test with")
		}
	}
}

// The source check and parts preview fill an unanswered input the same way. A
// required text field may be a server path, as Jellyfin's media folder is.
func TestASourcesEntryCanAskForAServerPath(t *testing.T) {
	e := &catalogv1.Entry{
		Id: "media", Name: "Media", Summary: "Reads media", Category: "media", Version: "1", Release: 1, License: "MIT",
		Source:  &catalogv1.Source{Repository: "https://example.com/media"},
		Inputs:  []*catalogv1.Input{{Name: "MEDIA_DIR", Label: "Media folder", Type: catalogv1.InputType_INPUT_TYPE_TEXT}},
		Compose: "services:\n  media:\n    image: media:1@sha256:" + strings.Repeat("a", 64) + "\n    volumes:\n      - ${MEDIA_DIR}:/media:ro\n",
	}
	if err := folder.ReadsAsInstall(t.Context(), e, false); err != nil {
		t.Fatalf("an entry with a required server path was refused: %v", err)
	}
}

// A source's entry can name the panel's own artwork, which Dockwell ships for the
// apps it tests. The catalog shows a source's entry without it, so another
// catalog's app can't pass for one of Dockwell's by its logo and screens.
func TestASourcesEntryShowsNoBundledArtwork(t *testing.T) {
	own, err := os.ReadFile("../../catalog/snapshot/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// As an entry built into Dockwell named its artwork, under /catalog-media/.
	file := strings.ReplaceAll(strings.Replace(string(own), "id: memos\n", "id: memos-src\n", 1), ": media/", ": /catalog-media/")
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("a", 40)}
	got := checkSource(t.Context(), "community", spec, commitFiles{entries: map[string][]byte{"memos-src.yaml": []byte(file)}}, claims{})
	if len(got.entries) != 1 {
		t.Fatalf("entries = %d, refused = %v", len(got.entries), got.refused)
	}
	e := got.entries[0]
	if e.GetIcon() != "" || len(e.GetScreenshots()) != 0 {
		t.Errorf("a source's entry kept the panel's artwork: icon %q, %d screenshots", e.GetIcon(), len(e.GetScreenshots()))
	}
	if e.GetDescription() == "" || e.GetDocumentation() == "" {
		t.Error("a source's entry lost its words and links, which are its own to give")
	}
}

// An entry's images are checked as an install reads them, so one that names no
// digest is refused however its file writes it (the first review of #332, its M3).
func TestASourcesEntryRunsPinnedImages(t *testing.T) {
	pinned := "nginx:1.27@sha256:" + strings.Repeat("a", 64)
	entry := func(services string) *catalogv1.Entry {
		return &catalogv1.Entry{
			Id: "thing", Name: "Thing", Summary: "Stands in", Category: "storage", Version: "1", Release: 1, License: "MIT",
			Source:  &catalogv1.Source{Repository: "https://example.com/thing"},
			Compose: "services:\n" + services,
		}
	}
	for name, services := range map[string]string{
		"a flow mapping": "  web:\n    image: " + pinned + "\n  side: {image: \"ghcr.io/stranger/moving:latest\"}\n",
		"a quoted key":   "  web:\n    \"image\": ghcr.io/stranger/moving:latest\n",
	} {
		if err := folder.ReadsAsInstall(t.Context(), entry(services), false); err == nil || !strings.Contains(err.Error(), "isn't pinned by its digest") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := folder.ReadsAsInstall(t.Context(), entry("  web:\n    image: "+pinned+"\n"), false); err != nil {
		t.Errorf("a pinned image: %v", err)
	}
}

// A commit read for a plan is kept apart, the newest few, and kept for good only once
// a source applied names it; one no source names goes (the first review of #332,
// its M1).
func TestASourceKeepsOnlyWhatsApplied(t *testing.T) {
	dir := t.TempDir()
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{"notes.yaml": []byte("id: notes\n")}, nil
	}
	s := &Sources{Dir: dir, Fetch: fetch}
	var specs []*resourcev1.CatalogSource
	for i := range 6 {
		spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: fmt.Sprintf("%040x", i+1)}
		specs = append(specs, spec)
		if _, err := s.files(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		// Each read a moment after the last, as mtimes order them.
		_, plan := s.folders(spec)
		at := time.Now().Add(-time.Duration(10-i) * time.Minute)
		if err := os.Chtimes(plan, at, at); err != nil {
			t.Fatal(err)
		}
	}
	s.prunePlans()
	count := func(sub string) int {
		n := 0
		_ = filepath.WalkDir(filepath.Join(dir, sub), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	if n := count("plans"); n != plansKept {
		t.Errorf("%d commits read for plans are kept, want %d", n, plansKept)
	}
	// The newest is applied: it's kept, and a kept commit no source names goes.
	stale := filepath.Join(dir, "kept", "0000000000000000", strings.Repeat("f", 40))
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "old.yaml"), []byte("id: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.tidy([]stored{{name: "community", spec: specs[5]}})
	kept, plan := s.folders(specs[5])
	if _, err := os.Stat(filepath.Join(kept, "notes.yaml")); err != nil {
		t.Errorf("the applied commit isn't kept: %v", err)
	}
	if _, err := os.Stat(plan); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the applied commit is still among the plans': %v", err)
	}
	if n := count("kept"); n != 1 {
		t.Errorf("%d files are kept, want the applied commit's one", n)
	}
}

// A source that can't be read is taken as unread for a while, so a host that's down
// doesn't make every view of the catalog wait for it (the first review of #332, its
// L2).
func TestAnUnreadSourceIsTriedAgainLater(t *testing.T) {
	asked := 0
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		asked++
		return nil, errors.New("the host didn't answer")
	}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s := &Sources{Fetch: fetch, Now: func() time.Time { return now }}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("a", 40)}
	for range 3 {
		if _, err := s.readSource(t.Context(), "community", spec, claims{}); err == nil {
			t.Fatal("an unreadable source was read")
		}
	}
	if asked != 1 {
		t.Errorf("an unreadable source was fetched %d times in a minute", asked)
	}
	now = now.Add(unreadWait + time.Second)
	if _, err := s.readSource(t.Context(), "community", spec, claims{}); err == nil || asked != 2 {
		t.Errorf("it wasn't tried again later: %v, %d", err, asked)
	}
}

// Reads of one commit at once share one fetch, and fetches of different commits run
// one at a time, as each pack's index takes memory while it's read: four at once
// reached 328 MB (the narrow check of #332).
func TestSourcesFetchOneAtATime(t *testing.T) {
	var asked, now, most atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		asked.Add(1)
		if n := now.Add(1); n > most.Load() {
			most.Store(n)
		}
		defer now.Add(-1)
		<-release
		return map[string][]byte{"notes.yaml": []byte("id: notes\n")}, nil
	}
	// A read that comes after its commit's fetch ended reads what that kept.
	s := &Sources{Dir: t.TempDir(), Fetch: fetch}
	same := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("a", 40)}
	var wg sync.WaitGroup
	results := make([]commitFiles, 6)
	for i := range results {
		wg.Go(func() {
			got, err := s.files(t.Context(), same)
			if err != nil {
				t.Error(err)
			}
			results[i] = got
		})
	}
	for i := range 3 {
		other := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat(string(rune('b'+i)), 40)}
		wg.Go(func() {
			if _, err := s.files(t.Context(), other); err != nil {
				t.Error(err)
			}
		})
	}
	// Let every read reach its fetch, or its wait for one, then let each go.
	for now.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := asked.Load(); n != 4 {
		t.Errorf("4 commits were fetched %d times", n)
	}
	if n := most.Load(); n != 1 {
		t.Errorf("%d fetches ran at once", n)
	}
	for i, got := range results {
		if string(got.entries["notes.yaml"]) != "id: notes\n" {
			t.Errorf("read %d got %v", i, got)
		}
	}
}

// What an older converter kept of a store's commit isn't read as kept, so it's
// converted again, and it stays where it is until that succeeds (ADR 0078, and the
// review of #548, its L4).
func TestAStoresCommitKeptByAnOlderConverterIsReadAgain(t *testing.T) {
	dir := t.TempDir()
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/appstore", Commit: "0123456789abcdef0123456789abcdef01234567", Format: resourcev1.CatalogFormat_CATALOG_FORMAT_ONEPANEL}
	s := &Sources{Dir: dir}
	kept, _ := s.folders(spec)
	keep(kept, commitFiles{entries: map[string][]byte{"memos.yaml": []byte("id: memos\n")}, leftOut: []byte(`{"format":"1panel","version":0}`)}, nil)
	if _, ok := s.kept(spec); ok {
		t.Error("what an older converter kept reads as kept")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("what an older converter kept went before anything took its place: %v", err)
	}
	other := &resourcev1.CatalogSource{Repository: spec.GetRepository(), Commit: "fedcba9876543210fedcba9876543210fedcba98", Format: spec.GetFormat()}
	fresh, _ := s.folders(other)
	keep(fresh, commitFiles{entries: map[string][]byte{"memos.yaml": []byte("id: memos\n")}, leftOut: fmt.Appendf(nil, `{"format":"1panel","version":%d}`, convert.Version)}, nil)
	if files, ok := s.kept(other); !ok || files.entries["memos.yaml"] == nil {
		t.Errorf("what this converter kept doesn't read as kept: %v", files.entries)
	}
	// The same folder read as Dockwell's entries is kept apart.
	dockwell := &resourcev1.CatalogSource{Repository: spec.GetRepository(), Commit: spec.GetCommit()}
	if other, _ := s.folders(dockwell); other == kept {
		t.Error("a store's commit and the same folder read as entries share where they're kept")
	}
}

// planSVG reads the SVG image a test's commit keeps for a plan.
func planSVG(dir, key, commit string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, "plans", key, commit, "media", "page.svg"))
}

// A read of a source is every reader's: a person who leaves the page while its
// entries are checked doesn't cancel each entry's read, so what's kept never says
// every entry failed for a reason none of them has.
func TestALeftReadStillChecksEveryEntry(t *testing.T) {
	own, err := os.ReadFile("../../catalog/snapshot/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := strings.Replace(string(own), "id: memos\n", "id: memos-src\n", 1)
	file = regexp.MustCompile(`(?m)^icon: .*$`).ReplaceAllString(file, "")
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{"memos-src.yaml": []byte(file)}, nil
	}
	s := &Sources{Dir: t.TempDir(), Fetch: fetch}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("c", 40)}
	if _, err := s.files(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	left, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := s.readSource(left, "community", spec, claims{})
	if err != nil || len(got.entries) != 1 {
		t.Fatalf("a read whose asker left: %d entries, refused %v, %v", len(got.entries), got.refused, err)
	}
	again, err := s.readSource(t.Context(), "community", spec, claims{})
	if err != nil || len(again.entries) != 1 {
		t.Errorf("what was kept of it: %d entries, refused %v, %v", len(again.entries), again.refused, err)
	}
}

// Readers of the same commit at once share one check, as they share its fetch: twenty
// lists after a restart, each closed at once, start one check, not twenty, and every
// reader gets what it found.
func TestReadersShareOneCheck(t *testing.T) {
	s, spec := memosSource(t)
	var checks atomic.Int32
	waiting := make(chan struct{})
	s.beforeCheck = func() {
		checks.Add(1)
		<-waiting
	}
	const readers = 20
	var wg sync.WaitGroup
	results := make([]sourceRead, readers)
	for i := range readers {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(t.Context())
			if i%2 == 0 {
				cancel() // as a browser that closed the page at once
			} else {
				defer cancel()
			}
			results[i], _ = s.readSource(ctx, "community", spec, claims{})
		})
	}
	// Every reader in, waiting on the one check.
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.checking) == 1
	})
	close(waiting)
	wg.Wait()
	if n := checks.Load(); n != 1 {
		t.Errorf("%d readers at once started %d checks, want 1", readers, n)
	}
	got, err := s.readSource(t.Context(), "community", spec, claims{})
	if err != nil || len(got.entries) != 1 {
		t.Errorf("what the check kept: %d entries, refused %v, %v", len(got.entries), got.refused, err)
	}
}

// A check cut short keeps nothing: what it said of each entry it couldn't read holds
// for nobody, and the next read checks again.
func TestACheckCutShortIsKeptForNobody(t *testing.T) {
	s, spec := memosSource(t)
	was := checkWait
	checkWait = time.Nanosecond
	_, err := s.readSource(t.Context(), "community", spec, claims{})
	checkWait = was
	if err == nil {
		t.Fatal("a check cut short said nothing")
	}
	got, err := s.readSource(t.Context(), "community", spec, claims{})
	if err != nil || len(got.entries) != 1 {
		t.Errorf("the next read: %d entries, refused %v, %v", len(got.entries), got.refused, err)
	}
}

// memosSource is a source of one entry, Dockwell's own Memos as another's, fetched.
func memosSource(t *testing.T) (*Sources, *resourcev1.CatalogSource) {
	t.Helper()
	own, err := os.ReadFile("../../catalog/snapshot/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := strings.Replace(string(own), "id: memos\n", "id: memos-src\n", 1)
	file = regexp.MustCompile(`(?m)^icon: .*$`).ReplaceAllString(file, "")
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		return map[string][]byte{"memos-src.yaml": []byte(file)}, nil
	}
	s := &Sources{Dir: t.TempDir(), Fetch: fetch}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("e", 40)}
	if _, err := s.files(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	return s, spec
}

// waitFor waits a few seconds for ok.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waited too long")
}

// A read where an entry's Compose file wasn't read as the reader was busy is kept for
// nobody, as one cut short isn't.
func TestAnUnsettledReadIsNotKept(t *testing.T) {
	if settled(sourceRead{unsettled: true}, nil) == nil {
		t.Error("an unsettled read would be kept")
	}
	if settled(sourceRead{}, context.DeadlineExceeded) == nil {
		t.Error("a read cut short would be kept")
	}
	if err := settled(sourceRead{}, nil); err != nil {
		t.Errorf("a whole read wouldn't be kept: %v", err)
	}
}
