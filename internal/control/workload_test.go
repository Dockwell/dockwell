// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestManagedWorkloadControls(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testManagedWorkloadControls)
}

func testManagedWorkloadControls(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	engine := f.addDockerServer(t)
	firstAgent := f.lastDocker
	server := firstAgent.id.ServerID
	peer := f.addServerNamed(t, "second", nil, nil)
	c := changeClient(b, b.origin)
	finished := func(plan *dockwellv1.Change) *dockwellv1.Change {
		t.Helper()
		apply(t, c, plan)
		return waitChange(t, c, plan.GetId(), plan.GetSummary(), func(ch *dockwellv1.Change) bool {
			if ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED {
				t.Fatalf("change %q failed: %s: %v", plan.GetSummary(), ch.GetError(), ch.GetSteps())
			}
			return ch.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED
		})
	}

	edit := serviceEdit("workload", "ghcr.io/acme/shop:1.4", map[string]string{"PRIVATE": "environment-fixture"})
	edit.Servers = []string{server, f.lastDocker.id.ServerID}
	edit.GetSpec().GetService().GetApp().Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
	made := finished(propose(t, c, "Run workload", edit))
	managed := made.GetPlan().GetResources()[0]
	name := docker.ContainerName(docker.Project(managed.GetResource().GetScope()), "workload")
	original, other := engine.Container(name), peer.Container(name)
	if original == nil || other == nil {
		t.Fatal("workload missing from an approved placement")
	}

	request := &dockwellv1.RestartServiceRequest{Id: managed.GetId(), ServerId: server, ContainerId: original.ID}
	restart := func() *dockwellv1.Change {
		t.Helper()
		res, err := c.RestartService(t.Context(), connect.NewRequest(request))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetChange()
	}
	plan := restart()
	if len(plan.GetPlan().GetServers()) != 1 || plan.GetPlan().GetServers()[0].GetServerId() != server || !plan.GetRestart() {
		t.Fatalf("restart plan is not scoped to the reviewed server: %v", plan.GetPlan())
	}
	finished(plan)
	current := engine.Container(name)
	if current.ID != original.ID || !current.State.StartedAt.After(original.State.StartedAt) ||
		!peer.Container(name).State.StartedAt.Equal(other.State.StartedAt) {
		t.Fatal("restart didn't affect exactly the reviewed instance")
	}

	firstAgent.stop()
	firstAgent.hello = func(h *agentv1.Hello) { h.DockerCapabilities.WorkloadControls = false }
	firstAgent.connect(t, f)
	if _, err := c.RestartService(t.Context(), connect.NewRequest(request)); err == nil || !strings.Contains(err.Error(), "update the agent") {
		t.Fatalf("older agent accepted an exact restart: %v", err)
	}
	firstAgent.stop()
	firstAgent.hello = nil
	firstAgent.connect(t, f)

	runEdit := func(base int64, want resourcev1.DesiredRunState) *dockwellv1.Edit {
		app := &resourcev1.Container{DesiredRunState: want}
		svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
		return &dockwellv1.Edit{
			Resource: managed.GetResource(), BaseRevision: base,
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
		}
	}
	stop := runEdit(1, resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED)
	unchanged := propose(t, c, "Save running intent", runEdit(1, resourcev1.DesiredRunState_DESIRED_RUN_STATE_RUNNING))
	if slices.ContainsFunc(unchanged.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "workload_starts" }) {
		t.Fatal("setting run intent on an already running workload announced a start")
	}
	plan = propose(t, c, "Stop workload", stop)
	if len(plan.GetPlan().GetServers()) != 2 || !strings.Contains(plan.GetPlan().String(), "health checks") || strings.Contains(plan.String(), "environment-fixture") {
		t.Fatal("stop didn't review fleet impact with its values redacted")
	}
	finished(plan)

	assertStopped := func(e *docker.Client, id string) {
		t.Helper()
		got, err := e.InspectContainer(t.Context(), id)
		if err != nil || got.State.Running || got.HostConfig.RestartPolicy.Name != "no" {
			t.Fatalf("container didn't stay stopped after Docker restarted: %v", err)
		}
	}
	for _, e := range []struct {
		client *docker.Client
		id     string
		reboot func()
	}{{engine.Client(), current.ID, engine.DaemonKilled}, {peer.Client(), other.ID, peer.DaemonKilled}} {
		e.reboot()
		assertStopped(e.client, e.id)
	}

	firstAgent.stop()
	firstAgent.connect(t, f)
	assertStopped(engine.Client(), current.ID)

	deploy := proto.CloneOf(edit)
	deploy.Servers = nil
	deploy.BaseRevision = 2
	deploy.GetSpec().GetService().GetApp().DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
	deploy.GetSpec().GetService().GetApp().Image = "ghcr.io/acme/shop:1.5"
	finished(propose(t, c, "Deploy the stopped workload", deploy))
	current = engine.Container(name)
	if current.ID == original.ID || !current.State.StartedAt.IsZero() {
		t.Fatal("the stopped deploy did not create a replacement that had never run")
	}
	engine.DaemonKilled()
	assertStopped(engine.Client(), current.ID)

	request.ContainerId = current.ID
	if _, err := c.RestartService(t.Context(), connect.NewRequest(request)); err == nil {
		t.Fatal("restart was allowed for a workload stopped by revision")
	}

	start := runEdit(3, resourcev1.DesiredRunState_DESIRED_RUN_STATE_RUNNING)
	finished(propose(t, c, "Start workload", start))
	started := engine.Container(name)
	if started.ID != current.ID || !started.State.Running || started.HostConfig.RestartPolicy.Name != "always" || !strings.Contains(strings.Join(started.Config.Env, "\n"), "PRIVATE=environment-fixture") {
		t.Fatal("start didn't restore the configured policy in place")
	}

	pinned := restart()
	if err := engine.Client().RemoveContainer(t.Context(), started.ID, true); err != nil {
		t.Fatal(err)
	}
	replacement := *started
	replacement.ID = strings.Repeat("a", 64)
	engine.AddContainer(replacement)
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: pinned.GetId(), Fingerprint: pinned.GetPlan().GetFingerprint()}); err == nil {
		t.Fatal("the restart could apply after its reviewed instance was replaced")
	}
	for _, call := range engine.Seen() {
		if strings.HasPrefix(call, http.MethodPost+" ") && strings.HasSuffix(call, "/containers/"+replacement.ID+"/restart") {
			t.Fatal("restart reached the replacement")
		}
	}
}

