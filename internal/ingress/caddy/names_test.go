// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// A name's pattern matches Basic credentials of that name whatever the password,
// empty, short, long or not ASCII, and no other name's, whatever its length (#606's
// review, M1).
func TestABasicNamesPatternMatchesItAlone(t *testing.T) {
	names := []string{"a", "ci", "ops", "admin", "ops.team_1", "a-b", "x" + strings.Repeat("y", 63), "Ci", "ci2", "c"}
	passwords := []string{"", "a", "ab", "abc", "correct horse battery staple", "éè", "\xff\x00", strings.Repeat("z", 72)}
	for _, name := range names {
		pattern := regexp.MustCompile(basicNamePattern(name))
		for _, other := range names {
			for _, pw := range passwords {
				header := "Basic " + base64.StdEncoding.EncodeToString([]byte(other+":"+pw))
				if got := pattern.MatchString(header); got != (other == name) {
					t.Errorf("%s's pattern on %s:%q = %v", name, other, pw, got)
				}
			}
		}
		if !pattern.MatchString("basic " + base64.StdEncoding.EncodeToString([]byte(name+":x"))) {
			t.Errorf("%s's pattern refuses the scheme in lower case, which Go takes", name)
		}
		if pattern.MatchString("Bearer " + base64.StdEncoding.EncodeToString([]byte(name+":x"))) {
			t.Errorf("%s's pattern takes another scheme", name)
		}
	}
}
