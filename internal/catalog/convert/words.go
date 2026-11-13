// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// A store's own words and logo for each app, as the control plane converts it: its
// description, its README, the same in the languages the panel speaks, and where it
// keeps the app's logo, for the control plane to read beside it (ADR 0083).

// Words are what a store writes of an app in one language.
type Words struct {
	// Summary is its line about the app, Description its paragraphs as plain text,
	// and Readme its README, in Markdown.
	Summary, Description, Readme string
}

// Languages are those an entry keeps words in beside its own, English: the ones
// the panel speaks, by the tags the panel names them by.
var Languages = []string{"zh-CN"}

// iconName is how a store may name an app's logo: a file of its folder, with the
// extension of a format the control plane serves.
var iconName = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9._-]{0,127}\.(png|jpe?g|webp|svg)$`)

// storeIcon is where a store keeps an app's logo, from the store's folder, if the
// file is there and named as a logo may be: in dir, as name, where name is the
// store's own word for it, such as Dokploy's meta.json logo.
func storeIcon(t *tree, dir, name string) string {
	if !iconName.MatchString(name) || strings.Contains(name, "..") {
		return ""
	}
	p := path.Join(dir, name)
	if !t.isFile(p) {
		return ""
	}
	return p
}

// IconName is the name an entry's logo is kept by in the folder media, from where
// the store keeps it: the entry's ID, and its extension, lowercased.
func IconName(id, from string) string {
	ext := strings.ToLower(path.Ext(from))
	return id + ext
}

// ShotName is the name a screenshot is kept by in the folder media: the entry's ID,
// its place, and its extension, lowercased, such as memos_shot-2.png. An ID has no
// underscore, so no screenshot is named as an app's logo, IconName, is.
func ShotName(id string, n int, from string) string {
	return fmt.Sprintf("%s_shot-%d%s", id, n, strings.ToLower(path.Ext(from)))
}

// ShotCaption is what a screenshot from a store says it shows, where the store gives
// it no words: the app, and which of its screens it is.
func ShotCaption(app string, n int) string {
	return fmt.Sprintf("%s, screen %d", app, n)
}

// readmeTag is how a README's language reads: Chinese where Han characters are a
// tenth of its letters or more, as 1Panel names its Chinese README just README.md.
func readmeTag(text string) string {
	han, letters := 0, 0
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
			letters++
		case unicode.IsLetter(r):
			letters++
		}
	}
	if letters > 0 && han*10 >= letters {
		return "zh-CN"
	}
	return ""
}

var (
	// markdownImage is an image in Markdown, which the panel never shows, as it
	// would be another host's file.
	markdownImage = regexp.MustCompile(`!\[[^\]\n]*\]\([^)\n]*\)`)
	// inlineTag is an HTML tag inside a line of text.
	inlineTag = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(\s[^<>\n]*)?/?>`)
	// htmlBlock starts a block of HTML, which runs to the next blank line, as
	// CommonMark reads one.
	htmlBlock = regexp.MustCompile(`^ {0,3}(<[A-Za-z/!?])`)
	// autolink is a link Markdown writes in angle brackets, which isn't HTML.
	autolink = regexp.MustCompile(`^<(https?|mailto):`)
)

// Readme is a store's README as an entry keeps it: Markdown, without its HTML or
// images, which the panel would never show, with each control character but tabs
// and line breaks taken out, within catalog.MaxReadme, cut where a line ends. Fenced
// code is kept as it is.
func Readme(text string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) && r != ' ' && r != ' ' {
			return r
		}
		return -1
	}, text)
	var out []string
	fenced, inHTML := false, false
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fenced = !fenced
			inHTML = false
		case fenced:
		case inHTML:
			// A block of HTML runs to the next blank line, which stays between blocks.
			if trimmed == "" {
				inHTML = false
				out = append(out, "")
			}
			continue
		case htmlBlock.MatchString(line) && !autolink.MatchString(trimmed):
			inHTML = true
			continue
		default:
			line = outsideCode(line, func(text string) string {
				return inlineTag.ReplaceAllString(markdownImage.ReplaceAllString(text, ""), "")
			})
			if strings.TrimSpace(line) == "" && trimmed != "" {
				continue // a line that was an image or a tag alone
			}
		}
		out = append(out, line)
	}
	md := strings.Join(collapseBlank(out), "\n")
	md = strings.TrimSpace(md)
	if len(md) > catalog.MaxReadme {
		cut := strings.LastIndexByte(md[:catalog.MaxReadme-len("\n\n…")], '\n')
		if cut <= 0 {
			cut = catalog.MaxReadme - len("\n\n…")
			for cut > 0 && !utf8.RuneStart(md[cut]) {
				cut--
			}
		}
		md = strings.TrimRight(md[:cut], "\n") + "\n\n…"
	}
	return md
}

// outsideCode is a line with f applied to its text outside code spans, which keep
// what they hold as it is, angle brackets included, as `http://<host>:port` does.
func outsideCode(line string, f func(string) string) string {
	var out strings.Builder
	for {
		open := strings.IndexByte(line, '`')
		if open < 0 {
			out.WriteString(f(line))
			return out.String()
		}
		run := len(line[open:]) - len(strings.TrimLeft(line[open:], "`"))
		fence := line[open : open+run]
		end := strings.Index(line[open+run:], fence)
		if end < 0 {
			out.WriteString(f(line))
			return out.String()
		}
		out.WriteString(f(line[:open]))
		out.WriteString(line[open : open+run+end+run])
		line = line[open+run+end+run:]
	}
}

// collapseBlank leaves at most one blank line between blocks.
func collapseBlank(lines []string) []string {
	var out []string
	blank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
			l = ""
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return out
}

// description is a store's paragraphs about an app as an entry keeps them: plain
// text, one paragraph to a blank line, within catalog.MaxDescription characters.
func description(text string) string {
	var paragraphs []string
	for p := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		if p = strings.Join(strings.Fields(CommentText(p)), " "); p != "" {
			paragraphs = append(paragraphs, p)
		}
	}
	out := strings.Join(paragraphs, "\n\n")
	if r := []rune(out); len(r) > catalog.MaxDescription {
		out = string(r[:catalog.MaxDescription-1]) + "…"
	}
	return out
}

// storeWords writes a store's words into an entry: its description and README in
// English, and in each language the panel speaks, what the store writes there, as
// the store wrote it. The summary the converter wrote stays.
func storeWords(e *catalogv1.Entry, app App) {
	e.Description = description(app.Description)
	e.Readme = Readme(app.Readme)
	for _, tag := range Languages {
		w, ok := app.Words[tag]
		if !ok {
			continue
		}
		t := &catalogv1.Translation{Description: description(w.Description), Readme: Readme(w.Readme)}
		if strings.TrimSpace(w.Summary) != "" {
			t.Summary = serverSummary(w.Summary, "")
		}
		if t.GetSummary() == "" && t.GetDescription() == "" && t.GetReadme() == "" {
			continue
		}
		if e.Translations == nil {
			e.Translations = map[string]*catalogv1.Translation{}
		}
		e.Translations[tag] = t
	}
}
