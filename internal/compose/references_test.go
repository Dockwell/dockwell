// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

const refWeb = "services:\n  web:\n    image: nginx:1.27\n"

func oneApp(t *testing.T, in Input) (*resourcev1.Service, *Stack) {
	t.Helper()
	s, err := parse(t.Context(), in)
	if err != nil {
		t.Fatal(Message(err))
	}
	if len(s.Services) != 1 {
		t.Fatalf("the file makes %d services", len(s.Services))
	}
	return s.Services[0].Spec, s
}

// What a command, an entrypoint or a health check's test is given keeps its name, in
// every way a file can write it, and the value goes in the environment, where a plan
// masks it. These are the shapes real files have, as 1Panel's store has them.
func TestWhatRunsKeepsAValueByName(t *testing.T) {
	const value = "s3cr3tMarker7Hq"
	for name, c := range map[string]struct {
		body       string
		vars       map[string]string
		command    []string
		entrypoint []string
		test       []string
		references []string
		env        map[string]string
	}{
		"a line": {
			body:       "    command: redis-server --requirepass ${P}\n",
			command:    []string{"redis-server", "--requirepass", "${P}"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"without braces": {
			body:       "    command: redis-server --requirepass $P\n",
			command:    []string{"redis-server", "--requirepass", "${P}"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"a flag only when there's a password": {
			body:       "    command: redis-server ${P:+--requirepass \"${P}\"}\n",
			command:    []string{"redis-server", "--requirepass", "${P}"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"no flag when there's none": {
			body:    "    command: redis-server ${E:+--requirepass \"${E}\"}\n",
			vars:    map[string]string{"E": ""},
			command: []string{"redis-server"},
		},
		"in quotes": {
			body:       "    command: start-from-init --masterkey \"${P}\" --tlsMode external\n",
			command:    []string{"start-from-init", "--masterkey", "${P}", "--tlsMode", "external"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"within a word": {
			body:       "    command: [\"--api-key=${P}\", --enable-cors]\n",
			command:    []string{"--api-key=${P}", "--enable-cors"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"several in a word": {
			body:       "    command: [\"--dsn=${A}://${B}:${P}@${A}${B}/x\"]\n",
			vars:       map[string]string{"A": "pg", "B": "user"},
			command:    []string{"--dsn=${A}://${B}:${P}@${A}${B}/x"},
			references: []string{"command"}, env: map[string]string{"A": "pg", "B": "user", "P": value},
		},
		"an entrypoint": {
			body:       "    entrypoint: [run, \"${P}\"]\n",
			entrypoint: []string{"run", "${P}"},
			references: []string{"entrypoint"}, env: map[string]string{"P": value},
		},
		"an entrypoint as a line": {
			body:       "    entrypoint: run --key ${P}\n",
			entrypoint: []string{"run", "--key", "${P}"},
			references: []string{"entrypoint"}, env: map[string]string{"P": value},
		},
		"a test as a list": {
			body:       "    healthcheck: {test: [CMD, redis-cli, -a, \"${P}\", ping]}\n",
			test:       []string{"CMD", "redis-cli", "-a", "${P}", "ping"},
			references: []string{"healthcheck.test"}, env: map[string]string{"P": value},
		},
		"a test as a line, which a shell splits": {
			body:       "    healthcheck: {test: \"redis-cli -a ${P} ping\"}\n",
			test:       []string{"CMD-SHELL", "redis-cli -a ${P} ping"},
			references: []string{"healthcheck.test"}, env: map[string]string{"P": value},
		},
		"all three": {
			body:       "    command: [a, \"${P}\"]\n    entrypoint: [b, \"${P}\"]\n    healthcheck: {test: [CMD, c, \"${P}\"]}\n",
			command:    []string{"a", "${P}"},
			entrypoint: []string{"b", "${P}"},
			test:       []string{"CMD", "c", "${P}"},
			references: []string{"command", "entrypoint", "healthcheck.test"}, env: map[string]string{"P": value},
		},
		"a default, unused": {
			body:       "    command: [run, \"${P:-other}\"]\n",
			command:    []string{"run", "${P}"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"another's default, which is the file's own words": {
			body:    "    command: [--port, \"${UNSET:-6379}\"]\n",
			command: []string{"--port", "6379"},
		},
		"a default with a name in it": {
			body:       "    command: [\"${UNSET:---key ${P}}\"]\n",
			command:    []string{"--key ${P}"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"a value that needs to be there": {
			body:       "    command: [\"${P:?give it one}\"]\n",
			command:    []string{"${P}"},
			references: []string{"command"}, env: map[string]string{"P": value},
		},
		"only whether it is set, which isn't its value": {
			body:    "    command: [\"${P:+--secure}\"]\n",
			command: []string{"--secure"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			vars := map[string]string{"P": value}
			for k, v := range c.vars {
				vars[k] = v
			}
			spec, s := oneApp(t, Input{File: []byte(refWeb + c.body), Variables: vars})
			app := spec.GetApp()
			if !slices.Equal(app.GetCommand(), c.command) {
				t.Errorf("command = %q, want %q", app.GetCommand(), c.command)
			}
			if !slices.Equal(app.GetEntrypoint(), c.entrypoint) {
				t.Errorf("entrypoint = %q, want %q", app.GetEntrypoint(), c.entrypoint)
			}
			if !slices.Equal(app.GetHealthcheck().GetTest(), c.test) {
				t.Errorf("test = %q, want %q", app.GetHealthcheck().GetTest(), c.test)
			}
			if !slices.Equal(app.GetReferences(), c.references) {
				t.Errorf("references = %q, want %q", app.GetReferences(), c.references)
			}
			if len(app.GetEnv()) != len(c.env) {
				t.Errorf("env = %v, want %v", app.GetEnv(), c.env)
			}
			for k, v := range c.env {
				if app.GetEnv()[k] != v {
					t.Errorf("env[%s] = %q, want %q", k, app.GetEnv()[k], v)
				}
			}
			if len(s.Exposed) != 0 {
				t.Errorf("it warns of %+v", s.Exposed)
			}
			if showsValue(s, value) {
				t.Errorf("the plan shows the value: %+v", shown(s))
			}
			if err := (&service.Driver{}).Validate(proto.Clone(spec)); err != nil {
				t.Errorf("Validate: %v", err)
			}
			exported, err := Export("p", s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(exported.File), value) {
				t.Errorf("the export holds the value:\n%s", exported.File)
			}
		})
	}
}

// From an .env the same as from the request, as both are one mechanism.
func TestAnEnvFileKeepsItsValuesOutToo(t *testing.T) {
	spec, s := oneApp(t, Input{File: []byte(refWeb + "    command: redis-server --requirepass ${P}\n"), Env: "P=from-the-env-file\n"})
	if got := spec.GetApp().GetCommand(); !slices.Equal(got, []string{"redis-server", "--requirepass", "${P}"}) {
		t.Errorf("command = %q", got)
	}
	if got := spec.GetApp().GetEnv()["P"]; got != "from-the-env-file" {
		t.Errorf("env = %q", got)
	}
	if showsValue(s, "from-the-env-file") {
		t.Errorf("the plan shows the value: %+v", shown(s))
	}
}

// A $ of a file's own keeps its meaning. In a field with no name in it, it is a $, as
// it always was, so a file that names nothing plans just what it did. In one that names
// something, it is $$, and it comes back out as a $. A field beside it is neither.
func TestADollarKeepsItsMeaning(t *testing.T) {
	for name, c := range map[string]struct {
		body       string
		command    []string
		entrypoint []string
		references []string
		rendered   []string
	}{
		"a field that names nothing": {
			body:    "    command: [sh, -c, \"echo $$HOME\"]\n",
			command: []string{"sh", "-c", "echo $HOME"},
		},
		"a line that names nothing": {
			body:    "    command: sh -c 'echo $$HOME'\n",
			command: []string{"sh", "-c", "echo $HOME"},
		},
		"one beside a name": {
			body:       "    command: [sh, -c, \"echo $${HOME} ${P}\"]\n",
			command:    []string{"sh", "-c", "echo $${HOME} ${P}"},
			references: []string{"command"},
			rendered:   []string{"sh", "-c", "echo ${HOME} a$${b}c"},
		},
		"in a word of its own, beside a name": {
			body:       "    command: [\"$$HOME\", \"${P}\"]\n",
			command:    []string{"$$HOME", "${P}"},
			references: []string{"command"},
			rendered:   []string{"$HOME", "a$${b}c"},
		},
		"before anything else": {
			body:       "    command: [\"$1\", \"${P}\", \"a$\"]\n",
			command:    []string{"$$1", "${P}", "a$$"},
			references: []string{"command"},
			rendered:   []string{"$1", "a$${b}c", "a$"},
		},
		"a field beside one that names something": {
			body:       "    command: [\"${P}\"]\n    entrypoint: [sh, -c, \"echo $$HOME\"]\n",
			command:    []string{"${P}"},
			entrypoint: []string{"sh", "-c", "echo $HOME"},
			references: []string{"command"},
			rendered:   []string{"a$${b}c"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec, s := oneApp(t, Input{File: []byte(refWeb + c.body), Variables: map[string]string{"P": "a$${b}c"}})
			app := spec.GetApp()
			if !slices.Equal(app.GetCommand(), c.command) {
				t.Errorf("command = %q, want %q", app.GetCommand(), c.command)
			}
			if !slices.Equal(app.GetEntrypoint(), c.entrypoint) {
				t.Errorf("entrypoint = %q, want %q", app.GetEntrypoint(), c.entrypoint)
			}
			if !slices.Equal(app.GetReferences(), c.references) {
				t.Errorf("references = %q, want %q", app.GetReferences(), c.references)
			}
			if len(c.references) > 0 {
				got, err := binding.Render(app.GetCommand(), app.GetEnv())
				if err != nil || !slices.Equal(got, c.rendered) {
					t.Errorf("it comes to %q, %v, want %q", got, err, c.rendered)
				}
			}
			// Through an export, and back, it is the same spec, however it says it.
			exported, err := Export("p", s)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := oneApp(t, Input{File: exported.File, Variables: map[string]string{"P": "a$${b}c"}})
			if !resource.Equal(spec, again) {
				t.Errorf("after an export it reads\n%v\nwant\n%v\nfrom\n%s", again, spec, exported.File)
			}
		})
	}
}

// Compose splits a command written as one string once its variables are in, so a value
// can be several words, as `command: ${START_COMMAND}` is throughout 1Panel's store.
// One name can't stand for that, so it stays the text it was, and the plan says so.
// Written as a list, each word stays one, and that is the way out it offers.
func TestAValueOfSeveralWordsStaysText(t *testing.T) {
	for name, c := range map[string]struct {
		body, value string
		command     []string
		kept        bool
	}{
		"a command line in a variable":  {"    command: ${V}\n", "--model foo --port 8000", []string{"--model", "foo", "--port", "8000"}, false},
		"a space, in no quotes":         {"    command: run --key ${V}\n", "two words", []string{"run", "--key", "two", "words"}, false},
		"a backslash Compose would eat": {"    command: run --key ${V}\n", `a\b`, []string{"run", "--key", "ab"}, false},
		"where Compose stops reading":   {"    command: run --key=${V}\n", "a;b", []string{"run", "--key=a"}, false},
		"a space, in quotes":            {"    command: run --key \"${V}\"\n", "two words", []string{"run", "--key", "${V}"}, true},
		"a list keeps one word":         {"    command: [run, --key, \"${V}\"]\n", "two words", []string{"run", "--key", "${V}"}, true},
		"a shell in a list reads it":    {"    command: [/bin/bash, -lc, \"exec vllm serve ${V}\"]\n", "--model foo", []string{"/bin/bash", "-lc", "exec vllm serve ${V}"}, true},
		// A line Compose couldn't read with this value in it holds this to nothing, and the value is one word.
		"a quote of its own": {"    command: run --key ${V}\n", "it's", []string{"run", "--key", "${V}"}, true},
		"a $ of its own":     {"    command: run --key ${V}\n", "a$b", []string{"run", "--key", "${V}"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			spec, s := oneApp(t, Input{File: []byte(refWeb + c.body), Variables: map[string]string{"V": c.value}})
			app := spec.GetApp()
			if !slices.Equal(app.GetCommand(), c.command) {
				t.Errorf("command = %q, want %q", app.GetCommand(), c.command)
			}
			warned := slices.Contains(s.Exposed, Exposed{Variable: "V", Kind: "service", Owner: "web", Field: "command"})
			if c.kept {
				if warned || !slices.Equal(app.GetReferences(), []string{"command"}) || app.GetEnv()["V"] != c.value {
					t.Errorf("it warns: %v, references %q, env %q", warned, app.GetReferences(), app.GetEnv())
				}
				return
			}
			if !warned {
				t.Errorf("it stays text with no word of it: %+v", s.Exposed)
			}
			if len(app.GetReferences()) > 0 || len(app.GetEnv()) > 0 {
				t.Errorf("it reads as a name too: references %q, env %q", app.GetReferences(), app.GetEnv())
			}
		})
	}
}

// One name has one value in a container's environment. A file can write two, which is a
// mistake to say and not to choose between, and saying so names no value.
func TestANameCantHaveTwoValues(t *testing.T) {
	const file = refWeb + "    command: [run, \"${P}\"]\n    environment: {P: \"in the environment\"}\n"
	_, err := parse(t.Context(), Input{File: []byte(file), Variables: map[string]string{"P": "from a variable"}})
	if err == nil {
		t.Fatal("it took two values for one name")
	}
	msg := Message(err)
	if !strings.Contains(msg, "the service web has P in its environment, and a variable of that name in its command") || !strings.Contains(msg, "Rename one of them") {
		t.Errorf("it says %q", msg)
	}
	if strings.Contains(msg, "from a variable") || strings.Contains(msg, "in the environment,") && strings.Contains(msg, `"in the environment"`) {
		t.Errorf("it repeats a value: %q", msg)
	}
	same := refWeb + "    command: [run, \"${P}\"]\n    environment: {P: \"${P}\", Q: \"${P}\"}\n"
	spec, _ := oneApp(t, Input{File: []byte(same), Variables: map[string]string{"P": "one"}})
	if got := spec.GetApp().GetEnv(); got["P"] != "one" || got["Q"] != "one" {
		t.Errorf("env = %v", got)
	}
}

// A value the environment holds can't be given again, for a secret least of all, and
// giving one can't tell anyone anything: the file is refused whatever the value is, by
// the same check as a value in environment, and nothing compares them.
func TestAFileCantGiveASecretAValueThroughACommand(t *testing.T) {
	label := "    labels: {dockwell.uses: \"P=secret:P@3\"}\n"
	for name, body := range map[string]string{
		"named in environment too": label + "    command: [run, \"${P}\"]\n    environment: {P: null}\n",
		"named by the command":     label + "    command: [run, \"${P}\"]\n",
		"as a line":                label + "    command: run --key ${P}\n",
		"in a health check":        label + "    healthcheck: {test: [CMD, run, \"${P}\"]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			// With no value there's nothing to ask for, as the environment holds it.
			spec, _ := oneApp(t, Input{File: []byte(refWeb + body)})
			if got := spec.GetSecrets()["P"]; got != "P" {
				t.Fatalf("secrets = %v", spec.GetSecrets())
			}
			if len(spec.GetApp().GetEnv()) != 0 {
				t.Errorf("env = %v", spec.GetApp().GetEnv())
			}
			if err := (&service.Driver{}).Validate(proto.Clone(spec)); err != nil {
				t.Errorf("Validate: %v", err)
			}
			for source, in := range map[string]Input{
				"the request":             {File: []byte(refWeb + body), Variables: map[string]string{"P": "a-guess-at-it"}},
				"an .env":                 {File: []byte(refWeb + body), Env: "P=a-guess-at-it\n"},
				"with nothing in it":      {File: []byte(refWeb + body), Variables: map[string]string{"P": ""}},
				"of several words":        {File: []byte(refWeb + body), Variables: map[string]string{"P": "a guess at it"}},
				"Compose would read away": {File: []byte(refWeb + body), Variables: map[string]string{"P": `a\guess;at`}},
			} {
				given, _ := oneApp(t, in)
				err := (&service.Driver{}).Validate(proto.Clone(given))
				if err == nil || !strings.Contains(err.Error(), "is set in the app's environment and from a secret too") {
					t.Errorf("a value from %s: Validate = %v, on %v", source, err, given)
				}
				if err != nil && strings.Contains(err.Error(), "guess") {
					t.Errorf("a value from %s: the refusal repeats it: %v", source, err)
				}
			}
		})
	}
}

// A form asks only for what a file needs: a name the label says the environment holds, in
// what runs, isn't asked for, and the same name anywhere else or in another service is.
func TestAFormDoesntAskForWhatALabelHolds(t *testing.T) {
	const file = "services:\n  web:\n    image: nginx:1.27\n    labels: {dockwell.uses: \"P=secret:P@3,Q=secret:Q@1\"}\n" +
		"    command: [run, \"${P}\"]\n    healthcheck: {test: [CMD, run, \"${Q}\"]}\n" +
		"  other:\n    image: nginx:1.27\n    command: [run, \"${Q}\"]\n"
	got := inspect(t.Context(), Input{File: []byte(file)})
	if want := []Variable{{Name: "Q", Required: true}}; !slices.Equal(got.Variables, want) {
		t.Errorf("Variables = %+v, want %+v", got.Variables, want)
	}
}

// A health check that is off reads no test, so what its test names goes nowhere: not
// in what the container names, and not in its environment.
func TestAFieldThatGoesUnreadNamesNothing(t *testing.T) {
	spec, _ := oneApp(t, Input{
		File:      []byte(refWeb + "    healthcheck: {disable: true, test: [CMD, run, \"${P}\"]}\n"),
		Variables: map[string]string{"P": "a-value"},
	})
	if len(spec.GetApp().GetReferences()) != 0 || len(spec.GetApp().GetEnv()) != 0 {
		t.Errorf("references %q, env %v", spec.GetApp().GetReferences(), spec.GetApp().GetEnv())
	}
	if !slices.Equal(spec.GetApp().GetHealthcheck().GetTest(), []string{"NONE"}) {
		t.Errorf("test = %q", spec.GetApp().GetHealthcheck().GetTest())
	}
	if err := (&service.Driver{}).Validate(proto.Clone(spec)); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// A name the label lists and nothing uses any more is gone from what it uses, so taking
// it out of the file stops using it, as for environment.
func TestALabelsUseGoesWithItsLastMention(t *testing.T) {
	spec, _ := oneApp(t, Input{File: []byte(refWeb + "    labels: {dockwell.uses: \"P=secret:P@3\"}\n    command: [run]\n")})
	if len(spec.GetSecrets()) != 0 {
		t.Errorf("secrets = %v", spec.GetSecrets())
	}
}

// The label says only what it can say: it can't stand in for a value outside the
// fields that keep one by name, nor make a word of a label of its own.
func TestALabelGivesNoValueElsewhere(t *testing.T) {
	_, err := parse(t.Context(), Input{File: []byte(refWeb + "    labels: {dockwell.uses: \"P=secret:P@3\", a: \"${P}\"}\n    environment: {P: null}\n")})
	if err == nil || !strings.Contains(Message(err), "the Compose file uses P, with no value") {
		t.Errorf("a label's own word = %v", err)
	}
}

// What an export writes reads back as the spec it came from, with what it can't write
// asked for again, and a rotation's revision still the store's to set. A variable's value
// is in the file, and a command's ${NAME} reads a shell, so docker compose needs it there.
func TestAnExportOfAReferenceReadsBack(t *testing.T) {
	spec := &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image:      "redis:8",
			Command:    []string{"redis-server", "--requirepass", "${SEC}", "--port", "${PORT}", "--name", "${OWN}", "$$HOME"},
			Entrypoint: []string{"run", "$HOME"},
			Env:        map[string]string{"OWN": "own-value-in-the-spec"},
			References: []string{"command"},
			Restart:    resourcev1.RestartPolicy_RESTART_POLICY_NO,
		}},
		Secrets:   map[string]string{"SEC": "SEC"},
		Variables: map[string]string{"PORT": "PORT"},
		Revisions: map[string]int64{"SEC": 3, "PORT": 2},
	}
	stack := &Stack{Services: []Item[*resourcev1.Service]{{Name: "web", Spec: spec}}, Variables: map[string]string{"PORT": "6379"}}
	exported, err := Export("p", stack)
	if err != nil {
		t.Fatal(err)
	}
	file := string(exported.File)
	for _, want := range []string{"${SEC}", "${PORT}", "${OWN}", "$$HOME", "SEC: null", "OWN: null", "PORT: \"6379\"", "A ${NAME} in a command", `dev.dockwell.references: '{"command":`} {
		if !strings.Contains(file, want) {
			t.Errorf("the export doesn't say %s:\n%s", want, file)
		}
	}
	if strings.Contains(file, "own-value-in-the-spec") {
		t.Errorf("the export holds a value:\n%s", file)
	}
	if want := []string{"web.OWN", "web.PORT", "web.SEC"}; !slices.Equal(exported.Omitted, want) {
		t.Errorf("omitted = %v, want %v", exported.Omitted, want)
	}
	// Only what the file can't write is asked for: the container's own.
	if _, err := parse(t.Context(), Input{File: exported.File}); err == nil || !strings.Contains(Message(err), "the Compose file uses OWN, with no value") {
		t.Errorf("with nothing given: %v", err)
	}
	back, _ := oneApp(t, Input{File: exported.File, Variables: map[string]string{"OWN": "own-value-in-the-spec"}})
	// What the control plane does to a variable an export wrote (useVariables).
	delete(back.GetApp().Env, "PORT")
	want := proto.Clone(spec).(*resourcev1.Service)
	want.Revisions = nil
	got := proto.Clone(back).(*resourcev1.Service)
	got.Revisions = nil
	got.GetApp().Networks = nil
	got.GetApp().Labels = nil
	if err := (&service.Driver{}).Validate(want); err != nil {
		t.Fatal(err)
	}
	if err := (&service.Driver{}).Validate(got); err != nil {
		t.Fatal(err)
	}
	got.GetApp().Networks, want.GetApp().Networks = nil, nil
	got.GetApp().Rollout, want.GetApp().Rollout = nil, nil
	if !resource.Equal(got, want) {
		t.Errorf("it reads back as\n%v\nwant\n%v\nfrom\n%s", got, want, file)
	}
	if u := binding.Uses(back); len(u) != 2 || u[0].Kind != variable.VariableKind || u[1].Kind != variable.SecretKind {
		t.Errorf("uses = %+v", u)
	}
}

// A file can't spend what it takes to read it twice over by naming a great many, and
// what is many stays one spec that reads, within what a file costs.
func TestManyNamesCostWhatAFileCosts(t *testing.T) {
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
	if _, err := guard(t.Context(), Input{File: []byte(file), Variables: vars}, false); err != nil {
		t.Fatalf("reading %d of them in the child: %.200v", len(vars), err)
	}
	spec, s := oneApp(t, Input{File: []byte(file), Variables: vars})
	if got := len(spec.GetApp().GetEnv()); got != len(vars) {
		t.Errorf("%d of %d in the environment", got, len(vars))
	}
	if len(s.Exposed) != 0 {
		t.Errorf("it warns: %d", len(s.Exposed))
	}
}
