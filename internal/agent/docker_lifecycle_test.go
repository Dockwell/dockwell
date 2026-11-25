// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

var externalHost = resource.Ref{Kind: host.Kind, Scope: "srv_cleanup", Name: host.Name}

func lifecycleKey(engineID string) *v1.DockerObjectKey {
	return &v1.DockerObjectKey{ServerId: externalHost.Scope, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: engineID}
}

func lifecyclePlan(t *testing.T, c *calls, key *v1.DockerObjectKey, action v1.DockerContainerAction, name string, volumes bool) *v1.DockerMaintenance {
	t.Helper()
	preview, err := c.dockerLifecyclePreview(t.Context(), &agentv1.DockerLifecyclePreviewCall{
		Keys: []*v1.DockerObjectKey{key}, Action: action, NewName: name, RemoveAnonymousVolumes: volumes,
	})
	if err != nil || len(preview.GetTargets()) == 0 {
		t.Fatalf("preview: %v, %v", preview, err)
	}
	var timeout uint32
	if action == lifecycleKill {
		timeout = 1
	}
	return &v1.DockerMaintenance{ChangeId: id.New(id.Change), Lifecycle: &v1.DockerContainerLifecycle{
		Action: action, Targets: preview.GetTargets(), RemoveAnonymousVolumes: volumes, TimeoutSeconds: timeout,
	}}
}

func TestExternalContainerReviewDoesNotSurviveAdoption(t *testing.T) {
	e := dockertest.New(t)
	d := e.Client()
	d.Adopted = &docker.Adopted{}
	c := cleanupCalls(t, d)
	item := e.AddContainer(docker.Container{
		Name: "/external", Config: docker.Config{Image: "busybox:1"},
		State: docker.State{Status: "running", Running: true},
	})
	key := lifecycleKey(item.ID)
	plan := lifecyclePlan(t, c, key, lifecyclePause, "", false)
	if err := d.Adopted.Take(item.ID, docker.Taken{Project: "env_test", Service: "app"}); err != nil {
		t.Fatal(err)
	}
	var receipts []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
	if err := c.runDockerLifecycle(ctx, externalHost, plan, nil); err == nil {
		t.Fatal("a plan reviewed before adoption was applied")
	}
	if len(receipts) != 1 || receipts[0].GetReason() != receiptReasonProtected {
		t.Fatalf("receipt: %v", receipts)
	}
	after, err := d.InspectContainer(t.Context(), item.ID)
	if err != nil || after.State.Paused {
		t.Fatalf("adopted container changed: %v, %v", after, err)
	}
}

func TestExternalLifecycleRejectsSystemAndConfirmsHostAccess(t *testing.T) {
	e := dockertest.New(t)
	d := e.Client()
	c := cleanupCalls(t, d)
	for _, item := range []docker.Container{
		{Name: "/dockwell-panel", Config: docker.Config{Image: "busybox:1"}},
		{Name: "/protected", Config: docker.Config{Image: "busybox:1", Labels: map[string]string{docker.LabelIngress: "yes"}}},
	} {
		created := e.AddContainer(item)
		_, err := c.dockerLifecyclePreview(t.Context(), &agentv1.DockerLifecyclePreviewCall{
			Keys: []*v1.DockerObjectKey{lifecycleKey(created.ID)}, Action: lifecycleStart,
		})
		if !errors.Is(err, errProtectedContainer) {
			t.Fatalf("protected container %s: %v", item.Name, err)
		}
	}
	hostNamespace := e.AddContainer(docker.Container{Name: "/host-namespace", HostConfig: docker.HostConfig{NetworkMode: "host"}})
	for _, fixture := range []struct {
		name   string
		config docker.HostConfig
		mounts []docker.MountPoint
		host   bool
	}{
		{name: "bridge"},
		{name: "host-network", config: docker.HostConfig{NetworkMode: "host"}, host: true},
		{name: "host-network-shared", config: docker.HostConfig{NetworkMode: "container:" + hostNamespace.ID}, host: true},
		{name: "host-pid-and-cap", config: docker.HostConfig{PidMode: "host", CapAdd: []string{"SYS_ADMIN"}}, host: true},
		{name: "host-pid", config: docker.HostConfig{PidMode: "host"}, host: true},
		{name: "cap-added", config: docker.HostConfig{CapAdd: []string{"CAP_SYS_ADMIN"}}, host: true},
		{name: "host-ipc", config: docker.HostConfig{IpcMode: "host"}, host: true},
		{name: "device", config: docker.HostConfig{Devices: []docker.DeviceMapping{{PathOnHost: "/dev/sda"}}}, host: true},
		{name: "apparmor", config: docker.HostConfig{SecurityOpt: []string{"apparmor:unconfined"}}, host: true},
		{name: "etc-bind", mounts: []docker.MountPoint{{Type: "bind", Source: "/etc", Destination: "/host-etc"}}, host: true},
		{name: "time-zone", mounts: []docker.MountPoint{{Type: "bind", Source: "/usr/share/zoneinfo", Destination: "/zoneinfo"}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, state := range []struct {
				status string
				action v1.DockerContainerAction
			}{{"created", lifecycleStart}, {"running", lifecycleRestart}, {"paused", lifecycleUnpause}} {
				item := e.AddContainer(docker.Container{
					Name: "/" + fixture.name + "-" + state.status, Config: docker.Config{Image: "busybox:1"},
					HostConfig: fixture.config, Mounts: fixture.mounts, State: docker.State{Status: state.status},
				})
				plan := lifecyclePlan(t, c, lifecycleKey(item.ID), state.action, "", false)
				if got := plan.GetLifecycle().GetTargets()[0].GetHostAccess(); got != fixture.host {
					t.Errorf("%s has host access %t, want %t", state.action, got, fixture.host)
				}
			}
		})
	}
}

