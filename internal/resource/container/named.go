// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"cmp"
	"maps"
	"regexp"
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
//   - a variable whose name says it's a secret, with a value of shortest characters
//     or more, wherever its value stands, as anything else would show it;
//   - any other, and a secret's shorter value, only where its value is a whole
//     word, or the whole of what follows = in a word, and never where a program's
//     name goes, so a variable can't come to change what runs.
//
// In a shell's script the name stays for the shell, quoted for where it stands, so
// a value is named there only where the shell reads "${NAME}" as exactly the value:
// one of plain characters, not after a backslash, a $ and a name, or inside ${…},
// not after a # on its line, and in no script with a here-document. The script as
// it will run is then read word by word against the script as it ran, and must
// come to the same words. Whatever can't be named stays as it was, and
// valuesShown refuses the container for it, as before: the worst case is the one
// before this.

// scriptPlain is a value a shell reads as the word it is, wherever it stands in a
// script: nothing it expands, quotes, splits or globs.
var scriptPlain = lazyre.Regexp(`^[A-Za-z0-9_@%+=:,./-]+$`)

// envName is a variable's name a field can write as ${NAME}.
var envName = lazyre.Regexp(`^[A-Za-z_][A-Za-z0-9_]*$`)

// candidate is a variable whose value a field may name.
type candidate struct {
	name, value string
	secret      bool
	// anywhere says the value is named wherever it stands: a secret's, of
	// shortest characters or more.
	anywhere bool
}

// nameValues writes, in each of spec's command, entrypoint and health check's test
// that doesn't name values already, each value of its environment it may name as
// that value's name, and lists the field in references. A field whose words
// wouldn't come back to what they were, or whose script wouldn't run as it did, is
// left as it was.
func nameValues(spec *resourcev1.Container) {
	cands := candidates(spec.GetEnv())
	if len(cands) == 0 {
		return
	}
	scripts := map[string]int{}
	for _, field := range binding.ReferenceFields {
		scripts[field] = binding.ScriptIn(spec, field)
	}
	shell := shellWords(spec)
	before := map[string][]string{}
	for _, field := range binding.ReferenceFields {
		words := binding.WordsOf(spec, field)
		if len(words) == 0 || slices.Contains(spec.GetReferences(), field) {
			continue
		}
		script := scripts[field]
		args := argumentWords(field, words)
		out := make([]string, len(words))
		named := map[string]string{}
		for i, w := range words {
			var spans []span
			switch {
			case i < len(shell[field]) && shell[field][i]:
				// The shell and its options: a name there would move the script.
			case i == script:
				spans = scriptSpans(w, cands)
			default:
				spans = wordSpans(w, cands, args[i])
			}
			for _, s := range spans {
				named[s.name] = spec.GetEnv()[s.name]
			}
			out[i] = writeNamed(w, spans)
		}
		if len(named) == 0 {
			continue
		}
		// What it names must come back to exactly what Docker has, the value written
		// into each word, as a container docker compose made reads back, and a
		// script must run as it did, with its names for the shell.
		if back, err := binding.Render(out, spec.GetEnv()); err != nil || !slices.Equal(back, words) {
			continue
		}
		if script >= 0 && !sameScript(words[script], out, script, named) {
			continue
		}
		before[field] = words
		setWords(spec, field, out)
		spec.References = append(spec.References, field)
	}
	// Where a script is mustn't move: the value would then be written into the
	// command line. Should it, nothing is named.
	for field := range scripts {
		if binding.ScriptIn(spec, field) != scripts[field] {
			for f, words := range before {
				setWords(spec, f, words)
			}
			spec.References = slices.DeleteFunc(spec.References, func(f string) bool { return before[f] != nil })
			return
		}
	}
}

