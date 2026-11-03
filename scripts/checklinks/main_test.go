// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	for text, want := range map[string]string{
		"Content fundamentals":                "content-fundamentals",
		"Re-importing from Claude Design":     "re-importing-from-claude-design",
		"Risks, open questions and decisions": "risks-open-questions-and-decisions",
		"1 · Foundations":                     "1--foundations",
		"The `dockwell` binary":               "the-dockwell-binary",
		"See [the PRD](prd.md) first":         "see-the-prd-first",
		"Ninety *seconds* to a URL":           "ninety-seconds-to-a-url",
		"Überblick":                           "überblick",
	} {
		if got := Slug(text); got != want {
			t.Errorf("Slug(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestChecker(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/guide.md", "# Guide\n\n## Set up\n\n## Set up\n\n<a id=\"custom\"></a>\n")
	write("assets/logo.svg", "<svg/>")
	write("README.md", strings.Join([]string{
		"[ok](docs/guide.md)",
		"[ok anchor](docs/guide.md#set-up)",
		"[ok duplicate heading](docs/guide.md#set-up-1)",
		"[ok explicit anchor](docs/guide.md#custom)",
		"[ok self](#top)",
		"# Top",
		`<img src="assets/logo.svg" srcset="assets/logo.svg 1x, assets/missing@2x.svg 2x">`,
		"[external](https://example.com/nope) and [mail](mailto:team@example.com)",
		"`[not a link](nowhere.md)` in code",
		"```",
		"[also not a link](fenced.md)",
		"```",
		"[missing file](docs/nope.md)",
		"[missing anchor](docs/guide.md#tear-down)",
		"[outside](../elsewhere.md)",
		"[ref]: docs/gone.md",
	}, "\n"))

	problems := newChecker(root, []string{"README.md"}).run()
	want := []string{
		`README.md:7: "assets/missing@2x.svg" doesn't exist`,
		`README.md:13: "docs/nope.md" doesn't exist`,
		`README.md:14: "docs/guide.md#tear-down": docs/guide.md has no heading or anchor #tear-down`,
		`README.md:15: "../elsewhere.md" points outside the repository`,
		`README.md:16: "docs/gone.md" doesn't exist`,
	}
	if len(problems) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(problems), len(want), strings.Join(problems, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(problems[i], w) {
			t.Errorf("problem %d = %q, want prefix %q", i, problems[i], w)
		}
	}
}
