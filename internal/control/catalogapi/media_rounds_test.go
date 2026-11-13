// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

// fetchCounter counts the fetches a snapshot makes of its host.
type fetchCounter struct {
	base    http.RoundTripper
	fetches atomic.Int32
}

func (c *fetchCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
		c.fetches.Add(1)
	}
	return c.base.RoundTrip(r)
}

// mediaStand is a store's commit as storeMedia reads it: a snapshot of files, an app
// for each of apps with its screenshots, and how many fetches the snapshot made.
type mediaStand struct {
	snap    *gitfetch.Snapshot
	run     convert.Run
	listed  map[string]gitfetch.Entry
	counter *fetchCounter
}

func newMediaStand(t *testing.T, files map[string]string, shots map[string][]string, icons map[string]string) mediaStand {
	t.Helper()
	repo := gitfetchtest.New(t)
	repo.Filter()
	head := repo.Commit(files)
	client := gitfetchtest.Client()
	counter := &fetchCounter{base: client.Transport}
	client.Transport = counter
	snap, err := gitfetch.Trees(t.Context(), client, repo.URL, head, gitfetch.TreeDefault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(snap.Close)
	all, err := snap.Walk("", func(string, int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]gitfetch.Entry{}
	for _, e := range all {
		listed[e.Path] = e
	}
	var run convert.Run
	for _, id := range slices.Sorted(maps.Keys(shots)) { // a run's results are sorted by ID
		r := convert.Result{ID: id, Entry: &catalogv1.Entry{Id: id, Name: id}, Icon: icons[id]}
		for _, p := range shots[id] {
			r.Screenshots = append(r.Screenshots, convert.Shot{Path: p})
		}
		run.Results = append(run.Results, r)
	}
	return mediaStand{snap: snap, run: run, listed: listed, counter: counter}
}

// read is storeMedia's images, how many screenshots it names, and how many fetches it made.
func (m mediaStand) read(t *testing.T) (images map[string][]byte, shots, fetches int) {
	t.Helper()
	before := m.counter.fetches.Load()
	images = storeMedia(t.Context(), m.snap, m.run, m.listed, func(file string) string {
		return fileAt("https://github.com/example/apps", strings.Repeat("a", 40), file)
	})
	for i := range m.run.Results {
		shots += len(m.run.Results[i].Entry.GetScreenshots())
	}
	return images, shots, int(m.counter.fetches.Load() - before)
}

// noise is a PNG image of one pixel followed by n bytes no pack compresses, so each
// file costs its size to fetch and still reads as an image.
func noise(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return string(png) + string(b)
}

// Every round of a commit's images, logos and screenshots together, is held to one
// budget of what its packs take from the host: once a round leaves too little for
// the next, the reading stops, rather than fetch a full pack each round (the review
// of #589, M2).
func TestAStoresImagesKeepToOneBudget(t *testing.T) {
	was := mediaPack
	mediaPack = 600 << 10
	t.Cleanup(func() { mediaPack = was })
	files := map[string]string{}
	shots := map[string][]string{}
	for i := range 40 {
		id := fmt.Sprintf("app%02d", i)
		files["Apps/"+id+"/shot.png"] = noise(t, 30<<10)
		shots[id] = []string{"Apps/" + id + "/shot.png"}
	}
	_, got, fetches := newMediaStand(t, files, shots, nil).read(t)
	if got != shotsARound || fetches != 2 {
		t.Errorf("it kept %d screenshots in %d fetches, want %d in 2: the first round, and the second refused at its budget", got, fetches, shotsARound)
	}
}

// A round that keeps nothing, or whose files are mostly over the most an image may
// be, ends the rounds, as the next is likely the same (the review of #589, M2).
func TestARoundThatKeepsNothingIsTheLast(t *testing.T) {
	files := map[string]string{}
	shots := map[string][]string{}
	for i := range 40 {
		id := fmt.Sprintf("app%02d", i)
		files["Apps/"+id+"/shot.png"] = "not an image " + id
		shots[id] = []string{"Apps/" + id + "/shot.png"}
	}
	if _, got, fetches := newMediaStand(t, files, shots, nil).read(t); got != 0 || fetches != 1 {
		t.Errorf("of files that aren't images, it kept %d in %d fetches, want none in 1", got, fetches)
	}

	was := folder.MediaLimits.File
	folder.MediaLimits.File = 40 << 10
	t.Cleanup(func() { folder.MediaLimits.File = was })
	files, shots = map[string]string{}, map[string][]string{}
	for i := range 20 {
		id := fmt.Sprintf("app%02d", i)
		size := 1 << 10
		if i < 9 {
			size = 50 << 10 // over the most an image may be here
		}
		files["Apps/"+id+"/shot.png"] = noise(t, size)
		shots[id] = []string{"Apps/" + id + "/shot.png"}
	}
	if _, got, fetches := newMediaStand(t, files, shots, nil).read(t); fetches != 1 || got == 0 {
		t.Errorf("of a round mostly too big, it kept %d in %d fetches, want some in 1", got, fetches)
	}
}

// Screenshots come whether or not the store keeps logos (the review of #589, L4).
func TestScreenshotsComeWithoutLogos(t *testing.T) {
	m := newMediaStand(t, map[string]string{"Apps/notes/shot.png": string(png), "Apps/board/shot.png": string(png)},
		map[string][]string{"notes": {"Apps/notes/shot.png"}, "board": {"Apps/board/shot.png"}}, nil)
	if images, got, _ := m.read(t); got != 2 || len(images) != 2 {
		t.Errorf("with no logos it kept %d screenshots, %d images", got, len(images))
	}
}

// A screenshot is kept by a name no app's logo can have, and its original is the
// page of the repository's own file at the commit read, never an address the store
// gives (the review of #589, L2 and L3).
func TestAScreenshotIsNamedApartAndLinksTheRepository(t *testing.T) {
	m := newMediaStand(t, map[string]string{"Apps/notes/shot.png": string(png), "Apps/notes-1/logo.png": string(png)},
		map[string][]string{"notes": {"Apps/notes/shot.png"}, "notes-1": nil}, map[string]string{"notes-1": "Apps/notes-1/logo.png"})
	images, _, _ := m.read(t)
	if len(images) != 2 {
		t.Fatalf("images = %d, want the logo of notes-1 and the screenshot of notes apart", len(images))
	}
	for i := range m.run.Results {
		r := &m.run.Results[i]
		if r.ID != "notes" {
			continue
		}
		shot := r.Entry.GetScreenshots()[0]
		if shot.GetPath() != "media/notes_shot-1.png" || shot.GetSource() != "https://github.com/example/apps/blob/"+strings.Repeat("a", 40)+"/Apps/notes/shot.png" {
			t.Errorf("the screenshot is %v", shot)
		}
	}
	commit := strings.Repeat("b", 40)
	for want, c := range map[string]struct{ repository, file string }{
		"https://github.com/example/apps/blob/" + commit + "/Apps/A%20B/x.png": {"https://github.com/example/apps.git", "Apps/A B/x.png"},
		"https://gitlab.com/example/apps/-/blob/" + commit + "/x.png":          {"https://gitlab.com/example/apps", "x.png"},
		"https://codeberg.org/example/apps/src/commit/" + commit + "/x.png":    {"https://codeberg.org/example/apps/", "x.png"},
		"": {"https://example.com/example/apps", "x.png"},
	} {
		if got := fileAt(c.repository, commit, c.file); got != want {
			t.Errorf("fileAt(%s, %s) = %q, want %q", c.repository, c.file, got, want)
		}
	}
	for want, repository := range map[string]string{
		"https://git.example.com/apps":      "https://git.example.com/apps.git",
		"https://git.example.com:8443/apps": "https://git.example.com:8443/apps/",
		"":                                  "https://ada:secret@git.example.com/apps",
	} {
		if got := repositoryPage(repository); got != want {
			t.Errorf("repositoryPage(%s) = %q, want %q", repository, got, want)
		}
	}
	for _, c := range []struct{ repository, commit, file string }{
		{"http://github.com/example/apps", commit, "x.png"},
		{"https://github.com/example/apps", "main", "x.png"},
		{"https://github.com/example/apps", commit, "../x.png"},
		{"https://github.com/example", commit, "x.png"},
	} {
		if got := fileAt(c.repository, c.commit, c.file); got != "" {
			t.Errorf("fileAt(%s, %s, %s) = %q", c.repository, c.commit, c.file, got)
		}
	}
}

// pngSaying is the start of a PNG image whose header says it's w by h pixels, all a
// reading of its size needs.
func pngSaying(w, h uint32) string {
	ihdr := append([]byte("IHDR"), binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, w), h)...)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	out := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 13)
	out = append(out, ihdr...)
	return string(binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr)))
}

// A logo is drawn a few dozen pixels wide, so one over 1024 a side leaves its app
// without one, where a screenshot of that size is kept (the review of #589, M3).
func TestALogoKeepsToItsPixels(t *testing.T) {
	m := newMediaStand(t, map[string]string{"Apps/notes/logo.png": pngSaying(2000, 2000), "Apps/notes/shot.png": pngSaying(2000, 2000), "Apps/board/logo.png": pngSaying(512, 512)},
		map[string][]string{"notes": {"Apps/notes/shot.png"}, "board": nil}, map[string]string{"notes": "Apps/notes/logo.png", "board": "Apps/board/logo.png"})
	m.read(t)
	for i := range m.run.Results {
		r := &m.run.Results[i]
		switch {
		case r.ID == "notes" && (r.Entry.GetIcon() != "" || len(r.Entry.GetScreenshots()) != 1):
			t.Errorf("notes: icon %q, %d screenshots, want no icon and its screenshot", r.Entry.GetIcon(), len(r.Entry.GetScreenshots()))
		case r.ID == "board" && r.Entry.GetIcon() != "media/board.png":
			t.Errorf("board's icon is %q", r.Entry.GetIcon())
		}
	}
}
