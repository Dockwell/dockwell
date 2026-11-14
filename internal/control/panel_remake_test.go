// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/panels"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// What Dokploy runs as a Swarm service is made again in Dockwell (ADR 0089): read
// from its running task, into a project named for Dokploy's, under its appName, on
// the same volumes, with what only Swarm, Traefik and Dokploy read left behind, and
// the port Swarm published. The plan scales the Swarm service to none first, and its
// undo deletes Dockwell's copy before it scales the service back.
func TestMakingADokploySwarmAppAgain(t *testing.T) {
	d := dokployShop(t)
	e, ic, c, task, replicas := d.e, d.ic, d.c, d.task, d.replicas
	id := d.id
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
	// Swarm's routing mesh holds a published port while the service publishes it,
	// so the scale takes it off for the copy, and the undo puts it back.
	published := func() []docker.ServicePort {
		svc, err := e.Client().InspectService(t.Context(), "shop-k2m4")
		if err != nil {
			t.Fatal(err)
		}
		return svc.Spec.EndpointSpec.Ports
	}
	if got := published(); len(got) != 0 {
		t.Errorf("Dokploy's service still publishes %v", got)
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
	if b := copied.HostConfig.PortBindings["80/tcp"]; len(b) != 1 || b[0].HostPort != "8085" {
		t.Errorf("Dockwell's copy publishes %v, not Dokploy's external port", copied.HostConfig.PortBindings)
	}
	for k := range copied.Config.Labels {
		if strings.HasPrefix(k, "com.docker.swarm.") || strings.HasPrefix(k, "dokploy.") || strings.HasPrefix(k, "traefik.") {
			t.Errorf("Dockwell's copy keeps the label %s", k)
		}
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
	if got := published(); len(got) != 1 || got[0].PublishedPort != 8085 || got[0].TargetPort != 80 || got[0].PublishMode != "ingress" {
		t.Errorf("after the undo Dokploy's service publishes %v", got)
	}
	if e.Container(copied.Name) != nil {
		t.Error("the undo left Dockwell's copy")
	}
}

// shopOnDokploy is a server whose Dokploy runs the app shop as the Swarm service
// shop-k2m4, publishing 8085, with its task running.
type shopOnDokploy struct {
	e        *dockertest.Engine
	id, task string
	ic       dockwellv1connect.ImportServiceClient
	c        dockwellv1connect.ChangeServiceClient
	replicas func() uint64
	stopper  *laterStopper
}

func dokployShop(t *testing.T) shopOnDokploy {
	t.Helper()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	later := &laterStopper{}
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&host.Driver{Panels: later}} })
	later.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	id := f.lastDocker.id.ServerID
	labels := map[string]string{"dokploy.project": "store", "dokploy.environment": "production", "dokploy.application": "shop"}
	shopSpec := swarmService("shop-k2m4", "nginx:1.26", labels)
	// Dokploy's external port, published through Swarm's routing mesh.
	shopSpec.EndpointSpec.Ports = []docker.ServicePort{{Protocol: "tcp", TargetPort: 80, PublishedPort: 8085, PublishMode: "ingress"}}
	e.Services = []docker.Service{
		{ID: "s1", Spec: swarmService("dokploy", "dokploy/dokploy:v0.31.2", nil)},
		{ID: "s2", Spec: shopSpec},
	}
	task := runByHand(t, e, "shop-k2m4.1.x7k2", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings = nil
		r.Labels = map[string]string{
			"com.docker.swarm.service.name": "shop-k2m4", "com.docker.swarm.task.id": "x7k2",
			"traefik.enable": "true",
		}
		maps.Copy(r.Labels, labels)
	})
	replicas := func() uint64 {
		svc, err := e.Client().InspectService(t.Context(), "shop-k2m4")
		if err != nil {
			t.Fatal(err)
		}
		n, _ := svc.Spec.Mode.Replicas()
		return n
	}
	return shopOnDokploy{e: e, id: id, task: task, ic: importClient(b), c: changeClient(b, b.origin), replicas: replicas, stopper: later}
}

