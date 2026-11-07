// SPDX-License-Identifier: AGPL-3.0-only

package docker

import "testing"

// Root is root however it's written, as Docker's runtime reads a number.
func TestUserRunsAs(t *testing.T) {
	for user, want := range map[string]RunsAs{
		"0": RunsAsRoot, "00": RunsAsRoot, "000": RunsAsRoot, "+0": RunsAsRoot, "-0": RunsAsRoot, "0:0": RunsAsRoot, "00:1000": RunsAsRoot,
		"root": RunsAsRoot, "root:root": RunsAsRoot, "root:www-data": RunsAsRoot, ":1000": RunsAsRoot,
		"1000": RunsAsOther, "1000:1000": RunsAsOther, "1000:0": RunsAsOther, "1000:root": RunsAsOther, "65534": RunsAsOther, "010": RunsAsOther,
		"www-data": RunsAsNamed, "app:app": RunsAsNamed, "toor": RunsAsNamed, "Root": RunsAsNamed, "\x200": RunsAsNamed,
		"": RunsAsImage,
	} {
		if got, _ := UserRunsAs(user); got != want {
			t.Errorf("UserRunsAs(%q) = %v, want %v", user, got, want)
		}
	}
	if _, name := UserRunsAs("www-data:www-data"); name != "www-data" {
		t.Errorf("the name of www-data:www-data is %q", name)
	}
}
