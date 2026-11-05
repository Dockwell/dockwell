// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"context"
	"maps"
	"slices"
)

// Exposed is a variable whose value a Compose file writes where anyone who can
// read what it makes sees it. Only a spec's sensitive fields are blanked (ADR
// 0021), so a value that is a secret belongs in a service's environment.
type Exposed struct {
	Variable string
	// Kind says what holds it: service, volume or network.
	Kind string
	// Owner is that service, volume or network's name.
	Owner string
	// Field is its field, such as command, labels or user.
	Field string
}

// What a file can write a value into that isn't told about, which
// docs/security/compose-variables.md lists with why. Everything else is, so a
// field added to what Dockwell reads warns until someone decides it needn't.
var (
	quietServiceFields = keys(
		// What runs and where it is reached, which no one keeps secret.
		"image", "ports",
		// Takes only a fixed word or another name in the file, or the file is
		// refused, so a secret can't land there.
		"restart", "network_mode", "networks", "depends_on",
		// Changes nothing on one server, so it isn't recorded.
		"expose",
	)
	quietVolumeFields  = keys("driver_opts") // blanked
	quietNetworkFields = keys("internal")    // true or false
)

// maxExposed is the most a file's report lists. A real file comes nowhere near
// it, and a plan keeps what it says for good: 256 KiB of $a$b… would otherwise
// be 85,000 notes.
const maxExposed = 100

// exposedIn says where raw, a file before its variables are read, writes a
// variable's value in plain sight, whatever values there are or aren't: a form
// says so before anyone types a secret. It costs one pass over the file's
// strings, and stops at maxExposed, saying there was more.
func exposedIn(ctx context.Context, raw map[string]any) (found []Exposed, more bool) {
	x := exposure{ctx: ctx, seen: map[Exposed]bool{}}
	for _, section := range []struct {
		key, kind string
		quiet     map[string]bool
	}{
		{"services", "service", quietServiceFields},
		{"volumes", "volume", quietVolumeFields},
		{"networks", "network", quietNetworkFields},
	} {
		owners, _ := raw[section.key].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(owners)) {
			fields, _ := owners[name].(map[string]any)
			for _, field := range slices.Sorted(maps.Keys(fields)) {
				if section.quiet[field] {
					continue
				}
				at := Exposed{Kind: section.kind, Owner: name, Field: field}
				switch value := fields[field]; {
				case section.kind == "service" && field == "environment":
					// A value here is blanked, and a name isn't. A map's keys are
					// never read for variables, so only a list can do this.
					x.keysOf(at, value)
				case section.kind == "service" && field == "healthcheck":
					// The rest of it is times and a count, or it's refused.
					check, _ := value.(map[string]any)
					x.value(at, check["test"])
				default:
					x.value(at, value)
				}
			}
		}
	}
	out := slices.Collect(maps.Keys(x.seen))
	slices.SortFunc(out, func(a, b Exposed) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.Field, b.Field), cmp.Compare(a.Variable, b.Variable))
	})
	return out, x.more
}

type exposure struct {
	ctx  context.Context //nolint:containedctx // one call's, for one walk
	seen map[Exposed]bool
	more bool
}

// value notes what the strings in v write.
func (x *exposure) value(at Exposed, v any) {
	if x.more || x.ctx.Err() != nil {
		return
	}
	switch v := v.(type) {
	case string:
		x.text(at, v)
	case []any:
		for _, e := range v {
			x.value(at, e)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			x.value(at, v[k])
		}
	}
}

// keysOf notes what a list-form environment's names write: the part of each
// entry before its first =, outside any ${…}.
func (x *exposure) keysOf(at Exposed, v any) {
	list, _ := v.([]any)
	for _, e := range list {
		if s, ok := e.(string); ok && !x.more && x.ctx.Err() == nil {
			x.text(at, nameOfEntry(s))
		}
	}
}

// text notes the variables whose value s can come to: ${NAME:-default} is
// NAME's or the default's, ${NAME:+other} only other's, and ${NAME:?why}'s why
// goes in a refusal. A default's own words aren't anyone's secret.
func (x *exposure) text(at Exposed, s string) {
	scanVariables(s, 0, func(name string, op byte) bool {
		if op != '+' {
			x.note(at, name)
		}
		return op != '?'
	})
}

func (x *exposure) note(at Exposed, name string) {
	at.Variable = name
	if !x.seen[at] && len(x.seen) == maxExposed {
		x.more = true
		return
	}
	x.seen[at] = true
}

// nameOfEntry is the part of a list entry, such as API_KEY=x, that names it.
func nameOfEntry(s string) string {
	depth := 0
	for i := range len(s) {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case '=':
			if depth == 0 {
				return s[:i]
			}
		}
	}
	return s
}
