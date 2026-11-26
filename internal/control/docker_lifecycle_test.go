// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

func (s *storageFixture) planLifecycle(t *testing.T, id string, action v1.DockerContainerAction, volumes bool) *v1.Change {
	t.Helper()
	selection := &v1.DockerLifecycleSelection{
		Keys:   []*v1.DockerObjectKey{{ServerId: s.server.id.ServerID, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: id}},
		Action: action, RemoveAnonymousVolumes: volumes,
	}
	if action == v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_KILL {
		selection.Signal = "SIGKILL"
	}
	got, err := s.api.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
		Operation: &v1.PlanDockerMaintenanceRequest_Lifecycle{Lifecycle: selection},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return got.Msg.GetChange()
}

func (s *storageFixture) applyLifecycleWithPasskey(t *testing.T, change *v1.Change) {
	t.Helper()
	const action = v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE
	req := &v1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()}
	_, err := s.changes.ApplyChange(t.Context(), connect.NewRequest(req))
	if asked := stepUpAsked(err); asked.GetAction() != action || asked.GetTarget() != req.Id+":"+req.Fingerprint {
		t.Fatalf("expected a passkey for this plan, got %v", err)
	}
	req.Proof = stepUpFor(t, s.person, action, req.Id+":"+req.Fingerprint)
	w := watch(t, s.changes, req.Id)
	if _, err := s.changes.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change ended %v: %s", got.GetState(), got.GetError())
	}
}

func TestDockerExternalLifecyclePasskeyGates(t *testing.T) {
	f := storageControl(t, store.SQLite, nil, "")
	const volume = "18fae5d7d7a13f0edfcd5b644a46c33e27de3cef51230b79c7857fcec0f1eb36"
	f.engine.AddVolume(docker.Volume{Name: volume, Driver: "local", Scope: "local", Labels: map[string]string{"com.docker.volume.anonymous": ""}})
	for _, fixture := range []struct {
		name   string
		host   docker.HostConfig
		mounts []docker.MountPoint
		remove bool
	}{
		{name: "pid-and-cap", host: docker.HostConfig{PidMode: "host", CapAdd: []string{"SYS_ADMIN"}}},
		{name: "system-path", mounts: []docker.MountPoint{{Type: "bind", Source: "/etc", Destination: "/host-etc"}}},
		{name: "anonymous", mounts: []docker.MountPoint{{Type: "volume", Name: volume, Destination: "/data"}}, remove: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			item := f.engine.AddContainer(docker.Container{
				Name: "/" + fixture.name, Config: docker.Config{Image: "busybox:1.37"},
				HostConfig: fixture.host, Mounts: fixture.mounts, State: docker.State{Status: "created"},
			})
			action := v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_START
			if fixture.remove {
				action = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_REMOVE
			}
			change := f.planLifecycle(t, item.ID, action, fixture.remove)
			f.applyLifecycleWithPasskey(t, change)
		})
	}
}

func TestDockerExternalLifecycleAndRunImportOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	f := storageControl(t, store.SQLite, d, "")
	ctx := t.Context()
	const image = "busybox:1.37"
	if err := d.PullImage(ctx, image, nil); err != nil {
		t.Fatal(err)
	}
	external := "outside-" + strings.ToLower(strings.TrimPrefix(dockertest.Scope(t), "env_"))
	engineID, err := d.CreateContainer(ctx, external, &docker.CreateRequest{
		Config: docker.Config{
			Image: image, Cmd: []string{"sh", "-c", "trap 'exit 0' TERM; while true; do sleep 1; done"},
			Volumes: map[string]struct{}{"/scratch": {}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		finish, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.RemoveOneOff(finish, engineID)
	})
	get := func() *docker.Container {
		t.Helper()
		got, err := d.InspectContainer(ctx, engineID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	applyAction := func(action v1.DockerContainerAction, running bool) {
		t.Helper()
		applyConfirmed(t, f.changes, f.planLifecycle(t, engineID, action, false))
		if got := get(); got.State.Running != running {
			t.Fatalf("%s left the container %s", action, got.State.Status)
		}
	}
	applyAction(v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_START, true)
	applyAction(v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_PAUSE, true)
	if !get().State.Paused {
		t.Fatal("Pause left the container running")
	}
	applyAction(v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_STOP, false)
	applyAction(v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_START, true)
	applyAction(v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_PAUSE, true)
	applyAction(v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_KILL, false)
	mounts := get().Mounts
	if len(mounts) != 1 {
		t.Fatalf("want one anonymous volume, got %v", mounts)
	}
	f.applyLifecycleWithPasskey(t, f.planLifecycle(t, engineID, v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_REMOVE, true))
	if _, err := d.InspectContainer(ctx, engineID); !docker.IsNotFound(err) {
		t.Fatalf("the removed container remains: %v", err)
	}
	if _, err := d.InspectVolume(ctx, mounts[0].Name); !docker.IsNotFound(err) {
		t.Fatalf("the removed anonymous volume remains: %v", err)
	}

	// This is the create-only proposal the Paste docker run sheet sends for
	// docker run --name web -e TITLE='two words' busybox:1.37 sleep 600.
	api := dockwellv1connect.NewResourceServiceClient(f.person, f.person.origin, connect.WithProtoJSON())
	envs, err := api.ListResources(ctx, connect.NewRequest(&v1.ListResourcesRequest{Kind: "environment"}))
	if err != nil || len(envs.Msg.GetResources()) != 1 {
		t.Fatalf("environments: %v %v", envs, err)
	}
	env := envs.Msg.GetResources()[0].GetId()
	dockertest.Cleanup(t, d, docker.Project(env))
	edit := runImage(image)
	edit.Resource.Kind, edit.Resource.Scope = "service", env
	edit.MustCreate, edit.Servers = true, []string{f.server.id.ServerID}
	app := edit.GetSpec().GetService().GetApp()
	app.Command, app.Env = []string{"sleep", "600"}, map[string]string{"TITLE": "two words"}
	applyConfirmed(t, f.changes, propose(t, f.changes, "Run web as a Dockwell app", edit))
	got := f.workload(t, env)
	if !got.State.Running || !slices.Equal(got.Config.Cmd, app.Command) || !slices.Contains(got.Config.Env, "TITLE=two words") {
		t.Fatal("the imported app does not run with the reviewed command and environment")
	}
	if _, err := f.changes.ProposeChange(ctx, connect.NewRequest(&v1.ProposeChangeRequest{
		Summary: "Import web again", Edits: []*v1.Edit{edit},
	})); err == nil {
		t.Fatal("the create-only import planned over the existing workload")
	}
}

func TestManagedPausedRedeployPlansAndKeepsPause(t *testing.T) {
	f := storageControl(t, store.SQLite, nil, "")
	pullsAnything(f.engine)
	made := propose(t, f.changes, "Run app", runImage("nginx:1"))
	applyConfirmed(t, f.changes, made)
	workload := storageScope(t, made, "service", "web")
	env := workload.GetResource().GetScope()
	edit := runImage("nginx:1")
	edit.Resource = workload.GetResource()
	edit.GetSpec().GetService().GetApp().DesiredRunState = spec.DesiredRunState_DESIRED_RUN_STATE_PAUSED
	applyConfirmed(t, f.changes, propose(t, f.changes, "Pause web", edit))
	before := f.workload(t, env)
	if !before.State.Paused {
		t.Fatal("workload was not paused")
	}
	edit.GetSpec().GetService().GetApp().Image = "nginx:2"
	deploy := propose(t, f.changes, "Deploy a new image", edit)
	plan := deploy.GetPlan()
	for _, placement := range plan.GetServers() {
		for _, step := range placement.GetSteps() {
			for _, diff := range step.GetChanges() {
				if diff.GetPath() == "state" {
					t.Fatalf("paused replacement planned a runtime transition: %v", diff)
				}
			}
		}
	}
	if !slices.ContainsFunc(plan.GetNotes(), func(note *v1.PlanNote) bool { return note.GetReason() == "stays_paused" }) {
		t.Fatal("plan does not say that web stays paused")
	}
	applyConfirmed(t, f.changes, deploy)
	after := f.workload(t, env)
	if !after.State.Paused || after.ID == before.ID {
		t.Fatal("redeploy did not replace the workload and keep it paused")
	}
	services := dockwellv1connect.NewServiceServiceClient(f.person, f.person.origin, connect.WithProtoJSON())
	pull, err := services.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{Id: workload.GetId(), Image: "nginx:2"}))
	if err != nil || noteOf(pull.Msg.GetChange(), "stays_paused") == nil {
		t.Fatalf("Pull again does not say it keeps the pause: %v %v", pull, err)
	}
	applyConfirmed(t, f.changes, pull.Msg.GetChange())
	if got := f.workload(t, env); !got.State.Paused || got.ID != after.ID {
		t.Fatal("pulling the same image disturbed the paused workload")
	}
	for _, id := range []string{workload.GetId(), env} {
		if _, err := f.changes.RestartService(t.Context(), connect.NewRequest(&v1.RestartServiceRequest{Id: id})); err == nil || !strings.Contains(err.Error(), "web is paused. Use Unpause first") {
			t.Fatalf("Restart of %s: %v", id, err)
		}
	}
	other := runImage("nginx:1")
	other.Resource = &v1.ResourceRef{Kind: "service", Scope: env, Name: "workers"}
	applyConfirmed(t, f.changes, propose(t, f.changes, "Deploy workers", other))
	restart, err := f.changes.RestartService(t.Context(), connect.NewRequest(&v1.RestartServiceRequest{Id: env}))
	if err != nil {
		t.Fatal(err)
	}
	stack := restart.Msg.GetChange()
	if note := noteOf(stack, "stays_paused"); note.GetResource().GetName() != "web" {
		t.Fatalf("stack restart does not say web stays paused: %v", note)
	}
	for _, placement := range stack.GetPlan().GetServers() {
		if slices.ContainsFunc(placement.GetSteps(), func(step *v1.PlanStep) bool { return step.GetResource().GetName() == "web" }) {
			t.Fatal("stack restart plans a step for the paused workload")
		}
	}
}

func TestPlanDockerLifecycleRefusesNonSIGKILLOnPausedContainer(t *testing.T) {
	f := storageControl(t, store.SQLite, nil, "")
	item := f.engine.AddContainer(docker.Container{
		Name: "/frozen-ext", Config: docker.Config{Image: "busybox:1"},
		State: docker.State{Status: "paused", Running: true, Paused: true},
	})
	key := &v1.DockerObjectKey{
		ServerId: f.server.id.ServerID,
		Kind:     v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER,
		EngineId: item.ID,
	}
	_, err := f.api.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
		Operation: &v1.PlanDockerMaintenanceRequest_Lifecycle{Lifecycle: &v1.DockerLifecycleSelection{
			Keys:   []*v1.DockerObjectKey{key},
			Action: v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_KILL,
			Signal: "SIGHUP",
		}},
	}))
	if err == nil || !strings.Contains(err.Error(), "default kill or SIGKILL") {
		t.Fatalf("expected paused SIGHUP refusal, got %v", err)
	}
	if _, err := f.api.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
		Operation: &v1.PlanDockerMaintenanceRequest_Lifecycle{Lifecycle: &v1.DockerLifecycleSelection{
			Keys:   []*v1.DockerObjectKey{key},
			Action: v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_KILL,
			Signal: "SIGKILL",
		}},
	})); err != nil {
		t.Fatalf("expected paused SIGKILL to plan, got %v", err)
	}
}
