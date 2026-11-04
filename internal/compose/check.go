// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// refusal is an ErrInvalid that knows the place in the file it is about, so
// a form can point at the line.
type refusal struct {
	error
	at place
}

func (r *refusal) Unwrap() error { return r.error }

func invalidAt(at place, format string, args ...any) error {
	return &refusal{error: invalid(format, args...), at: at}
}

// Variable is a variable a Compose file uses.
type Variable struct {
	Name string
	// Required is true when the file gives it no default, so it needs a
	// value. A variable used both ways needs one.
	Required bool
}

// Variables lists the variables a file uses, by name, as Parse reads them:
// $NAME, ${NAME} and ${NAME:?why} need a value, and ${NAME:-default} and
// ${NAME:+other} have what they need. A $$ is a dollar sign. A file that
// isn't YAML uses none.
func Variables(file []byte) []Variable {
	if len(file) > MaxFile {
		return nil
	}
	var raw map[string]any
	if yaml.Unmarshal(file, &raw) != nil {
		return nil
	}
	seen := map[string]bool{}
	note := func(name string, required bool) { seen[name] = seen[name] || required }
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			scanVariables(v, note)
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
	out := make([]Variable, 0, len(seen))
	for name, required := range seen {
		out = append(out, Variable{Name: name, Required: required})
	}
	slices.SortFunc(out, func(a, b Variable) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// scanVariables notes each variable s uses, with the syntax substitute
// reads.
func scanVariables(s string, note func(name string, required bool)) {
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
				note(inner[:n], rest == "" || strings.HasPrefix(rest, "?"))
				if rest != "" {
					scanVariables(rest[1:], note)
				}
			}
			i = end + 1
		case nameStart(next):
			j := i + 1
			for j < len(s) && nameRest(s[j]) {
				j++
			}
			note(s[i+1:j], true)
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
	indexed   = regexp.MustCompile(`\[\d+\]`)
)

// Line says which line of file err, from Parse, is about, from 1, or 0 when
// it is about the file as a whole. It only points the way, so it is the
// nearest line it can find, such as the service's own.
func Line(file []byte, err error) int {
	var doc yaml.Node
	if yerr := yaml.Unmarshal(file, &doc); yerr != nil || len(doc.Content) == 0 {
		if m := yamlLine.FindStringSubmatch(Message(err)); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	root := doc.Content[0]
	var at *refusal
	if errors.As(err, &at) {
		return lineOf(root, at.at)
	}
	if m := mentioned.FindStringSubmatch(Message(err)); m != nil {
		return lineOf(root, place{owner: fmt.Sprintf("the %s %s", m[1], m[2])})
	}
	return 0
}

// lineOf finds a place in a file's YAML. Its owner names an entry, and may
// say more, as "the service web's healthcheck" does.
func lineOf(root *yaml.Node, at place) int {
	path := strings.FieldsFunc(indexed.ReplaceAllString(at.path, ""), func(r rune) bool { return r == '.' })
	node := root
	line := 0
	if m := owner.FindStringSubmatch(at.owner); m != nil {
		section := child(node, m[1]+"s")
		if node = child(section, m[2]); node == nil {
			return nodeLine(section)
		}
		line = keyLine(section, m[2])
		if m[3] != "" {
			path = append(strings.Fields(m[3]), path...)
		}
	}
	for _, key := range path {
		next := child(node, key)
		if next == nil {
			return line
		}
		line = keyLine(node, key)
		node = next
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

// child is a mapping's value at key, or nil.
func child(n *yaml.Node, key string) *yaml.Node {
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
