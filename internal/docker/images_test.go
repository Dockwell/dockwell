// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// What a registry calls an image comes from that image's repository, however a
// reference writes it. ADR 0021 says what taking the first of several comes to.
func TestDigestComesFromTheImagesOwnRepository(t *testing.T) {
	const (
		a = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		b = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, c := range []struct {
		name, ref string
		digests   []string
		want      string
	}{
		{"a tag, with one digest", "ghcr.io/acme/shop:1", []string{"ghcr.io/acme/shop@" + a}, a},
		{"another repository's digest isn't this one's", "ghcr.io/acme/shop:1", []string{"ghcr.io/other/shop@" + b, "ghcr.io/acme/shop@" + a}, a},
		{"several of one repository: the first", "ghcr.io/acme/shop:1", []string{"ghcr.io/acme/shop@" + a, "ghcr.io/acme/shop@" + b}, a},
		{"Docker Hub's name, however it's written", "docker.io/library/nginx:1.27", []string{"nginx@" + a}, a},
		{"a reference's own digest, over any list", "ghcr.io/acme/shop:1@" + b, []string{"ghcr.io/acme/shop@" + a}, b},
		{"an image built here has none", "acme/built-here:1", nil, ""},
		{"nor does one only another repository has", "acme/built-here:1", []string{"ghcr.io/acme/shop@" + a}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (&docker.Image{RepoDigests: c.digests}).Digest(c.ref); got != c.want {
				t.Errorf("Digest(%s) of %v = %q, want %q", c.ref, c.digests, got, c.want)
			}
		})
	}
}
