// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// notYAML says a file isn't YAML, in words for people, and which lines to look
// at. The parser's own sentence helps no one who isn't writing a parser, and the
// line it names is often not the mistake's: it counts lines from 0 in a parse
// error, and from 1 in a scan error, and a colon it didn't expect is usually about
// the line before.
func notYAML(err error, file []byte) error {
	text := err.Error()
	if strings.Contains(text, "aliasing") {
		return invalid("the Compose file's aliases expand too far, which YAML calls excessive aliasing, so Dockwell doesn't read it. Write the repeated parts out, or use fewer aliases")
	}
	all := yamlLine.FindAllStringSubmatch(text, -1)
	if len(all) == 0 {
		return invalid("the Compose file isn't valid YAML. Check its indentation, and that quotes and brackets close")
	}
	named := make([]int, len(all))
	for i, m := range all {
		named[i], _ = strconv.Atoi(m[1])
	}
	first, last := named[0], named[len(named)-1]
	switch {
	case strings.Contains(text, "already defined"):
		key := yamlKey.FindStringSubmatch(text)
		if key != nil && len(named) > 1 {
			return atLines(first, first, "`%s` is set twice, here and on line %d. Keep one of them", key[1], named[1])
		}
	case strings.Contains(text, "while scanning a quoted scalar"):
		return atLines(first, first, "this quote doesn't close. Close it, or escape the quote inside it")
	case strings.Contains(text, "while parsing a flow"):
		// It finds out at what comes after, the file's end as often as not.
		end := bytes.Count(file, []byte("\n"))
		if !bytes.HasSuffix(file, []byte("\n")) {
			end++
		}
		to := max(min(last+1, end), 1)
		from := max(min(last, to), 1)
		if len(named) > 1 {
			from = min(first+1, to)
		}
		return atLines(from, to, "a bracket doesn't close. Close it, and check the commas between what is in it")
	case strings.HasPrefix(text, "yaml: while parsing"):
		return atLines(last+1, last+1, "this line doesn't line up with the ones around it. Check its indentation")
	case strings.Contains(text, "mapping values are not allowed"):
		return atLines(max(last-1, 1), last, "check for a missing colon on the first, or too much indentation on the second")
	}
	return atLines(last, last, "check the indentation, and that quotes and brackets close")
}

// atLines is a refusal about a file's lines, from 1, which says them first.
func atLines(from, to int, format string, args ...any) error {
	where := fmt.Sprintf("line %d", from)
	if to > from {
		where = fmt.Sprintf("near lines %d\u2013%d", from, to)
	}
	return &refusal{error: invalid(where+": "+format, args...), from: from, to: to}
}

// refusal is an ErrInvalid that knows the place in the file it is about, so
// a form can point at the line: a place, or the lines themselves.
type refusal struct {
	error
	at       place
	from, to int
}

func (r *refusal) Unwrap() error { return r.error }

func invalidAt(at place, format string, args ...any) error {
	return &refusal{error: invalid(format, args...), at: at}
}

// refusedAt says where in a file an ErrInvalid from further on is about.
func refusedAt(at place, err error) error {
	var known *refusal
	if err == nil || errors.As(err, &known) {
		return err
	}
	return &refusal{error: err, at: at}
}

// Variable is a variable a Compose file uses.
type Variable struct {
	Name string
	// Required is true when the file gives it no default, so it needs a
	// value. A variable used both ways needs one.
	Required bool
}

// Check reads a file as Parse does, and says what it says of itself too, for a
// form to show as someone types one. It takes one turn at reading, and all of
// that reading is the child's, so its limits cover it: a file made to cost too
// much is stopped there, and says nothing of itself. Its error is what Parse's
// is.
func Check(ctx context.Context, file []byte, given map[string]string) (Inspection, *Stack, error) {
	seen, stack, err := read(ctx, file, given, true)
	if err != nil && !errors.Is(err, ErrInvalid) || errors.Is(err, ErrCostly) {
		return Inspection{}, nil, err
	}
	return seen, stack, err
}

// Inspection is what a file says of itself, whether or not Parse would take
// it: what a form shows as someone types one.
type Inspection struct {
	// Name is what its name: calls it. Empty for a file with none, or one
	// named by a variable that has no value yet.
	Name string `json:"name,omitempty"`
	// Variables are the variables it uses, by name.
	Variables []Variable `json:"variables,omitempty"`
	// Exposed says where it writes a value in plain sight, whether or not one
	// has been given.
	Exposed []Exposed `json:"exposed,omitempty"`
	// ExposedMore says it writes more than Exposed lists.
	ExposedMore bool `json:"exposed_more,omitempty"`
	// Refused says why Parse would refuse the file once its variables have values,
	// where that's nothing to do with their values, so a form can say so before
	// anyone types a secret for a file it then refuses.
	Refused *Refused `json:"refused,omitempty"`
}

