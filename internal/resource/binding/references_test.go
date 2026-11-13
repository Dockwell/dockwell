// SPDX-License-Identifier: AGPL-3.0-only

package binding_test

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/binding"
)

// A word is read once, so what a value holds is the value, however much it reads like a
// name or a $$. A password with a $ in it is that password.
func TestAValueIsNotReadAgain(t *testing.T) {
	values := map[string]string{"A": "${B}", "B": "$$", "C": "x"}
	got, err := binding.Render([]string{"${A}", "${B}", "${C}${C}", "$$C", "$${C}", "no name", ""}, values)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"${B}", "$$", "xx", "$C", "${C}", "no name", ""}; !slices.Equal(got, want) {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

// What can't be read says which word, and doesn't repeat it, as someone may have written
// a value in it. Nor does a name with no value.
func TestWhatCantBeRenderedSaysWhichAndNoMore(t *testing.T) {
	for _, word := range []string{"$s3cret", "${s3cret", "${s-3cret}", "${}", "s3cret$", "$", "${1s3cret}"} {
		_, err := binding.Render([]string{"fine", word}, nil)
		if err == nil || !strings.Contains(err.Error(), "word 2") {
			t.Errorf("Render(%q) = %v", word, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("Render(%q) repeats it: %v", word, err)
		}
	}
	_, err := binding.Render([]string{"${GONE}"}, map[string]string{"OTHER": "v4lue"})
	if err == nil || !strings.Contains(err.Error(), "GONE") || strings.Contains(err.Error(), "v4lue") {
		t.Errorf("Render of a name with no value = %v", err)
	}
}

func TestNamesInLists(t *testing.T) {
	got, err := binding.NamesIn([]string{"${B}${A}", "$$C", "${A}", "x"})
	if err != nil || !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("NamesIn = %v, %v", got, err)
	}
}

// Each refusal of a spec's references, and none says what the field holds.
func TestCheckWordsRefuses(t *testing.T) {
	known := func(n string) bool { return n == "KNOWN" }
	for name, c := range map[string]struct {
		c    *resourcev1.Container
		says string
	}{
		"a field that can't": {&resourcev1.Container{Image: "x", References: []string{"image"}}, "only command, entrypoint, healthcheck.test can name values"},
		"twice":              {&resourcev1.Container{Command: []string{"${KNOWN}"}, References: []string{"command", "command"}}, "lists command twice"},
		"nothing named":      {&resourcev1.Container{Command: []string{"s3cret"}, References: []string{"command"}}, "it names nothing"},
		"a stray $":          {&resourcev1.Container{Command: []string{"$s3cret"}, References: []string{"command"}}, "word 1 has a $ that isn't ${NAME}"},
		"no value for it":    {&resourcev1.Container{Command: []string{"${OTHER}"}, References: []string{"command"}}, "names OTHER, and the container has no value for it"},
		"an empty field":     {&resourcev1.Container{References: []string{"entrypoint"}}, "lists entrypoint, and it names nothing"},
		"a test with none":   {&resourcev1.Container{References: []string{"healthcheck.test"}}, "lists healthcheck.test, and it names nothing"},
		// What reading back says of a command it can't vouch for can't be kept.
		"what can't be read back": {&resourcev1.Container{Command: []string{binding.Unread}, References: []string{"command"}}, "it names nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			err := binding.CheckWords(c.c, known)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("CheckWords = %v, want it to say %q", err, c.says)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("it repeats a word: %v", err)
			}
		})
	}
	sorted := &resourcev1.Container{Command: []string{"${KNOWN}"}, Entrypoint: []string{"${KNOWN}"}, References: []string{"entrypoint", "command"}}
	if err := binding.CheckWords(sorted, known); err != nil || !slices.Equal(sorted.GetReferences(), []string{"command", "entrypoint"}) {
		t.Errorf("CheckWords = %v, references %v", err, sorted.GetReferences())
	}
}

// What Docker says goes where a person reads it. A long value is masked wherever it is,
// a short one only where it stands alone, and either as Go's %q writes it.
func TestHideMasksAValueInWhatDockerSays(t *testing.T) {
	for _, c := range []struct {
		text   string
		values []string
		want   string
	}{
		{`exec: "hunter2-long": executable file not found`, []string{"hunter2-long"}, `exec: "•••": executable file not found`},
		{`exec: "abc": not found`, []string{"abc"}, `exec: "•••": not found`},
		{`exec: "a\"b-quoted": not found`, []string{`a"b-quoted`}, `exec: "•••": not found`},
		{`exec: "a\"b": not found`, []string{`a"b`}, `exec: "•••": not found`},
		{`port 8080 and 18080 and 80801`, []string{"8080"}, `port ••• and 18080 and 80801`},
		{`failed with 1 error at line 12`, []string{"1"}, `failed with ••• error at line 12`},
		{`the abc in abcd and xabc`, []string{"abc"}, `the ••• in abcd and xabc`},
		{`both long-value-here and long-value`, []string{"long-value", "long-value-here"}, `both ••• and •••`},
		{`nothing here`, []string{""}, `nothing here`},
		{`nothing here`, nil, `nothing here`},
	} {
		if got := binding.Hide(c.text, c.values); got != c.want {
			t.Errorf("Hide(%q, %q) = %q, want %q", c.text, c.values, got, c.want)
		}
	}
}

