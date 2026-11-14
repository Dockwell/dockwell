// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/lazyre"
	"github.com/dockwell/dockwell/internal/resource/binding"
)

// A taken-on container's values kept by name (ADR 0096). Docker has a container's
// command, entrypoint and health check as Compose made them, with the values of
// its environment in them, which a plan would show. So where it can, the take-on
// writes each such value as ${NAME}, in ADR 0022's form: the field lists itself in
// references, a $ of its own is $$, and the value goes in only as a container is
// made. A plan then shows the name.
//
// Where a value is named:
//   - a variable whose name says it's a secret, wherever its value stands, at any
//     length, as anything else would show it;
//   - any other, of shortest characters or more, only where its value is a whole
//     word, or the whole of what follows = in a word, and never where the program's
//     name goes, so a variable can't come to change what runs.
//
// In a shell's script the name stays for the shell, quoted for where it stands, so
// a value is named there only where the shell reads it as the word it is: one of
// plain characters. Whatever can't be named stays as it was, and valuesShown
// refuses the container for it, as before: the worst case is the one before this.

// scriptPlain is a value a shell reads as the word it is, wherever it stands in a
// script: nothing it expands, quotes, splits or globs.
var scriptPlain = lazyre.Regexp(`^[A-Za-z0-9_@%+=:,./-]+$`)

// envName is a variable's name a field can write as ${NAME}.
var envName = lazyre.Regexp(`^[A-Za-z_][A-Za-z0-9_]*$`)

// candidate is a variable whose value a field may name.
type candidate struct {
	name, value string
	secret      bool
}

// nameValues writes, in each of spec's command, entrypoint and health check's test
// that doesn't name values already, each value of its environment it may name as
// that value's name, and lists the field in references. A field whose words
// wouldn't come back to what they were is left as it was.
func nameValues(spec *resourcev1.Container) {
	cands := candidates(spec.GetEnv())
	if len(cands) == 0 {
		return
	}
	for _, field := range binding.ReferenceFields {
		words := binding.WordsOf(spec, field)
		if len(words) == 0 || slices.Contains(spec.GetReferences(), field) {
			continue
		}
		script := binding.ScriptIn(spec, field)
		out := make([]string, len(words))
		named := false
		for i, w := range words {
			var spans []span
			if i == script {
				spans = scriptSpans(w, cands)
			} else {
				spans = wordSpans(w, cands, programWord(field, words, i))
			}
			named = named || len(spans) > 0
			out[i] = writeNamed(w, spans)
		}
		if !named {
			continue
		}
		// What it names must come back to exactly what Docker has, the value written
		// into each word, as a container docker compose made reads back.
		if back, err := binding.Render(out, spec.GetEnv()); err != nil || !slices.Equal(back, words) {
			continue
		}
		switch field {
		case binding.FieldCommand:
			spec.Command = out
		case binding.FieldEntrypoint:
			spec.Entrypoint = out
		case binding.FieldTest:
			spec.Healthcheck.Test = out
		}
		spec.References = append(spec.References, field)
	}
}

// candidates are the variables whose values may be named, the longest value first,
// a secret's before another's of the same value, then by name.
func candidates(env map[string]string) []candidate {
	var out []candidate
	for _, name := range slices.Sorted(maps.Keys(env)) {
		value := env[name]
		secret := secretName().MatchString(name)
		if value == "" || !envName().MatchString(name) || !secret && len(value) < shortest {
			continue
		}
		out = append(out, candidate{name: name, value: value, secret: secret})
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		if c := cmp.Compare(len(b.value), len(a.value)); c != 0 {
			return c
		}
		if a.secret != b.secret {
			if a.secret {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.name, b.name)
	})
	return out
}

// span is a value in a word, named.
type span struct {
	at, end int
	name    string
}

// programWord reports the word of a field that names the program: the first of a
// command or an entrypoint, or the first after CMD in a health check's test.
func programWord(field string, words []string, i int) bool {
	switch field {
	case binding.FieldCommand, binding.FieldEntrypoint:
		return i == 0
	case binding.FieldTest:
		return i == 0 || i == 1 && words[0] == "CMD"
	}
	return false
}

