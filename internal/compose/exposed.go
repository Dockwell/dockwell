// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// Exposed is a variable whose value a Compose file writes where anyone who can
// read what it makes sees it: in a plan, in the store's record of that plan, and in
// an export. Only a spec's sensitive fields are blanked (ADR 0021), so a value
// that is a secret belongs in a service's environment.
type Exposed struct {
	Variable string
	// Kind says what holds it: service, volume or network.
	Kind string
	// Owner is that service, volume or network's name.
	Owner string
	// Field is its field, such as command, labels or user.
	Field string
}

// What a file can write a value into that a plan doesn't warn of. Anything else
// warns, so a field added to what Dockwell reads warns until someone decides it
// needn't. TestEveryFieldAVariableReaches names each, and fails on one that isn't
// named.
var (
	quietServiceFields = keys(
		// Container.env is sensitive, so a plan masks it and an export leaves it
		// out. An entry's name isn't, so a list's names still warn (walk).
		"environment",
		// Changes nothing on one server, or orders the change, so neither is kept.
		"expose", "depends_on",
	)
	// Volume.options is sensitive, as a CIFS password has to go there.
	quietVolumeFields  = keys("driver_opts")
	quietNetworkFields = keys()
)

// maxExposed is the most a file's plan lists. A real file comes nowhere near it,
// and a plan keeps what it says for good: 256 KiB of $a$b… would otherwise be
// 85,000 notes. The plan says when there are more.
const maxExposed = 100

// exposure notes where the values of a file's variables land, as they are read. It
// goes by what they are given: a default is the file's own words, and nothing is
// written of a variable with nothing in it.
type exposure struct {
	seen map[Exposed]bool
	more bool
}

// note records that name's value lands at at. naming says it lands in the name of
// an entry, which isn't quiet whatever the entry's field is. What it records is from
// what the file's keys have been checked to be: a field is one Dockwell takes, and a
// variable's name is what the reader takes as one, so neither is the file's to word.
func (x *exposure) note(at place, name string, naming bool) {
	var e Exposed
	var quiet map[string]bool
	for _, section := range []struct {
		kind  string
		quiet map[string]bool
	}{{"service", quietServiceFields}, {"volume", quietVolumeFields}, {"network", quietNetworkFields}} {
		if owner, ok := strings.CutPrefix(at.owner, "the "+section.kind+" "); ok {
			e, quiet = Exposed{Variable: name, Kind: section.kind, Owner: owner}, section.quiet
		}
	}
	if e.Kind == "" {
		return // the file's name or version, which nothing keeps
	}
	field, _, _ := strings.Cut(at.path, ".")
	field, _, _ = strings.Cut(field, "[")
	e.Field = field
	if quiet[field] && !naming {
		return
	}
	if !x.seen[e] && len(x.seen) == maxExposed {
		x.more = true
		return
	}
	if x.seen == nil {
		x.seen = map[Exposed]bool{}
	}
	x.seen[e] = true
}

// report lists what note found, in a steady order, and whether there was more.
func (x *exposure) report() ([]Exposed, bool) {
	out := slices.Collect(maps.Keys(x.seen))
	slices.SortFunc(out, func(a, b Exposed) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.Field, b.Field), cmp.Compare(a.Variable, b.Variable))
	})
	return out, x.more
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
