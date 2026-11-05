// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A field a variable can reach, for a value it takes there, and what Dockwell does
// with it. warns says a plan warns of a value there. shows is what a plan and an
// export then show of it, or empty for what they don't: a sensitive field, or one
// that isn't kept.
type fieldRow struct {
	kind, field, file, value, shows string
	warns                           bool
}

func serviceRow(field, body, value, shows string) fieldRow {
	return fieldRow{kind: "service", field: field, file: "services:\n  web:\n    image: nginx:1.27\n" + body, value: value, shows: shows}
}

func networkRow(field, body, value, shows string) fieldRow {
	return fieldRow{kind: "network", field: field, file: "services:\n  web:\n    image: nginx:1.27\n    networks: [back]\nnetworks:\n  back:\n" + body, value: value, shows: shows}
}

func volumeRow(field, body, value, shows string) fieldRow {
	return fieldRow{kind: "volume", field: field, file: "services:\n  web:\n    image: nginx:1.27\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n" + body, value: value, shows: shows}
}

func warns(r fieldRow) fieldRow {
	r.warns = true
	return r
}

const marker = "s3cr3tMarker7Hq"

func fieldRows() []fieldRow {
	return []fieldRow{
		// Free text, where a secret can really land.
		warns(serviceRow("command", "    command: run ${V}\n", marker, marker)),
		warns(serviceRow("entrypoint", "    entrypoint: [run, \"${V}\"]\n", marker, marker)),
		warns(serviceRow("healthcheck.test", "    healthcheck: {test: [CMD, run, \"${V}\"]}\n", marker, marker)),
		warns(serviceRow("labels", "    labels: {k: \"${V}\"}\n", marker, marker)),
		warns(networkRow("labels", "    labels: {k: \"${V}\"}\n", marker, marker)),
		warns(networkRow("driver_opts", "    driver_opts: {opt: \"${V}\"}\n", marker, marker)),
		warns(volumeRow("labels", "    labels: {k: \"${V}\"}\n", marker, marker)),

		// A name, a number or a path, which shows as what it is.
		{kind: "service", field: "image", file: "services:\n  web:\n    image: ${V}\n", value: "nginx:1.27", shows: "nginx:1.27"},
		serviceRow("container_name", "    container_name: ${V}\n    networks: [back]\nnetworks:\n  back: {}\n", marker, marker),
		serviceRow("network_mode", "    network_mode: ${V}\n", marker, marker),
		serviceRow("networks", "    networks: {back: {aliases: [\"${V}\"]}}\nnetworks:\n  back: {}\n", marker, marker),
		// Not free text, as a user is a name or a number, though nothing checks it.
		serviceRow("user", "    user: ${V}\n", marker, marker),
		serviceRow("cap_add", "    cap_add: [\"${V}\"]\n", "NET_ADMIN", "NET_ADMIN"),
		serviceRow("cap_drop", "    cap_drop: [\"${V}\"]\n", "MKNOD", "MKNOD"),
		serviceRow("group_add", "    group_add: [\"${V}\"]\n", "12345", "12345"),
		serviceRow("ports", "    ports: [\"${V}:80\"]\n", "18080", "18080"),
		serviceRow("restart", "    restart: ${V}\n", "always", "always"),
		serviceRow("cpus", "    cpus: ${V}\n", "1.5", "1.5"),
		serviceRow("mem_limit", "    mem_limit: ${V}\n", "64M", "64M"),
		serviceRow("mem_reservation", "    mem_reservation: ${V}\n", "32M", "32M"),
		serviceRow("deploy", "    deploy: {resources: {limits: {memory: \"${V}\"}}}\n", "64M", "64M"),
		serviceRow("privileged", "    privileged: ${V}\n", "true", "true"),
		// A size or a count, which goes in no spec, as Dockwell sets a log's rotation itself.
		serviceRow("logging", "    logging: {driver: json-file, options: {max-size: \"${V}\"}}\n", "64m", ""),
		serviceRow("stop_grace_period", "    stop_grace_period: ${V}\n", "45s", "45s"),
		serviceRow("healthcheck.interval", "    healthcheck: {test: [CMD, run], interval: \"${V}\"}\n", "45s", "45s"),
		serviceRow("healthcheck.timeout", "    healthcheck: {test: [CMD, run], timeout: \"${V}\"}\n", "45s", "45s"),
		serviceRow("healthcheck.start_period", "    healthcheck: {test: [CMD, run], start_period: \"${V}\"}\n", "45s", "45s"),
		serviceRow("healthcheck.retries", "    healthcheck: {test: [CMD, run], retries: \"${V}\"}\n", "7", "7"),
		serviceRow("healthcheck.disable", "    healthcheck: {disable: \"${V}\"}\n", "true", ""),
		serviceRow("volumes", "    volumes: [\"/srv/${V}:/data\"]\n", marker, marker),
		serviceRow("working_dir", "    working_dir: /srv/${V}\n", marker, marker),
		networkRow("driver", "    driver: ${V}\n", "overlay", "overlay"),
		networkRow("internal", "    internal: ${V}\n", "true", "true"),
		networkRow("external", "    external: ${V}\n", "true", "back"),
		networkRow("name", "    external: true\n    name: ${V}\n", marker, marker),
		volumeRow("driver", "    driver: ${V}\n", "custom", "custom"),

		// Sensitive, so a plan masks it.
		serviceRow("environment", "    environment: {K: \"${V}\"}\n", marker, ""),
		volumeRow("driver_opts", "    driver_opts: {type: \"${V}\"}\n", "tmpfs", ""),

		// Not kept, or never read.
		serviceRow("depends_on", "    depends_on: {db: {condition: \"${V}\"}}\n  db:\n    image: nginx:1.27\n", "service_started", ""),
		serviceRow("expose", "    expose: [\"${V}\"]\n", "18080", ""),
		serviceRow("build", "    build: {context: \"${V}\"}\n", marker, ""),
	}
}

