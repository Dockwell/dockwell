// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestToolboxPlanCannotChangeSecretBinding(t *testing.T) {
	ref := resource.Ref{Kind: "server.host", Scope: "srv_server", Name: "host"}
	op := &v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Service{Service: &v1.ToolboxService{
		Environment: []*v1.ToolboxBinding{{Name: "KEY", EnvironmentId: "env_1", Source: "worker", Secret: true, Revision: 1}},
	}}}
	ask := &v1.HostToolboxPlan{Operation: op, ChangeId: "chg_change"}
	want := resource.Desired{Ref: ref, Toolbox: ask}
	plan := proto.CloneOf(ask)
	plan.Evidence = strings.Repeat("a", 64)
	got := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionUpdate, Toolbox: (*resource.ToolboxPlan)(plan)}}
	if err := checkToolboxPlan(want, got); err != nil {
		t.Fatal(err)
	}
	plan.GetOperation().GetService().GetEnvironment()[0].EnvironmentId = "env_2"
	if err := checkToolboxPlan(want, got); err == nil {
		t.Fatal("an agent may not pick a different environment's secrets")
	}
	if err := checkToolboxPlan(resource.Desired{Ref: ref}, got); err == nil {
		t.Fatal("an agent may not turn an unrelated plan into a toolbox operation")
	}
}
