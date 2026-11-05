// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
)

// A file says where a variable's value would land before anyone gives it one,
// everywhere but what the threat model lists.
func TestExposedSaysWhereAValueLands(t *testing.T) {
	const head = "services:\n  w:\n    image: nginx:1\n"
	const s = "S"
	field := func(kind, owner, name string, variables ...string) []Exposed {
		out := make([]Exposed, len(variables))
		for i, v := range variables {
			out[i] = Exposed{Variable: v, Kind: kind, Owner: owner, Field: name}
		}
		return out
	}
	w := func(name string, variables ...string) []Exposed { return field("service", "w", name, variables...) }
	for name, c := range map[string]struct {
		file string
		want []Exposed
	}{
		"command":            {head + "    command: [--pw, \"${S}\"]\n", w("command", s)},
		"command as a line":  {head + "    command: run --pw $S\n", w("command", s)},
		"entrypoint":         {head + "    entrypoint: [x, \"$S\"]\n", w("entrypoint", s)},
		"health check":       {head + "    healthcheck: {test: [CMD, x, \"${S}\"]}\n", w("healthcheck", s)},
		"health as a line":   {head + "    healthcheck: {test: \"x ${S}\"}\n", w("healthcheck", s)},
		"label in a list":    {head + "    labels: [\"a=${S}\"]\n", w("labels", s)},
		"label in a map":     {head + "    labels: {a: \"${S}\"}\n", w("labels", s)},
		"user":               {head + "    user: \"${S}\"\n", w("user", s)},
		"working directory":  {head + "    working_dir: \"/${S}\"\n", w("working_dir", s)},
		"bind path":          {head + "    volumes: [\"/srv/${S}:/x\"]\n", w("volumes", s)},
		"long-form bind":     {head + "    volumes: [{type: bind, source: \"/srv/$S\", target: /x}]\n", w("volumes", s)},
		"name in a list":     {head + "    environment: [\"${S}=x\"]\n", w("environment", s)},
		"name, no equals":    {head + "    environment: [\"$S\"]\n", w("environment", s)},
		"volume label":       {"volumes:\n  v:\n    labels: {a: \"${S}\"}\n" + head, field("volume", "v", "labels", s)},
		"volume driver":      {"volumes:\n  v:\n    driver: \"${S}\"\n" + head, field("volume", "v", "driver", s)},
		"network label":      {"networks:\n  n:\n    labels: {a: \"${S}\"}\n" + head, field("network", "n", "labels", s)},
		"network option":     {"networks:\n  n:\n    driver_opts: {a: \"${S}\"}\n" + head, field("network", "n", "driver_opts", s)},
		"network driver":     {"networks:\n  n:\n    driver: \"${S}\"\n" + head, field("network", "n", "driver", s)},
		"through an alias":   {"x-c: &c [--pw, \"${S}\"]\n" + head + "    command: *c\n", w("command", s)},
		"through a merge":    {"x-c: &c\n  command: [--pw, \"${S}\"]\n" + head + "    <<: *c\n", w("command", s)},
		"a default":          {head + "    command: \"${S:-x}\"\n", w("command", s)},
		"unset's default":    {head + "    command: \"${S-x}\"\n", w("command", s)},
		"needed":             {head + "    command: \"${S:?why}\"\n", w("command", s)},
		"unset's needed":     {head + "    command: \"${S?why}\"\n", w("command", s)},
		"inside a default":   {head + "    command: \"${A:-$S}\"\n", w("command", "A", s)},
		"inside an other":    {head + "    command: \"${A:+--pw=$S}\"\n", w("command", s)},
		"its own other":      {head + "    command: \"run ${S:+--pw=$S}\"\n", w("command", s)},
		"unset's other":      {head + "    command: \"${A+$S}\"\n", w("command", s)},
		"only whether it is": {head + "    command: \"${S:+--flag}\"\n", nil},
		"a reason's":         {head + "    command: \"${A:?why $S}\"\n", w("command", "A")},
		"a dollar":           {head + "    command: \"$$S ${A:-$$S}\"\n", w("command", "A")},

		"what runs":             {"name: \"a$S\"\nservices:\n  w:\n    image: \"nginx:$S\"\n", nil},
		"where it's reached":    {head + "    ports: [\"$S:80\"]\n", nil},
		"a value in a map":      {head + "    environment: {A: \"$S\"}\n", nil},
		"a value in a list":     {head + "    environment: [\"A=$S\"]\n", nil},
		"a value with a brace":  {head + "    environment: [\"${A:-a=b}=$S\"]\n", w("environment", "A")},
		"a volume's options":    {"volumes:\n  v:\n    driver_opts: {o: \"$S\"}\n" + head, nil},
		"restart":               {head + "    restart: \"${S:-no}\"\n", nil},
		"a health check's time": {head + "    healthcheck: {test: x, interval: \"$S\", retries: \"$S\"}\n", nil},
		"expose":                {head + "    expose: [\"$S\"]\n", nil},
		"an extension":          {"x-a: \"$S\"\n" + head + "    x-b: \"$S\"\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := inspect(t.Context(), []byte(c.file), nil)
			if !slices.Equal(got.Exposed, c.want) {
				t.Errorf("with no value, Exposed = %+v, want %+v", got.Exposed, c.want)
			}
			if got.ExposedMore {
				t.Error("ExposedMore on a short file")
			}
		})
	}
}