// A field that doesn't name values runs ${NAME} as those words, which is worth saying
// where the container has a value of that name. A shell that reads it for itself is
// what that is for, so it isn't counted.
func TestNamedAsText(t *testing.T) {
	known := func(n string) bool { return n == "PW" || n == "TOKEN" }
	for name, c := range map[string]struct {
		c    *resourcev1.Container
		want map[string][]string
	}{
		"a command": {&resourcev1.Container{Command: []string{"redis-server", "--requirepass", "${PW}"}}, map[string][]string{"command": {"PW"}}},
		"two names, once each": {
			&resourcev1.Container{Command: []string{"--a=${PW}", "--b=${TOKEN}", "--c=${PW}"}},
			map[string][]string{"command": {"PW", "TOKEN"}},
		},
		"an entrypoint":             {&resourcev1.Container{Entrypoint: []string{"run", "${TOKEN}"}}, map[string][]string{"entrypoint": {"TOKEN"}}},
		"a test":                    {&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "redis-cli", "-a", "${PW}", "ping"}}}, map[string][]string{"healthcheck.test": {"PW"}}},
		"listed":                    {&resourcev1.Container{Command: []string{"${PW}"}, References: []string{"command"}}, nil},
		"a name it has no value of": {&resourcev1.Container{Command: []string{"${OTHER}", "${HOME}"}}, nil},
		"a shell's script":          {&resourcev1.Container{Command: []string{"sh", "-c", "exec redis-server --requirepass ${PW}"}}, nil},
		"a shell with flags":        {&resourcev1.Container{Command: []string{"/bin/bash", "-lc", "exec run ${PW}"}}, nil},
		"a shell's script beside":   {&resourcev1.Container{Entrypoint: []string{"/bin/sh", "-c"}, Command: []string{"exec run ${PW}"}}, nil},
		"a word before the shell":   {&resourcev1.Container{Command: []string{"--key=${PW}", "sh", "-c", "echo ${TOKEN}"}}, map[string][]string{"command": {"PW"}}},
		"a shell with no -c":        {&resourcev1.Container{Command: []string{"sh", "/run.sh", "${PW}"}}, map[string][]string{"command": {"PW"}}},
		"a test in a shell":         {&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "redis-cli -a ${PW} ping"}}}, nil},
		"a dollar of its own":       {&resourcev1.Container{Command: []string{"$PW", "$HOME"}}, nil},
		// What follows a shell's script are its $0, $1 and so on, which it passes on
		// as they are.
		"a shell's arguments": {
			&resourcev1.Container{Command: []string{"sh", "-c", `exec "$@"`, "--", "redis-server", "--requirepass", "${PW}"}},
			map[string][]string{"command": {"PW"}},
		},
		"a wrapper entrypoint": {
			&resourcev1.Container{Entrypoint: []string{"/bin/sh", "-c", `set -e; exec "$@"`, "--"}, Command: []string{"redis-server", "--requirepass", "${PW}"}},
			map[string][]string{"command": {"PW"}},
		},
		"a shell's $0": {
			&resourcev1.Container{Command: []string{"bash", "-c", `exec redis-server --requirepass "$0"`, "${PW}"}},
			map[string][]string{"command": {"PW"}},
		},
		"a script beside and an argument": {
			&resourcev1.Container{Entrypoint: []string{"sh", "-c"}, Command: []string{"exec run ${PW} \"$0\"", "${TOKEN}"}},
			map[string][]string{"command": {"TOKEN"}},
		},
		// And a shell's options, wherever its -c is among them.
		"a shell with an option's word":   {&resourcev1.Container{Command: []string{"bash", "-o", "pipefail", "-c", "redis-cli -a ${PW} ping"}}, nil},
		"a cluster with an option's word": {&resourcev1.Container{Command: []string{"bash", "-eo", "pipefail", "-c", "run ${PW}"}}, nil},
		"a shell with a long option":      {&resourcev1.Container{Command: []string{"bash", "--login", "-c", "run ${PW}"}}, nil},
		"a long option's word":            {&resourcev1.Container{Command: []string{"bash", "--rcfile", "/etc/rc", "-c", "run ${PW}"}}, nil},
		"an option after -c":              {&resourcev1.Container{Command: []string{"sh", "-c", "-e", "run ${PW}"}}, nil},
		"a script after --":               {&resourcev1.Container{Command: []string{"sh", "-c", "--", "run ${PW}"}}, nil},
		"busybox's shell":                 {&resourcev1.Container{Command: []string{"busybox", "sh", "-c", "run ${PW}"}}, nil},
		"no -c before --":                 {&resourcev1.Container{Command: []string{"sh", "--", "/run.sh", "${PW}"}}, map[string][]string{"command": {"PW"}}},
		"no -c before -":                  {&resourcev1.Container{Command: []string{"sh", "-", "${PW}"}}, map[string][]string{"command": {"PW"}}},
		"+c":                              {&resourcev1.Container{Command: []string{"sh", "+c", "run ${PW}"}}, map[string][]string{"command": {"PW"}}},
		"a test that runs a shell":        {&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "sh", "-c", "redis-cli -a ${PW} ping"}}}, nil},
		"a test's shell's argument": {
			&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "sh", "-c", `redis-cli -a "$1" ping`, "sh", "${PW}"}}},
			map[string][]string{"healthcheck.test": {"PW"}},
		},
		"a test with -c and no shell": {&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "-c", "${PW}"}}}, map[string][]string{"healthcheck.test": {"PW"}}},
	} {
		got := binding.NamedAsText(c.c, known)
		if len(got) != len(c.want) {
			t.Errorf("%s: NamedAsText = %v, want %v", name, got, c.want)
			continue
		}
		for field, names := range c.want {
			if !slices.Equal(got[field], names) {
				t.Errorf("%s: NamedAsText = %v, want %v", name, got, c.want)
			}
		}
	}
}

