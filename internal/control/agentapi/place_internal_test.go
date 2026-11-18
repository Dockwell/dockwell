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

// What a sample says each container uses is folded into one figure for each
// service its server's health places, with how many containers it has, never by
// what the agent names; what isn't a container of Docker's, or isn't a number,
// goes, and a memory limit counts only where every container has one (#797;
// #848's review, M4 and L4).
func TestASamplesContainersAreFoldedByTheServiceTheirHealthPlaces(t *testing.T) {
	web1, web2, web3, db, other := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64)
	agents := NewAgents(nil, slog.New(slog.DiscardHandler))
	agents.ReportHealth("srv_1", &agentv1.Health{ScannedAt: timestamppb.Now(), ReportedAt: timestamppb.Now(), Instances: []*dockwellv1.InstanceHealth{
		{Project: "env_01abc", Service: "web", Number: 1, Container: "env_01abc-web-1", ContainerId: web1},
		{Project: "env_01abc", Service: "web", Number: 2, Container: "env_01abc-web-2", ContainerId: web2},
		{Project: "env_01abc", Service: "web", Number: 3, Container: "env_01abc-web-3", ContainerId: web3},
		{Project: "env_01abc", Service: "db", Number: 1, Container: "env_01abc-db-1", ContainerId: db},
	}})
	asked := map[string]int{}
	h := &Handler{agents: agents, services: func(_ context.Context, project, name string) string {
		asked[project+"/"+name]++
		return "svc_" + name
	}}
	got := h.placeContainers(t.Context(), "srv_1", []*dockwellv1.ContainerUsage{
		{ContainerId: web1, ServiceId: "svc_someone_elses", CpuPercent: 50, MemoryBytes: 100 << 20, MemoryLimitBytes: 512 << 20},
		{ContainerId: web2, CpuPercent: 50, MemoryBytes: 100 << 20, MemoryLimitBytes: 512 << 20},
		{ContainerId: web3, CpuPercent: math.NaN(), MemoryBytes: 100 << 20, MemoryLimitBytes: 512 << 20},
		{ContainerId: db, CpuPercent: 10, MemoryBytes: 1 << 20, MemoryLimitBytes: 64 << 20},
		{ContainerId: other, CpuPercent: 10},
		{ContainerId: "../../etc", CpuPercent: 10},
	})
	if len(got) != 2 {
		t.Fatalf("folded %v, want web's three and db's one, a service each: one its health doesn't have, or that isn't an ID, goes", got)
	}
	web := got[0]
	if web.GetServiceId() != "svc_web" || web.GetContainerId() != "" || web.GetReplicas() != 3 || web.GetCpuPercent() != 100 ||
		web.GetMemoryBytes() != 300<<20 || web.GetMemoryLimitBytes() != 3*512<<20 {
		t.Errorf("web = %v, want its three summed, placed by the control plane, NaN as 0, no container ID", web)
	}
	if got[1].GetServiceId() != "svc_db" || got[1].GetReplicas() != 1 {
		t.Errorf("db = %v", got[1])
	}
	if asked["env_01abc/web"] != 3 {
		t.Errorf("asked for services %v", asked)
	}

	// One container with no limit makes the whole unlimited.
	again := h.placeContainers(t.Context(), "srv_1", []*dockwellv1.ContainerUsage{
		{ContainerId: web1, MemoryBytes: 1, MemoryLimitBytes: 512 << 20},
		{ContainerId: web2, MemoryBytes: 1},
	})
	if again[0].GetMemoryLimitBytes() != 0 {
		t.Errorf("a service with a container under no limit reads a limit of %d", again[0].GetMemoryLimitBytes())
	}
	if none := (&Handler{agents: agents, services: h.services}).placeContainers(t.Context(), "srv_2", got); len(none) != 0 {
		t.Errorf("a server with no health keeps %v, want none", none)
	}
}
