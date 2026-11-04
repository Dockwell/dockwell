// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/compose"
)

// vars are what the templates in resolved are read with, and resolved
// what each comes to, as docker compose resolves it.
var (
	vars     = map[string]string{"SET": "s", "EMPTY": "", "OTHER": "o"}
	resolved = map[string]string{
		"${SET}":                 "s",
		"$SET-x":                 "s-x",
		"${UNSET:-d}":            "d",
		"${EMPTY:-d}":            "d",
		"${EMPTY-d}":             "",
		"${UNSET-d}":             "d",
		"${UNSET:-${OTHER}}":     "o",
		"${SET:-${UNSET}}":       "s",
		"${UNSET:-${UNSET2:-d}}": "d",
		"${UNSET:-x}-${SET}":     "x-s",
		"${SET:+alt}":            "alt",
		"${EMPTY:+alt}":          "",
		"${EMPTY+alt}":           "alt",
		"${UNSET:+${UNSET2}}":    "",
		"${SET:+${OTHER}}":       "o",
		"$$SET":                  "$SET",
		"${UNSET:-$$}":           "$",
		"${UNSET:-{a}}":          "{a}",
		"cost $5":                "cost $5",
		"${SET:?needed}":         "s",
	}
)

// environment is a file whose service web sets V to tmpl.
func environment(tmpl string) []byte {
	return []byte("services: {web: {image: a, environment: {V: '" + tmpl + "'}}}")
}

// Variables resolve as docker compose resolves them, defaults and
// alternatives inside each other included, except that one with no value,
// and no default where it is used, is refused at every level rather than
// left blank.
func TestVariables(t *testing.T) {
	for tmpl, want := range resolved {
		t.Run(tmpl, func(t *testing.T) {
			s, err := compose.Parse(t.Context(), environment(tmpl), vars)
			if err != nil {
				t.Fatalf("Parse = %v", err)
			}
			if got := s.Services[0].Spec.GetApp().GetEnv()["V"]; got != want {
				t.Errorf("%s = %q, want %q", tmpl, got, want)
			}
		})
	}
	for tmpl, want := range map[string]string{
		"${UNSET}":             "the Compose file uses UNSET, with no value",
		"$UNSET":               "the Compose file uses UNSET, with no value",
		"${UNSET:-${INNER}}":   "the Compose file uses INNER, with no value",
		"${UNSET-${INNER}}":    "the Compose file uses INNER, with no value",
		"${UNSET:-x}-${AFTER}": "the Compose file uses AFTER, with no value",
		"${SET:+${INNER}}":     "the Compose file uses INNER, with no value",
		"${A}${B:-${C}}":       "the Compose file uses A, C, with no value",
		"${UNSET:?set it}":     "the service web's environment.V needs UNSET: set it. Give it a value in variables",
		"${EMPTY:?}":           "the service web's environment.V needs EMPTY, which has no value. Give it one in variables",
		"${UNSET?${INNER}}":    "the Compose file uses INNER, with no value",
		"${1}":                 "the service web's environment.V has a variable Dockwell can't read, ${1}",
		"${SET":                "the service web's environment.V has a variable Dockwell can't read, ${SET",
		"${SET:*x}":            "the service web's environment.V has a variable Dockwell can't read, ${SET:*x}",
	} {
		t.Run(tmpl, func(t *testing.T) {
			_, err := compose.Parse(t.Context(), environment(tmpl), vars)
			if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(compose.Message(err), want) {
				t.Errorf("Parse = %v, want %q", err, want)
			}
		})
	}
}

// Each level of nesting reads the rest of its string again and expands to
// almost nothing, so the budget for what variables expand to doesn't see
// it: 160,000 levels in a file of 960 KiB took 34 s of CPU.
func TestVariablesNestToALimit(t *testing.T) {
	nested := func(depth int) []byte {
		return environment(strings.Repeat("${A:-", depth) + "x" + strings.Repeat("}", depth))
	}
	s, err := compose.Parse(t.Context(), nested(compose.MaxNesting), nil)
	if err != nil {
		t.Fatalf("%d levels: %v", compose.MaxNesting, err)
	}
	if got := s.Services[0].Spec.GetApp().GetEnv()["V"]; got != "x" {
		t.Errorf("%d levels read as %q, want x", compose.MaxNesting, got)
	}
	for _, depth := range []int{compose.MaxNesting + 1, 40_000} {
		_, err := compose.Parse(t.Context(), nested(depth), nil)
		if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(err.Error(), "nests variables") {
			t.Errorf("%d levels: Parse = %v, want them refused", depth, err)
		}
	}
	// A default that isn't used costs nothing, and isn't read.
	if _, err := compose.Parse(t.Context(), environment("${SET:-"+strings.Repeat("${UNSET:-", compose.MaxNesting+1)+"x"+strings.Repeat("}", compose.MaxNesting+2)), vars); err != nil {
		t.Errorf("a deep default that isn't used: %v", err)
	}
}

