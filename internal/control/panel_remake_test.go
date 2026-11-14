// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/panels"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// What Dokploy runs as a Swarm service is made again in Dockwell (ADR 0089): read
// from its running task, into a project named for Dokploy's, under its appName, on
// the same volumes, with what only Swarm, Traefik and Dokploy read left behind. The
// plan scales the Swarm service to none first, and its undo deletes Dockwell's copy
// before it scales the service back.
func TestMakingADokploySwarmAppAgain(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	root := t.TempDir()
	dynamic := filepath.Join(root, "etc", "dokploy", "traefik", "dynamic")
	if err := os.MkdirAll(dynamic, 0o755); err != nil {
		t.Fatal(err)
	}
	file := "http:\n  routers:\n    shop-k2m4-router-1:\n      rule: Host(`shop.example.com`)\n      service: shop-k2m4-service-1\n" +
		"  services:\n    shop-k2m4-service-1:\n      loadBalancer:\n        servers:\n          - url: http://shop-k2m4:80\n"
	if err := os.WriteFile(filepath.Join(dynamic, "shop-k2m4.yml"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	f.panelsRoot = root
	later := &laterStopper{}
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	e := f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&host.Driver{Panels: later}, &route.Driver{Ingress: ing}}
	})
	later.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	id := f.lastDocker.id.ServerID
	labels := map[string]string{"dokploy.project": "store", "dokploy.environment": "production", "dokploy.application": "shop"}
	e.Services = []docker.Service{
		{ID: "s1", Spec: swarmService("dokploy", "dokploy/dokploy:v0.31.2", nil)},
		{ID: "s2", Spec: swarmService("shop-k2m4", "nginx:1.26", labels)},
	}
	runByHand(t, e, "dokploy-traefik", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	task := runByHand(t, e, "shop-k2m4.1.x7k2", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings = nil
		r.Labels = map[string]string{
			"com.docker.swarm.service.name": "shop-k2m4", "com.docker.swarm.task.id": "x7k2",
			"traefik.enable": "true",
		}
		maps.Copy(r.Labels, labels)
	})
	ic, c := importClient(b), changeClient(b, b.origin)
	replicas := func() uint64 {
		svc, err := e.Client().InspectService(t.Context(), "shop-k2m4")
		if err != nil {
			t.Fatal(err)
		}
		n, _ := svc.Spec.Mode.Replicas()
		return n
	}

	res, err := ic.ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	step := panelSteps(res.Msg)["IMPORT_KIND_APP shop"]
	if step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE || step.GetBecomes() != "the service shop-k2m4 in the project store, made again as it runs, on its volumes" {
		t.Fatalf("shop reads %v", step)
	}
	planned := res.Msg.GetChange()
	if !noteSays(planned, "remakes_dokploy", "Dokploy scales each to none first") {
		t.Errorf("the plan's notes are %v", planned.GetPlan().GetNotes())
	}
	var kinds []string
	for _, st := range planned.GetPlan().GetServers()[0].GetSteps() {
		kinds = append(kinds, st.GetResource().GetKind())
		if p := st.GetPanel(); p != nil && (!p.GetApps() || p.GetParts()[0].GetName() != "shop-k2m4" || p.GetParts()[0].GetWasReplicas() != 1) {
			t.Errorf("the scale reads %v", p)
		}
	}
	if len(kinds) == 0 || kinds[0] != "server.host" {
		t.Errorf("the plan's steps are %v, want the scale first", kinds)
	}
	if done := actedOn(t, c, planned, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the import ended %v: %s", done.GetState(), done.GetError())
	}
	if replicas() != 0 {
		t.Error("Dokploy's service still runs")
	}
	var copied *docker.Container
	for _, name := range e.Containers() {
		if cc := e.Container(name); cc != nil && cc.Config.Labels[docker.LabelService] == "shop-k2m4" && cc.ID != task {
			copied = cc
		}
	}
	if copied == nil || !copied.State.Running {
		t.Fatalf("Dockwell's copy is %v", copied)
	}
	for k := range copied.Config.Labels {
		if strings.HasPrefix(k, "com.docker.swarm.") || strings.HasPrefix(k, "dokploy.") || strings.HasPrefix(k, "traefik.") {
			t.Errorf("Dockwell's copy keeps the label %s", k)
		}
	}

	// Its domain comes over as a route to Dockwell's copy, by the port Dokploy's
	// Traefik file names.
	handover, err := ic.PlanPanelDomains(t.Context(), connect.NewRequest(&dockwellv1.PlanPanelDomainsRequest{ServerId: id, Panel: dockwellv1.PanelKind_PANEL_KIND_DOKPLOY}))
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, handover.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the handover ended %v: %s", done.GetState(), done.GetError())
	}
	var routed *resourcev1.Route
	for _, r := range ing.routes {
		routed = r
	}
	if routed.GetDomain() != "shop.example.com" || routed.GetService() != "shop-k2m4" || routed.GetPort() != 80 {
		t.Errorf("the route is %v", routed)
	}
	back, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: handover.Msg.GetChange().GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, back.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the handover's undo ended %v: %s", done.GetState(), done.GetError())
	}

	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: planned.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if done := actedOn(t, c, undo.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", done.GetState(), done.GetError())
	}
	if replicas() != 1 {
		t.Error("the undo left Dokploy's service at none")
	}
	if e.Container(copied.Name) != nil {
		t.Error("the undo left Dockwell's copy")
	}
}