// shown lists what a plan and an export show of what a file makes, with sensitive
// fields masked, as Diff masks them.
func shown(s *Stack) []resource.Change {
	var out []resource.Change
	add := func(m proto.Message) { out = append(out, resource.Diff(nil, m)...) }
	for _, i := range s.Services {
		add(i.Spec)
	}
	for _, i := range s.Networks {
		add(i.Spec)
	}
	for _, i := range s.Volumes {
		add(i.Spec)
	}
	return out
}

func showsValue(s *Stack, want string) bool {
	return slices.ContainsFunc(shown(s), func(c resource.Change) bool {
		return !c.Sensitive && (strings.Contains(strings.ToLower(c.To), strings.ToLower(want)) || strings.Contains(c.Path, want))
	})
}

// Every field a variable can reach has a row here, which decides whether a plan warns
// of a value in it: where it is free text, and not where it is a name, a number or a
// path, as nearly every real file writes those from a variable. A row is held to what
// it says, and to what a plan and an export show. A key Dockwell takes without a row
// fails, so a new field can't go without someone deciding it. For each source of a
// value: an .env, and the request's variables, which are the same mechanism.
func TestEveryFieldAVariableReaches(t *testing.T) {
	rows := fieldRows()
	covered := map[string]map[string]bool{"service": {}, "network": {}, "volume": {}, "healthcheck": {}}
	for _, r := range rows {
		key, in, inside := strings.Cut(r.field, ".")
		covered[r.kind][key] = true
		if inside {
			covered[key][in] = true
		}
	}
	for kind, all := range map[string]map[string]bool{"service": serviceKeys, "network": networkKeys, "volume": volumeKeys, "healthcheck": healthKeys} {
		for _, k := range slices.Sorted(maps.Keys(all)) {
			if !covered[kind][k] {
				t.Errorf("a %s's %s has no row here, so nothing says whether a plan warns of a value in it. Add one", kind, k)
			}
		}
	}
	for _, source := range []string{"env", "variables"} {
		for _, r := range rows {
			t.Run(source+"/"+r.kind+"/"+r.field, func(t *testing.T) {
				in := Input{File: []byte(r.file)}
				if source == "env" {
					in.Env = "V=" + r.value + "\n"
				} else {
					in.Variables = map[string]string{"V": r.value}
				}
				s, err := parse(t.Context(), in)
				if err != nil {
					t.Fatal(Message(err))
				}
				owner := map[string]string{"service": "web", "network": "back", "volume": "data"}[r.kind]
				key, _, _ := strings.Cut(r.field, ".")
				if r.warns && !slices.Contains(s.Exposed, Exposed{Variable: "V", Kind: r.kind, Owner: owner, Field: key}) {
					t.Errorf("the plan doesn't name V in %s: %+v", r.field, s.Exposed)
				}
				if !r.warns && len(s.Exposed) > 0 {
					t.Errorf("it warns of a field that decides it needn't: %+v", s.Exposed)
				}
				if r.shows == "" {
					if showsValue(s, r.value) {
						t.Errorf("the value shows, so this row is wrong: %+v", shown(s))
					}
				} else if !showsValue(s, r.shows) {
					t.Errorf("%q doesn't show, so this row is wrong: %+v", r.shows, shown(s))
				}
			})
		}
	}
}