// What Parse says is what a form said while the file was being typed.
func TestParseSaysWhatInspectDoes(t *testing.T) {
	file := []byte("services:\n  w:\n    image: nginx:1\n    command: x $S ${T:-a}\n    user: \"${U:-0}\"\n")
	stack, err := Parse(t.Context(), file, map[string]string{"S": "a"})
	if err != nil {
		t.Fatal(err)
	}
	want := inspect(t.Context(), file, nil).Exposed
	if len(want) != 3 || !slices.Equal(stack.Exposed, want) {
		t.Errorf("Parse said %+v, inspect %+v", stack.Exposed, want)
	}
}

// The threat model says these can't hold a secret, as a file that tries is
// refused, or what it says isn't kept. If this fails, take the field out of
// what exposedIn stays quiet about.
func TestAQuietFieldCantHoldASecret(t *testing.T) {
	const secret = "hunter2"
	const head = "services:\n  w:\n    image: nginx:1\n"
	for name, c := range map[string]struct {
		file    string
		refused bool
	}{
		"restart":      {head + "    restart: \"$S\"\n", true},
		"network_mode": {head + "    network_mode: \"$S\"\n", true},
		"networks":     {head + "    networks: [\"$S\"]\n", true},
		"depends_on":   {head + "    depends_on: [\"$S\"]\n", true},
		"interval":     {head + "    healthcheck: {test: x, interval: \"$S\"}\n", true},
		"timeout":      {head + "    healthcheck: {test: x, timeout: \"$S\"}\n", true},
		"start_period": {head + "    healthcheck: {test: x, start_period: \"$S\"}\n", true},
		"retries":      {head + "    healthcheck: {test: x, retries: \"$S\"}\n", true},
		"internal":     {"networks:\n  n:\n    internal: \"$S\"\n" + head, true},
		"expose":       {head + "    expose: [\"$S\"]\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			stack, err := Parse(t.Context(), []byte(c.file), map[string]string{"S": secret})
			if c.refused {
				if err == nil {
					t.Errorf("a file with %s of %s was taken", name, secret)
				}
				return
			}
			if err != nil {
				return // refused is as good
			}
			for _, s := range stack.Services {
				if strings.Contains(prototext.Format(s.Spec), secret) {
					t.Errorf("the spec keeps it: %s", prototext.Format(s.Spec))
				}
			}
		})
	}
}

// A plan keeps what it says for good, so a file can't make it say much.
func TestExposedStopsAtItsLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n  w:\n    image: nginx:1\n    command: \"")
	for i := range maxExposed + 50 {
		fmt.Fprintf(&b, "$V%03d", i)
	}
	b.WriteString("\"\n")
	first := inspect(t.Context(), []byte(b.String()), nil)
	if len(first.Exposed) != maxExposed || !first.ExposedMore {
		t.Fatalf("Exposed = %d, more = %t, want %d and more", len(first.Exposed), first.ExposedMore, maxExposed)
	}
	if first.Exposed[0].Variable != "V000" || first.Exposed[maxExposed-1].Variable != fmt.Sprintf("V%03d", maxExposed-1) {
		t.Errorf("the list isn't the first %d in order: %+v ... %+v", maxExposed, first.Exposed[0], first.Exposed[maxExposed-1])
	}
	if got := inspect(t.Context(), []byte(b.String()), nil); !slices.Equal(got.Exposed, first.Exposed) {
		t.Error("it lists others each time")
	}
	// Exactly at the limit is everything, and says so.
	b.Reset()
	b.WriteString("services:\n  w:\n    image: nginx:1\n    command: \"")
	for i := range maxExposed {
		fmt.Fprintf(&b, "$V%03d", i)
	}
	b.WriteString("\"\n")
	if got := inspect(t.Context(), []byte(b.String()), nil); len(got.Exposed) != maxExposed || got.ExposedMore {
		t.Errorf("at the limit: %d, more = %t", len(got.Exposed), got.ExposedMore)
	}
}

// name: is read by itself, whatever else the file gets wrong.
func TestAFileIsNamedByItsOwnVariables(t *testing.T) {
	for name, c := range map[string]struct {
		file  string
		given map[string]string
		want  string
	}{
		"plain":                    {"name: shop\n", nil, "shop"},
		"by a variable":            {"name: \"$N\"\n", map[string]string{"N": "shop"}, "shop"},
		"by a default":             {"name: \"${N:-shop}\"\n", nil, "shop"},
		"part of it with no value": {"name: \"a-$N\"\n", nil, ""},
		"by one with no value":     {"name: \"$N\"\n", nil, ""},
		"with others missing":      {"name: \"$N\"\nservices:\n  w:\n    image: \"$MISSING\"\n", map[string]string{"N": "shop"}, "shop"},
		"with others unreadable":   {"name: \"$N\"\nservices:\n  w:\n    image: \"${!}\"\n", map[string]string{"N": "shop"}, "shop"},
		"unreadable itself":        {"name: \"${!}\"\n", nil, ""},
		"not a string":             {"name: 7\n", nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := inspect(t.Context(), []byte(c.file), c.given).Name; got != c.want {
				t.Errorf("Name = %q, want %q", got, c.want)
			}
		})
	}
}