// Refused is a refusal, as far as a form needs it.
type Refused struct {
	Message string `json:"message"`
	From    int    `json:"from,omitempty"`
	To      int    `json:"to,omitempty"`
}

// inspect reads what a file says of itself, as Parse reads it: $NAME, ${NAME}
// and ${NAME:?why} need a value, and ${NAME:-default} and ${NAME:+other} have
// what they need. A $$ is a dollar sign. A file Parse wouldn't read further
// than this says nothing: one that isn't YAML, or comes to too much once its
// aliases are read, which reading each alias again here would pay for. A caller
// takes a turn at reading first, and with a guard this is the child's to do.
func inspect(ctx context.Context, file []byte, given map[string]string) Inspection {
	var raw map[string]any
	if yaml.Unmarshal(file, &raw) != nil {
		return Inspection{}
	}
	dropExtensions(raw)
	if measure(raw) != nil {
		return Inspection{}
	}
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if ctx.Err() != nil {
				return
			}
			scanVariables(v, 0, func(name string, op byte) bool {
				seen[name] = seen[name] || op == 0 || op == '?'
				return true
			})
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(raw)
	out := Inspection{Variables: make([]Variable, 0, len(seen)), Name: fileNamed(ctx, raw, given)}
	for name, required := range seen {
		out.Variables = append(out.Variables, Variable{Name: name, Required: required})
	}
	slices.SortFunc(out.Variables, func(a, b Variable) int { return cmp.Compare(a.Name, b.Name) })
	out.Exposed, out.ExposedMore = exposedIn(ctx, raw)
	out.Refused = refusedPastVariables(ctx, file, raw, given, out.Variables)
	return out
}

// refusedPastVariables reads on past variables with no value, by giving each one,
// for what a file would be refused for whatever they hold. Parse stops at the
// first it can't fill in, which is before most of what it refuses. A refusal of a
// field a variable is in may be about that stand-in, so it says nothing of those.
func refusedPastVariables(ctx context.Context, file []byte, raw map[string]any, given map[string]string, used []Variable) *Refused {
	filled := maps.Clone(given)
	if filled == nil {
		filled = map[string]string{}
	}
	lacking := false
	for _, v := range used {
		if _, ok := filled[v.Name]; !ok && v.Required {
			filled[v.Name], lacking = "1", true
		}
	}
	if !lacking {
		return nil
	}
	_, err := parse(ctx, file, filled)
	var at *refusal
	if !errors.As(err, &at) || at.from > 0 || holdsVariable(raw, at.at) {
		return nil
	}
	from, to := linesOf(file, err)
	return &Refused{Message: Message(err), From: from, To: to}
}

// holdsVariable reports a place in a file that may hold a variable, which is any
// it can't find.
func holdsVariable(raw map[string]any, at place) bool {
	var node any = raw
	m := owner.FindStringSubmatch(at.owner)
	path := strings.FieldsFunc(at.path, func(r rune) bool { return r == '.' })
	if m != nil {
		section, _ := raw[m[1]+"s"].(map[string]any)
		if section == nil {
			return true
		}
		if node = section[m[2]]; node == nil {
			return true
		}
		if m[3] != "" {
			path = append(strings.Fields(m[3]), path...)
		}
	} else if at.owner != fileOwner {
		return true
	}
	for _, part := range path {
		key, index := part, -1
		if i := indexed.FindStringSubmatch(part); i != nil {
			key = part[:len(part)-len(i[0])]
			index, _ = strconv.Atoi(i[1])
		}
		if key != "" {
			fields, ok := node.(map[string]any)
			if !ok {
				return true
			}
			if node, ok = fields[key]; !ok {
				return false // a field that isn't there holds nothing: the file's mistake
			}
		}
		if index >= 0 {
			list, ok := node.([]any)
			if !ok || index >= len(list) {
				return true
			}
			node = list[index]
		}
	}
	return mentionsVariable(node)
}

func mentionsVariable(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, "$")
	case []any:
		return slices.ContainsFunc(v, mentionsVariable)
	case map[string]any:
		for _, e := range v {
			if mentionsVariable(e) {
				return true
			}
		}
	}
	return false
}

// fileNamed is what a file's name: comes to, or empty if it uses a variable
// with no value.
func fileNamed(ctx context.Context, raw map[string]any, given map[string]string) string {
	name, ok := raw["name"].(string)
	if !ok {
		return ""
	}
	in := &interpolator{ctx: ctx, variables: given, missing: map[string]bool{}}
	out, err := in.substitute(place{owner: fileOwner, path: "name"}, name)
	if err != nil || len(in.missing) > 0 {
		return ""
	}
	return out
}