// What makes those two fields quiet is that the schema masks them. If it stopped,
// they would need a note, so this fails first.
func TestTheQuietFieldsAreSensitive(t *testing.T) {
	for name, fd := range map[string]bool{
		"Container.env":  resource.IsSensitive((&resourcev1.Container{}).ProtoReflect().Descriptor().Fields().ByName("env")),
		"Volume.options": resource.IsSensitive((&resourcev1.Volume{}).ProtoReflect().Descriptor().Fields().ByName("options")),
	} {
		if !fd {
			t.Errorf("%s isn't (sensitive), so a value in it shows", name)
		}
	}
}

// A field no one has decided about warns, rather than slipping past.
func TestAFieldNoOneHasDecidedWarns(t *testing.T) {
	var x exposure
	x.note(place{owner: "the service web", path: "someday.soon"}, "V", false)
	x.note(place{owner: "the network back", path: "someday"}, "V", false)
	x.note(place{owner: "the volume data", path: "someday[0]"}, "V", false)
	got, _ := x.report()
	want := []Exposed{
		{Variable: "V", Kind: "network", Owner: "back", Field: "someday"},
		{Variable: "V", Kind: "service", Owner: "web", Field: "someday"},
		{Variable: "V", Kind: "volume", Owner: "data", Field: "someday"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("report = %+v, want %+v", got, want)
	}
}

// A value only warns if a variable gave it: a default is the file's own words, and
// what has nothing in it writes nothing.
func TestAValueTheFileGaveItselfDoesntWarn(t *testing.T) {
	const web = "services:\n  web:\n    image: nginx:1.27\n"
	for name, c := range map[string]struct {
		body string
		vars map[string]string
		want []string
	}{
		"a default, used":            {body: "    command: run ${V:-x}\n"},
		"a default without a colon":  {body: "    command: run ${V-x}\n"},
		"a default for an empty one": {body: "    command: run ${V:-x}\n", vars: map[string]string{"V": ""}},
		"nothing in it":              {body: "    command: run ${V}\n", vars: map[string]string{"V": ""}},
		"nothing in it, braces off":  {body: "    command: run $V\n", vars: map[string]string{"V": ""}},
		"nothing in it, a default":   {body: "    command: run ${V-x}\n", vars: map[string]string{"V": ""}},
		"another word for it":        {body: "    command: run ${V:+other}\n", vars: map[string]string{"V": "set"}},
		"a $ of its own":             {body: "    command: run $$V\n", vars: map[string]string{"V": "set"}},
		"an extension":               {body: "    x-note: ${V}\n", vars: map[string]string{"V": "set"}},
		"in environment, a mapping":  {body: "    environment: {K: \"${V}\"}\n", vars: map[string]string{"V": "set"}},
		"in environment, a list":     {body: "    environment: [\"K=${V}\"]\n", vars: map[string]string{"V": "set"}},
		"given, over a default":      {body: "    command: run ${V:-x}\n", vars: map[string]string{"V": "set"}, want: []string{"V"}},
		"given, without braces":      {body: "    command: run $V\n", vars: map[string]string{"V": "set"}, want: []string{"V"}},
		"given, needing a value":     {body: "    command: run ${V:?needs it}\n", vars: map[string]string{"V": "set"}, want: []string{"V"}},
		"given, within a default":    {body: "    command: run ${W:-${V}}\n", vars: map[string]string{"V": "set"}, want: []string{"V"}},
		"given, within another word": {body: "    command: run ${W:+${V}}\n", vars: map[string]string{"V": "set", "W": "on"}, want: []string{"V"}},
		// An entry's name isn't hidden, as the plan and an export show it.
		"naming a list's entry": {body: "    environment: [\"${V}=x\"]\n", vars: map[string]string{"V": "set"}, want: []string{"V"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := parse(t.Context(), Input{File: []byte(web + c.body), Variables: c.vars})
			if err != nil {
				t.Fatal(Message(err))
			}
			var got []string
			for _, e := range s.Exposed {
				got = append(got, e.Variable)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("it warns of %v, want %v", got, c.want)
			}
		})
	}
}

// A file can't fill a plan, which keeps what it says for good, with notes: it lists
// so many, and says there are more. Reading it stays within what a file costs.
func TestAFileCantFillAPlanWithNotes(t *testing.T) {
	guarded(t)
	vars := map[string]string{}
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx:1.27\n    command: run")
	for i := 0; b.Len() < MaxFile-64; i++ {
		name := "v" + strconv.Itoa(i)
		vars[name] = "1"
		b.WriteString(" $" + name)
	}
	file := b.String() + "\n"
	if len(vars) <= maxExposed {
		t.Fatalf("only %d variables, so this tests nothing", len(vars))
	}
	if _, err := guard(t.Context(), Input{File: []byte(file), Variables: vars}, false); err != nil {
		t.Errorf("reading %d of them in the child: %.200v", len(vars), err)
	}
	s, err := parse(t.Context(), Input{File: []byte(file), Variables: vars})
	if err != nil {
		t.Fatal(Message(err))
	}
	if len(s.Exposed) != maxExposed || !s.ExposedMore {
		t.Errorf("it lists %d of %d, more is %v, want %d and more", len(s.Exposed), len(vars), s.ExposedMore, maxExposed)
	}
	again, err := parse(t.Context(), Input{File: []byte(file), Variables: vars})
	if err != nil {
		t.Fatal(Message(err))
	}
	if !slices.Equal(again.Exposed, s.Exposed) {
		t.Error("which it lists differs from one read to the next")
	}
}

// Each name a plan says has a check of its own, so a file can't get here with one
// that reads as words. This is what holds if a new note names something that hasn't:
// it refuses the file, and doesn't repeat what it refuses.
func TestAPlanSaysNoNameThatReadsAsWords(t *testing.T) {
	for _, name := range []string{"a b", "a, b", "(a)", "a\nb", "a\tb", "a;b", "a\"b", "'a'", "", strings.Repeat("a", maxPlanName+1)} {
		for what, s := range map[string]*Stack{
			"a notice's service":  {Notices: []Notice{{Service: name}}},
			"a notice's name":     {Notices: []Notice{{Service: "web", Names: []string{"db", name}}}},
			"an exposure's name":  {Exposed: []Exposed{{Variable: name, Owner: "web", Field: "labels"}}},
			"an exposure's owner": {Exposed: []Exposed{{Variable: "V", Owner: name, Field: "labels"}}},
			"an exposure's field": {Exposed: []Exposed{{Variable: "V", Owner: "web", Field: name}}},
			"a name after a good": {Notices: []Notice{{Service: "web", Names: []string{"db"}}, {Service: name}}},
		} {
			err := checkPlanWords(s)
			if err == nil {
				t.Errorf("%s of %q passes", what, name)
				continue
			}
			if name != "" && strings.Contains(err.Error(), name) {
				t.Errorf("%s: the refusal repeats it: %v", what, err)
			}
		}
	}
	// What real names are all pass.
	for _, name := range []string{
		"web", "db_1", "a.b-c", "nginx:1.27", "ghcr.io/acme/web:1.4.2", "[::1]:5000/acme/web:1",
		"nginx@sha256:" + strings.Repeat("0", 64), "8080", "0.5", "command", "_V",
	} {
		s := &Stack{Notices: []Notice{{Service: "web", Names: []string{name}}}}
		if err := checkPlanWords(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Nothing a file can write gets as far as that check today, so that it is asked at
// all takes having it refuse everything. A file with no names to say still reads.
func TestReadingAFileAsksWhatAPlanCanSay(t *testing.T) {
	was := planName
	planName = regexp.MustCompile(`^$`)
	t.Cleanup(func() { planName = was })
	const web = "services:\n  web:\n    image: nginx:1.27\n"
	for name, c := range map[string]struct {
		body string
		vars map[string]string
	}{
		"a notice":    {body: "    network_mode: host\n    container_name: db\n"},
		"an exposure": {body: "    command: run ${V}\n", vars: map[string]string{"V": "x"}},
	} {
		_, err := parse(t.Context(), Input{File: []byte(web + c.body), Variables: c.vars})
		if err == nil || !strings.Contains(Message(err), "names something a plan can't say") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parse(t.Context(), Input{File: []byte(web)}); err != nil {
		t.Error(Message(err))
	}
}

// A form says where a variable's value could land in free text before anyone gives
// it one, in every form a file can write that.
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
		"name in a list":     {head + "    environment: [\"${S}=x\"]\n", w("environment", s)},
		"name, no equals":    {head + "    environment: [\"$S\"]\n", w("environment", s)},
		"volume label":       {"volumes:\n  v:\n    labels: {a: \"${S}\"}\n" + head, field("volume", "v", "labels", s)},
		"network label":      {"networks:\n  n:\n    labels: {a: \"${S}\"}\n" + head, field("network", "n", "labels", s)},
		"network option":     {"networks:\n  n:\n    driver_opts: {a: \"${S}\"}\n" + head, field("network", "n", "driver_opts", s)},
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
		"a user":                {head + "    user: \"${S}\"\n", nil},
		"a working directory":   {head + "    working_dir: \"/${S}\"\n", nil},
		"a mount":               {head + "    volumes: [\"/srv/${S}:/x\", {type: bind, source: \"/srv/$S\", target: /y}]\n", nil},
		"a driver":              {"volumes:\n  v:\n    driver: \"${S}\"\nnetworks:\n  n:\n    driver: \"${S}\"\n" + head, nil},
		"what's never read":     {head + "    build: {context: \"$S\"}\n", nil},
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
			got := inspect(t.Context(), Input{File: []byte(c.file)})
			if !slices.Equal(got.Exposed, c.want) {
				t.Errorf("with no value, Exposed = %+v, want %+v", got.Exposed, c.want)
			}
			if got.ExposedMore {
				t.Error("ExposedMore on a short file")
			}
		})
	}
}

// A form asks the same quiet fields as a plan, so what it warns of before there's a
// value is what a plan warns of once there is one, on every row. It goes by what a
// value could be, so no value at all still warns.
func TestAFormWarnsWhereAPlanWould(t *testing.T) {
	for _, r := range fieldRows() {
		t.Run(r.kind+"/"+r.field, func(t *testing.T) {
			got := inspect(t.Context(), Input{File: []byte(r.file)}).Exposed
			owner := map[string]string{"service": "web", "network": "back", "volume": "data"}[r.kind]
			key, _, _ := strings.Cut(r.field, ".")
			if r.warns && !slices.Contains(got, Exposed{Variable: "V", Kind: r.kind, Owner: owner, Field: key}) {
				t.Errorf("the form doesn't name V in %s: %+v", r.field, got)
			}
			if !r.warns && len(got) > 0 {
				t.Errorf("the form warns of a field that decides it needn't: %+v", got)
			}
		})
	}
}

// They differ only in what a value is: a plan goes by those it has, and a form by
// what could come, so a default's words warn a form and not a plan, as someone may
// yet give that variable a value.
func TestAPlanGoesByTheValuesItHasAndAFormByWhatCouldCome(t *testing.T) {
	file := []byte("services:\n  w:\n    image: nginx:1\n    command: x $S ${T:-a}\n")
	stack, err := Parse(t.Context(), file, map[string]string{"S": "a"})
	if err != nil {
		t.Fatal(err)
	}
	s := Exposed{Variable: "S", Kind: "service", Owner: "w", Field: "command"}
	if !slices.Equal(stack.Exposed, []Exposed{s}) {
		t.Errorf("Parse said %+v", stack.Exposed)
	}
	if got := inspect(t.Context(), Input{File: file}).Exposed; !slices.Equal(got, []Exposed{s, {Variable: "T", Kind: "service", Owner: "w", Field: "command"}}) {
		t.Errorf("inspect said %+v", got)
	}
}

// What a file reads beside it counts as Parse counts it: a variable an .env gives
// needs no other value, and a relative path has somewhere to start.
func TestCheckReadsWhatIsBesideTheFile(t *testing.T) {
	file := []byte("services:\n  w:\n    image: nginx:1\n    volumes: [\"./data:/data\"]\n    command: x ${S} ${T}\n")
	seen, stack, err := Check(t.Context(), Input{File: file, Env: "S=a\nT=b\n", Directory: "/srv/app"})
	if err != nil {
		t.Fatal(Message(err))
	}
	if stack == nil || !strings.Contains(stack.Services[0].Spec.GetApp().GetMounts()[0].String(), "/srv/app/data") {
		t.Errorf("the mount is %+v", stack)
	}
	for _, v := range seen.Variables {
		if v.Required {
			t.Errorf("%s needs a value, and the .env gives it", v.Name)
		}
	}
	if len(seen.Variables) != 2 {
		t.Errorf("Variables = %+v", seen.Variables)
	}
	// With T left out, it's still asked for, and the refusal is about T.
	seen, _, err = Check(t.Context(), Input{File: file, Env: "S=a\n", Directory: "/srv/app"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "T") {
		t.Errorf("with T left out = %v", err)
	}
	for _, v := range seen.Variables {
		if want := v.Name == "T"; v.Required != want {
			t.Errorf("%s required = %t, want %t", v.Name, v.Required, want)
		}
	}
	// Asked for values it doesn't have yet, what it refuses whatever they hold is
	// worked out with what's beside it too: this path has somewhere to start.
	seen, _, _ = Check(t.Context(), Input{File: file, Directory: "/srv/app"})
	if seen.Refused != nil {
		t.Errorf("before it has values, a path that has somewhere to start is refused: %+v", seen.Refused)
	}
	// And an .env can name it.
	if seen, _, _ := Check(t.Context(), Input{File: []byte("name: \"$N\"\nservices:\n  w:\n    image: nginx:1\n"), Env: "N=shop\n"}); seen.Name != "shop" {
		t.Errorf("an .env that names the file gives %q", seen.Name)
	}
	// With no directory, it says what's missing.
	if _, _, err := Check(t.Context(), Input{File: file, Env: "S=a\nT=b\n"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "directory") {
		t.Errorf("with no directory = %v", err)
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
	first := inspect(t.Context(), Input{File: []byte(b.String())})
	if len(first.Exposed) != maxExposed || !first.ExposedMore {
		t.Fatalf("Exposed = %d, more = %t, want %d and more", len(first.Exposed), first.ExposedMore, maxExposed)
	}
	if first.Exposed[0].Variable != "V000" || first.Exposed[maxExposed-1].Variable != fmt.Sprintf("V%03d", maxExposed-1) {
		t.Errorf("the list isn't the first %d in order: %+v ... %+v", maxExposed, first.Exposed[0], first.Exposed[maxExposed-1])
	}
	if got := inspect(t.Context(), Input{File: []byte(b.String())}); !slices.Equal(got.Exposed, first.Exposed) {
		t.Error("it lists others each time")
	}
	// Exactly at the limit is everything, and says so.
	b.Reset()
	b.WriteString("services:\n  w:\n    image: nginx:1\n    command: \"")
	for i := range maxExposed {
		fmt.Fprintf(&b, "$V%03d", i)
	}
	b.WriteString("\"\n")
	if got := inspect(t.Context(), Input{File: []byte(b.String())}); len(got.Exposed) != maxExposed || got.ExposedMore {
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
			if got := inspect(t.Context(), Input{File: []byte(c.file), Variables: c.given}).Name; got != c.want {
				t.Errorf("Name = %q, want %q", got, c.want)
			}
		})
	}
}
