// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"slices"
	"strings"

	"github.com/mattn/go-shellwords"

	"github.com/dockwell/dockwell/internal/resource/binding"
)

// interpolated is what reading a file's variables says beside the file it gives back.
type interpolated struct {
	exposure *exposure
	// references are what each service's fields keep by name, by the service.
	references map[string]*keptReferences
}

// keptReferences is what one service's fields keep by name.
type keptReferences struct {
	// fields are those that name values.
	fields []string
	// names are what they name.
	names []string
	// values are what variables gave those names, which become the container's
	// environment, as any value that may be a secret does. A name a service uses by
	// reference, by its label, may have none.
	values map[string]string
}

// listedUses are the names each service's label says it uses, by the service, from
// the file as it stands. A field can name one with no value here, as the value is the
// environment's, and an export writes them so. What the label means is usesOf's to
// say, once it is read, so one that can't be read names nothing here.
func listedUses(raw map[string]any) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	services, _ := raw["services"].(map[string]any)
	for name, v := range services {
		svc, _ := v.(map[string]any)
		label, _ := labelIn(svc["labels"], binding.LabelKey)
		uses, err := binding.ParseLabel(label)
		if err != nil || len(uses) == 0 {
			continue
		}
		out[name] = make(map[string]bool, len(uses))
		for _, u := range uses {
			out[name][u.As] = true
		}
	}
	return out
}

// labelIn finds a label in a file's labels, a mapping or a list of key=value.
func labelIn(labels any, key string) (string, bool) {
	switch l := labels.(type) {
	case map[string]any:
		v, ok := l[key].(string)
		return v, ok
	case []any:
		for _, e := range l {
			if s, ok := e.(string); ok {
				if k, v, found := strings.Cut(s, "="); found && k == key {
					return v, true
				}
			}
		}
	}
	return "", false
}

// referencing says whether at is a field that keeps what variables give it by name:
// one that runs, and that an approver most needs to read, so masking it isn't an
// answer (ADR 0022). Any other field holds a value as text.
func referencing(at place) (string, bool) {
	service, ok := strings.CutPrefix(at.owner, "the service ")
	return service, ok && slices.Contains(binding.ReferenceFields, at.path)
}

// referenceField reads one of those fields. It reads it as any other first, for what
// that refuses and for what it says in the end, then again with a variable's value
// landing as its name. A field that names nothing is as it always was, with $ a $ of
// its own. One that names something writes a $ of its own as $$ throughout, as a
// Compose file does.
func (in *interpolator) referenceField(service string, at place, v any) (any, error) {
	exposure := in.exposure
	in.exposure, in.listing = nil, in.listed[service]
	defer func() { in.exposure, in.listing, in.naming = exposure, nil, false }()
	literal, err := in.walk(at, v)
	if err != nil {
		return nil, err
	}
	in.naming = true
	named, err := in.walk(at, v)
	in.naming = false
	if err != nil {
		return nil, err
	}
	words := wordsOfField(named)
	names, err := binding.NamesIn(words)
	if err != nil {
		return nil, invalid("%s can't name a value: %v", at, err)
	}
	if len(names) == 0 {
		return literal, nil
	}
	// Compose splits a command written as one string into words once its variables
	// are read, so a value can make more than one. A name can't stand for that.
	if line, ok := named.(string); ok && at.path != binding.FieldTest {
		split := in.splitNamed(line, literal.(string), names)
		for _, name := range split.text {
			in.note(exposure, at, name, split.whole)
		}
		if split.words == nil {
			return literal, nil
		}
		words, named = split.words, anyOf(split.words)
		if names, err = binding.NamesIn(words); err != nil {
			return nil, invalid("%s can't name a value: %v", at, err)
		}
		if len(names) == 0 {
			return literal, nil
		}
	}
	in.keep(service, at.path, names)
	return named, nil
}

// note says a value lands in plain sight at at. whole says it does as the rest of a
// line that one value made text, and not for its own sake.
func (in *interpolator) note(x *exposure, at place, name string, whole bool) {
	if x != nil {
		if value, ok := in.variables[name]; ok && value != "" {
			x.noteText(at, name, whole && !splitsALine(value))
		}
	}
}