func TestExternalLifecycleCanRenameSeveralComposeProjects(t *testing.T) {
	e := dockertest.New(t)
	c := cleanupCalls(t, e.Client())
	preview := &agentv1.DockerLifecyclePreviewCall{Action: lifecycleRename, NewName: "new"}
	for _, project := range []string{"one", "two"} {
		e.AddContainer(docker.Container{
			Name: "/" + project + "-web-1", State: docker.State{Status: "created"},
			Config: docker.Config{Image: "busybox:1", Labels: map[string]string{
				docker.LabelProject: project, docker.LabelService: "web",
			}},
		})
		preview.Keys = append(preview.Keys, &v1.DockerObjectKey{
			ServerId: externalHost.Scope, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE, EngineId: "outside:" + project,
		})
	}
	plan, err := c.dockerLifecyclePreview(t.Context(), preview)
	if err != nil || len(plan.GetTargets()) != 2 {
		t.Fatalf("preview: %v, %v", plan, err)
	}
	for _, target := range plan.GetTargets() {
		if target.GetNewName() != "new-"+target.GetName() {
			t.Fatalf("rename target: %v", target)
		}
	}
}

func TestExternalLifecycleDoesNotReportUnsentDispatchAsUnknown(t *testing.T) {
	e := dockertest.New(t)
	c := cleanupCalls(t, e.Client())
	item := e.AddContainer(docker.Container{
		Name: "/before-dispatch", Config: docker.Config{Image: "busybox:1"},
		State: docker.State{Status: "created"},
	})
	plan := lifecyclePlan(t, c, lifecycleKey(item.ID), lifecycleStart, "", false)
	dir := filepath.Join(c.cfg.Dir, "docker-cleanup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Change where the journal would be written after the initial read but
	// before the action. The Engine's inspect is the pre-dispatch boundary.
	file := filepath.Join(dir, cleanupEvidence("lifecycle", plan.GetChangeId())+".json")
	e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/"+item.ID+"/json") {
			return os.Mkdir(file, 0o700)
		}
		return nil
	})
	var receipts []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
	if err := c.runDockerLifecycle(ctx, externalHost, plan, nil); err == nil || !strings.Contains(err.Error(), "before dispatch") {
		t.Fatalf("expected a pre-dispatch failure, got %v", err)
	}
	if len(receipts) != 1 || receipts[0].GetState() != receiptFailed || e.Started("before-dispatch") != 0 {
		t.Fatalf("receipts %v, starts %d", receipts, e.Started("before-dispatch"))
	}
}

