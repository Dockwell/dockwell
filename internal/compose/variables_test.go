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
