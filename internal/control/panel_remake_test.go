// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
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
	if !strings.Contains(step.GetReason(), "Turn off Autodeploy for shop in Dokploy before you apply, or a push to its repository scales its service back up beside its copy") {
		t.Errorf("shop's step says nothing of Autodeploy: %q", step.GetReason())
	}
	planned := res.Msg.GetChange()
	if !noteSays(planned, "remakes_autodeploy_dokploy", "turn off Autodeploy for each in Dokploy before you apply") {
		t.Errorf("the plan says nothing of Autodeploy: %v", planned.GetPlan().GetNotes())
	}
	if !noteSays(planned, "remakes_dokploy", "Dockwell scales each to none in Dokploy first, and takes it off its ports and networks") {
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
	// It joins no network, so its name answers on Dokploy's for the copy alone,
	// which joins that network under it, and not Swarm's routing mesh.
	joins := func() string {
		svc, err := e.Client().InspectService(t.Context(), "shop-k2m4")
		if err != nil {
			t.Fatal(err)
		}
		return string(svc.Spec.TaskTemplate.Networks)
	}
	if got := joins(); got != "[]" {
		t.Errorf("Dokploy's service still joins %s", got)
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
	// A Swarm task's container has no restart policy, as Swarm restarts it; the
	// copy restarts as its service did.
	if got := copied.HostConfig.RestartPolicy.Name; got != "unless-stopped" {
		t.Errorf("Dockwell's copy restarts %q", got)
	}
	if ep := copied.NetworkSettings.Networks["dokploy-network"]; ep == nil || !slices.Contains(ep.Aliases, "shop-k2m4") {
		t.Errorf("Dockwell's copy is on %v, not on dokploy-network as shop-k2m4", maps.Keys(copied.NetworkSettings.Networks))
	} else if copied.NetworkSettings.Networks["ingress"] != nil || slices.Contains(ep.Aliases, "x7k2abcdef12") {
		t.Errorf("Dockwell's copy keeps what Swarm gave its task: %v", ep.Aliases)
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
	if got := joins(); got != `[{"Target":"dokploy-network"}]` {
		t.Errorf("after the undo Dokploy's service joins %s", got)
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
	// Dokploy deploys shop from Git: its code folder is a checkout.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "dokploy", "applications", "shop-k2m4", "code", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.panelsRoot = root
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
	// Dokploy's network, where its Traefik reaches the app by its appName, and
	// Swarm's routing mesh.
	e.AddNetwork(docker.Network{ID: "d0kn", Name: "dokploy-network", Driver: "overlay", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "10.0.1.0/24"}}}})
	e.AddNetwork(docker.Network{ID: "1ngr", Name: "ingress", Driver: "overlay", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "10.0.0.0/24"}}}})
	shopSpec.TaskTemplate.Networks = json.RawMessage(`[{"Target":"dokploy-network"}]`)
	e.Services = []docker.Service{
		{ID: "s1", Spec: swarmService("dokploy", "dokploy/dokploy:v0.31.2", nil)},
		{ID: "s2", Spec: shopSpec},
	}
	task := runByHand(t, e, "shop-k2m4.1.x7k2", func(r *docker.CreateRequest) {
		// A Swarm task's container has no restart policy: Swarm restarts it.
		r.HostConfig.PortBindings, r.HostConfig.RestartPolicy = nil, docker.RestartPolicy{}
		// Swarm pins each task's address on each network it joins.
		r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{
			"dokploy-network": {Aliases: []string{"x7k2abcdef12"}, IPAMConfig: &docker.EndpointIPAM{IPv4Address: "10.0.1.7"}},
			"ingress":         {IPAMConfig: &docker.EndpointIPAM{IPv4Address: "10.0.0.7"}},
		}
		r.HostConfig.NetworkMode = "dokploy-network"
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
	e.Services[2].Spec.TaskTemplate.RestartPolicy.Condition = "on-failure"
	e.AddNetwork(docker.Network{ID: "d0kn", Name: "dokploy-network", Driver: "overlay"})
	e.AddNetwork(docker.Network{ID: "f0rm", Name: "forum-q1_default", Driver: "overlay"})
	for _, svc := range []string{"web", "db"} {
		runByHand(t, e, "forum-q1_"+svc+".1.a"+svc, func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings, r.HostConfig.RestartPolicy = nil, docker.RestartPolicy{}
			r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{"dokploy-network": {}, "forum-q1_default": {Aliases: []string{svc}}}
			r.HostConfig.NetworkMode = "forum-q1_default"
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
	// Each copy answers on Dokploy's network to the name the stack gave it, as
	// others there reached it, and leaves the stack's own network behind.
	for _, name := range e.Containers() {
		c := e.Container(name)
		if c == nil || c.Config.Labels["com.docker.swarm.service.name"] != "" || c.Config.Labels[docker.LabelService] == "" {
			continue
		}
		service := c.Config.Labels[docker.LabelService]
		if ep := c.NetworkSettings.Networks["dokploy-network"]; ep == nil || !slices.Contains(ep.Aliases, "forum-q1_"+service) || c.NetworkSettings.Networks["forum-q1_default"] != nil {
			t.Errorf("the copy of %s is on %v", service, c.NetworkSettings.Networks)
		}
	}
	// Each copy restarts as its service did: web as Swarm's default, db on failure.
	for service, want := range map[string]string{"web": "unless-stopped", "db": "on-failure"} {
		var policy string
		for _, name := range e.Containers() {
			if c := e.Container(name); c != nil && c.Config.Labels[docker.LabelService] == service && c.Config.Labels["com.docker.swarm.service.name"] == "" {
				policy = c.HostConfig.RestartPolicy.Name
			}
		}
		if policy != want {
			t.Errorf("the copy of %s restarts %q, not %q", service, policy, want)
		}
	}
}

// A container that shares a Swarm task's network would lose it as the copy takes
// the task's place, so the app stays where it is, saying what leans on it.
func TestWhatLeansOnATaskKeepsItsApp(t *testing.T) {
	d := dokployShop(t)
	runByHand(t, d.e, "shop-sidecar", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.HostConfig.NetworkMode = "container:shop-k2m4.1.x7k2"
	})
	res, err := d.ic.ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: d.id}))
	if err != nil {
		t.Fatal(err)
	}
	if step := panelSteps(res.Msg)["IMPORT_KIND_APP shop"]; step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || !strings.Contains(step.GetReason(), "shop-sidecar shares its network") {
		t.Errorf("shop reads %v", step)
	}
}

