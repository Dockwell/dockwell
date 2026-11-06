// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"fmt"
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
// the image is what's wrong, and a form has no command. But an image for another
// architecture can really run, as 386 does on amd64, and anything does under emulation,
// so an app that fails for its own reasons isn't told to choose another build.
func TestAnImageForAnotherArchitectureSaysSoOnlyWhereItCouldntRun(t *testing.T) {
	const (
		arch        = "The image for web isn't built for this server's architecture. Choose a build for this server, or another image, then try again."
		stopped     = "web stopped right after it started, with exit code 1. Check its command and variables, then try again."
		restarting  = "web keeps restarting: it exited with code %d. Check its command and variables, then try again."
		refused     = "exec /docker-entrypoint.sh: exec format error"
		appsOwnWord = "DATABASE_URL isn't set"
	)
	for _, c := range []struct {
		name   string
		arch   string
		exit   int // keeps restarting with this, or stops with 1 where it's 0
		prints []string
		want   string
		why    string
	}{
		{"a real refusal: another architecture, restarting, exit 255", "arm64", 255, nil, arch, resource.ReasonImage},
		{"exit 255 from an image for this server is the app's", "amd64", 255, nil, fmt.Sprintf(restarting, 255), resource.ReasonExits},
		{"exit 255 from an image that doesn't say is the app's", "", 255, nil, fmt.Sprintf(restarting, 255), resource.ReasonExits},
		{"an app that runs under emulation and exits 1 is the app's", "386", 1, []string{appsOwnWord}, fmt.Sprintf(restarting, 1), resource.ReasonExits},
		{"so it is with no restart policy", "386", 0, []string{appsOwnWord}, stopped, resource.ReasonExits},
		{"the runtime's words say so whatever it exits with", "arm64", 0, []string{refused}, arch, resource.ReasonImage},
		{"and whatever the image says it's for", "amd64", 1, []string{refused}, arch, resource.ReasonImage},
		{"an image for this server that exits 1 is the app's", "amd64", 0, nil, stopped, resource.ReasonExits},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := dockertest.New(t)
			const image = "ghcr.io/acme/shop:1"
			e.Registry[image] = dockertest.Image{Exits: c.exit == 0, Prints: c.prints, Architecture: c.arch, Config: docker.Config{Cmd: []string{"shop"}}}
			d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second}}
			ref := resource.Ref{Kind: container.Kind, Name: "web"}
			spec := &resourcev1.Container{Image: image}
			if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
				t.Fatal(err)
			}
			if c.exit != 0 {
				e.SetRestarting("web", c.exit)
			}
			err := d.Verify(t.Context(), ref, spec)
			if plain, _ := resource.Explain(err); plain != c.want || resource.ReasonOf(err) != c.why {
				t.Errorf("it says %q, for the reason %q; want %q, for %q", plain, resource.ReasonOf(err), c.want, c.why)
			}
		})
	}
}