func TestAdoptingAContainerPinsTheReviewedID(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		containers := containerClient(b, b.origin)
		const name = "outside"
		old := e.AddContainer(docker.Container{ID: strings.Repeat("b", 64), Name: "/" + name, Config: docker.Config{Image: "alpine:latest"}})
		if err := e.Client().RemoveContainer(t.Context(), old.ID, true); err != nil {
			t.Fatal(err)
		}
		newer := e.AddContainer(docker.Container{ID: strings.Repeat("c", 64), Name: "/" + name, Config: docker.Config{Image: "alpine:latest"}})
		_, err := containers.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name, ExpectedContainerId: old.ID}))
		if reasonOf(err) != "container_replaced" {
			t.Fatalf("adoption with a stale ID: %v", err)
		}
		if got := e.Container(name); got.ID != newer.ID || got.Config.Labels[docker.LabelAdopted] != "" {
			t.Fatal("the replacement was touched by a stale adoption")
		}

		// The applied adoption itself is evidence of ownership, even with
		// no health report yet.
		ctx := t.Context()
		adopted, err := containers.AdoptContainer(ctx, connect.NewRequest(&dockwellv1.AdoptContainerRequest{
			Id: name, ExpectedContainerId: newer.ID,
		}))
		if err != nil {
			t.Fatal(err)
		}
		c := changeClient(b, b.origin)
		plan := adopted.Msg.GetChange()
		applyConfirmed(t, c, plan)
		inventory := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		obj, err := inventory.GetDockerObject(ctx, connect.NewRequest(&dockwellv1.GetDockerObjectRequest{
			Key: &dockwellv1.DockerObjectKey{ServerId: f.lastDocker.id.ServerID, EngineId: newer.ID},
		}))
		if err != nil || obj.Msg.GetObject().GetManagedRevision() != 1 ||
			obj.Msg.GetObject().GetManagedResourceId() != plan.GetPlan().GetResources()[0].GetId() {
			t.Fatalf("the inventory didn't identify the recorded adoption: %v, %v", obj, err)
		}
	})
}