// wordSpans finds the values a word that isn't a script names: a secret's
// anywhere, any other's only as the whole word, or the whole of what follows its
// first =, and none in the program's word but a secret's.
func wordSpans(w string, cands []candidate, program bool) []span {
	var out []span
	for _, c := range cands {
		if c.secret {
			out = addAll(out, w, c)
			continue
		}
		switch {
		case program:
		case w == c.value:
			out = addSpan(out, span{0, len(w), c.name})
		default:
			if eq := strings.IndexByte(w, '='); eq >= 0 && w[eq+1:] == c.value {
				out = addSpan(out, span{eq + 1, len(w), c.name})
			}
		}
	}
	return out
}

// scriptSpans finds the values a shell's script names: only one of plain
// characters, a secret's anywhere, any other's only as one of the script's words,
// or the whole of what follows its first =, and never where a command's name goes.
func scriptSpans(script string, cands []candidate) []span {
	var out []span
	words := scriptWords(script)
	for _, c := range cands {
		if !scriptPlain().MatchString(c.value) {
			continue
		}
		if c.secret {
			out = addAll(out, script, c)
			continue
		}
		for _, sw := range words {
			text := script[sw.at:sw.end]
			switch {
			case sw.program:
			case text == c.value:
				out = addSpan(out, span{sw.at, sw.end, c.name})
			default:
				if eq := strings.IndexByte(text, '='); eq >= 0 && text[eq+1:] == c.value {
					out = addSpan(out, span{sw.at + eq + 1, sw.end, c.name})
				}
			}
		}
	}
	return out
}

// scriptWord is a word of a script, without the quotes around it, and whether a
// command's name goes there.
type scriptWord struct {
	at, end int
	program bool
}

// scriptWords splits a script into its words, roughly as a shell does: at blanks
// and at what ends a command, ;, &, |, ( and ), a line break, < and >. A quoted
// word's quotes aren't part of it. The first word, and the first after what ends a
// command, is where a command's name goes. It's rough on purpose: it only says
// where an ordinary value may stand as a word of its own, and a word it reads
// wrong only leaves a value as it was, refused as before.
func scriptWords(s string) []scriptWord {
	var out []scriptWord
	program := true
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case strings.IndexByte(";&|()\n", c) >= 0:
			program = true
			i++
		case c == '<' || c == '>':
			i++
		default:
			end := i
			for end < len(s) && strings.IndexByte(" \t;&|()\n<>", s[end]) < 0 {
				end++
			}
			at, last := i, end
			if last-at >= 2 && (s[at] == '"' || s[at] == '\'') && s[last-1] == s[at] {
				at, last = at+1, last-1
			}
			out = append(out, scriptWord{at: at, end: last, program: program})
			program = false
			i = end
		}
	}
	return out
}

// addAll adds each place a candidate's value stands in w.
func addAll(out []span, w string, c candidate) []span {
	for from := 0; ; {
		i := strings.Index(w[from:], c.value)
		if i < 0 {
			return out
		}
		at := from + i
		out = addSpan(out, span{at, at + len(c.value), c.name})
		from = at + len(c.value)
	}
}

// addSpan adds s where it overlaps none already there, which are longer values
// or a secret's of the same.
func addSpan(out []span, s span) []span {
	for _, o := range out {
		if s.at < o.end && o.at < s.end {
			return out
		}
	}
	return append(out, s)
}

// writeNamed writes w with each span as its name, and each $ of its own as $$.
func writeNamed(w string, spans []span) string {
	slices.SortFunc(spans, func(a, b span) int { return cmp.Compare(a.at, b.at) })
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(strings.ReplaceAll(w[last:s.at], "$", "$$"))
		b.WriteString("${" + s.name + "}")
		last = s.end
	}
	b.WriteString(strings.ReplaceAll(w[last:], "$", "$$"))
	return b.String()
}
