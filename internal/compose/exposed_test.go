// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
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
// with it. shows is what a plan and an export then show, or empty for what they
// don't: a sensitive field, or one that isn't kept.
type fieldRow struct {
	kind, field, file, value, shows string
}

func serviceRow(field, body, value, shows string) fieldRow {
	return fieldRow{"service", field, "services:\n  web:\n    image: nginx:1.27\n" + body, value, shows}
}

func networkRow(field, body, value, shows string) fieldRow {
	return fieldRow{"network", field, "services:\n  web:\n    image: nginx:1.27\n    networks: [back]\nnetworks:\n  back:\n" + body, value, shows}
}

func volumeRow(field, body, value, shows string) fieldRow {
	return fieldRow{"volume", field, "services:\n  web:\n    image: nginx:1.27\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n" + body, value, shows}
}

const marker = "s3cr3tMarker7Hq"

func fieldRows() []fieldRow {
	return []fieldRow{
		{"service", "image", "services:\n  web:\n    image: ${V}\n", "nginx:1.27", "nginx:1.27"},
		serviceRow("command", "    command: run ${V}\n", marker, marker),
		serviceRow("entrypoint", "    entrypoint: [run, \"${V}\"]\n", marker, marker),
		serviceRow("environment", "    environment: {K: \"${V}\"}\n", marker, ""),
		serviceRow("ports", "    ports: [\"${V}:80\"]\n", "18080", "18080"),
		serviceRow("restart", "    restart: ${V}\n", "always", "always"),
		serviceRow("labels", "    labels: {k: \"${V}\"}\n", marker, marker),
		serviceRow("volumes", "    volumes: [\"/srv/${V}:/data\"]\n", marker, marker),
		serviceRow("networks", "    networks: {back: {aliases: [\"${V}\"]}}\nnetworks:\n  back: {}\n", marker, marker),
		serviceRow("network_mode", "    network_mode: ${V}\n", marker, marker),
		serviceRow("user", "    user: ${V}\n", marker, marker),
		serviceRow("working_dir", "    working_dir: /srv/${V}\n", marker, marker),
		serviceRow("healthcheck", "    healthcheck: {test: [CMD, run, \"${V}\"]}\n", marker, marker),
		serviceRow("depends_on", "    depends_on: {db: {condition: \"${V}\"}}\n  db:\n    image: nginx:1.27\n", "service_started", ""),
		serviceRow("expose", "    expose: [\"${V}\"]\n", "18080", ""),
		serviceRow("container_name", "    container_name: ${V}\n    networks: [back]\nnetworks:\n  back: {}\n", marker, marker),
		// Nothing reads it, so its variables aren't read at all.
		serviceRow("build", "    build: {context: \"${V}\"}\n", marker, ""),
		serviceRow("deploy", "    deploy: {resources: {limits: {memory: \"${V}\"}}}\n", "64M", "64M"),
		serviceRow("cpus", "    cpus: ${V}\n", "1.5", "1.5"),
		serviceRow("mem_limit", "    mem_limit: ${V}\n", "64M", "64M"),
		serviceRow("mem_reservation", "    mem_reservation: ${V}\n", "32M", "32M"),
		serviceRow("cap_add", "    cap_add: [\"${V}\"]\n", "NET_ADMIN", "NET_ADMIN"),
		serviceRow("cap_drop", "    cap_drop: [\"${V}\"]\n", "MKNOD", "MKNOD"),
		serviceRow("group_add", "    group_add: [\"${V}\"]\n", "12345", "12345"),
		serviceRow("privileged", "    privileged: ${V}\n", "true", "true"),
		networkRow("driver", "    driver: ${V}\n", "overlay", "overlay"),
		networkRow("driver_opts", "    driver_opts: {opt: \"${V}\"}\n", marker, marker),
		networkRow("internal", "    internal: ${V}\n", "true", "true"),
		networkRow("labels", "    labels: {k: \"${V}\"}\n", marker, marker),
		networkRow("external", "    external: ${V}\n", "true", "back"),
		networkRow("name", "    external: true\n    name: ${V}\n", marker, marker),
		volumeRow("driver", "    driver: ${V}\n", "custom", "custom"),
		volumeRow("driver_opts", "    driver_opts: {type: \"${V}\"}\n", "tmpfs", ""),
		volumeRow("labels", "    labels: {k: \"${V}\"}\n", marker, marker),
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

// Every field a variable can reach is either named in the plan, as what it holds
// shows in the plan and an export, or is one of a few that don't show. A row says
// which, and it is held to it both ways. A key Dockwell takes without a row here
// fails, so a new field can't go without someone deciding it. For each source of a
// value: an .env, and the request's variables, which are the same mechanism.
func TestEveryFieldAVariableReaches(t *testing.T) {
	rows := fieldRows()
	covered := map[string]map[string]bool{"service": {}, "network": {}, "volume": {}}
	for _, r := range rows {
		covered[r.kind][r.field] = true
	}
	for kind, all := range map[string]map[string]bool{"service": serviceKeys, "network": networkKeys, "volume": volumeKeys} {
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
				s, err := parse(in)
				if err != nil {
					t.Fatal(Message(err))
				}
				owner := map[string]string{"service": "web", "network": "back", "volume": "data"}[r.kind]
				noted := slices.Contains(s.Exposed, Exposed{Variable: "V", Kind: r.kind, Owner: owner, Field: r.field})
				if r.shows == "" {
					if noted || len(s.Exposed) > 0 {
						t.Errorf("it warns of a field that doesn't show: %+v", s.Exposed)
					}
					if showsValue(s, r.value) {
						t.Errorf("the value shows, so this field needs a note: %+v", shown(s))
					}
					return
				}
				if !noted {
					t.Errorf("the plan doesn't name V in %s: %+v", r.field, s.Exposed)
				}
				if !showsValue(s, r.shows) {
					t.Errorf("%q doesn't show, so the note isn't true of it: %+v", r.shows, shown(s))
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
			s, err := parse(Input{File: []byte(web + c.body), Variables: c.vars})
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
	if err := guard(t.Context(), Input{File: []byte(file), Variables: vars}); err != nil {
		t.Errorf("reading %d of them in the child: %.200v", len(vars), err)
	}
	s, err := parse(Input{File: []byte(file), Variables: vars})
	if err != nil {
		t.Fatal(Message(err))
	}
	if len(s.Exposed) != maxExposed || !s.ExposedMore {
		t.Errorf("it lists %d of %d, more is %v, want %d and more", len(s.Exposed), len(vars), s.ExposedMore, maxExposed)
	}
	again, err := parse(Input{File: []byte(file), Variables: vars})
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
		_, err := parse(Input{File: []byte(web + c.body), Variables: c.vars})
		if err == nil || !strings.Contains(Message(err), "names something a plan can't say") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parse(Input{File: []byte(web)}); err != nil {
		t.Error(Message(err))
	}
}
