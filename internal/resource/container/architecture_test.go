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

// Real Docker reports an image built for another architecture as a container that
// exited, as the kernel refuses its binary inside it, and a service, which has a restart
// policy, as one that keeps restarting, with exit code 255: Docker 28 on CI said that
// of a real arm64 image on amd64. Either read as something to fix in its command, where
// the image is what's wrong, and a form has no command.
func TestAnImageForAnotherArchitectureSaysSoThoughItOnlyExited(t *testing.T) {
	const (
		arch     = "The image for web isn't built for this server's architecture. Choose a build for this server, or another image, then try again."
		stopped  = "web stopped right after it started, with exit code 1. Check its command and variables, then try again."
		restarts = "web keeps restarting: it exited with code 255. Check its command and variables, then try again."
	)
	for _, c := range []struct {
		name       string
		arch       string
		restarting bool
		want       string
		why        string
	}{
		{"keeps restarting, as a service does on a real engine", "arm64", true, arch, resource.ReasonImage},
		{"keeps restarting, built for this server", "amd64", true, restarts, resource.ReasonExits},
		{"keeps restarting, and doesn't say", "", true, restarts, resource.ReasonExits},
		{"stopped, with no restart policy", "arm64", false, arch, resource.ReasonImage},
		{"stopped, built for this server", "amd64", false, stopped, resource.ReasonExits},
		{"stopped, and doesn't say", "", false, stopped, resource.ReasonExits},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := dockertest.New(t)
			const image = "ghcr.io/acme/shop:1"
			e.Registry[image] = dockertest.Image{Exits: !c.restarting, Architecture: c.arch, Config: docker.Config{Cmd: []string{"shop"}}}
			d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second}}
			ref := resource.Ref{Kind: container.Kind, Name: "web"}
			spec := &resourcev1.Container{Image: image}
			if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
				t.Fatal(err)
			}
			if c.restarting {
				e.SetRestarting("web", 255)
			}
			err := d.Verify(t.Context(), ref, spec)
			if plain, _ := resource.Explain(err); plain != c.want || resource.ReasonOf(err) != c.why {
				t.Errorf("it says %q, for the reason %q; want %q, for %q", plain, resource.ReasonOf(err), c.want, c.why)
			}
		})
	}
}