// shellWords marks, by field, the words of a shell and its options before its
// script, the word sh -c reads, across the entrypoint and the command as Docker
// runs one after the other, or in a health check's test.
func shellWords(spec *resourcev1.Container) map[string][]bool {
	out := map[string][]bool{}
	entrypoint, command := spec.GetEntrypoint(), spec.GetCommand()
	at := -1
	if s := binding.ScriptIn(spec, binding.FieldEntrypoint); s >= 0 {
		at = s
	} else if s := binding.ScriptIn(spec, binding.FieldCommand); s >= 0 {
		at = len(entrypoint) + s
	}
	if at >= 0 {
		out[binding.FieldEntrypoint] = make([]bool, len(entrypoint))
		out[binding.FieldCommand] = make([]bool, len(command))
		for i := range at {
			if i < len(entrypoint) {
				out[binding.FieldEntrypoint][i] = true
			} else {
				out[binding.FieldCommand][i-len(entrypoint)] = true
			}
		}
	}
	if s := binding.ScriptIn(spec, binding.FieldTest); s >= 0 {
		test := make([]bool, len(spec.GetHealthcheck().GetTest()))
		for i := 1; i < s; i++ {
			test[i] = true
		}
		out[binding.FieldTest] = test
	}
	return out
}

// setWords puts words in field of spec.
func setWords(spec *resourcev1.Container, field string, words []string) {
	switch field {
	case binding.FieldCommand:
		spec.Command = words
	case binding.FieldEntrypoint:
		spec.Entrypoint = words
	case binding.FieldTest:
		spec.Healthcheck.Test = words
	}
}

