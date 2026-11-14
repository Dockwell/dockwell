// SPDX-License-Identifier: AGPL-3.0-only

package host

import "testing"

// Versions go as releases do: by number, then a release after its pre-releases,
// and a development build before any release.
func TestVersionsGoAsReleasesDo(t *testing.T) {
	for _, c := range [][2]string{
		{"0.9.3", "0.9.4"},
		{"v0.9.4", "0.10.0"},
		{"0.9.4-rc.1", "0.9.4"},
		{"0.9.4-rc.2", "0.9.4-rc.10"},
		{"0.9.4-alpha", "0.9.4-beta"},
		{"0.9.4-1", "0.9.4-alpha"},
		{"devel", "0.9.4"},
	} {
		if !Older(c[0], c[1]) {
			t.Errorf("%s isn't older than %s", c[0], c[1])
		}
		if Older(c[1], c[0]) {
			t.Errorf("%s is older than %s", c[1], c[0])
		}
	}
	if Older("0.9.4", "v0.9.4") || Older("0.9.4", "devel") {
		t.Error("a version is older than itself, or than a development build")
	}
}
