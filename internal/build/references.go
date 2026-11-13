// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/dockwell/dockwell/internal/registry"
)

// dockerfileImages is what a Dockerfile asks BuildKit to pull, as a build that
// carries registry logins needs to know (ADR 0086): every image its FROM lines,
// COPY and ADD --from and RUN --mount from= name, past the names of its own
// stages, with build arguments put in; and the frontend its syntax directive names,
// if any. It reads the Dockerfile as BuildKit does: parser directives first, lines
// joined where they end with the escape character, comments skipped.
type dockerfileImages struct {
	images []string
	syntax string
}

// errUnresolvedImage is a Dockerfile that names an image by a build argument this
// build doesn't give, so nothing says what it pulls.
var errUnresolvedImage = errors.New("names an image by a build argument it isn't given")

// The patterns a Dockerfile is read with, made on first use, as the agent makes
// none of them until a build with logins reads one.
var (
	// directiveLine is a parser directive after its comment mark, as BuildKit reads one.
	directiveLine = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)
	})
	varRef = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?[-+][^}]*)?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
	})
	heredocStart = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`<<(-?)(["']?)([A-Za-z_][A-Za-z0-9_]*)(["']?)`)
	})
)

// stageIndex says whether a --from names an earlier stage by its number.
func stageIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// heredocs are the words that end each heredoc an instruction starts, in order, with
// a "-" before one whose lines may start with tabs. Only RUN, COPY and ADD take them.
func heredocs(ins string) []string {
	f := strings.Fields(ins)
	if len(f) == 0 {
		return nil
	}
	switch strings.ToUpper(f[0]) {
	case "RUN", "COPY", "ADD":
	default:
		return nil
	}
	var out []string
	for _, m := range heredocStart().FindAllStringSubmatch(ins, -1) {
		out = append(out, m[1]+m[3])
	}
	return out
}

// directives are the parser directives at the top of lines, each a comment that
// starts with mark, read as BuildKit v0.34.1 reads them (frontend/dockerfile/parser):
// they end at the first line that isn't one, at one BuildKit doesn't know, or at one
// said twice.
func directives(lines []string, mark string) map[string]string {
	out := map[string]string{}
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, mark)
		if !ok {
			break
		}
		m := directiveLine().FindStringSubmatch(strings.TrimLeftFunc(rest, unicode.IsSpace))
		if m == nil {
			break
		}
		k := strings.ToLower(m[1])
		if _, seen := out[k]; seen || (k != "syntax" && k != "escape" && k != "check") {
			break
		}
		out[k] = m[2]
	}
	return out
}

// syntaxOf is the frontend text names, as BuildKit's Dockerfile frontend finds it
// (parser.DetectSyntax): past a first line that starts with #!, a # syntax
// directive, or else a // one, or else a "syntax" in a JSON document; the first word
// of what it says.
func syntaxOf(text string, lines []string) string {
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		lines = lines[1:]
		_, text, _ = strings.Cut(text, "\n")
	}
	v, ok := directives(lines, "#")["syntax"]
	if !ok {
		v, ok = directives(lines, "//")["syntax"]
	}
	if !ok {
		var doc map[string]any
		if json.Unmarshal([]byte(text), &doc) == nil {
			v, _ = doc["syntax"].(string)
		}
	}
	first, _, _ := strings.Cut(v, " ")
	return first
}

