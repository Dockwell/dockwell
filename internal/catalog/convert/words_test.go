// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/catalog"
)

// On the server, an app keeps its store's own words and logo: 1Panel's READMEs in
// English and Chinese, with its line in Chinese; Dokploy's description and the logo
// its meta.json names; Coolify's logo by its header. What it keeps of a README is
// Markdown without HTML or images, and the logo is named for the control plane to
// read beside it (ADR 0083).
func TestOnServerKeepsAStoresWordsAndLogo(t *testing.T) {
	src := func(store Store) Source {
		return Source{Name: store.Name, Repository: "https://example.com/store", Commit: strings.Repeat("a", 40), License: store.License}
	}
	onePanel := storeOf(t, FormatOnePanel)
	run, err := OnServer(t.Context(), onePanel, listingOf(t, onePanel, "testdata/onepanel", nil), src(onePanel), committed, 0)
	if err != nil {
		t.Fatal(err)
	}
	notes := resultsByID(run)["notes"]
	if notes.Entry == nil {
		t.Fatalf("notes was left out: %s", notes.LeftOut)
	}
	if notes.Icon != "apps/notes/logo.png" {
		t.Errorf("notes' logo is at %q", notes.Icon)
	}
	readme := notes.Entry.GetReadme()
	for _, want := range []string{"## Introduction", "**Notes** keeps your notes. Reach it at `http://<server-ip>:port`.", "- Plain text, with Markdown", "echo \"<kept as it is>\""} {
		if !strings.Contains(readme, want) {
			t.Errorf("its README lost %q:\n%s", want, readme)
		}
	}
	for _, gone := range []string{"<div", "<img", "example.com/shot.png", "![", "<b>"} {
		if strings.Contains(readme, gone) {
			t.Errorf("its README kept %q:\n%s", gone, readme)
		}
	}
	zh := notes.Entry.GetTranslations()["zh-CN"]
	if zh.GetSummary() != "转换器测试用的替身应用" || !strings.Contains(zh.GetReadme(), "**Notes** 是一个笔记应用。") {
		t.Errorf("its words in Chinese = %v", zh)
	}
	if len(notes.Entry.GetTranslations()) != 1 {
		t.Errorf("it keeps words in %d languages, where the panel speaks one beside English", len(notes.Entry.GetTranslations()))
	}
	if err := catalog.CheckTagged(notes.Entry); err != nil {
		t.Errorf("its entry fails the catalog's check: %v", err)
	}
	// Kept as a file, as the control plane keeps a converted commit, it reads back whole.
	raw, err := Encode(notes.Entry, src(onePanel), "2026-11-12")
	if err != nil {
		t.Fatal(err)
	}
	back, err := catalog.ParseTagged(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.GetReadme() != readme || back.GetTranslations()["zh-CN"].GetReadme() != zh.GetReadme() {
		t.Errorf("what's kept reads back as %v", back)
	}

	dokploy := storeOf(t, FormatDokploy)
	run, err = OnServer(t.Context(), dokploy, listingOf(t, dokploy, "testdata/dokploy", nil), src(dokploy), committed, 0)
	if err != nil {
		t.Fatal(err)
	}
	wiki := resultsByID(run)["wiki"]
	if wiki.Entry == nil {
		t.Fatalf("wiki was left out: %s", wiki.LeftOut)
	}
	if wiki.Icon != "blueprints/wiki/wiki.svg" || wiki.Entry.GetDescription() != "A wiki for a team. It keeps pages in Markdown.\n\nIt runs anywhere Dokploy does." ||
		wiki.Entry.GetSummary() != "A wiki for a team." {
		t.Errorf("wiki: logo %q, entry %v", wiki.Icon, wiki.Entry)
	}

	coolify := storeOf(t, FormatCoolify)
	run, err = OnServer(t.Context(), coolify, listingOf(t, coolify, "testdata/coolify", nil), src(coolify), committed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if board := resultsByID(run)["board"]; board.Entry == nil || board.Icon != "public/svgs/board.svg" {
		t.Errorf("board: logo %q, left out %q", board.Icon, board.LeftOut)
	}
}

// A store names its logo, so a name that leaves its folder, or names a file that
// isn't an image, or isn't there, gives the app no logo.
func TestAStoreLogoStaysInItsFolder(t *testing.T) {
	dokploy := storeOf(t, FormatDokploy)
	l := listingOf(t, dokploy, "testdata/dokploy", map[string]string{"blueprints/LICENSE.png": "x", "blueprints/wiki/run.sh": "x"})
	tr := &tree{mem: l}
	for name, want := range map[string]string{
		"wiki.svg":                        "blueprints/wiki/wiki.svg",
		"../LICENSE.png":                  "",
		"..png":                           "",
		"run.sh":                          "",
		"missing.png":                     "",
		"/etc/passwd.png":                 "",
		"sub/wiki.svg":                    "",
		"WIKI.SVG":                        "",
		strings.Repeat("a", 200) + ".png": "",
	} {
		if got := storeIcon(tr, "blueprints/wiki", name); got != want {
			t.Errorf("storeIcon(%q) = %q, want %q", name, got, want)
		}
	}
	if got := IconName("my-app", "apps/x/Logo.PNG"); got != "my-app.png" {
		t.Errorf("IconName = %q", got)
	}
}

func TestReadme(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"HTML runs to a blank line": {"a\n\n<table>\n<tr><td>x</td></tr>\n</table>\n\nb", "a\n\nb"},
		"an autolink isn't HTML":    {"<https://example.com>", "<https://example.com>"},
		"a tag inside a line goes":  {"one <br> two <span class=\"x\">three</span>", "one  two three"},
		"code spans keep brackets":  {"run `a <b> c` and <i>d</i>", "run `a <b> c` and d"},
		"fences keep everything":    {"```\n<div>\n![x](y)\n```", "```\n<div>\n![x](y)\n```"},
		"images go":                 {"see ![a](https://example.com/a.png) here\n\n![b](b.png)", "see  here"},
		"blank lines collapse":      {"a\n\n\n\n\nb", "a\n\nb"},
		"controls go":               {"a\x00b\x1b[31mc\u2028d\r\ne", "ab[31mcd\ne"},
		"invalid UTF-8 goes":        {"a\xffb", "ab"},
	} {
		if got := Readme(c.in); got != c.want {
			t.Errorf("%s: Readme(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
	long := strings.Repeat("A line of a README that goes on.\n", 2000)
	got := Readme(long)
	if len(got) > catalog.MaxReadme || !strings.HasSuffix(got, "\n\n…") || !strings.HasSuffix(strings.TrimSuffix(got, "\n\n…"), "on.") {
		t.Errorf("a long README is cut to %d bytes, ending %q", len(got), got[len(got)-20:])
	}
	if got := Readme(strings.Repeat("长", catalog.MaxReadme)); len(got) > catalog.MaxReadme || !strings.HasSuffix(got, "…") {
		t.Errorf("a README of one long line is cut to %d bytes", len(got))
	}
}

func TestReadmeTag(t *testing.T) {
	for text, want := range map[string]string{
		"## 产品介绍\n\n**Memos** 是一款隐私优先的轻量级笔记服务":            "zh-CN",
		"## Introduction\n\nMemos is a note service, 备忘录": "",
		"": "",
	} {
		if got := readmeTag(text); got != want {
			t.Errorf("readmeTag(%q) = %q, want %q", text, got, want)
		}
	}
}