// scanVariables notes each variable s uses, with the syntax substitute reads, as
// far down as substitute goes: each level reads the rest of its string again, so
// what is nested deeper isn't looked at. Parse refuses it. op is what follows
// the name, without its colon: 0 for none, or '-', '+' or '?'. note says whether
// to read on into the words after it.
func scanVariables(s string, depth int, note func(name string, op byte) bool) {
	if depth >= MaxNesting {
		return
	}
	for i := 0; i < len(s)-1; {
		if s[i] != '$' {
			i++
			continue
		}
		switch next := s[i+1]; {
		case next == '$':
			i += 2
		case next == '{':
			end := closing(s, i+1)
			if end < 0 {
				return
			}
			inner := s[i+2 : end]
			n := 0
			for n < len(inner) && (n == 0 && nameStart(inner[n]) || n > 0 && nameRest(inner[n])) {
				n++
			}
			if n > 0 {
				rest := strings.TrimPrefix(inner[n:], ":")
				op := byte(0)
				if rest != "" {
					op = rest[0]
				}
				if note(inner[:n], op) && rest != "" {
					scanVariables(rest[1:], depth+1, note)
				}
			}
			i = end + 1
		case nameStart(next):
			j := i + 1
			for j < len(s) && nameRest(s[j]) {
				j++
			}
			note(s[i+1:j], 0)
			i = j
		default:
			i++
		}
	}
}

var (
	owner     = regexp.MustCompile(`^the (service|volume|network) ([^' ]+)(?:'s (.+))?$`)
	mentioned = regexp.MustCompile(`^the (service|volume|network) ([^' ]+)`)
	yamlLine  = regexp.MustCompile(`\bline (\d+)\b`)
	yamlKey   = regexp.MustCompile(`mapping key "([^"]+)" already defined`)
	indexed   = regexp.MustCompile(`\[(\d+)\]$`)
)

// Lines says which lines of file err, from Parse, is about, from 1, or 0 and 0
// when it is about the file as a whole. They only point the way, so they are the
// nearest it can find, such as the service's own.
func Lines(ctx context.Context, file []byte, err error) (from, to int) {
	// A file the guard stopped is read no further, and is about no one line.
	if len(file) > MaxFile || errors.Is(err, ErrCostly) {
		return 0, 0
	}
	select {
	case parsing <- struct{}{}:
		defer func() { <-parsing }()
	case <-ctx.Done():
		return 0, 0
	}
	return linesOf(file, err)
}

// linesOf is Lines, for a caller with a turn at reading.
func linesOf(file []byte, err error) (from, to int) {
	var at *refusal
	if errors.As(err, &at) && at.from > 0 {
		return at.from, at.to
	}
	var doc yaml.Node
	if yerr := yaml.Unmarshal(file, &doc); yerr != nil || len(doc.Content) == 0 {
		if all := yamlLine.FindAllStringSubmatch(Message(err), -1); len(all) > 0 {
			n, _ := strconv.Atoi(all[len(all)-1][1])
			return n, n
		}
		return 0, 0
	}
	root := doc.Content[0]
	line := 0
	if errors.As(err, &at) {
		line = lineOf(root, at.at)
	} else if m := mentioned.FindStringSubmatch(Message(err)); m != nil {
		line = lineOf(root, place{owner: fmt.Sprintf("the %s %s", m[1], m[2])})
	}
	return line, line
}

// lineOf finds a place in a file's YAML. Its owner names an entry, and may
// say more, as "the service web's healthcheck" does.
func lineOf(root *yaml.Node, at place) int {
	path := strings.FieldsFunc(at.path, func(r rune) bool { return r == '.' })
	node := root
	line := 0
	if m := owner.FindStringSubmatch(at.owner); m != nil {
		section := entry(node, m[1]+"s")
		if node = entry(section, m[2]); node == nil {
			return nodeLine(section)
		}
		line = keyLine(section, m[2])
		if m[3] != "" {
			path = append(strings.Fields(m[3]), path...)
		}
	}
	for _, part := range path {
		key, index := part, -1
		if m := indexed.FindStringSubmatch(part); m != nil {
			key = part[:len(part)-len(m[0])]
			index, _ = strconv.Atoi(m[1])
		}
		if key != "" {
			next := entry(node, key)
			if next == nil {
				return line
			}
			line = keyLine(node, key)
			node = next
		}
		if index >= 0 {
			if node.Kind != yaml.SequenceNode || index >= len(node.Content) {
				return line
			}
			node = node.Content[index]
			line = node.Line
		}
	}
	return line
}

func nodeLine(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line
}

// keyLine is the line of a mapping's key.
func keyLine(n *yaml.Node, key string) int {
	if n == nil || n.Kind != yaml.MappingNode {
		return 0
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i].Line
		}
	}
	return 0
}

// entry is a mapping's value at key, or nil.
func entry(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
