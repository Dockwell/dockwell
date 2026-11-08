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
		"nothing":                  {nil, "name what to do: convert"},
		"no source":                {[]string{"convert", "--from", "1panel"}, "name the source's checkout with --source"},
		"a format it doesn't read": {[]string{"convert", "--from", "umbrel", "--source", t.TempDir(), "--out", t.TempDir(), "--commit", "abc"}, `reads 1panel, not "umbrel"`},
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
