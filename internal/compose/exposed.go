// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// Exposed is a variable whose value a Compose file writes in free text, where a
// secret can really land, and where anyone who can read what it makes sees it: in a
// plan, in the store's record of that plan, and in an export. Only a spec's sensitive
// fields are blanked (ADR 0021), so a value that is a secret belongs in a service's
// environment.
type Exposed struct {
	Variable string
	// Kind says what holds it: service, volume or network.
	Kind string
	// Owner is that service, volume or network's name.
	Owner string
	// Field is its field, such as command, labels or healthcheck.
	Field string
}

// What a plan doesn't warn of. A value shows in every field a spec keeps but its
// sensitive ones, and a plan warns only where it is free text, as in a command or a
// label, where a secret can really land. A name, a number or a path shows as what it
// is. Nearly every real file has container_name: ${CONTAINER_NAME} and
// ports: ${PORT}:80, so a warning there would be on nearly every plan, people would
// stop reading what is always there, and the one about a password in a command would
// go unread.
//
// Anything not named here warns, so a field added to what Dockwell reads warns until
// someone decides it needn't. TestEveryFieldAVariableReaches has a row for each, and
// fails on one without. A field is a key, or a key and what's in it, as with a health
// check's timings, beside its test, which is what it runs.
var (
	quietServiceFields = keys(
		// Names.
		"image", "container_name", "network_mode", "networks", "user", "cap_add", "cap_drop", "group_add",
		// Numbers, and one of a few words.
		"ports", "restart", "cpus", "mem_limit", "mem_reservation", "deploy", "privileged",
		"healthcheck.interval", "healthcheck.timeout", "healthcheck.start_period", "healthcheck.retries", "healthcheck.disable",
		// Paths.
		"volumes", "working_dir",
		// Changes nothing on one server, or orders the change, so neither is kept.
		"expose", "depends_on",
		// Container.env is sensitive, so a plan masks it and an export leaves it
		// out. An entry's name isn't, so a list's names still warn (walk).
		"environment",
	)
	// A driver is a name. Volume.options is sensitive, as a CIFS password has to go
	// there.
	quietVolumeFields = keys("driver", "driver_opts")
	// A network's driver_opts are free text, and not sensitive.
	quietNetworkFields = keys("driver", "internal", "external", "name")
)

// quietField says a plan doesn't warn of a value at path, in what quiet is about.
// It gives path's key too, which is what a note names.
func quietField(quiet map[string]bool, path string) (string, bool) {
	field, in := path, ""
	if i := strings.IndexAny(path, ".["); i >= 0 {
		field, in = path[:i], path[i+1:]
	}
	if i := strings.IndexAny(in, ".["); i >= 0 {
		in = in[:i]
	}
	return field, quiet[field] || quiet[field+"."+in]
}

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
	field, isQuiet := quietField(quiet, at.path)
	e.Field = field
	if isQuiet && !naming {
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
