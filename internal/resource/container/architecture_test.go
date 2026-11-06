// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Real Docker often reports an image built for another architecture as a container
// that exited, as the kernel refuses its binary inside it. That read as something to
// fix in its command, where the image is what's wrong, and a form has no command.
func TestAnImageForAnotherArchitectureSaysSoThoughItOnlyExited(t *testing.T) {
	for _, c := range []struct {
		name string
		arch string
		want string
		why  string
	}{
		{"another architecture", "arm64", "The image for web isn't built for this server's architecture. Choose a build for this server, or another image, then try again.", resource.ReasonImage},
		{"this server's", "amd64", "web stopped right after it started, with exit code 1. Check its command and variables, then try again.", resource.ReasonExits},
		{"one that doesn't say", "", "web stopped right after it started, with exit code 1. Check its command and variables, then try again.", resource.ReasonExits},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := dockertest.New(t)
			const image = "ghcr.io/acme/shop:1"
			e.Registry[image] = dockertest.Image{Exits: true, Architecture: c.arch, Config: docker.Config{Cmd: []string{"shop"}}}
			d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second}}
			ref := resource.Ref{Kind: container.Kind, Name: "web"}
			spec := &resourcev1.Container{Image: image}
			if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
				t.Fatal(err)
			}
			err := d.Verify(t.Context(), ref, spec)
			if plain, _ := resource.Explain(err); plain != c.want || resource.ReasonOf(err) != c.why {
				t.Errorf("it says %q, for the reason %q; want %q, for %q", plain, resource.ReasonOf(err), c.want, c.why)
			}
		})
	}
}
