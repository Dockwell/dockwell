// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

// digestsAsked stands in for the agent's digest reads: what it was asked, a call at a
// time, and what it answers.
type digestsAsked struct {
	calls  [][]string
	answer func(image string) string
}

func (d *digestsAsked) Digests(_ context.Context, _ string, images []string) (map[string]string, error) {
	d.calls = append(d.calls, slices.Clone(images))
	out := map[string]string{}
	for _, image := range images {
		out[image] = d.answer(image)
	}
	return out, nil
}

func goodDigest(string) string { return "sha256:" + strings.Repeat("7", 64) }

func byTag(compose string) *catalogv1.Entry {
	return &catalogv1.Entry{Id: "app", Name: "App", Compose: compose}
}

// A plan pins only the images an entry's services run, as an install reads the file:
// an image: line elsewhere, such as a variable in a service's environment, was never
// checked as an image, and the agent isn't asked about it (the review of #548, its
// M2).
func TestAPlanPinsOnlyWhatServicesRun(t *testing.T) {
	asked := &digestsAsked{answer: goodDigest}
	h := (&Handler{}).PinWith(asked)
	e := byTag("services:\n  web:\n    image: nginx:1.27\n    environment:\n      image: 127.0.0.1:2375/x:1.0\n")
	out, _, err := h.pinImages(t.Context(), e, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked.calls) != 1 || !slices.Equal(asked.calls[0], []string{"nginx:1.27"}) {
		t.Errorf("the agent was asked about %v", asked.calls)
	}
	if !strings.Contains(out.GetCompose(), "image: nginx:1.27@sha256:") || !strings.Contains(out.GetCompose(), "image: 127.0.0.1:2375/x:1.0\n") {
		t.Errorf("pinned what isn't a service's image, or missed what is:\n%s", out.GetCompose())
	}
}

// Where an image's registry is gets checked again when the plan is made, and the
// agent is never asked about one on this server or a private network (the review of
// #548, its M2).
func TestAPlanRefusesAPrivateRegistry(t *testing.T) {
	for _, image := range []string{"10.0.0.5:5000/web:1.0", "localhost:5000/web:1.0", "127.0.0.1:2375/web:1.0", "169.254.169.254/web:1.0"} {
		asked := &digestsAsked{answer: goodDigest}
		h := (&Handler{}).PinWith(asked)
		_, _, err := h.pinImages(t.Context(), byTag("services:\n  web:\n    image: "+image+"\n"), "", nil)
		if err == nil || !strings.Contains(err.Error(), "where a store's app may not pull from") || len(asked.calls) != 0 {
			t.Errorf("%s: %v, asked %v", image, err, asked.calls)
		}
	}
}

// A digest a registry answers with that isn't one pins nothing (the review of #548,
// its L1).
func TestAPlanRefusesADigestThatIsntOne(t *testing.T) {
	for _, bad := range []string{"sha256:short", "sha256:" + strings.Repeat("G", 64), "md5:" + strings.Repeat("a", 32), "sha256:" + strings.Repeat("a", 64) + "\nx"} {
		h := (&Handler{}).PinWith(&digestsAsked{answer: func(string) string { return bad }})
		_, _, err := h.pinImages(t.Context(), byTag("services:\n  web:\n    image: nginx:1.27\n"), "", nil)
		if err == nil || !strings.Contains(err.Error(), "a digest that isn't one") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// An app of more images than an agent reads at once is asked about in calls of 20
// (the review of #548, its L7).
func TestAPlanAsksForDigestsTwentyAtATime(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n")
	for i := range 25 {
		fmt.Fprintf(&b, "  s%d:\n    image: example/s%d:1.0\n", i, i)
	}
	asked := &digestsAsked{answer: goodDigest}
	if _, _, err := (&Handler{}).PinWith(asked).pinImages(t.Context(), byTag(b.String()), "", nil); err != nil {
		t.Fatal(err)
	}
	if len(asked.calls) != 2 || len(asked.calls[0]) != 20 || len(asked.calls[1]) != 5 {
		t.Errorf("calls of %v", asked.calls)
	}
}

// What an older converter kept of a commit a source names stays until this one has
// converted it again, so a repository that's gone leaves what's installed from it its
// upgrades, and what's converted then takes its place (the review of #548, its L4).
func TestAStaleConversionStaysUntilANewOneSucceeds(t *testing.T) {
	dir := t.TempDir()
	spec := &resourcev1.CatalogSource{Repository: "https://catalog.example.com:1/gone", Commit: strings.Repeat("0", 40), Format: resourcev1.CatalogFormat_CATALOG_FORMAT_ONEPANEL}
	s := &Sources{Dir: dir, Client: gitfetchtest.Client()}
	kept, plan := s.folders(spec)
	old := commitFiles{entries: map[string][]byte{"memos.yaml": []byte("id: memos\n")}, leftOut: []byte(`{"format":"1panel","version":0}`)}
	keep(kept, old, nil)
	files, err := s.files(t.Context(), spec)
	if err != nil || string(files.entries["memos.yaml"]) != "id: memos\n" {
		t.Fatalf("a commit that can't be converted again reads %v, %v", files.entries, err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("what an older converter kept went before a new conversion succeeded: %v", err)
	}
	keep(plan, commitFiles{entries: map[string][]byte{"memos.yaml": []byte("id: memos\nname: Memos\n")}, leftOut: fmt.Appendf(nil, `{"format":"1panel","version":%d}`, convert.Version)}, nil)
	s.tidy([]stored{{name: "onepanel", spec: spec}})
	if got, err := readKept(kept); err != nil || string(got.entries["memos.yaml"]) != "id: memos\nname: Memos\n" {
		t.Errorf("what's converted now didn't take the older one's place where a source keeps it: %v, %v", got.entries, err)
	}
}

// A left-out app's reason is one line of a length a person reads, however a store
// words what's refused (the review of #548, its H3).
func TestALeftOutReasonIsCut(t *testing.T) {
	long := "it says " + strings.Repeat("a lot ", 1<<16) + "\nfirst_steps: x"
	got := cut(long, maxReason)
	if r := []rune(got); len(r) != maxReason || !strings.HasSuffix(got, "…") || strings.ContainsAny(got, "\n\r") {
		t.Errorf("a long reason is cut to %d characters: %q", len(r), got[:40])
	}
}
