// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

// store commits a tree laid out as another store's catalog is: an app's folder with
// its versions, beside a large file the catalog never reads.
func store(t *testing.T, filter bool) (*gitfetchtest.Repo, string) {
	t.Helper()
	r := gitfetchtest.New(t)
	if filter {
		r.Filter()
	}
	r.Link("apps/memos/link.yml", "data.yml")
	commit := r.Commit(map[string]string{
		"README.md":                             "a store\n",
		"apps/memos/data.yml":                   "name: Memos\n",
		"apps/memos/0.25.0/docker-compose.yml":  "services:\n  memos:\n    image: neosmemo/memos:0.25.0\n",
		"apps/memos/0.25.0/data.yml":            "additionalProperties: {}\n",
		"apps/memos/0.25.0/data/.gitkeep":       "\n",
		"apps/memos/0.25.0/build/archive.tar":   strings.Repeat("x", 3<<20),
		"apps/memos/0.25.0/deep/a/b/c/d/e/f.md": "deep\n",
		"other/thing.txt":                       "not a catalog's\n",
	})
	return r, commit
}

func TestTreesReadsOnlyWhatsAsked(t *testing.T) {
	for _, filter := range []bool{true, false} {
		t.Run(map[bool]string{true: "a host that filters", false: "a host that doesn't"}[filter], func(t *testing.T) {
			r, commit := store(t, filter)
			snap, err := gitfetch.Trees(t.Context(), gitfetchtest.Client(), r.URL, commit, gitfetch.TreeDefault)
			if err != nil {
				t.Fatal(err)
			}
			defer snap.Close()
			if !snap.Time.Equal(r.When(commit)) {
				t.Errorf("Time = %v, want when it was committed", snap.Time)
			}
			listed, err := snap.Walk("apps", func(p string, depth int) bool { return depth <= 3 && !strings.HasSuffix(p, "/deep") })
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			byPath := map[string]gitfetch.Entry{}
			for _, e := range listed {
				paths = append(paths, e.Path)
				byPath[e.Path] = e
			}
			want := []string{
				"apps/memos", "apps/memos/0.25.0", "apps/memos/0.25.0/build", "apps/memos/0.25.0/build/archive.tar",
				"apps/memos/0.25.0/data", "apps/memos/0.25.0/data.yml", "apps/memos/0.25.0/data/.gitkeep",
				"apps/memos/0.25.0/deep", "apps/memos/0.25.0/docker-compose.yml", "apps/memos/data.yml",
			}
			slices.Sort(paths)
			if !slices.Equal(paths, want) {
				t.Errorf("Walk listed\n%v\nwant\n%v (no link, nothing past what descend allows)", paths, want)
			}
			files, big, err := snap.Read(t.Context(), []gitfetch.Entry{
				byPath["apps/memos/data.yml"], byPath["apps/memos/0.25.0/docker-compose.yml"],
				byPath["apps/memos/0.25.0/data/.gitkeep"], byPath["apps/memos/0.25.0/build/archive.tar"], byPath["apps/memos"],
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"apps/memos/0.25.0/data/.gitkeep", "apps/memos/0.25.0/docker-compose.yml", "apps/memos/data.yml"}) {
				t.Errorf("Read returned %v", got)
			}
			if string(files["apps/memos/data.yml"]) != "name: Memos\n" {
				t.Errorf("data.yml = %q", files["apps/memos/data.yml"])
			}
			if !slices.Equal(big, []string{"apps/memos/0.25.0/build/archive.tar"}) {
				t.Errorf("big = %v, want the archive over 1 MiB", big)
			}
		})
	}
}

func TestTreesBounds(t *testing.T) {
	r, commit := store(t, true)
	all := func(p string, _ int) bool { return !strings.HasSuffix(p, "/deep") }
	for _, tc := range []struct {
		name string
		lim  func(*gitfetch.TreeLimits)
		read bool
		want string
	}{
		{"too deep", func(l *gitfetch.TreeLimits) { l.Depth = 2 }, false, "over 2 deep"},
		{"too many entries", func(l *gitfetch.TreeLimits) { l.Entries = 4 }, false, "over 4 files and folders"},
		{"too many files asked for", func(l *gitfetch.TreeLimits) { l.Files = 1 }, true, "more than the 1 a source reads"},
		{"too much together", func(l *gitfetch.TreeLimits) { l.Total = 10 }, true, "over 10 bytes"},
		{"a first round too large", func(l *gitfetch.TreeLimits) { l.Trees = 100 }, false, "more than a source reads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lim := gitfetch.TreeDefault
			tc.lim(&lim)
			snap, err := gitfetch.Trees(t.Context(), gitfetchtest.Client(), r.URL, commit, lim)
			if err == nil {
				defer snap.Close()
				var listed []gitfetch.Entry
				listed, err = snap.Walk("", all)
				if err == nil && tc.read {
					_, _, err = snap.Read(t.Context(), listed)
				}
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one saying %q", err, tc.want)
			}
		})
	}
	snap, err := gitfetch.Trees(t.Context(), gitfetchtest.Client(), r.URL, commit, gitfetch.TreeDefault)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if _, err := snap.Walk("apps/nothing", all); err == nil || !strings.Contains(err.Error(), "has no folder apps/nothing") {
		t.Errorf("a folder the commit hasn't: err = %v", err)
	}
}
