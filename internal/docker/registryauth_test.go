// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"net/url"
	"testing"
)

// Docker sends a login to the registry of the image it pulls, whatever the login
// says it's for, so a login goes only with a pull of an image on its registry, or
// asking about one, and with nothing else (ADR 0086).
func TestALoginGoesOnlyWithAPullFromItsRegistry(t *testing.T) {
	ctx := WithRegistryAuth(context.Background(), "ghcr.io", "header")
	for _, c := range []struct {
		path  string
		query url.Values
		want  string
	}{
		{"/v1.43/images/create", url.Values{"fromImage": {"ghcr.io/acme/web:1.0"}}, "header"},
		{"/v1.43/distribution/ghcr.io/acme/web:1.0/json", nil, "header"},
		{"/v1.43/distribution/ghcr.io%2Facme%2Fweb:1.0/json", nil, "header"},
		// Another registry's image, as a helper's, gets none.
		{"/v1.43/images/create", url.Values{"fromImage": {"busybox:1.37"}}, ""},
		{"/v1.43/images/create", url.Values{"fromImage": {"ghcr.io.example.com/acme/web:1.0"}}, ""},
		{"/v1.43/distribution/registry.example.com/acme/web:1.0/json", nil, ""},
		// Nor does anything else Docker is asked.
		{"/v1.43/containers/create", nil, ""},
		{"/v1.43/images/ghcr.io/acme/web:1.0/json", nil, ""},
		{"/v1.43/build", url.Values{"t": {"ghcr.io/acme/web:1.0"}}, ""},
	} {
		if got := registryAuthFor(ctx, c.path, c.query); got != c.want {
			t.Errorf("%s %v carries %q, want %q", c.path, c.query, got, c.want)
		}
	}
	if got := registryAuthFor(context.Background(), "/v1.43/images/create", url.Values{"fromImage": {"ghcr.io/acme/web:1.0"}}); got != "" {
		t.Errorf("a pull with no login carries %q", got)
	}
	hub := WithRegistryAuth(context.Background(), "docker.io", "hub")
	if got := registryAuthFor(hub, "/v1.43/images/create", url.Values{"fromImage": {"acme/private:1.0"}}); got != "hub" {
		t.Errorf("a Docker Hub image pulled with Docker Hub's login carries %q", got)
	}
}
