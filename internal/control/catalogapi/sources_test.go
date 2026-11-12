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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/convert"
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
		if err != nil || string(got["notes.yaml"]) != "id: notes\n" {
			t.Fatalf("files = %q, %v", got, err)
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

// Only an entry's file, <id>.yaml, is read on and kept, inside the folder a commit
// is kept in, whatever names the fetch gives (the first review of #332, its H1).
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
		}, nil
	}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: "0123456789abcdef0123456789abcdef01234567"}
	got, err := (&Sources{Dir: dir, Fetch: fetch}).files(t.Context(), spec)
	if err != nil || len(got) != 1 || got["notes.yaml"] == nil {
		t.Fatalf("files = %v, %v", slices.Sorted(maps.Keys(got)), err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a name reached out of the folder: %v", err)
	}
	var kept []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			kept = append(kept, filepath.Base(p))
		}
		return nil
	})
	if !slices.Equal(kept, []string{"notes.yaml"}) {
		t.Errorf("kept %v", kept)
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
	if err := readsAsInstall(t.Context(), e, false); err != nil {
		t.Fatalf("an entry with a required server path was refused: %v", err)
	}
}

// A source's entry can name the panel's own artwork, which Dockwell ships for the
// apps it tests. The catalog shows a source's entry without it, so another
// catalog's app can't pass for one of Dockwell's by its logo and screens.
func TestASourcesEntryShowsNoBundledArtwork(t *testing.T) {
	own, err := os.ReadFile("../../catalog/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := strings.Replace(string(own), "id: memos\n", "id: memos-src\n", 1)
	got := checkSource(t.Context(), "community", &resourcev1.CatalogSource{}, map[string][]byte{"memos-src.yaml": []byte(file)}, map[string]string{})
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
		if err := readsAsInstall(t.Context(), entry(services), false); err == nil || !strings.Contains(err.Error(), "isn't pinned by its digest") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := readsAsInstall(t.Context(), entry("  web:\n    image: "+pinned+"\n"), false); err != nil {
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
		if _, err := s.readSource(t.Context(), "community", spec, map[string]string{}); err == nil {
			t.Fatal("an unreadable source was read")
		}
	}
	if asked != 1 {
		t.Errorf("an unreadable source was fetched %d times in a minute", asked)
	}
	now = now.Add(unreadWait + time.Second)
	if _, err := s.readSource(t.Context(), "community", spec, map[string]string{}); err == nil || asked != 2 {
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
	results := make([]map[string][]byte, 6)
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
		if string(got["notes.yaml"]) != "id: notes\n" {
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
	keep(kept, map[string][]byte{"memos.yaml": []byte("id: memos\n"), leftOutFile: []byte(`{"format":"1panel","version":0}`)})
	if _, ok := s.kept(spec); ok {
		t.Error("what an older converter kept reads as kept")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("what an older converter kept went before anything took its place: %v", err)
	}
	other := &resourcev1.CatalogSource{Repository: spec.GetRepository(), Commit: "fedcba9876543210fedcba9876543210fedcba98", Format: spec.GetFormat()}
	fresh, _ := s.folders(other)
	keep(fresh, map[string][]byte{"memos.yaml": []byte("id: memos\n"), leftOutFile: fmt.Appendf(nil, `{"format":"1panel","version":%d}`, convert.Version)})
	if files, ok := s.kept(other); !ok || files["memos.yaml"] == nil {
		t.Errorf("what this converter kept doesn't read as kept: %v", files)
	}
	// The same folder read as Dockwell's entries is kept apart.
	dockwell := &resourcev1.CatalogSource{Repository: spec.GetRepository(), Commit: spec.GetCommit()}
	if other, _ := s.folders(dockwell); other == kept {
		t.Error("a store's commit and the same folder read as entries share where they're kept")
	}
}
