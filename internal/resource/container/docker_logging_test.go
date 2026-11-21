// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"testing"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

func TestChangingOnlyLoggingKeepsTheImageEvenAfterItsTagMoves(t *testing.T) {
	engine := dockertest.New(t)
	const refName = "dockwell.invalid/example:current"
	engine.AddLocalImage(refName, dockertest.Image{})
	local := resource.Local{Drivers: drivers(engine)}
	ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "rotation"}
	want := &spec.Container{Image: refName}
	apply(t, local, ref, want)
	before := engine.Container(ref.Name)
	engine.AddLocalImage(refName, dockertest.Image{})
	want.Logging = &spec.ContainerLogging{
		Mode:   spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_LIMITS,
		Limits: &spec.DockerDaemonLogging{Driver: "json-file", MaxSizeBytes: 10 << 20, MaxFiles: 5},
	}
	apply(t, local, ref, want)
	after := engine.Container(ref.Name)
	if after == nil || after.ID == before.ID || after.Image != before.Image || after.HostConfig.LogConfig.Config["max-file"] != "5" {
		t.Fatalf("the replacement must change only logging: before=%+v, after=%+v", before, after)
	}
	unchanged(t, local, ref, want)
}
