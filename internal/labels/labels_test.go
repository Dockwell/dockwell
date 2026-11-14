// SPDX-License-Identifier: AGPL-3.0-only

package labels

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// A label reads as key:value, and a selector as such terms, a key alone and ! in
// front included, one way (ADR 0091).
func TestLabelsAndSelectors(t *testing.T) {
	if k, v, err := Parse(" region:eu "); err != nil || k != "region" || v != "eu" {
		t.Errorf("Parse = %q, %q, %v", k, v, err)
	}
	if k, v, err := Parse("gpu"); err != nil || k != "gpu" || v != "" {
		t.Errorf("Parse of a key alone = %q, %q, %v", k, v, err)
	}
	if _, _, err := Parse("region:eu:west"); err == nil {
		t.Error("a value with : was taken")
	}
	got, err := Apply(map[string]string{"region": "us", "arch": "amd64"}, []string{"region:eu", "pool:web"}, []string{"arch"})
	if err != nil || !maps.Equal(got, map[string]string{"region": "eu", "pool": "web"}) {
		t.Errorf("Apply = %v, %v", got, err)
	}
	if _, err := Apply(nil, []string{"region:eu"}, []string{"region"}); err == nil || !strings.Contains(err.Error(), "both set and removed") {
		t.Errorf("setting and removing one key: %v", err)
	}
	if s := Strings(map[string]string{"pool": "web", "gpu": "", "region": "eu"}); !slices.Equal(s, []string{"gpu", "pool:web", "region:eu"}) {
		t.Errorf("Strings = %v", s)
	}

	sel, err := ParseSelector("pool:web  region:eu !draining")
	if err != nil || sel.String() != "pool:web region:eu !draining" {
		t.Fatalf("ParseSelector = %v, %v", sel, err)
	}
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"pool": "web", "region": "eu"}, true},
		{map[string]string{"pool": "web", "region": "eu", "draining": ""}, false},
		{map[string]string{"pool": "web", "region": "us"}, false},
		{map[string]string{"region": "eu"}, false},
	} {
		if got := sel.Matches(c.labels); got != c.want {
			t.Errorf("%q matches %v = %v", sel, c.labels, got)
		}
	}
	has, _ := ParseSelector("gpu !region:us")
	if !has.Matches(map[string]string{"gpu": "a100", "region": "eu"}) || has.Matches(map[string]string{"region": "eu"}) {
		t.Error("a key alone, or ! with a value, matched wrongly")
	}
	if empty, err := ParseSelector("  "); err != nil || !empty.Matches(nil) {
		t.Errorf("an empty selector: %v", err)
	}
	if _, err := ParseSelector("pool:web -region"); err == nil || !strings.Contains(err.Error(), "the selector") {
		t.Errorf("a bad term: %v", err)
	}
}