// sameScript reports a script, named as out has it, that a shell reads as the
// same words as it read the script as it ran, with the names in named for their
// values.
func sameScript(was string, out []string, script int, named map[string]string) bool {
	rendered, err := binding.RenderField(out, named, script)
	if err != nil {
		return false
	}
	before, ok := scriptTokens(was, nil)
	if !ok {
		return false
	}
	after, ok := scriptTokens(rendered[script], named)
	return ok && slices.Equal(before, after)
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
		out = append(out, candidate{name: name, value: value, secret: secret, anywhere: secret && len(value) >= shortest})
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

// argumentWords marks the words of a field that are a program's arguments, where
// an ordinary value may be named: of a command or an entrypoint, as arguments
// reads them; of a health check's test, those after CMD's program.
func argumentWords(field string, words []string) []bool {
	if field != binding.FieldTest {
		return arguments(words, nil)
	}
	out := make([]bool, len(words))
	if len(words) > 1 && words[0] == "CMD" {
		copy(out[1:], arguments(words[1:], nil))
	}
	return out
}

// wordSpans finds the values a word that isn't a script names: a secret's of
// shortest characters or more anywhere, any other only as the whole word, or the
// whole of what follows its first =, of a word that's a program's argument.
func wordSpans(w string, cands []candidate, arg bool) []span {
	var out []span
	for _, c := range cands {
		if c.anywhere {
			out = addAll(out, w, c, nil)
			continue
		}
		switch {
		case !arg:
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
// characters, where the shell reads "${NAME}" as the value (scriptSafe), and only
// of a variable the script doesn't set, read or unset itself; a secret's of
// shortest characters or more anywhere, any other only as a word of the script
// that's a program's argument, or the whole of what follows its first =. A script
// with a here-document, a $( or a backquote names nothing: what's in one isn't
// read as the words around it, and quotes inside one aren't the ones outside.
func scriptSpans(script string, cands []candidate) []span {
	if strings.Contains(script, "<<") || strings.Contains(script, "$(") || strings.Contains(script, "`") {
		return nil
	}
	var out []span
	words := scriptWords(script)
	for _, c := range cands {
		if !scriptPlain().MatchString(c.value) || scriptSets(script, c.name) {
			continue
		}
		if c.anywhere {
			out = addAll(out, script, c, func(at int) bool { return scriptSafe(script, at) })
			continue
		}
		for _, sw := range words {
			text := script[sw.at:sw.end]
			switch {
			case !sw.arg:
			case text == c.value && scriptSafe(script, sw.at):
				out = addSpan(out, span{sw.at, sw.end, c.name})
			default:
				if eq := strings.IndexByte(text, '='); eq >= 0 && text[eq+1:] == c.value && scriptSafe(script, sw.at+eq+1) {
					out = addSpan(out, span{sw.at + eq + 1, sw.end, c.name})
				}
			}
		}
	}
	return out
}

// scriptSets reports a script that may set, read or unset a variable itself, so
// what "${NAME}" reads there is no longer the value: NAME=, or NAME named after
// unset, read, export and their kin, or a for loop.
func scriptSets(script, name string) bool {
	q := regexp.QuoteMeta(name)
	assigns := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + q + `(\[[^]]*\])?\+?=`)
	names := regexp.MustCompile(`\b(unset|read|readarray|mapfile|local|declare|typeset|readonly|export|getopts|for|select)\b[^;&|\n]*\b` + q + `\b`)
	return assigns.MatchString(script) || names.MatchString(script)
}

// scriptSafe reports a place in a script where "${NAME}" reads as the value would:
// not after a backslash, a $, or a $ and a name, not inside ${…}, and not after a #
// on its line, which may start a comment.
func scriptSafe(s string, at int) bool {
	if at > 0 && (s[at-1] == '\\' || s[at-1] == '$') {
		return false
	}
	i := at
	for i > 0 && isNameByte(s[i-1]) {
		i--
	}
	if i > 0 && i < at && s[i-1] == '$' {
		return false
	}
	if open := strings.LastIndex(s[:at], "${"); open >= 0 && !strings.Contains(s[open:at], "}") {
		return false
	}
	line := strings.LastIndexByte(s[:at], '\n') + 1
	return !strings.Contains(s[line:at], "#")
}

func isNameByte(c byte) bool {
	return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// scriptWord is a word of a script, without the quotes around it, whether it's
// where a redirection sends or reads, and whether it's a program's argument.
type scriptWord struct {
	at, end  int
	redirect bool
	arg      bool
}

// scriptWords splits a script into its words, roughly as a shell does: at blanks
// and at what ends a command, ;, &, |, ( and ), and a line break. A quoted word's
// quotes aren't part of it. A word after <, > and their kin, and a number of a
// file before one, are a redirection's. Each command's words are read by arguments
// for which are its program's arguments. It's rough on purpose: it only says where
// an ordinary value may stand as a word of its own, and a word it reads wrong only
// leaves a value as it was, refused as before.
func scriptWords(s string) []scriptWord {
	var out []scriptWord
	start := 0 // the first word of the command being read
	mark := func() {
		texts := make([]string, 0, len(out)-start)
		redirects := make([]bool, 0, len(out)-start)
		for _, w := range out[start:] {
			texts = append(texts, s[w.at:w.end])
			redirects = append(redirects, w.redirect)
		}
		for i, a := range arguments(texts, redirects) {
			out[start+i].arg = a
		}
		start = len(out)
	}
	redirect := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '<' || c == '>' || c == '&' && i+1 < len(s) && s[i+1] == '>':
			// A number of a file right before it is the redirection's, as 2> is.
			if n := len(out); n > start && out[n-1].end == i && strings.Trim(s[out[n-1].at:out[n-1].end], "0123456789") == "" {
				out[n-1].redirect = true
			}
			redirect = true
			for i++; i < len(s) && strings.IndexByte("<>&|", s[i]) >= 0; i++ {
			}
		case strings.IndexByte(";&|()\n", c) >= 0:
			mark()
			redirect = false
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
			out = append(out, scriptWord{at: at, end: last, redirect: redirect})
			redirect = false
			i = end
		}
	}
	mark()
	return out
}

// shellKeywords leave the next word where a command's program goes.
var shellKeywords = []string{"!", "if", "then", "else", "elif", "do", "while", "until", "{"}

// assignment is a NAME=value word before a command, which leaves the next word
// where its program goes.
var assignment = lazyre.Regexp(`^[A-Za-z_][A-Za-z0-9_]*=`)

// runsAnother are programs that run another, or read a script, or are a shell's
// own words for doing so, so what follows them may be a program's name: none of
// their words is read as an argument.
var runsAnother = []string{
	"exec", "env", "nice", "nohup", "setsid", "timeout", "time", "xargs", "chroot", "sudo", "doas", "su", "runuser",
	"gosu", "su-exec", "s6-setuidgid", "chpst", "setpriv", "capsh", "tini", "dumb-init", "catatonit", "busybox",
	"command", "builtin", "eval", "source", ".", "ionice", "stdbuf", "taskset", "flock", "watch", "strace", "ltrace",
	"unshare", "nsenter", "for", "case", "select", "function",
	"sh", "bash", "ash", "dash", "zsh", "ksh", "mksh",
}

// arguments marks the words of a command that are its program's arguments: those
// after its program, the first word past shell keywords, NAME=value words and
// redirections, and none of a redirection's. A program that runs another, or one
// whose word holds a quote, a $ or a =, which can't be read for sure, has none.
func arguments(words []string, redirects []bool) []bool {
	out := make([]bool, len(words))
	at := -1
	for i, w := range words {
		if i < len(redirects) && redirects[i] || slices.Contains(shellKeywords, w) || assignment().MatchString(w) {
			continue
		}
		at = i
		break
	}
	if at < 0 {
		return out
	}
	program := words[at]
	if slices.Contains(runsAnother, program[strings.LastIndexByte(program, '/')+1:]) || strings.ContainsAny(program, "$`\"'\\=") {
		return out
	}
	for i := at + 1; i < len(words); i++ {
		out[i] = i >= len(redirects) || !redirects[i]
	}
	return out
}

// addAll adds each place a candidate's value stands in w, where ok says it may, or
// everywhere for a nil ok.
func addAll(out []span, w string, c candidate, ok func(at int) bool) []span {
	for from := 0; ; {
		i := strings.Index(w[from:], c.value)
		if i < 0 {
			return out
		}
		at := from + i
		if ok == nil || ok(at) {
			out = addSpan(out, span{at, at + len(c.value), c.name})
		}
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

// scriptTokens reads a script's words as sh does, as far as quoting and a $ go:
// quotes come off, a backslash keeps what follows as sh keeps it, ${NAME} for a
// name in values is its value, and any other $ form is kept as a mark of its own,
// a $NAME with the whole of its name. What ends a command is a word of its own.
// False for what it can't read, as a quote that isn't closed.
func scriptTokens(s string, values map[string]string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	in := false
	flush := func() {
		if in {
			out = append(out, cur.String())
			cur.Reset()
			in = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case strings.IndexByte(";&|()<>\n", c) >= 0:
			flush()
			out = append(out, string(c))
		case c == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
			in = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			in = true
		case c == '"':
			in = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				switch {
				case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0:
					i++
					cur.WriteByte(s[i])
				case s[i] == '$':
					i += dollar(s[i:], values, &cur) - 1
				default:
					cur.WriteByte(s[i])
				}
			}
			if i >= len(s) {
				return nil, false
			}
		case c == '$':
			i += dollar(s[i:], values, &cur) - 1
			in = true
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	flush()
	return out, true
}

// dollar writes what a $ at the start of s reads as, and says how many bytes it
// took: ${NAME}'s value for a name in values, or a mark that stands for the form
// itself, ${…} or $NAME with the whole of its name, or a $ of its own.
func dollar(s string, values map[string]string, b *strings.Builder) int {
	if strings.HasPrefix(s, "${") {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			b.WriteString("\x00" + s)
			return len(s)
		}
		if v, ok := values[s[2:end]]; ok {
			b.WriteString(v)
		} else {
			b.WriteString("\x00" + s[:end+1])
		}
		return end + 1
	}
	n := 1
	for n < len(s) && isNameByte(s[n]) {
		n++
	}
	if n > 1 {
		b.WriteString("\x00" + s[:n])
		return n
	}
	b.WriteByte('$')
	return 1
}
