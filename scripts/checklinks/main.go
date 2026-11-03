// SPDX-License-Identifier: AGPL-3.0-only

// Command checklinks fails when a Markdown file in the repository links to a
// file or heading that doesn't exist. External links are not fetched.
//
//	go run ./scripts/checklinks
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

func main() {
	files, err := markdownFiles(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "checklinks:", err)
		os.Exit(2)
	}
	c := newChecker(".", files)
	problems := c.run()
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "%d broken link(s) in %d Markdown files. Fix the target or the anchor.\n", len(problems), len(files))
		os.Exit(1)
	}
}

// markdownFiles lists tracked and new, unignored Markdown files.
func markdownFiles(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.md").Output()
	if err != nil {
		return nil, fmt.Errorf("can't list Markdown files with git: %w", err)
	}
	var files []string
	for f := range bytes.SplitSeq(out, []byte{0}) {
		if name := string(f); name != "" && !strings.Contains(name, "node_modules/") {
			if _, err := os.Stat(name); err == nil {
				files = append(files, name)
			}
		}
	}
	return files, nil
}

var (
	inlineLink = regexp.MustCompile(`!?\[(?:[^\[\]]|\[[^\]]*\])*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	refLink    = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+.*)?$`)
	htmlAttr   = regexp.MustCompile(`\b(?:href|src|srcset)="([^"]+)"`)
	htmlAnchor = regexp.MustCompile(`<a\s[^>]*\b(?:id|name)="([^"]+)"`)
	heading    = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	codeSpan   = regexp.MustCompile("`+[^`]*`+")
	mdLinkText = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	scheme     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// checker resolves links in files, given as slash paths relative to root.
type checker struct {
	root    string
	files   []string
	anchors map[string]map[string]bool
}

func newChecker(root string, files []string) *checker {
	return &checker{root: root, files: files, anchors: map[string]map[string]bool{}}
}

func (c *checker) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.root, filepath.FromSlash(name)))
}

func (c *checker) exists(name string) bool {
	_, err := os.Stat(filepath.Join(c.root, filepath.FromSlash(name)))
	return err == nil
}

// link is one reference found in a file.
type link struct {
	line   int
	target string
}

func (c *checker) run() []string {
	var problems []string
	for _, file := range c.files {
		data, err := c.read(file)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: can't read: %v", file, err))
			continue
		}
		for _, l := range extractLinks(data) {
			if msg := c.check(file, l.target); msg != "" {
				problems = append(problems, fmt.Sprintf("%s:%d: %s", file, l.line, msg))
			}
		}
	}
	return problems
}

func (c *checker) check(from, target string) string {
	if target == "" || scheme.MatchString(target) || strings.HasPrefix(target, "//") {
		return "" // external, or nothing to check
	}
	pathPart, fragment, _ := strings.Cut(target, "#")
	decoded, err := url.PathUnescape(pathPart)
	if err != nil {
		return fmt.Sprintf("%q is not a valid path", target)
	}

	resolved := from
	if decoded != "" {
		resolved = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(from), decoded)))
		if strings.HasPrefix(resolved, "../") {
			return fmt.Sprintf("%q points outside the repository", target)
		}
		if !c.exists(resolved) {
			return fmt.Sprintf("%q doesn't exist (resolved to %s)", target, resolved)
		}
	}
	if fragment == "" || !strings.HasSuffix(resolved, ".md") {
		return ""
	}
	anchors, err := c.anchorsOf(resolved)
	if err != nil {
		return fmt.Sprintf("can't read %s for anchors: %v", resolved, err)
	}
	if !anchors[strings.ToLower(fragment)] {
		return fmt.Sprintf("%q: %s has no heading or anchor #%s", target, resolved, fragment)
	}
	return ""
}

func (c *checker) anchorsOf(file string) (map[string]bool, error) {
	if a, ok := c.anchors[file]; ok {
		return a, nil
	}
	data, err := c.read(file)
	if err != nil {
		return nil, err
	}
	a := collectAnchors(data)
	c.anchors[file] = a
	return a, nil
}

// extractLinks returns every link target outside code, with its line number.
func extractLinks(data []byte) []link {
	var links []link
	forEachProseLine(data, func(n int, line string) {
		line = codeSpan.ReplaceAllString(line, "")
		for _, m := range inlineLink.FindAllStringSubmatch(line, -1) {
			links = append(links, link{n, m[1]})
		}
		if m := refLink.FindStringSubmatch(line); m != nil {
			links = append(links, link{n, m[1]})
		}
		for _, m := range htmlAttr.FindAllStringSubmatch(line, -1) {
			// srcset may list several candidates: "a.svg 1x, b.svg 2x".
			for cand := range strings.SplitSeq(m[1], ",") {
				if f := strings.Fields(cand); len(f) > 0 {
					links = append(links, link{n, f[0]})
				}
			}
		}
	})
	return links
}

// collectAnchors returns the GitHub-style anchors of every heading, plus
// explicit <a id> and <a name> anchors.
func collectAnchors(data []byte) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	forEachProseLine(data, func(_ int, line string) {
		for _, m := range htmlAnchor.FindAllStringSubmatch(line, -1) {
			anchors[strings.ToLower(m[1])] = true
		}
		m := heading.FindStringSubmatch(line)
		if m == nil {
			return
		}
		slug := Slug(m[2])
		if k := seen[slug]; k > 0 {
			anchors[fmt.Sprintf("%s-%d", slug, k)] = true
		} else {
			anchors[slug] = true
		}
		seen[slug]++
	})
	return anchors
}

// Slug turns heading text into its anchor the way GitHub does: lowercase,
// formatting and punctuation dropped, spaces turned into hyphens.
func Slug(text string) string {
	text = mdLinkText.ReplaceAllString(text, "$1")
	text = strings.NewReplacer("`", "", "*", "", "~", "").Replace(text)
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case unicode.IsLetter(r), unicode.IsNumber(r), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// forEachProseLine calls fn for each line outside fenced code blocks.
func forEachProseLine(data []byte, fn func(n int, line string)) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	fence := ""
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimLeft(line, " ")
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		fn(n, line)
	}
}
