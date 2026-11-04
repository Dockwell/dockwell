// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
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
//
// It also says where a variable's value lands that isn't hidden (Exposed).
func interpolate(raw map[string]any, variables map[string]string) (map[string]any, []Exposed, error) {
	in := &interpolator{variables: variables, missing: map[string]bool{}}
	out, err := in.walk(place{owner: fileOwner}, raw)
	if len(in.missing) > 0 {
		names := slices.Sorted(maps.Keys(in.missing))
		return nil, nil, invalid("the Compose file uses %s, with no value. Give it one in variables, or a default in the file, such as ${%s:-value}",
			strings.Join(names, ", "), names[0])
	}
	if err != nil {
		return nil, nil, err
	}
	return out.(map[string]any), in.report(), nil
}

// Exposed is a variable whose value a Compose file writes where anyone who
// can read the service sees it: its command, entrypoint, labels or health
// check. Only a spec's environment is blanked (ADR 0021), so a value that is
// a secret belongs there, with $$NAME in the command for the shell to read.
type Exposed struct {
	Variable string
	Service  string
	// Field is the field it lands in: command, entrypoint, labels or
	// healthcheck.
	Field string
}

// checkVariables refuses more variables than Dockwell takes, before they cost
// anything.
func checkVariables(variables map[string]string) error {
	total := 0
	for k, v := range variables {
		total += len(k) + len(v)
	}
	if total > MaxVariables {
		return invalid("the variables come to %.1f MiB, and Dockwell takes up to %d KiB of them. Pass less, or set long values on the server", float64(total)/(1<<20), MaxVariables>>10)
	}
	return nil
}

const fileOwner = "the Compose file"

// What variables may add to a file. The cap on a file means little if one
// short line can repeat a long value, so what they expand to counts across
// the whole file, as it grows, before compose-go reads the result. A file
// of 50 KiB and one value of 64 KiB otherwise allocated 288 MiB.
const (
	// MaxExpansion is how many bytes variables may expand to in a file.
	MaxExpansion = 2 << 20
	// MaxVariables is how many bytes the variables themselves may hold.
	MaxVariables = 1 << 20
	// MaxNesting is how deep variables may sit inside each other's
	// defaults. Each level reads the rest of its string again and expands
	// to almost nothing, so the budget above doesn't see it: a file of
	// 960 KiB nested 160,000 deep took 34 s and 128 MiB of stack.
	MaxNesting = 16
)

type interpolator struct {
	variables map[string]string
	missing   map[string]bool
	// expanded counts what variables have expanded to so far.
	expanded int
	// depth counts the variables being read inside each other.
	depth int
	// exposed notes where a value lands in plain sight.
	exposed map[Exposed]bool
}

// note records that name's value lands at at, if that is in plain sight. It
// counts a variable with no value yet, as it lands there once it has one, so
// a form can say so before anyone types a secret into it.
func (in *interpolator) note(at place, name string) {
	service, ok := strings.CutPrefix(at.owner, "the service ")
	if !ok {
		return
	}
	field := ""
	switch path := indexed.ReplaceAllString(at.path, ""); {
	case path == "command", path == "entrypoint":
		field = path
	case path == "healthcheck.test":
		field = "healthcheck"
	case path == "labels", strings.HasPrefix(path, "labels."):
		field = "labels"
	default:
		return // what it runs, which no one keeps secret, or what is blanked
	}
	if in.exposed == nil {
		in.exposed = map[Exposed]bool{}
	}
	in.exposed[Exposed{Variable: name, Service: service, Field: field}] = true
}

// report lists what note found, in a steady order.
func (in *interpolator) report() []Exposed {
	out := slices.Collect(maps.Keys(in.exposed))
	slices.SortFunc(out, func(a, b Exposed) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Field, b.Field), cmp.Compare(a.Variable, b.Variable))
	})
	return out
}

// spend counts n bytes against MaxExpansion.
func (in *interpolator) spend(at place, n int) error {
	if in.expanded += n; in.expanded > MaxExpansion {
		return invalid("%s takes the variables past %d MiB once they are expanded, which Dockwell doesn't read. Use each long value less often, or shorten it", at, MaxExpansion>>20)
	}
	return nil
}

// walk resolves the strings in v, which is at.
func (in *interpolator) walk(at place, v any) (any, error) {
	switch v := v.(type) {
	case string:
		// A list's entry can write its name from a variable, and only a value is
		// hidden. A mapping's keys are never read for variables.
		if in.exposure != nil && strings.HasPrefix(at.owner, "the service ") && strings.HasPrefix(at.path, "environment[") {
			in.naming = true
			_, err := in.substitute(at, nameOfEntry(v))
			in.naming = false
			if err != nil {
				return nil, err
			}
		}
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
	if !strings.Contains(s, "$") {
		return s, nil
	}
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
			if err := in.spend(at, len(v)); err != nil {
				return "", err
			}
			b.WriteString(v)
			i = end + 1
		case nameStart(next):
			j := i + 1
			for j < len(s) && nameRest(s[j]) {
				j++
			}
			in.note(at, s[i+1:j])
			v := in.value(s[i+1 : j])
			if err := in.spend(at, len(v)); err != nil {
				return "", err
			}
			b.WriteString(v)
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
	if in.depth == MaxNesting {
		return "", invalid("%s nests variables more than %d deep, so Dockwell can't read it. Use fewer defaults inside defaults, or pass the value in variables", at, MaxNesting)
	}
	in.depth++
	defer func() { in.depth-- }()
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
		in.note(at, name)
		return in.value(name), nil
	case ":-", "-":
		if nonEmpty || set && op == "-" {
			in.note(at, name)
			return value, nil
		}
		return in.substitute(at, tail)
	case ":+", "+":
		if nonEmpty || set && op == "+" {
			return in.substitute(at, tail)
		}
		return value, nil
	case ":?", "?":
		// It needs a value, so it lands here whether or not it has one yet.
		in.note(at, name)
		if nonEmpty || set && op == "?" {
			return in.landed(at, name, value), nil
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
	return invalidAt(at, "%s has a variable Dockwell can't read, %s. Write one as ${NAME} or ${NAME:-default}, and a $ of its own as $$", at, variable)
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
