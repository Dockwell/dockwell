// SPDX-License-Identifier: AGPL-3.0-only

package human

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour:               "1 h",
		24 * time.Hour:          "24 h",
		30 * time.Minute:        "30 min",
		90 * time.Minute:        "90 min",
		45 * time.Second:        "45 s",
		15 * time.Second:        "15 s",
		1500 * time.Millisecond: "1.5 s",
		0:                       "0 s",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestList(t *testing.T) {
	for want, names := range map[string][]string{
		"none":             nil,
		"hz-1":             {"hz-1"},
		"hz-1 and hz-2":    {"hz-1", "hz-2"},
		"hz-1, hz-2 and x": {"hz-1", "hz-2", "x"},
	} {
		if got := List(names); got != want {
			t.Errorf("List(%q) = %q, want %q", names, got, want)
		}
	}
}
