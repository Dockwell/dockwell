// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"strings"
	"testing"
)

// Root is root however it's written, as Docker's runtime reads a number.
func TestUserRunsAs(t *testing.T) {
	for user, want := range map[string]RunsAs{
		"0": RunsAsRoot, "00": RunsAsRoot, "000": RunsAsRoot, "+0": RunsAsRoot, "-0": RunsAsRoot, "0:0": RunsAsRoot, "00:1000": RunsAsRoot,
		"root": RunsAsRoot, "root:root": RunsAsRoot, "root:www-data": RunsAsRoot, ":1000": RunsAsRoot,
		"1000": RunsAsOther, "1000:1000": RunsAsOther, "1000:0": RunsAsOther, "1000:root": RunsAsOther, "65534": RunsAsOther, "010": RunsAsOther,
		"2147483647": RunsAsOther, "2147483648": RunsAsNamed, "4294967296": RunsAsNamed, "-4294967296": RunsAsNamed, "4294967296:0": RunsAsNamed,
		"-1": RunsAsNamed, "18446744073709551616": RunsAsNamed,
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

// Another tag or digest is the same repository, and an image's ID names that image
// alone: read as a name and a tag, every ID would be one repository.
func TestRepository(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	for ref, want := range map[string]string{
		"nginx":                             "nginx",
		"nginx:1.27":                        "nginx",
		"docker.io/library/nginx:1.27":      "nginx",
		"ghcr.io/acme/web:1.4@" + id:        "ghcr.io/acme/web",
		"localhost:5000/acme/web:1.4":       "localhost:5000/acme/web",
		id:                                  id,
		"sha256:" + strings.Repeat("b", 64): "sha256:" + strings.Repeat("b", 64),
	} {
		if got := Repository(ref); got != want {
			t.Errorf("Repository(%q) = %q, want %q", ref, got, want)
		}
	}
}