// A stack deployed by hand carries none of Dokploy's labels: it stays behind,
// saying so, and the rest of the import still plans, where the agent would refuse
// to scale it.
func TestAStackDeployedByHandStaysBehind(t *testing.T) {
	d := dokployShop(t)
	plain := swarmService("plain-p1_web", "nginx:1.26", map[string]string{"com.docker.stack.namespace": "plain-p1"})
	d.e.Services = append(d.e.Services, docker.Service{ID: "s9", Spec: plain})
	runByHand(t, d.e, "plain-p1_web.1.p1", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.Labels = map[string]string{"com.docker.swarm.service.name": "plain-p1_web", "com.docker.stack.namespace": "plain-p1"}
	})
	res, err := d.ic.ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: d.id}))
	if err != nil {
		t.Fatal(err)
	}
	steps := panelSteps(res.Msg)
	if s := steps["IMPORT_KIND_APP plain-p1"]; s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || !strings.Contains(s.GetReason(), "carries none of Dokploy's labels") {
		t.Errorf("the stack deployed by hand reads %v", s)
	}
	if s := steps["IMPORT_KIND_APP shop"]; s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE || res.Msg.GetChange() == nil {
		t.Errorf("shop reads %v, with a plan %v", s, res.Msg.GetChange() != nil)
	}
}
