// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerInventoryAcrossAgentsAndStores(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		agent := f.lastDocker
		server := agent.id.ServerID
		client := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		external := strings.Repeat("d", 64)
		engine.AddContainer(docker.Container{ID: external, Name: "/fixture-external", Config: docker.Config{Image: "fixture/image:1"}})

		changes := changeClient(b, b.origin)
		apply(t, changes, propose(t, changes, "Run fixture", runImage("ghcr.io/acme/shop:1.4")))
		listed, err := f.lastDocker.client.ListContainers(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range listed {
			project, svc := row.Labels[docker.LabelProject], row.Labels[docker.LabelService]
			if project == "" || svc == "" {
				continue
			}
			f.agents.ReportHealth(server, &agentv1.Health{
				ScannedAt: timestamppb.Now(), Instances: []*v1.InstanceHealth{{
					Project: project, Service: svc, ContainerId: row.ID, Container: strings.TrimPrefix(row.Names[0], "/"),
				}},
			})
			// A workload with the same labels on another server has no
			// matching placement there, even if its agent also claims it.
			other := f.addServerNamed(t, "fixture-other", nil, nil)
			otherID := f.lastDocker.id.ServerID
			other.AddContainer(docker.Container{ID: external, Name: "/fixture-external", Config: docker.Config{
				Image: "fixture/image:1", Labels: row.Labels,
			}})
			f.agents.ReportHealth(otherID, &agentv1.Health{ScannedAt: timestamppb.Now(), Instances: []*v1.InstanceHealth{{
				Project: project, Service: svc, ContainerId: external, Container: "fixture-external",
			}}})
			spoofed, err := client.GetDockerObject(t.Context(), connect.NewRequest(&v1.GetDockerObjectRequest{
				Key: &v1.DockerObjectKey{ServerId: otherID, EngineId: external},
			}))
			if err != nil || spoofed.Msg.GetObject().GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL ||
				spoofed.Msg.GetObject().GetManagedResourceId() != "" {
				t.Fatalf("a claim on another server was trusted: %v, %v", spoofed, err)
			}
			break
		}
		engine.SetHealth(external, "unhealthy", "fixture health details")
		res, err := client.GetDockerOverview(t.Context(), connect.NewRequest(&v1.GetDockerOverviewRequest{ServerIds: []string{server}}))
		if err != nil {
			t.Fatal(err)
		}
		if data := res.Msg.GetServerSummaries(); len(data) != 1 || data[0].GetContainers() < 2 || data[0].GetComposeProjects() == 0 || data[0].GetNeedsAttention() != 1 ||
			len(data[0].GetAttention()) != 1 || data[0].GetRootDirectory() == "" || data[0].GetLoggingDriver() == "" {
			t.Fatalf("Docker counts: %v", res.Msg)
		}

		attention, err := client.ListDockerObjects(t.Context(), connect.NewRequest(&v1.ListDockerObjectsRequest{
			ServerIds: []string{server}, Filter: &v1.DockerObjectFilter{NeedsAttention: true},
		}))
		if err != nil || len(attention.Msg.GetObjects()) != 1 || attention.Msg.GetObjects()[0].GetKey().GetEngineId() != external {
			t.Fatalf("attention filter: %v, %v", attention, err)
		}

		selection := &v1.ListDockerObjectsRequest{ServerIds: []string{server}, PageSize: 1}
		got := make(map[v1.DockerOwnership]bool)
		for range 8 {
			page, err := client.ListDockerObjects(t.Context(), connect.NewRequest(selection))
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Msg.GetObjects() {
				if row.GetKey().GetServerId() != server {
					t.Fatalf("wrong server in row: %v", row.GetKey())
				}
				got[row.GetOwnership()] = true
				if row.GetOwnership() == v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED && row.GetManagedResourceId() == "" {
					t.Fatalf("missing managed app link: %v", row)
				}
			}
			selection.PageToken = page.Msg.GetNextPageToken()
			if selection.GetPageToken() == "" {
				break
			}
		}
		if selection.GetPageToken() != "" || !got[v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED] || !got[v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL] {
			t.Fatalf("inventory ownership and cursor: %v, %q", got, selection.GetPageToken())
		}
		detail, err := client.GetDockerObject(t.Context(), connect.NewRequest(&v1.GetDockerObjectRequest{
			Key: &v1.DockerObjectKey{ServerId: server, EngineId: external},
		}))
		if err != nil || detail.Msg.GetObject().GetName() != "fixture-external" {
			t.Fatalf("reading exact container: %v, %v", detail, err)
		}
		agent.stop()
		waitConnected(t, f, server, false)
		offline, err := client.GetDockerOverview(t.Context(), connect.NewRequest(&v1.GetDockerOverviewRequest{ServerIds: []string{server}}))
		if err != nil || len(offline.Msg.GetUnavailable()) != 1 ||
			offline.Msg.GetUnavailable()[0].GetState() != v1.DockerReadState_DOCKER_READ_STATE_OFFLINE {
			t.Fatalf("offline inventory: %v, %v", offline, err)
		}
	})
}