func TestServerContainerWorkloadControls(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		c := changeClient(b, b.origin)
		plan := webEdit("ghcr.io/acme/shop:1.4", nil)
		plan.Resource = &dockwellv1.ResourceRef{Kind: "container", Name: "standalone", Scope: server}
		plan.GetSpec().GetContainer().Ports = nil
		run := func(ch *dockwellv1.Change) {
			t.Helper()
			apply(t, c, ch)
			waitChange(t, c, ch.GetId(), ch.GetSummary(), func(done *dockwellv1.Change) bool {
				if done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_FAILED {
					t.Fatalf("%s failed: %s", done.GetSummary(), done.GetError())
				}
				return done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED
			})
		}
		initial := propose(t, c, "Run standalone", plan)
		run(initial)
		before := e.Container("standalone")
		r := initial.GetPlan().GetResources()[0]
		e.SetRestarting("standalone", 1)
		restart, err := c.RestartService(t.Context(), connect.NewRequest(&dockwellv1.RestartServiceRequest{
			Id: r.GetId(), ServerId: server, ContainerId: before.ID,
		}))
		if err != nil {
			t.Fatal(err)
		}
		run(restart.Msg.GetChange())
		if after := e.Container("standalone"); after.ID != before.ID || !after.State.StartedAt.After(before.State.StartedAt) {
			t.Fatal("standalone restart didn't keep the same container")
		}
		stop := &dockwellv1.Edit{
			Resource: r.GetResource(), BaseRevision: 1,
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Container{Container: &resourcev1.Container{
				DesiredRunState: resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED,
			}}},
		}
		stopping := propose(t, c, "Stop standalone", stop)
		run(stopping)
		if after := e.Container("standalone"); after.ID != before.ID || after.State.Running {
			t.Fatal("standalone stop didn't preserve the stopped container")
		}
		undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: stopping.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		agent := f.lastDocker
		agent.stop()
		agent.hello = func(h *agentv1.Hello) { h.DockerCapabilities.WorkloadControls = false }
		agent.connect(t, f)
		if _, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: stopping.GetId()})); err == nil || !strings.Contains(err.Error(), "update the agent") {
			t.Fatalf("older agent planned an undo of Stop: %v", err)
		}
		if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: undo.Msg.GetChange().GetId(), Fingerprint: undo.Msg.GetChange().GetPlan().GetFingerprint()}); err == nil || !strings.Contains(err.Error(), "update the agent") {
			t.Fatalf("older agent applied a planned undo of Stop: %v", err)
		}
		agent.stop()
		agent.hello = nil
		agent.connect(t, f)
		run(undo.Msg.GetChange())
		if after := e.Container("standalone"); after.ID != before.ID || !after.State.Running {
			t.Fatal("undo didn't start the same container again")
		}
	})
}

func TestFailedStopRestoresTheRestartPolicy(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c := changeClient(b, b.origin)
		edit := serviceEdit("workload", "ghcr.io/acme/shop:1.4", nil)
		edit.GetSpec().GetService().GetApp().Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
		made := apply(t, c, propose(t, c, "Run workload", edit))
		ref := made.GetPlan().GetResources()[0].GetResource()
		name := docker.ContainerName(docker.Project(ref.GetScope()), ref.GetName())
		before := e.Container(name)
		var failedStop, failedImmediateRepair atomic.Bool
		e.Intercept(func(req *http.Request) error {
			if req.Method != http.MethodPost {
				return nil
			}
			switch {
			case strings.HasSuffix(req.URL.Path, "/containers/"+before.ID+"/stop") && failedStop.CompareAndSwap(false, true):
				return errors.New("injected Docker stop failure")
			case strings.HasSuffix(req.URL.Path, "/containers/"+before.ID+"/update") && failedStop.Load() && failedImmediateRepair.CompareAndSwap(false, true):
				// The revision rollback must still see the effective policy and restore it.
				return errors.New("injected Docker policy update failure")
			}
			return nil
		})
		defer e.Intercept(nil)
		stop := proto.CloneOf(edit)
		stop.GetSpec().GetService().GetApp().DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
		failed := apply(t, c, propose(t, c, "Stop workload", stop))
		if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || failed.GetRolledBackBy() == "" || !failedImmediateRepair.Load() {
			t.Fatalf("expected a failed stop followed by a revision rollback: %v", failed)
		}
		rb := waitWatch(t, watch(t, c, failed.GetRolledBackBy())).last()
		if after := e.Container(name); rb.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || after == nil || after.ID != before.ID || !after.State.Running || after.HostConfig.RestartPolicy.Name != "always" {
			t.Fatalf("rollback did not restore the running container's policy: %v", rb)
		}
	})
}

func stopWorkloadEdit(ref *dockwellv1.ResourceRef) *dockwellv1.Edit {
	app := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		DesiredRunState: resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED,
	}}}
	return &dockwellv1.Edit{Resource: ref, BaseRevision: 1, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: app}}}
}
