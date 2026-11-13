// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/resource/binding"
)

// Exposed is a variable whose value a Compose file writes in free text, where a
// secret can really land, and where anyone who can read what it makes sees it: in a
// plan, in the store's record of that plan, and in an export. Only a spec's sensitive
// fields are blanked (ADR 0021), and a command, an entrypoint and a health check's
// test keep a value by name (ADR 0022), so a value that is a secret belongs in one of
// those. A command written as one string, with a value of several words, is the
// exception, as a name can't stand for more than one word.
type Exposed struct {
	Variable string
	// Kind says what holds it: service, volume or network.
	Kind string
	// Owner is that service, volume or network's name.
	Owner string
	// Field is its field, such as command, labels or healthcheck.
	Field string
	// Whole says it is text as the rest of a command written as one string, which
	// another value on it made text, and not for its own sake.
	Whole bool
}

// What a plan doesn't warn of. A value shows in every field a spec keeps but its
// sensitive ones and those that keep it by name, and a plan warns only where it is
// free text, as in a label, where a secret can really land. A name, a number or a
// path shows as what it is. Nearly every real file has container_name: ${CONTAINER_NAME} and
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
		"image", "container_name", "network_mode", "networks", "user", "cap_add", "cap_drop", "group_add", "extra_hosts",
		// Numbers, and one of a few words.
		"ports", "restart", "cpus", "mem_limit", "mem_reservation", "shm_size", "deploy", "privileged", "stop_grace_period",
		"healthcheck.interval", "healthcheck.timeout", "healthcheck.start_period", "healthcheck.retries", "healthcheck.disable",
		// What a container asks of itself: a name, a switch, a signal, a count or an
		// address, and two that go in no spec (ADR 0085).
		"hostname", "init", "tty", "stdin_open", "read_only", "stop_signal", "pids_limit", "ulimits", "dns", "pull_policy", "security_opt",
		"healthcheck.start_interval",
		// Paths.
		"volumes", "tmpfs", "working_dir",
		// Changes nothing on one server, or orders the change, so neither is kept.
		"expose", "depends_on",
		// Container.env is sensitive, so a plan masks it and an export leaves it
		// out. An entry's name isn't, so a list's names still warn (walk).
		"environment",
		// A size or a count, and Dockwell's own rotation takes its place.
		"logging",
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
func (x *exposure) note(at place, name string, naming bool) { x.record(at, name, naming, false) }

// noteText records that name's value lands as text in a field that would otherwise
// keep it by name: as what one name can't stand for, or, with whole, as the rest of a
// line another value made all text.
func (x *exposure) noteText(at place, name string, whole bool) { x.record(at, name, false, whole) }

func (x *exposure) record(at place, name string, naming, whole bool) {
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
	e.Field, e.Whole = field, whole
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

// exposedIn says where raw, a file before its variables are read, could write a
// value in free text, whatever values there are or aren't, for a form to say so
// before anyone types a secret. A plan goes by the values it was given (exposure),
// so what a form says is what a plan would say if each had one. It asks the same
// quiet fields, and costs one pass over the file's strings, stopping at maxExposed
// and saying there was more.
func exposedIn(ctx context.Context, raw map[string]any) (found []Exposed, more bool) {
	x := rawExposure{ctx: ctx, seen: map[Exposed]bool{}}
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
				at := Exposed{Kind: section.kind, Owner: name, Field: field}
				value := fields[field]
				// These keep a value by name, whatever it is. One made of several words
				// is the exception, which only a plan can tell, once it has the value.
				if section.kind == "service" && (field == binding.FieldCommand || field == binding.FieldEntrypoint) {
					continue
				}
				// A value here is blanked, and a name isn't. A map's keys are never
				// read for variables, so only a list can do this.
				if section.kind == "service" && field == "environment" {
					x.keysOf(at, value)
					continue
				}
				if _, quiet := quietField(section.quiet, field); quiet {
					continue
				}
				// Part of a field can be quiet where the field isn't, as a health
				// check's timings are. Only its first key can say so.
				within, isMap := value.(map[string]any)
				if !isMap {
					x.value(at, value)
					continue
				}
				for _, key := range slices.Sorted(maps.Keys(within)) {
					if _, quiet := quietField(section.quiet, field+"."+key); !quiet && (section.kind != "service" || field+"."+key != binding.FieldTest) {
						x.value(at, within[key])
					}
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

// rawExposure is what exposedIn has found.
type rawExposure struct {
	ctx  context.Context //nolint:containedctx // one call's, for one walk
	seen map[Exposed]bool
	more bool
}

// value notes what the strings in v could write.
func (x *rawExposure) value(at Exposed, v any) {
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

// keysOf notes what a list-form environment's names could write: the part of each
// entry before its first =, outside any ${…}.
func (x *rawExposure) keysOf(at Exposed, v any) {
	list, _ := v.([]any)
	for _, e := range list {
		if s, ok := e.(string); ok && !x.more && x.ctx.Err() == nil {
			x.text(at, nameOfEntry(s))
		}
	}
}

// text notes the variables whose value s can come to: ${NAME:-default} is NAME's
// or the default's, ${NAME:+other} only other's, and ${NAME:?why}'s why goes in a
// refusal. A default's own words aren't anyone's secret.
func (x *rawExposure) text(at Exposed, s string) {
	scanVariables(s, 0, func(name string, op byte) bool {
		if op != '+' {
			x.note(at, name)
		}
		return op != '?'
	})
}

func (x *rawExposure) note(at Exposed, name string) {
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