func TestExternalLifecycleDoesNotBlockDockerCleanup(t *testing.T) {
	e := dockertest.New(t)
	c := cleanupCalls(t, e.Client())
	item := e.AddContainer(docker.Container{
		Name: "/stopping", Config: docker.Config{Image: "busybox:1"},
		State: docker.State{Status: "running", Running: true},
	})
	plan := lifecyclePlan(t, c, lifecycleKey(item.ID), lifecycleStop, "", false)
	arrived, release := make(chan struct{}), make(chan struct{})
	e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/"+item.ID+"/stop") {
			close(arrived)
			<-release
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- c.runDockerLifecycle(t.Context(), externalHost, plan, nil) }()
	select {
	case <-arrived:
	case err := <-done:
		t.Fatalf("lifecycle ended without sending Stop: %v", err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("lifecycle did not reach Docker")
	}
	if c.cfg.disks.cleanupMu.TryLock() {
		c.cfg.disks.cleanupMu.Unlock()
	} else {
		t.Error("Stop held the Docker cleanup and image action lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExternalLifecycleSignalPickerValues(t *testing.T) {
	for _, valid := range []string{"SIGTERM", "SIGKILL", "SIGRTMIN+15", "1", "64"} {
		if !docker.ValidLifecycleSignal(valid) {
			t.Errorf("rejected %q", valid)
		}
	}
	for _, invalid := range []string{"SIGFOO", "SIGRTMIN+16", "0", "65", "70", "+1"} {
		if docker.ValidLifecycleSignal(invalid) {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestExternalContainerLifecycleOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	c := cleanupCalls(t, d)
	ctx := t.Context()
	const image = "busybox:1.37"
	if err := d.PullImage(ctx, image, nil); err != nil {
		t.Fatal(err)
	}
	name := "lifecycle-" + strings.ToLower(strings.TrimPrefix(dockertest.Scope(t), "env_"))
	named := name + "-data"
	if _, err := d.CreateVolume(ctx, &docker.VolumeCreate{Name: named}); err != nil {
		t.Fatal(err)
	}
	engineID, err := d.CreateContainer(ctx, name, &docker.CreateRequest{
		Config: docker.Config{
			Image: image, Cmd: []string{"sh", "-c", "trap '' TERM; while true; do sleep 1; done"},
			Volumes: map[string]struct{}{"/scratch": {}},
		},
		HostConfig: docker.HostConfig{Binds: []string{named + ":/data"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.RemoveOneOff(ctx, engineID)
		_ = d.RemoveVolume(ctx, named)
	})
	key := lifecycleKey(engineID)
	apply := func(action v1.DockerContainerAction, newName string, volumes bool) *v1.DockerMaintenance {
		t.Helper()
		plan := lifecyclePlan(t, c, key, action, newName, volumes)
		for range 2 {
			if err := c.runDockerLifecycle(ctx, externalHost, plan, &agentv1.DockerCleanupProtection{}); err != nil {
				t.Fatal(err)
			}
		}
		return plan
	}
	apply(lifecycleStart, "", false)
	apply(lifecyclePause, "", false)
	paused, err := d.InspectContainer(ctx, engineID)
	if err != nil || !paused.State.Paused {
		t.Fatalf("expected paused container: %v, %v", paused, err)
	}
	apply(lifecycleUnpause, "", false)
	apply(lifecyclePause, "", false)
	apply(lifecycleStop, "", false)
	apply(lifecycleStart, "", false)
	apply(lifecycleRestart, "", false)
	apply(lifecycleRename, name+"-renamed", false)
	item, err := d.InspectContainer(ctx, engineID)
	if err != nil || item.Name != "/"+name+"-renamed" || !item.State.Running {
		t.Fatalf("expected renamed running container: %v, %v", item, err)
	}
	apply(lifecyclePause, "", false)
	apply(lifecycleKill, "", false)
	plan := apply(lifecycleRemove, "", true)
	volumes := plan.GetLifecycle().GetTargets()[0].GetAnonymousVolumes()
	if len(volumes) != 1 {
		t.Fatalf("reviewed anonymous volumes: %v", volumes)
	}
	if _, err := d.InspectVolume(ctx, volumes[0]); !docker.IsNotFound(err) {
		t.Fatalf("anonymous volume was not removed: %v", err)
	}
	if _, err := d.InspectVolume(ctx, named); err != nil {
		t.Fatalf("named volume was removed: %v", err)
	}
}
