// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerLoggingReviewPinsTheCurrentContainer(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		changes := changeClient(b, b.origin)
		dockerAPI := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		edit := serviceEdit("logging", "ghcr.io/acme/shop:1.4", nil)
		edit.GetSpec().GetService().GetApp().DesiredRunState = spec.DesiredRunState_DESIRED_RUN_STATE_STOPPED
		made := apply(t, changes, propose(t, changes, "Create stopped workload", edit))
		workload := made.GetPlan().GetResources()[0]
		name := docker.ContainerName(docker.Project(workload.GetResource().GetScope()), "logging")
		before := engine.Container(name)
		observe := func(c *docker.Container) {
			f.agents.ReportHealth(f.lastDocker.id.ServerID, &agentv1.Health{
				ScannedAt: timestamppb.Now(),
				Instances: []*v1.InstanceHealth{{
					ContainerId: c.ID, Container: name,
					Project: c.Config.Labels[docker.LabelProject], Service: c.Config.Labels[docker.LabelService],
				}},
			})
		}
		observe(before)
		ask := &v1.PlanDockerContainerLoggingRequest{
			Container:    &v1.DockerObjectKey{ServerId: f.lastDocker.id.ServerID, EngineId: before.ID, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER},
			BaseRevision: 1,
			Logging: &spec.ContainerLogging{
				Mode:   spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_LIMITS,
				Limits: &spec.DockerDaemonLogging{Driver: "local", MaxSizeBytes: 10 << 20, MaxFiles: 5},
			},
		}
		planned, err := dockerAPI.PlanDockerContainerLogging(t.Context(), connect.NewRequest(ask))
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.Client().RemoveContainer(t.Context(), before.ID, false); err != nil {
			t.Fatal(err)
		}
		replacement := *before
		replacement.ID = strings.Repeat("a", 64)
		engine.AddContainer(replacement)
		observe(&replacement)
		ch := planned.Msg.GetChange()
		if _, err := applyChange(t, changes, &v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err == nil || !strings.Contains(err.Error(), "reviewed container is gone") {
			t.Fatalf("rotation accepted a replacement identity: %v", err)
		}
		ask.Container.EngineId = replacement.ID
		planned, err = dockerAPI.PlanDockerContainerLogging(t.Context(), connect.NewRequest(ask))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, changes, planned.Msg.GetChange())
		after := engine.Container(name)
		if after == nil || after.ID == replacement.ID || after.State.Running || after.HostConfig.LogConfig == nil ||
			after.HostConfig.LogConfig.Type != "local" || after.HostConfig.LogConfig.Config["max-file"] != "5" {
			t.Fatal("the reviewed limit did not take effect while the workload stayed stopped")
		}
	})
}
