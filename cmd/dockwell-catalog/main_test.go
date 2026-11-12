// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/catalog/convert"
)

// The command says what it needs, and does nothing without it.
func TestItSaysWhatItNeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"nothing":                  {nil, "name what to do: convert or check"},
		"no source":                {[]string{"convert", "--from", "1panel"}, "name the source's checkout with --source"},
		"a format it doesn't read": {[]string{"convert", "--from", "umbrel", "--source", t.TempDir(), "--out", t.TempDir(), "--commit", "abc"}, `not "umbrel"`},
		"a folder with no apps":    {[]string{"convert", "--from", "1panel", "--source", t.TempDir(), "--out", t.TempDir(), "--commit", "abc"}, "github.com/1Panel-dev/appstore"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), tc.args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run(%q) = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
}

// An overlay is read by its file's name, with what a person looked at, and one the
// format can't read stops the run.
func TestItReadsOverlays(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), []byte("summary: Keeps notes\ncategory: notes\nreaches:\n  - \"notes: the server's own network\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not an overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := convert.ReadOverlays(dir)
	if err != nil || len(got) != 1 || got["notes"].Entry.GetSummary() != "Keeps notes" || len(got["notes"].Reaches) != 1 {
		t.Errorf("ReadOverlays = %v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("not_a_field: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := convert.ReadOverlays(dir); err == nil || !strings.Contains(err.Error(), "the overlay bad.yaml") {
		t.Errorf("ReadOverlays read a field the format hasn't: %v", err)
	}
}

// check checks a folder as a server reads a catalog source: an entry that names an
// image the folder has passes, and one that isn't what its name says, or a file
// that isn't an entry, is named with why, and fails the check.
func TestItChecksAFolder(t *testing.T) {
	dir := t.TempDir()
	own, err := os.ReadFile("../../internal/catalog/entries/memos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entry := strings.ReplaceAll(string(own), "/catalog-media/", "media/")
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memos.yaml", []byte(entry))
	write("README.md", []byte("A catalog.\n"))
	for _, image := range []string{"memos-icon.webp", "memos-timeline.webp"} {
		data, err := os.ReadFile("../../web/public/catalog-media/" + image)
		if err != nil {
			t.Fatal(err)
		}
		write("media/"+image, data)
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"check", dir}, &out, &out); err != nil || !strings.Contains(out.String(), "Checked 1 entry and 2 images") {
		t.Fatalf("a folder that passes: %v\n%s", err, out.String())
	}
	write("media/memos-icon.webp", []byte("<svg onload=alert(1)>"))
	write("broken.yaml", []byte("id: broken\n"))
	out.Reset()
	err = run(t.Context(), []string{"check", dir}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "a server would leave out or lose 3") {
		t.Errorf("a folder that fails: %v", err)
	}
	for _, want := range []string{"media/memos-icon.webp: by its first bytes it isn't", "broken.yaml: ", "media/memos-icon.webp: memos.yaml names it"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("it doesn't say %q:\n%s", want, out.String())
		}
	}
	if err := run(t.Context(), []string{"check"}, &out, &out); err == nil || !strings.Contains(err.Error(), "name the folder") {
		t.Errorf("no folder: %v", err)
	}
}