// An import whose copy of a Swarm app fails to start, after the host scaled
// Dokploy's service to none, rolls back, and the rollback scales it again, so the
// app isn't left down.
func TestAFailedRemakeScalesDokploysServiceBack(t *testing.T) {
	d := dokployShop(t)
	res, err := d.ic.ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: d.id}))
	if err != nil {
		t.Fatal(err)
	}
	// Something takes Dokploy's port once the scale has freed it, before the copy
	// starts: the apply's own checks ran before the scale, so only the start finds it.
	var once bool
	d.stopper.afterApply = func() {
		if !once {
			once = true
			runByHand(t, d.e, "squatter", func(r *docker.CreateRequest) {
				r.HostConfig.Binds, r.HostConfig.PortBindings = nil, map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8085"}}}
			})
		}
	}
	if done := actedOn(t, d.c, res.Msg.GetChange(), ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the import ended %v: %s", done.GetState(), done.GetError())
	}
	for deadline := time.Now().Add(10 * time.Second); d.replicas() != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the failure Dokploy's service runs %d", d.replicas())
		}
	}
	for _, name := range d.e.Containers() {
		if cc := d.e.Container(name); cc != nil && cc.Config.Labels[docker.LabelService] == "shop-k2m4" && cc.ID != d.task && cc.State.Running {
			t.Errorf("Dockwell's copy %s runs beside Dokploy's", name)
		}
	}
}

// A stack Dokploy deploys with docker stack deploy comes again in a project of its
// own, each service named as the stack named it, so its services still find each
// other; each Swarm service scales to none as its copy starts.
func TestMakingADokployStackAgain(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	later := &laterStopper{}
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&host.Driver{Panels: later}} })
	later.s = &panels.Stopper{Docker: e.Client()}
	waitForHost(t, f, f.lastDocker)
	id := f.lastDocker.id.ServerID
	labels := map[string]string{"com.docker.stack.namespace": "forum-q1", "dokploy.project": "community", "dokploy.application": "forum"}
	e.Services = []docker.Service{
		{ID: "s1", Spec: swarmService("dokploy", "dokploy/dokploy:v0.31.2", nil)},
		{ID: "s2", Spec: swarmService("forum-q1_web", "nginx:1.26", labels)},
		{ID: "s3", Spec: swarmService("forum-q1_db", "nginx:1.26", labels)},
	}
	for _, svc := range []string{"web", "db"} {
		runByHand(t, e, "forum-q1_"+svc+".1.a"+svc, func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings = nil
			r.Labels = map[string]string{"com.docker.swarm.service.name": "forum-q1_" + svc, "com.docker.stack.namespace": "forum-q1"}
		})
	}
	ic, c := importClient(b), changeClient(b, b.origin)
	res, err := ic.ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	step := panelSteps(res.Msg)["IMPORT_KIND_APP forum"]
	if step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE || !strings.Contains(step.GetBecomes(), "its services, db and web, in the project forum") {
		t.Fatalf("the stack reads %v", step)
	}
	planned := res.Msg.GetChange()
	var created []string
	for _, st := range planned.GetPlan().GetServers()[0].GetSteps() {
		if st.GetResource().GetKind() == "service" {
			created = append(created, st.GetResource().GetName())
		}
		if p := st.GetPanel(); p != nil && len(p.GetParts()) != 2 {
			t.Errorf("the scale reads %v", p)
		}
	}
	if !slices.Equal(created, []string{"web", "db"}) && !slices.Equal(created, []string{"db", "web"}) {
		t.Errorf("the plan makes %v", created)
	}
	if done := actedOn(t, c, planned, ""); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the import ended %v: %s", done.GetState(), done.GetError())
	}
	for _, name := range []string{"forum-q1_web", "forum-q1_db"} {
		svc, err := e.Client().InspectService(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := svc.Spec.Mode.Replicas(); n != 0 {
			t.Errorf("%s still runs %d", name, n)
		}
	}
}
