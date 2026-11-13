// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/cli/apicli"
)

// proposals is a ChangeService that keeps each proposal and plans an update of it.
type proposals struct {
	dockwellv1connect.UnimplementedChangeServiceHandler
	mu   sync.Mutex
	asks []*dockwellv1.ProposeChangeRequest
}

func (p *proposals) ProposeChange(_ context.Context, req *connect.Request[dockwellv1.ProposeChangeRequest]) (*connect.Response[dockwellv1.ProposeChangeResponse], error) {
	p.mu.Lock()
	p.asks = append(p.asks, req.Msg)
	p.mu.Unlock()
	return connect.NewResponse(&dockwellv1.ProposeChangeResponse{Change: &dockwellv1.Change{
		Id: stubChange, State: dockwellv1.ChangeState_CHANGE_STATE_PLANNED,
		Plan: &dockwellv1.Plan{
			Summary:   &dockwellv1.PlanSummary{Change: 1},
			Resources: []*dockwellv1.PlannedResource{{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_1", Name: "web"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE}},
		},
	}}), nil
}

// oneService is a ServiceService with one service, web, as a read has it: its own
// values blanked but for what it shows.
type oneService struct {
	dockwellv1connect.UnimplementedServiceServiceHandler
}

func (oneService) ListServices(context.Context, *connect.Request[dockwellv1.ListServicesRequest]) (*connect.Response[dockwellv1.ListServicesResponse], error) {
	return connect.NewResponse(&dockwellv1.ListServicesResponse{Services: []*dockwellv1.ServiceInfo{{
		Id: "svc_1", Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: "env_1", Name: "web"}, Revision: 4,
		Spec: &resourcev1.Service{
			Runs:     &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"DB_HOST": "db", "DB_PASSWORD": "", "OLD": ""}}},
			ShownEnv: []string{"DB_HOST"},
		},
	}}}), nil
}

// An update of a service keeps the values of its own it doesn't set: one it reads
// back hidden by name, and one it shows by value, still shown, from the revision it
// read. --remove-env takes one out. Before, an update emptied them all (ADR 0081).
func TestServicesUpdateKeepsWhatItDoesntSet(t *testing.T) {
	stub := &proposals{}
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewChangeServiceHandler(stub))
	mux.Handle(dockwellv1connect.NewServiceServiceHandler(oneService{}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	env := map[string]string{apicli.EnvURL: srv.URL, apicli.EnvToken: wellFormedToken(), "XDG_CONFIG_HOME": t.TempDir()}

	r := run(t, env, "services", "update", "web", "--app-image", "nginx:1.28", "--app-env", "MODE=fast", "--remove-env", "OLD", "--plan-only")
	if r.code != exitOK {
		t.Fatalf("services update = %d:\n%s%s", r.code, r.out, r.err)
	}
	if len(stub.asks) != 1 {
		t.Fatalf("proposed %d times", len(stub.asks))
	}
	edit := stub.asks[0].GetEdits()[0]
	spec := edit.GetSpec().GetService()
	if got := spec.GetApp().GetEnv(); len(got) != 2 || got["MODE"] != "fast" || got["DB_HOST"] != "db" {
		t.Errorf("the update sets env %v", got)
	}
	if !slices.Equal(spec.GetShownEnv(), []string{"DB_HOST"}) {
		t.Errorf("the update shows %v", spec.GetShownEnv())
	}
	if !slices.Equal(edit.GetKeepEnv(), []string{"DB_PASSWORD"}) || edit.GetBaseRevision() != 4 {
		t.Errorf("the update keeps %v from revision %d", edit.GetKeepEnv(), edit.GetBaseRevision())
	}

	r = run(t, env, "services", "update", "web", "--app-image", "nginx:1.28", "--remove-env", "NONE", "--plan-only")
	if r.code == exitOK || len(stub.asks) != 1 {
		t.Errorf("removing a value it hasn't got = %d, after %d proposals:\n%s", r.code, len(stub.asks), r.err)
	}
}
