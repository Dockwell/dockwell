// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Docker's daemon adds label=disable to a container that's privileged or shares
// the server's processes, SELinux or not. Such a service reads back as its spec all
// the same, with no drift that an apply can't clear (ADR 0097, the review's H1).
func TestPrivilegedAndHostPidServicesShowNoDriftOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	dockertest.Cleanup(t, client, docker.Project(scope))
	const image = "redis:7-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: &container.Docker{Client: client}}})}
	for name, app := range map[string]*resourcev1.Container{
		"privileged": {Image: image, Privileged: true},
		"pid":        {Image: image, HostPid: true},
	} {
		t.Run(name, func(t *testing.T) {
			ref := resource.Ref{Kind: service.Kind, Scope: scope, Name: name}
			// As the revision engine checks every edit's spec, which puts it in the form
			// it's stored and sent in, Docker's defaults among it.
			spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
			if err := local.Drivers.Validate(service.Kind, spec); err != nil {
				t.Fatal(err)
			}
			steps, err := local.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
			if err != nil {
				t.Fatal(err)
			}
			if err := local.Apply(t.Context(), steps[0], spec); err != nil {
				t.Fatal(err)
			}
			got, err := local.Observe(t.Context(), []resource.Ref{ref})
			if err != nil {
				t.Fatal(err)
			}
			if changes := resource.Diff(spec, got[0]); len(changes) != 0 {
				t.Errorf("it reads back with drift: %+v", changes)
			}
		})
	}
}
