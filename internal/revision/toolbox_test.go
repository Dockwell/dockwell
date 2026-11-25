// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestToolboxPlanSurvivesStoredPlanRoundTrip(t *testing.T) {
	p := &v1.HostToolboxPlan{Operation: &v1.HostToolboxOperation{Operation: &v1.HostToolboxOperation_Ntp{Ntp: false}}}
	host := resource.Ref{Kind: "server.host", Name: "host", Scope: "srv_server"}
	toolbox := (*resource.ToolboxPlan)(p)
	raw, err := json.Marshal(Plan{
		Resources: []ResourcePlan{{Ref: host, Toolbox: toolbox}},
		Servers:   []ServerPlan{{Server: host.Scope, Steps: []resource.Step{{Ref: host, Decision: resource.Decision{Toolbox: toolbox}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got.Resources[0].Toolbox.Proto(), p) || !proto.Equal(got.Servers[0].Steps[0].Toolbox.Proto(), p) {
		t.Fatal("toolbox operation changed after reading the stored plan")
	}
}