// The YAML library counts nodes, not bytes, so a short file can repeat a
// long string: this one came to 87 GiB and took the control plane down.
func TestAFileComesToALimit(t *testing.T) {
	aliases := func(literal, count int) []byte {
		return []byte("x-a: &a " + strings.Repeat("v", literal) + "\nservices:\n  web:\n    image: nginx:1\n    labels: [" +
			strings.TrimSuffix(strings.Repeat("*a,", count), ",") + "]\n")
	}
	for name, file := range map[string][]byte{
		"a long string, many times":  aliases(128<<10, 40_000),
		"a short string, many times": aliases(16<<10, 70_000),
		"a long string, 5 times":     aliases(128<<10, 5),
	} {
		_, err := compose.Parse(t.Context(), file, nil)
		if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(err.Error(), "once its aliases are read") {
			t.Errorf("%s: Parse = %.200v, want it refused", name, err)
		}
	}

	// An extension nobody uses costs nothing, and what's used a few times passes.
	file := "x-unused: &u " + strings.Repeat("v", 200<<10) + "\n" +
		"x-common: &c {restart: always}\nservices:\n  a:\n    image: nginx:1\n    <<: *c\n  b:\n    image: nginx:1\n    <<: *c\n"
	if _, err := compose.Parse(t.Context(), []byte(file), nil); err != nil {
		t.Errorf("a file with anchors: %v", err)
	}

	// A file under the cap with no aliases always passes, however small its values.
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx:1\n    command: [a")
	for b.Len() < compose.MaxFile-10 {
		b.WriteString(",a")
	}
	b.WriteString("]\n")
	if _, err := compose.Parse(t.Context(), []byte(b.String()), nil); err != nil {
		t.Errorf("a file of %d bytes and no aliases: %.200v", b.Len(), err)
	}
}

// compose-go's canonical forms take time that grows with the square of a
// file's depth: 17 KiB nested 9,000 deep took 30 s and 48 GiB of allocations.
func TestAFileNestsToALimit(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("[", depth) + strings.Repeat("]", depth)
	}
	// Nesting past Compose's own forms has nowhere to go but an extension, or
	// a name a file sets itself, which the rules refuse later.
	deep := func(depth int) []byte {
		return []byte("services:\n  web:\n    image: nginx:1\n    deep: " + nested(depth) + "\n")
	}
	for _, depth := range []int{compose.MaxDepth - 2, 9000} {
		_, err := compose.Parse(t.Context(), deep(depth), nil)
		if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(err.Error(), "levels deep") {
			t.Errorf("%d levels: Parse = %.200v, want them refused for it", depth, err)
		}
	}
	if _, err := compose.Parse(t.Context(), deep(compose.MaxDepth-3), nil); err == nil || strings.Contains(err.Error(), "levels deep") {
		t.Errorf("%d levels: Parse = %.200v, want them through to the rules", compose.MaxDepth, err)
	}

	// An extension isn't read, wherever it is and however it is made.
	for name, file := range map[string]string{
		"at the top":        "x-deep: " + nested(9000) + "\nservices:\n  web:\n    image: nginx:1\n",
		"on a service":      "services:\n  web:\n    image: nginx:1\n    x-deep: " + nested(9000) + "\n",
		"on a volume":       "services:\n  web:\n    image: nginx:1\nvolumes:\n  data:\n    x-deep: " + nested(9000) + "\n",
		"wide on a service": "services:\n  web:\n    image: nginx:1\n    x-wide: " + strings.Repeat("[", 30) + strings.Repeat("a,", 100_000) + "a" + strings.Repeat("]", 30) + "\n",
	} {
		if _, err := compose.Parse(t.Context(), []byte(file), nil); err != nil {
			t.Errorf("%s: %.200v", name, err)
		}
	}

	// In what a file sets for a service, x- is a name like any other.
	s, err := compose.Parse(t.Context(), []byte("services:\n  web:\n    image: nginx:1\n    labels: {x-team: a}\n    environment: {x-key: b}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	app := s.Services[0].Spec.GetApp()
	if app.GetLabels()["x-team"] != "a" || app.GetEnv()["x-key"] != "b" {
		t.Errorf("x- names in labels and environment are gone: %v, %v", app.GetLabels(), app.GetEnv())
	}

}
