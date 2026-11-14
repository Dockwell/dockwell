// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"strings"
	"testing"
)

// A label's key and value are as Kubernetes has them (ADR 0091).
func TestALabelIsAsKubernetesHasIt(t *testing.T) {
	for _, key := range []string{"region", "pool", "example.com/tier", "a", "A_b-c.d", strings.Repeat("k", 63), strings.Repeat("p", 60) + ".com/x"} {
		if err := CheckLabelKey(key); err != nil {
			t.Errorf("CheckLabelKey(%q) = %v", key, err)
		}
	}
	for key, says := range map[string]string{
		"":                              "needs a key",
		"/region":                       "no prefix",
		"Example.com/tier":              "isn't a domain",
		"example.com/":                  "no name after its prefix",
		"-region":                       "start and end with a letter or digit",
		"region-":                       "start and end with a letter or digit",
		"re gion":                       "letters, digits, -, _ and . alone",
		"re:gion":                       "letters, digits, -, _ and . alone",
		strings.Repeat("k", 64):         "past the 63",
		"a/b/c":                         "letters, digits, -, _ and . alone",
		strings.Repeat("p", 254) + "/x": "past the 253",
	} {
		if err := CheckLabelKey(key); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("CheckLabelKey(%q) = %v, want it to say %q", key, err, says)
		}
	}
	for _, value := range []string{"", "eu", "web-1", "1.2.3"} {
		if err := CheckLabelValue("k", value); err != nil {
			t.Errorf("CheckLabelValue(%q) = %v", value, err)
		}
	}
	if err := CheckLabelValue("region", "eu/west"); err == nil || !strings.Contains(err.Error(), "the value of the label region") {
		t.Errorf("a value with / = %v", err)
	}
	many := map[string]string{}
	for i := range MaxLabels + 1 {
		many[strings.Repeat("k", i+1)[:min(i+1, 60)]+string(rune('a'+i%26))] = ""
	}
	if err := CheckLabels(many); err == nil {
		t.Error("too many labels were taken")
	}
}
