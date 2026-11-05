// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dockwell reads a Compose file without compose-go's loader, which checks
// it against the Compose schema first (ADR 0021). compose-go's transforms
// and types assume what the schema allows, and panic on other values: a
// network named by a number, say. So every field Dockwell takes is checked
// here to hold what the schema allows there, before anything reads it.
// Fields Dockwell doesn't take are refused by name elsewhere (check).

// kinds are what a value in a YAML file can be.
type kinds uint8

const (
	aString kinds = 1 << iota
	aNumber
	aBool
	aList
	aMapping
	nothing // a key with no value, or ~
)

// shape is what one field of a Compose file holds: the kinds of value the
// Compose schema allows there, and inside a list or a mapping, what each
// entry holds.
type shape struct {
	kinds kinds
	// item is what each entry of a list holds.
	item *shape
	// fields are what a mapping's fields hold, by name. Fields not named
	// here are left to check, which refuses those Dockwell doesn't take.
	fields map[string]*shape
	// value is what each entry of a mapping of names holds, such as each
	// service, when fields is nil.
	value *shape
	// owner names an entry of a mapping of names, such as "the service
	// web", which the entry's own fields are then named after.
	owner func(name string) string
	// example is how Compose writes it, for the fix.
	example string
}

func shaped(k kinds, example string) *shape { return &shape{kinds: k, example: example} }

func object(example string, fields map[string]*shape) *shape {
	return &shape{kinds: aMapping, fields: fields, example: example}
}

// fileShape is a whole file's shape. It is made the first time a file is
// read, so a process that reads none, as an agent, never makes it.
var fileShape = sync.OnceValue(func() *shape {
	str := func(example string) *shape { return shaped(aString, example) }
	// A command, as one string or a list of them.
	command := func(example string) *shape {
		return &shape{kinds: aString | aList | nothing, item: str(example), example: example}
	}
	// A list of key=value strings, or a mapping, as environment and
	// labels are.
	listOrMapping := func(example string) *shape {
		return &shape{
			kinds: aList | aMapping, item: str(example), example: example,
			value: shaped(aString|aNumber|aBool|nothing, example),
		}
	}
	options := &shape{kinds: aMapping, value: shaped(aString|aNumber, "driver_opts: {type: tmpfs}"), example: "driver_opts: {type: tmpfs}"}
	boolean := func(example string) *shape { return shaped(aBool|aString, example) }
	number := func(example string) *shape { return shaped(aNumber|aString, example) }

	service := object("{image: nginx:1.27}", map[string]*shape{
		"image":       str("image: nginx:1.27"),
		"command":     command(`command: ["serve", "--port", "8080"]`),
		"entrypoint":  command(`entrypoint: ["/bin/app"]`),
		"environment": listOrMapping("environment: {API_URL: https://api.example.com}"),
		"ports": {kinds: aList, example: `ports: ["8080:80"]`, item: &shape{
			kinds: aString | aNumber | aMapping, example: `ports: ["8080:80"]`, fields: map[string]*shape{
				"target": number("target: 80"), "published": number(`published: "8080"`),
				"host_ip": str("host_ip: 127.0.0.1"), "protocol": str("protocol: udp"), "mode": str("mode: host"),
			},
		}},
		"restart": str("restart: unless-stopped"),
		"labels":  listOrMapping("labels: {team: web}"),
		"volumes": {kinds: aList, example: `volumes: ["data:/var/lib/data"]`, item: &shape{
			kinds: aString | aMapping, example: `volumes: ["data:/var/lib/data"]`, fields: map[string]*shape{
				"type": str("type: volume"), "source": str("source: data"), "target": str("target: /var/lib/data"),
				"read_only": boolean("read_only: true"), "consistency": str("consistency: cached"),
				"bind": object("bind: {selinux: z}", map[string]*shape{"selinux": str("selinux: z")}),
			},
		}},
		"networks": {
			kinds: aList | aMapping, item: str("networks: [back]"), example: "networks: [back]",
			value: shaped(aMapping|nothing, "networks: {back: {}}"),
		},
		"network_mode": str("network_mode: bridge"),
		"user":         str(`user: "1000"`),
		"working_dir":  str("working_dir: /app"),
		"healthcheck": object(`healthcheck: {test: ["CMD", "curl", "-f", "http://localhost"]}`, map[string]*shape{
			"test":     {kinds: aString | aList, item: str(`test: ["CMD", "pg_isready"]`), example: `test: ["CMD", "pg_isready"]`},
			"interval": str("interval: 30s"), "timeout": str("timeout: 5s"), "start_period": str("start_period: 1m"),
			"retries": number("retries: 3"), "disable": boolean("disable: true"),
		}),
		"depends_on": {
			kinds: aList | aMapping, item: str("depends_on: [db]"), example: "depends_on: [db]",
			value: object("depends_on: {db: {condition: service_healthy}}", map[string]*shape{
				"condition": str("condition: service_healthy"), "required": boolean("required: true"), "restart": boolean("restart: true"),
			}),
		},
		"expose": {kinds: aList, item: shaped(aString|aNumber, `expose: ["8080"]`), example: `expose: ["8080"]`},
	})
	volume := &shape{kinds: aMapping | nothing, example: "data: {driver: local}", fields: map[string]*shape{
		"driver": str("driver: local"), "driver_opts": options, "labels": listOrMapping("labels: {backup: daily}"),
	}}
	network := &shape{kinds: aMapping | nothing, example: "back: {internal: true}", fields: map[string]*shape{
		"driver": str("driver: bridge"), "driver_opts": options, "internal": boolean("internal: true"),
		"labels": listOrMapping("labels: {team: web}"),
	}}
	named := func(what string, entry *shape, example string) *shape {
		return &shape{
			kinds: aMapping | nothing, value: entry, example: example,
			owner: func(name string) string { return "the " + what + " " + name },
		}
	}
	return object("services: {web: {image: nginx:1.27}}", map[string]*shape{
		"name":     str("name: shop"),
		"version":  str(`version: "3.8"`),
		"services": named("service", service, "services: {web: {image: nginx:1.27}}"),
		"volumes":  named("volume", volume, "volumes: {data: {}}"),
		"networks": named("network", network, "networks: {back: {}}"),
	})
})