// keep records that a service's field names names, and what they are given.
func (in *interpolator) keep(service, field string, names []string) {
	k := in.kept[service]
	if k == nil {
		k = &keptReferences{values: map[string]string{}}
		in.kept[service] = k
	}
	k.fields = append(k.fields, field)
	k.names = append(k.names, names...)
	for _, name := range names {
		if value, ok := in.variables[name]; ok {
			k.values[name] = value
		}
	}
}

// split is what splitNamed makes of a line.
type split struct {
	// words are the line's words, with the names that stand for their values in
	// them, or nil when nothing on the line can keep its name.
	words []string
	// text names the values that are in the words as text, or all of the line's when
	// words is nil.
	text []string
	// whole says the line is all text, words and all.
	whole bool
}

// splitNamed splits a command written as one string, with its names in it, into
// words, so that they stand for what Compose would make of it with the values in.
//
// A value with a space, a quote, a backslash or another character Compose reads as
// part of the line can make more words than one, or other words than its own, and a
// name can't stand for that. That value is written into the line as text, and the
// plan says so, and every other value keeps its name, as long as the words come to
// just what Compose makes of the line. If they don't, which no file in 1Panel's store
// comes near, the file's words go on meaning what they mean, all as text, and the plan
// says that too.
//
// A line that Compose can't read with its values in, as one with a lone quote can't,
// says nothing to hold the words to, so each value stays the one word it is. Nor can
// one with a name a label says the environment holds, which is a name whatever it is
// given: a value given for it, of any shape, is refused as set twice, and doesn't
// become text to get round that.
func (in *interpolator) splitNamed(line, literal string, names []string) split {
	words, err := shellwords.Parse(line)
	if err != nil {
		return split{text: names, whole: true}
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		if in.listing[name] {
			return split{words: words}
		}
		values[name] = in.variables[name]
	}
	said, err := shellwords.Parse(literal)
	if err != nil {
		return split{words: words}
	}
	if got, err := binding.Render(words, values); err == nil && slices.Equal(got, said) {
		return split{words: words}
	}
	var text []string
	for _, name := range names {
		if splitsALine(values[name]) {
			text = append(text, name)
		}
	}
	if len(text) > 0 {
		if mixed, err := shellwords.Parse(writeIn(line, values, text)); err == nil {
			if got, err := binding.Render(mixed, values); err == nil && slices.Equal(got, said) {
				return split{words: mixed, text: text}
			}
		}
	}
	return split{text: names, whole: true}
}

// splitsALine reports a value that, written into a line, can make other words than
// its own: one with a space, a quote, a backslash, or what Compose's splitter stops
// at or takes as a command's.
func splitsALine(value string) bool {
	return strings.ContainsAny(value, " \t\r\n\\'\"`;&|<>()")
}

// writeIn writes the values of names into a line of words with names in them, as
// text, in the form those words take, with a $ of their own as $$.
func writeIn(line string, values map[string]string, names []string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		switch {
		case strings.HasPrefix(line[i:], "$$"):
			b.WriteString("$$")
			i += 2
		case strings.HasPrefix(line[i:], "${"):
			end := strings.IndexByte(line[i:], '}')
			if end < 0 {
				b.WriteString(line[i:])
				return b.String()
			}
			name := line[i+2 : i+end]
			if slices.Contains(names, name) {
				b.WriteString(strings.ReplaceAll(values[name], "$", "$$"))
			} else {
				b.WriteString(line[i : i+end+1])
			}
			i += end + 1
		default:
			b.WriteByte(line[i])
			i++
		}
	}
	return b.String()
}

// wordsOfField lists the words of a field as it stands: a string, or a list of them.
func wordsOfField(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// anyOf is a list of words as a read file holds one.
func anyOf(words []string) []any {
	out := make([]any, len(words))
	for i, w := range words {
		out[i] = w
	}
	return out
}
