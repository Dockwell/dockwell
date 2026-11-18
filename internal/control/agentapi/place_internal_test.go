// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// What a sample says each container uses is kept for the containers its server's
// health has, each placed by the control plane, never by what the agent names;
// what isn't a container of Docker's, or isn't a number, goes (#797).
func TestASamplesContainersArePlacedByTheirServerHealth(t *testing.T) {
	web, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	agents := NewAgents(nil, slog.New(slog.DiscardHandler))
	agents.ReportHealth("srv_1", &agentv1.Health{ScannedAt: timestamppb.Now(), ReportedAt: timestamppb.Now(), Instances: []*dockwellv1.InstanceHealth{
		{Project: "env_01abc", Service: "web", Number: 1, Container: "env_01abc-web-1", ContainerId: web},
	}})
	asked := []string{}
	h := &Handler{agents: agents, services: func(_ context.Context, project, name string) string {
		asked = append(asked, project+"/"+name)
		return "svc_web"
	}}
	got := h.placeContainers(t.Context(), "srv_1", []*dockwellv1.ContainerUsage{
		{ContainerId: web, ServiceId: "svc_someone_elses", CpuPercent: math.NaN(), MemoryBytes: 1 << 20},
		{ContainerId: other, ServiceId: "svc_web", CpuPercent: 10},
		{ContainerId: "../../etc", CpuPercent: 10},
	})
	if len(got) != 1 || got[0].GetContainerId() != web {
		t.Fatalf("kept %v, want web's container alone: one its health doesn't have, or that isn't an ID, goes", got)
	}
	if got[0].GetServiceId() != "svc_web" || got[0].GetCpuPercent() != 0 || got[0].GetMemoryBytes() != 1<<20 {
		t.Errorf("web's figures = %v, want placed by the control plane, CPU that's no number as 0", got[0])
	}
	if len(asked) != 1 || asked[0] != "env_01abc/web" {
		t.Errorf("asked for services %v, want web's project and name", asked)
	}
	if none := (&Handler{agents: agents}).placeContainers(t.Context(), "srv_2", got); len(none) != 0 {
		t.Errorf("a server with no health keeps %v, want none", none)
	}
}