// readDockerfileImages reads text, with args as the build's own build arguments.
func readDockerfileImages(text string, args map[string]string) (dockerfileImages, error) {
	var out dockerfileImages
	text = strings.TrimPrefix(text, "\uFEFF")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out.syntax = syntaxOf(text, lines)
	escape := byte('\\')
	if directives(lines, "#")["escape"] == "`" {
		escape = '`'
	}
	i := 0
	var instructions []string
	var cur strings.Builder
	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if cur.Len() > 0 && strings.HasPrefix(trimmed, "#") {
			// A comment inside a continued instruction is dropped.
			continue
		}
		if strings.HasSuffix(strings.TrimRight(line, " \t"), string(escape)) {
			l := strings.TrimRight(line, " \t")
			cur.WriteString(l[:len(l)-1])
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(line)
		ins := cur.String()
		instructions = append(instructions, ins)
		cur.Reset()
		// A heredoc's lines are what it holds, not instructions, up to the line that
		// ends each, as BuildKit reads them.
		for _, end := range heredocs(ins) {
			for i++; i < len(lines); i++ {
				l := lines[i]
				if strings.HasPrefix(end, "-") {
					l = strings.TrimLeft(l, "\t")
				}
				if l == strings.TrimPrefix(end, "-") {
					break
				}
			}
		}
	}
	if cur.Len() > 0 {
		instructions = append(instructions, cur.String())
	}

	// Build arguments: the build's own first, then each ARG's default, global ones
	// before the first FROM, and those of the stage they're declared in.
	vars := map[string]string{}
	given := func(name string) (string, bool) {
		if v, ok := args[name]; ok {
			return v, true
		}
		v, ok := vars[name]
		return v, ok
	}
	expand := func(s string) string {
		return varRef().ReplaceAllStringFunc(s, func(ref string) string {
			m := varRef().FindStringSubmatch(ref)
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if v, ok := given(name); ok {
				return v
			}
			return ref
		})
	}
	stages := map[string]bool{}
	add := func(image string) error {
		image = expand(image)
		switch {
		case strings.Contains(image, "$"):
			return fmt.Errorf("its Dockerfile %w: %s", errUnresolvedImage, image)
		case strings.EqualFold(image, "scratch"), stages[strings.ToLower(image)]:
			return nil
		}
		out.images = append(out.images, image)
		return nil
	}
	for _, ins := range instructions {
		f := strings.Fields(ins)
		if len(f) == 0 {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "ARG":
			for _, kv := range f[1:] {
				name, def, ok := strings.Cut(kv, "=")
				if _, set := args[name]; ok && !set {
					vars[name] = strings.Trim(expand(def), `"'`)
				}
			}
		case "FROM":
			rest := withoutFlags(f[1:])
			if len(rest) == 0 {
				continue
			}
			if err := add(rest[0]); err != nil {
				return out, err
			}
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				stages[strings.ToLower(rest[2])] = true
			}
		case "COPY", "ADD":
			for _, flag := range f[1:] {
				if !strings.HasPrefix(flag, "--") {
					break
				}
				if from, ok := strings.CutPrefix(flag, "--from="); ok && !stageIndex(from) {
					if err := add(from); err != nil {
						return out, err
					}
				}
			}
		case "RUN":
			for _, flag := range f[1:] {
				if !strings.HasPrefix(flag, "--") {
					break
				}
				mount, ok := strings.CutPrefix(flag, "--mount=")
				if !ok {
					continue
				}
				for _, opt := range strings.Split(mount, ",") {
					if from, ok := strings.CutPrefix(opt, "from="); ok && !stageIndex(from) {
						if err := add(from); err != nil {
							return out, err
						}
					}
				}
			}
		}
	}
	return out, nil
}

// withoutFlags is what follows an instruction's --flags.
func withoutFlags(f []string) []string {
	for len(f) > 0 && strings.HasPrefix(f[0], "--") {
		f = f[1:]
	}
	return f
}

// registries are the registries the images are on, each once.
func (d dockerfileImages) registries() (map[string]bool, error) {
	out := map[string]bool{}
	for _, image := range d.images {
		ref, err := registry.Parse(image)
		if err != nil {
			return nil, fmt.Errorf("its Dockerfile names %q, which isn't an image", image)
		}
		out[ref.Registry] = true
	}
	return out, nil
}

// officialFrontend says whether a syntax directive names Docker's own Dockerfile
// frontend, docker/dockerfile on Docker Hub, which reads a Dockerfile as BuildKit's
// does, or names none.
func officialFrontend(syntax string) bool {
	if syntax == "" {
		return true
	}
	ref, err := registry.Parse(syntax)
	return err == nil && ref.Registry == registry.Hub && ref.Repository == "docker/dockerfile"
}
