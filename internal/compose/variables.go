// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"maps"
	"slices"
	"strings"
)

// interpolate resolves the variables in every string of a file, as docker
// compose does: $NAME and ${NAME}; ${NAME:-default}, a default when NAME
// is empty or unset, and ${NAME-default} when unset; ${NAME:+other} and
// ${NAME+other}, the other way round; ${NAME:?why} and ${NAME?why}, which
// refuse the file; variables inside defaults, others and reasons too; and
// $$ for a $ of its own.
//
// Unlike docker compose, a variable with no value, and no default where it
// is used, is refused rather than left blank, at every level. A default
// that isn't used needs nothing. compose-go's own substitution leaves a
// variable blank inside a default, and after one in the same string.
func interpolate(raw map[string]any, variables map[string]string) (map[string]any, error) {
	in := &interpolator{variables: variables, missing: map[string]bool{}}
	out, err := in.walk(place{owner: fileOwner}, raw)
	if len(in.missing) > 0 {
		names := slices.Sorted(maps.Keys(in.missing))
		return nil, invalid("the Compose file uses %s, with no value. Give it one in variables, or a default in the file, such as ${%s:-value}",
			strings.Join(names, ", "), names[0])
	}
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

const fileOwner = "the Compose file"

type interpolator struct {
	variables map[string]string
	missing   map[string]bool
}

// walk resolves the strings in v, which is at.
func (in *interpolator) walk(at place, v any) (any, error) {
	switch v := v.(type) {
	case string:
		return in.substitute(at, v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, k := range slices.Sorted(maps.Keys(v)) {
			next := at.field(k)
			if at.owner == fileOwner {
				switch at.path {
				case "services":
					next = place{owner: "the service " + k}
				case "volumes":
					next = place{owner: "the volume " + k}
				case "networks":
					next = place{owner: "the network " + k}
				}
			}
			e, err := in.walk(next, v[k])
			if err != nil {
				return nil, err
			}
			out[k] = e
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			var err error
			if out[i], err = in.walk(at.index(i), e); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

// substitute resolves the variables in s.
func (in *interpolator) substitute(at place, s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch next := s[i+1]; {
		case next == '$':
			b.WriteByte('$')
			i += 2
		case next == '{':
			end := closing(s, i+1)
			if end < 0 {
				return "", unreadable(at, s[i:])
			}
			v, err := in.braced(at, s[i:end+1])
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i = end + 1
		case nameStart(next):
			j := i + 1
			for j < len(s) && nameRest(s[j]) {
				j++
			}
			b.WriteString(in.value(s[i+1 : j]))
			i = j
		default:
			// A $ before anything else is a $, as docker compose has it.
			b.WriteByte('$')
			i++
		}
	}
	return b.String(), nil
}

// braced resolves one ${…}.
func (in *interpolator) braced(at place, whole string) (string, error) {
	inner := whole[2 : len(whole)-1]
	n := 0
	for n < len(inner) && (n == 0 && nameStart(inner[n]) || n > 0 && nameRest(inner[n])) {
		n++
	}
	if n == 0 {
		return "", unreadable(at, whole)
	}
	name, rest := inner[:n], inner[n:]
	value, set := in.variables[name]
	nonEmpty := set && value != ""
	op := rest
	if len(rest) > 2 {
		op = rest[:2]
	}
	if len(op) == 2 && op[0] != ':' {
		op = op[:1]
	}
	tail := rest[len(op):]
	switch op {
	case "":
		return in.value(name), nil
	case ":-", "-":
		if nonEmpty || set && op == "-" {
			return value, nil
		}
		return in.substitute(at, tail)
	case ":+", "+":
		if nonEmpty || set && op == "+" {
			return in.substitute(at, tail)
		}
		return value, nil
	case ":?", "?":
		if nonEmpty || set && op == "?" {
			return value, nil
		}
		why, err := in.substitute(at, tail)
		if err != nil {
			return "", err
		}
		if why == "" {
			return "", invalid("%s needs %s, which has no value. Give it one in variables", at, name)
		}
		return "", invalid("%s needs %s: %s. Give it a value in variables", at, name, why)
	}
	return "", unreadable(at, whole)
}

// value is a variable's value, noting it missing if it has none.
func (in *interpolator) value(name string) string {
	v, ok := in.variables[name]
	if !ok {
		in.missing[name] = true
	}
	return v
}

func unreadable(at place, variable string) error {
	return invalid("%s has a variable Dockwell can't read, %s. Write one as ${NAME} or ${NAME:-default}, and a $ of its own as $$", at, variable)
}

// closing finds the } that closes the { at open, counting the braces
// inside it, as docker compose does; -1 if none does.
func closing(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func nameStart(c byte) bool { return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func nameRest(c byte) bool { return nameStart(c) || '0' <= c && c <= '9' }
