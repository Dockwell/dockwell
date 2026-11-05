// SPDX-License-Identifier: AGPL-3.0-only

package binding

import (
	"cmp"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// The fields of a container that may name values rather than hold them
// (Container.references). A word in one of them is read as a Compose file reads
// it, in part: ${NAME} is the value of NAME in the container's environment, and
// $$ is a $ of its own. A $ in any other field is a $, as it always was, so a
// spec made before these has the meaning it had. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
const (
	FieldCommand    = "command"
	FieldEntrypoint = "entrypoint"
	FieldTest       = "healthcheck.test"
)

// ReferenceFields lists the fields that may name values.
var ReferenceFields = []string{FieldCommand, FieldEntrypoint, FieldTest}

// Masked stands for a value in what a person reads.
const Masked = "•••"

// Unread is what a container's word reads back as when it isn't what its label
// says it should come to. The text there may hold a value, and it can't be told
// which part, so none of it is passed on. It names no variable, so it can't be
// kept as a spec (Adopt): CheckWords refuses it.
const Unread = "(not what this container's references come to)"

// readWord goes through word, which is a word of a field that names values:
// text gets the words of its own, $$ read as $, and name each ${NAME}. It says
// which $ it can't read by what it is, and never repeats the word, which someone
// may have written a value in.
func readWord(word string, text, name func(string)) error {
	for {
		i := strings.IndexByte(word, '$')
		if i < 0 {
			text(word)
			return nil
		}
		text(word[:i])
		word = word[i:]
		switch {
		case strings.HasPrefix(word, "$$"):
			text("$")
			word = word[2:]
		case strings.HasPrefix(word, "${"):
			end := strings.IndexByte(word, '}')
			if end < 0 || !isName(word[2:end]) {
				return errors.New("a ${ that doesn't name a variable as ${NAME}. Write one so, and a $ of its own as $$")
			}
			name(word[2:end])
			word = word[end+1:]
		default:
			return errors.New("a $ that isn't ${NAME}. Write a variable as ${NAME}, and a $ of its own as $$")
		}
	}
}

// isName reports what a variable can be called, as Compose has it.
func isName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		letter := c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// NamesIn lists the variables words name, each once, sorted, and says of one it
// can't read which word it is, by position.
func NamesIn(words []string) ([]string, error) {
	var names []string
	for i, w := range words {
		err := readWord(w, func(string) {}, func(n string) { names = append(names, n) })
		if err != nil {
			return nil, fmt.Errorf("word %d has %w", i+1, err)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// Render writes words with the values they name. It reads each word once, so
// what a value holds is the value, however much it reads like a name or a $$. It
// fails, by name and never by value, on a name with no value.
func Render(words []string, values map[string]string) ([]string, error) {
	if words == nil {
		return nil, nil
	}
	out := make([]string, len(words))
	for i, w := range words {
		var b strings.Builder
		var missing string
		err := readWord(w, func(s string) { b.WriteString(s) }, func(n string) {
			v, ok := values[n]
			if !ok && missing == "" {
				missing = n
			}
			b.WriteString(v)
		})
		if err != nil {
			return nil, fmt.Errorf("word %d has %w", i+1, err)
		}
		if missing != "" {
			return nil, fmt.Errorf("there's no value for %s, which word %d names", missing, i+1)
		}
		out[i] = b.String()
	}
	return out, nil
}

// WordsOf says what a field of a container holds, for a field that may name
// values.
func WordsOf(c *resourcev1.Container, field string) []string {
	switch field {
	case FieldCommand:
		return c.GetCommand()
	case FieldEntrypoint:
		return c.GetEntrypoint()
	case FieldTest:
		return c.GetHealthcheck().GetTest()
	}
	return nil
}

// ReferencedNames lists what a container's fields name, each once, sorted. A word
// it can't read names nothing here, as Validate says what is wrong with it.
func ReferencedNames(c *resourcev1.Container) []string {
	var names []string
	for _, field := range c.GetReferences() {
		found, _ := NamesIn(WordsOf(c, field))
		names = append(names, found...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// CheckWords refuses a container's references that aren't what they should be: a
// field that can't name values, one listed twice, one that names nothing, and a
// name that known doesn't have, which is what its environment has beside what its
// service uses. It sorts them, so specs that mean the same compare equal. It never
// repeats a word.
func CheckWords(c *resourcev1.Container, known func(string) bool) error {
	slices.Sort(c.References)
	for i, field := range c.GetReferences() {
		if !slices.Contains(ReferenceFields, field) {
			return fmt.Errorf("references lists %q, and only %s can name values. Remove it", field, strings.Join(ReferenceFields, ", "))
		}
		if i > 0 && c.GetReferences()[i-1] == field {
			return fmt.Errorf("references lists %s twice. List it once", field)
		}
		names, err := NamesIn(WordsOf(c, field))
		if err != nil {
			return fmt.Errorf("the %s names values, and its %w", field, err)
		}
		if len(names) == 0 {
			return fmt.Errorf("references lists %s, and it names nothing. Write ${NAME} in it, or take it out of references", field)
		}
		for _, n := range names {
			if !known(n) {
				return fmt.Errorf("the %s names %s, and the container has no value for it. Set %s in its environment, or use a variable or secret under that name", field, n, n)
			}
		}
	}
	return nil
}

// NamedAsText says, by field, the names that read as ${NAME} in a container's fields
// that don't name values, for which known has a value: those run as the words
// ${NAME}, a password among them, which anyone who reads the spec knows. A shell's
// script, the word sh -c reads or a CMD-SHELL test, gets it from the environment as
// it runs, which is what it is for, so that isn't counted. The words after a script
// are its $0, $1 and so on, which the shell passes on as they are, so they are.
func NamedAsText(c *resourcev1.Container, known func(string) bool) map[string][]string {
	out := map[string][]string{}
	note := func(field string, words []string, script int) {
		for i, w := range words {
			if i == script {
				continue
			}
			for _, m := range spelledName.FindAllStringSubmatch(w, -1) {
				if known(m[1]) && !slices.Contains(out[field], m[1]) {
					out[field] = append(out[field], m[1])
				}
			}
		}
	}
	listed := func(field string) bool { return slices.Contains(c.GetReferences(), field) }
	// A shell's script can be in the entrypoint or in the command, as Docker runs
	// one after the other.
	entrypoint, command := c.GetEntrypoint(), c.GetCommand()
	script := shellScript(slices.Concat(entrypoint, command))
	if !listed(FieldEntrypoint) {
		note(FieldEntrypoint, entrypoint, script)
	}
	if !listed(FieldCommand) {
		note(FieldCommand, command, script-len(entrypoint))
	}
	if test := c.GetHealthcheck().GetTest(); !listed(FieldTest) && len(test) > 1 && test[0] == "CMD" {
		note(FieldTest, test[1:], shellScript(test[1:]))
	}
	for field := range out {
		slices.Sort(out[field])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// spelledName is ${NAME}, as a word that doesn't name values can spell it.
var spelledName = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// shells are what reads a script with -c, by the name of their program, as in
// busybox sh.
var shells = []string{"sh", "bash", "ash", "dash", "zsh", "ksh", "mksh"}

// shellScript is where in argv a shell's script is, the word it reads with -c, or
// -1 when no shell reads one.
func shellScript(argv []string) int {
	for i, word := range argv {
		if !slices.Contains(shells, path.Base(word)) {
			continue
		}
		if script := scriptAfter(argv, i+1); script >= 0 {
			return script
		}
	}
	return -1
}

// scriptAfter reads a shell's options from argv[from:] as sh and bash do, and says
// where its script is: the first word after them, when they include -c, as in
// sh -ec, bash -o pipefail -c, bash --login -c, or -1 when they don't. -o and -O
// each take the next word, as --rcfile, --init-file and zsh's --emulate do, and --
// or - ends them.
func scriptAfter(argv []string, from int) int {
	reads := false
	for i := from; i < len(argv); i++ {
		word := argv[i]
		switch {
		case word == "--" || word == "-":
			if reads && i+1 < len(argv) {
				return i + 1
			}
			return -1
		case strings.HasPrefix(word, "--"):
			if slices.Contains([]string{"--rcfile", "--init-file", "--emulate"}, word) {
				i++
			}
		case len(word) > 1 && (word[0] == '-' || word[0] == '+'):
			for _, flag := range word[1:] {
				switch flag {
				case 'c':
					reads = reads || word[0] == '-'
				case 'o', 'O':
					i++
				}
			}
		default:
			if reads {
				return i
			}
			return -1
		}
	}
	return -1
}

// WritesReferences reports a spec that needs an agent to write its references in:
// a service's app or a container that lists any.
func WritesReferences(spec proto.Message) bool {
	switch s := spec.(type) {
	case *resourcev1.Service:
		return len(s.GetApp().GetReferences()) > 0
	case *resourcev1.Container:
		return len(s.GetReferences()) > 0
	}
	return false
}

// shortest is the shortest value Hide masks wherever it is. A shorter one, such as
// a port or a word, is masked only where it stands alone, as a value that short
// would otherwise take the digits out of a number, or the middle out of a word.
const shortest = 6

// Hide masks each of values in text that Docker or something else wrote, for a
// container whose command holds them, before it goes anywhere a person reads. It
// also finds a value as Go's %q would write it, as a runtime's errors quote what
// they couldn't run. The longest go first, so one value within another leaves
// nothing of the longer.
func Hide(text string, values []string) string {
	sorted := slices.Clone(values)
	slices.SortFunc(sorted, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, v := range sorted {
		if v == "" {
			continue
		}
		quoted := strconv.Quote(v)
		for _, form := range []string{quoted[1 : len(quoted)-1], v} {
			if len(form) >= shortest {
				text = strings.ReplaceAll(text, form, Masked)
			} else {
				text = hideWhole(text, form)
			}
		}
	}
	return text
}

// hideWhole masks form where nothing of a word or a number touches it.
func hideWhole(text, form string) string {
	var b strings.Builder
	for {
		i := strings.Index(text, form)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(form)
		if edge(text[:i], true) || edge(text[end:], false) {
			b.WriteString(text[:end])
		} else {
			b.WriteString(text[:i] + Masked)
		}
		text = text[end:]
	}
}

// edge reports text that a word or a number runs on from, at its end or at its
// start.
func edge(text string, atEnd bool) bool {
	if text == "" {
		return false
	}
	if atEnd {
		return isWord(rune(text[len(text)-1]))
	}
	return isWord(rune(text[0]))
}

func isWord(r rune) bool { return r == '_' || r >= 0x80 || unicode.IsLetter(r) || unicode.IsDigit(r) }

// ValuesOf is the values a container's fields name, from env, for Hide. A name env
// doesn't have gives nothing.
func ValuesOf(names []string, env map[string]string) []string {
	var out []string
	for _, n := range names {
		if v, ok := env[n]; ok {
			out = append(out, v)
		}
	}
	return out
}
