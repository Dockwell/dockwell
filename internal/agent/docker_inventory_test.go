// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestDockerInventoryProjectsOnlyAllowedFields(t *testing.T) {
	e := dockertest.New(t)
	const secret = "value-that-must-stay-local"
	id := strings.Repeat("a", 64)
	started := time.Now().Add(-time.Minute).UTC()
	e.AddContainer(docker.Container{
		ID: id, Name: "/sample-api", Created: started.Add(-time.Second), RestartCount: 3,
		Config: docker.Config{
			Image: "fixture/api:1", Env: []string{"PASSWORD=" + secret}, Cmd: []string{secret}, Entrypoint: []string{secret},
			Labels: map[string]string{
				"generic": secret, docker.LabelProject: "sample", docker.LabelService: "api",
				docker.LabelWorkingDir: "/srv/compose-fixture",
			},
		},
		State: docker.State{
			Running: true, Status: "running", StartedAt: started,
			Health: &docker.Health{Status: "healthy", Log: []docker.HealthRun{{Output: secret}}},
		},
		HostConfig: docker.HostConfig{
			RestartPolicy: docker.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3},
			Memory:        10 << 20, LogConfig: &docker.LogConfig{Type: "json-file", Config: map[string]string{
				"max-size": "10m", "max-file": "5", "token": secret,
			}},
		},
	})
	list, err := dockerInventoryList(t.Context(), e.Client(), &agentv1.DockerInventoryListCall{})
	if err != nil || len(list.GetObjects()) != 1 || list.GetObjects()[0].GetContainer().GetRestartCount() != 3 {
		t.Fatalf("list = %v, %v", list, err)
	}
	detail, err := dockerInventoryDetail(t.Context(), e.Client(), &agentv1.DockerInventoryDetailCall{ContainerId: id})
	if err != nil {
		t.Fatal(err)
	}
	members, err := dockerInventoryList(t.Context(), e.Client(), &agentv1.DockerInventoryListCall{ComposeMembers: true})
	if err != nil || len(members.GetObjects()) != 1 {
		t.Fatalf("Compose members = %v, %v", members, err)
	}
	member := members.GetObjects()[0].GetContainer()
	if member.GetComposeDirectory() != "/srv/compose-fixture" || member.GetStartedAt() != nil || member.GetImage() != "" ||
		member.GetCreatedAt() == nil || member.GetRestartCount() != 0 {
		t.Error("Compose membership did not stay lightweight")
	}
	for _, answer := range []proto.Message{list, detail, members} {
		encoded, err := proto.Marshal(answer)
		if err != nil || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "generic") {
			t.Fatalf("inventory exposed unprojected fields or did not encode: %v", err)
		}
	}
	c := detail.GetObject().GetContainer()
	if c.GetComposeProject() != "sample" || c.GetHealth() != v1.DockerHealth_DOCKER_HEALTH_HEALTHY ||
		!c.GetStartedAt().AsTime().Equal(started) || c.GetLogMaxSize() != "10m" || c.GetLogMaxFiles() != 5 ||
		c.GetMemoryLimitBytes() != 10<<20 || c.GetRestartRetries() != 3 {
		t.Fatalf("safe details: %v", c)
	}
}

func TestDockerInventoryCursorRejectsChangedMembershipAndFindsReplacement(t *testing.T) {
	e := dockertest.New(t)
	for i := range 3 {
		e.AddContainer(docker.Container{ID: fmt.Sprintf("%064x", i+1), Name: fmt.Sprintf("/sample-%d", i)})
	}
	first, err := dockerInventoryList(t.Context(), e.Client(), &agentv1.DockerInventoryListCall{PageSize: 1})
	if err != nil || len(first.GetObjects()) != 1 || first.GetNextId() == "" {
		t.Fatalf("first page: %v, %v", first, err)
	}
	e.AddContainer(docker.Container{ID: fmt.Sprintf("%064x", 42), Name: "/sample-replacement"})
	more, err := dockerInventoryList(t.Context(), e.Client(), &agentv1.DockerInventoryListCall{
		PageSize: 1, AfterId: first.GetNextId(), Snapshot: first.GetSnapshot(),
	})
	if err != nil || !more.GetResync() || len(more.GetObjects()) != 0 {
		t.Fatalf("stale cursor: %v, %v", more, err)
	}
	gone, err := dockerInventoryDetail(t.Context(), e.Client(), &agentv1.DockerInventoryDetailCall{
		ContainerId: fmt.Sprintf("%064x", 41), LastKnownName: "sample-replacement",
	})
	if err != nil || gone.GetObject() != nil || gone.GetReplacementKey().GetEngineId() != fmt.Sprintf("%064x", 42) {
		t.Fatalf("a missing ID should only offer its replacement: %v, %v", gone, err)
	}
}

func TestDockerComposeMembershipCursorSurvivesHealthChanges(t *testing.T) {
	e := dockertest.New(t)
	for i := range 257 {
		e.AddContainer(docker.Container{
			ID: fmt.Sprintf("%064x", i+1), Name: fmt.Sprintf("/fixture-%d", i),
			Config: docker.Config{Labels: map[string]string{docker.LabelProject: "fixture", docker.LabelService: "web"}},
		})
	}
	first, err := dockerInventoryList(t.Context(), e.Client(), &agentv1.DockerInventoryListCall{ComposeMembers: true})
	if err != nil || len(first.GetObjects()) != 256 || first.GetNextId() == "" {
		t.Fatalf("first Compose page: %v", err)
	}
	e.SetHealth("fixture-256", "unhealthy", "fixture diagnostic")
	after, err := dockerInventoryList(t.Context(), e.Client(), &agentv1.DockerInventoryListCall{
		ComposeMembers: true, AfterId: first.GetNextId(), Snapshot: first.GetSnapshot(),
	})
	if err != nil || after.GetResync() || len(after.GetObjects()) != 1 ||
		after.GetObjects()[0].GetContainer().GetHealth() != v1.DockerHealth_DOCKER_HEALTH_UNHEALTHY {
		t.Fatalf("health changed during the membership walk: %v, %v", after, err)
	}
}