// place names where a value is in the file, such as "the service web's
// ports[0].target": the entry it is in, and the path within it.
type place struct{ owner, path string }

func (p place) String() string {
	if p.path == "" {
		return p.owner
	}
	return p.owner + "'s " + p.path
}

func (p place) field(name string) place {
	if p.path == "" {
		return place{p.owner, name}
	}
	return place{p.owner, p.path + "." + name}
}

func (p place) index(i int) place { return place{p.owner, p.path + "[" + strconv.Itoa(i) + "]"} }

// conform refuses a value that isn't of a kind its shape allows, here or
// anywhere inside it.
func conform(at place, v any, s *shape) error {
	k := kindOf(v)
	if k&s.kinds == 0 {
		return wrongKind(at, v, s)
	}
	switch v := v.(type) {
	case []any:
		for i, e := range v {
			if err := conform(at.index(i), e, s.item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(v)) {
			inner, next := s.value, at.field(name)
			switch {
			case s.fields != nil:
				inner = s.fields[name]
			case s.owner != nil:
				next = place{owner: s.owner(name)}
			}
			if inner == nil || strings.HasPrefix(name, "x-") {
				continue
			}
			if err := conform(next, v[name], inner); err != nil {
				return err
			}
		}
	case map[any]any:
		// The YAML library makes one only when a name isn't a string.
		for _, name := range slices.SortedFunc(maps.Keys(v), func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) }) {
			if _, ok := name.(string); !ok {
				return invalid("%s names something %v, which isn't a string. Quote it, as %q", at, name, fmt.Sprint(name))
			}
		}
		return invalid("%s names something with a value that isn't a string. Quote the names in it", at)
	}
	return nil
}

// kindOf is what a value the YAML library decoded is. A mapping with names
// that aren't strings is a mapping too, which conform refuses.
func kindOf(v any) kinds {
	switch v.(type) {
	case nil:
		return nothing
	case string:
		return aString
	case int, int64, uint64, float64:
		return aNumber
	case bool:
		return aBool
	case []any:
		return aList
	case map[string]any, map[any]any:
		return aMapping
	}
	return 0 // a date, say
}

// wrongKind says what a value is, what Compose takes there, and how to
// write it.
func wrongKind(at place, v any, s *shape) error {
	var is string
	switch v := v.(type) {
	case nil:
		is = "empty"
	case bool:
		is = strconv.FormatBool(v)
	case time.Time:
		is = "a date"
	default:
		is = kindWords(kindOf(v))
	}
	fix := "Change it in the Compose file"
	switch {
	case s.kinds&aString != 0 && (kindOf(v)&(aNumber|aBool) != 0 || is == "a date"):
		fix = fmt.Sprintf("Quote it, as %q", scalar(v))
	case s.example != "":
		fix = "Write it as Compose does, such as " + s.example
	}
	return invalid("%s is %s, where Compose takes %s. %s", at, is, kindWords(s.kinds), fix)
}

func scalar(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.Format(time.DateOnly)
	}
	return fmt.Sprint(v)
}

// kindWords names kinds, such as "a string, a number, true, false or
// nothing".
func kindWords(k kinds) string {
	var words []string
	for _, w := range []struct {
		k     kinds
		words []string
	}{
		{aString, []string{"a string"}},
		{aNumber, []string{"a number"}},
		{aBool, []string{"true", "false"}},
		{aList, []string{"a list"}},
		{aMapping, []string{"a mapping"}},
		{nothing, []string{"nothing"}},
	} {
		if k&w.k != 0 {
			words = append(words, w.words...)
		}
	}
	switch len(words) {
	case 0:
		return "nothing it can read"
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}