// L1 and L2: a shell's script keeps each name for the shell to read from the
// container's environment, quoted for where it stands, so a value is never in the
// script, nor in the process's command line, and never read as code. Other words
// have their values written in, as before (ADR 0085).
func TestAScriptKeepsItsNamesForTheShell(t *testing.T) {
	values := map[string]string{"PW": "x'; rm -rf / $(id) \"y", "USER": "app"}
	script := "redis-server --requirepass ${PW} --user=${USER}; echo '${PW}' \"${PW}\" \\'${PW}"
	got, err := binding.RenderField([]string{"sh", "-c", script}, values, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := "redis-server --requirepass \"${PW}\" --user=\"${USER}\"; echo ''\"${PW}\"'' \"${PW}\" \\'\"${PW}\""
	if got[2] != want {
		t.Errorf("the script is\n%s\nwant\n%s", got[2], want)
	}
	if strings.Contains(strings.Join(got, " "), "rm -rf") {
		t.Errorf("a value was written into the script: %q", got)
	}
	if got, _ := binding.RenderField([]string{"redis-server", "--requirepass", "${PW}"}, values, -1); got[2] != values["PW"] {
		t.Errorf("a word that isn't a script is %q", got[2])
	}
	if _, err := binding.RenderField([]string{"sh", "-c", "echo ${NOPE}"}, values, 2); err == nil || !strings.Contains(err.Error(), "no value for NOPE") {
		t.Errorf("a name with no value in a script: %v", err)
	}
	for name, c := range map[string]struct {
		spec  *resourcev1.Container
		field string
		want  int
	}{
		"a command after sh -c": {&resourcev1.Container{Entrypoint: []string{"sh", "-c"}, Command: []string{"echo ${X}"}}, binding.FieldCommand, 0},
		"its entrypoint":        {&resourcev1.Container{Entrypoint: []string{"sh", "-c"}, Command: []string{"echo ${X}"}}, binding.FieldEntrypoint, -1},
		"a command of its own":  {&resourcev1.Container{Command: []string{"bash", "-ec", "echo ${X}"}}, binding.FieldCommand, 2},
		"no shell":              {&resourcev1.Container{Command: []string{"redis-server", "${X}"}}, binding.FieldCommand, -1},
		"a CMD-SHELL test":      {&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "redis-cli -a ${X} ping"}}}, binding.FieldTest, 1},
		"a CMD test with sh -c": {&resourcev1.Container{Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "sh", "-c", "redis-cli -a ${X} ping"}}}, binding.FieldTest, 3},
	} {
		if got := binding.ScriptIn(c.spec, c.field); got != c.want {
			t.Errorf("%s: the script is word %d, want %d", name, got, c.want)
		}
	}
}
