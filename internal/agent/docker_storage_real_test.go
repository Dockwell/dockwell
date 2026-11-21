// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

func TestDockerNetworkBridgeIPAMOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	disabled, err := os.ReadFile("/proc/sys/net/ipv6/conf/all/disable_ipv6")
	if errors.Is(err, os.ErrNotExist) || err == nil && strings.TrimSpace(string(disabled)) == "1" {
		t.Skip("IPv6 is disabled on the Docker runner")
	}
	c := cleanupCalls(t, client)
	ref := resource.Ref{Kind: network.Kind, Scope: dockertest.Scope(t), Name: "addresses"}
	name := network.TargetOf(ref).Name
	enabled := true
	wanted := &spec.Network{Driver: "bridge", Ipam: &spec.NetworkIPAM{
		EnableIpv6: &enabled,
		Pools: []*spec.NetworkIPAMPool{
			{Subnet: "10.245.232.0/24", Gateway: "10.245.232.10"},
			{Subnet: "fd79:4cf7:fab3::/64", Gateway: "fd79:4cf7:fab3::10"},
		},
	}}
	check := func(engineName string) *agentv1.CheckDockerNetworkResult {
		t.Helper()
		out, err := c.checkDockerNetwork(t.Context(), &agentv1.CheckDockerNetworkCall{EngineName: engineName, Spec: wanted})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := check(name); len(out.GetFindings()) != 0 {
		t.Fatalf("unexpected conflict: %v", out)
	}
	driver := &network.Driver{Client: client}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}, DockerStorageGuard: storageCreateGuard(name)}
	if err := driver.Apply(t.Context(), step, wanted); err != nil {
		t.Fatal(err)
	}
	n, err := client.NetworkNamed(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	cleanRealNetwork(t, client, n.ID)
	if err := driver.Apply(t.Context(), step, wanted); err != nil {
		t.Fatalf("retry of the same creation: %v", err)
	}
	row, err := c.dockerNetworkDetail(t.Context(), n.ID)
	if facts := row.GetObject().GetNetwork(); err != nil || !facts.GetIpv6Enabled() || len(facts.GetPools()) != 2 ||
		!slices.ContainsFunc(facts.GetPools(), func(p *spec.NetworkIPAMPool) bool { return p.GetGateway() == "10.245.232.10" }) {
		t.Fatalf("observed IPAM: %v, %v", row, err)
	}
	if out := check(name + "_other"); !slices.ContainsFunc(out.GetFindings(), func(f *v1.DockerNetworkFinding) bool {
		return f.GetCode() == v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_SUBNET_OVERLAP
	}) {
		t.Fatalf("overlap not found: %v", out)
	}
}

func storageCreateGuard(name string) *resource.DockerStorageGuard {
	return &resource.DockerStorageGuard{Create: &v1.DockerStorageCreateGuard{
		EngineName: name, CreationToken: strings.Repeat("a", 64),
	}}
}

func cleanRealNetwork(t *testing.T, client *docker.Client, engineID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveNetwork(ctx, engineID); err != nil && !docker.IsNotFound(err) {
			t.Errorf("remove test network: %v", err)
		}
	})
}

func makeStorageTestNetwork(t *testing.T, client *docker.Client, name string) string {
	t.Helper()
	engineID, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: name, Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	cleanRealNetwork(t, client, engineID)
	return engineID
}

func TestDockerNetworkAttachmentRejectsReplacementOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	c := cleanupCalls(t, client)
	name := strings.ToLower(dockertest.Scope(t))
	old := makeStorageTestNetwork(t, client, name)
	if err := client.RemoveNetwork(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	replacement := makeStorageTestNetwork(t, client, name)
	g := &v1.DockerNetworkAttachmentGuard{
		NetworkName: name, NetworkId: old,
		ContainerId: strings.Repeat("a", 64), Action: v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT,
	}
	if err := c.verifyDockerAttachment(t.Context(), g); err == nil || !strings.Contains(err.Error(), "reviewed network changed") {
		t.Fatalf("stale network was accepted: %v", err)
	}
	n, err := client.NetworkNamed(t.Context(), name)
	if err != nil || n.ID != replacement || len(n.Containers) != 0 {
		t.Fatalf("replacement was changed: %v, %v", n, err)
	}
}

func TestDockerNetworkCleanupProtectsDependantsOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	c := cleanupCalls(t, client)
	name := strings.ToLower(dockertest.Scope(t))
	desired := makeStorageTestNetwork(t, client, name+"-desired")
	disposable := makeStorageTestNetwork(t, client, name+"-disposable")
	answer, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Networks: true}, ProtectedNetworks: []string{name + "-desired"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var candidate *v1.DockerCleanupTarget
	for _, row := range answer.GetTargets() {
		if row.GetEngineId() == desired && row.GetReason() != reasonDesired {
			t.Fatalf("desired network was offered: %v", row)
		}
		if row.GetEngineId() == disposable && row.GetEvidence() != "" {
			candidate = row
		}
	}
	if candidate == nil {
		t.Fatal("the detached external network was not offered")
	}
	candidate.ServerId = "srv_cleanup"
	receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{candidate}, &agentv1.DockerCleanupProtection{
		Networks: []string{name + "-desired"},
	})
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved {
		t.Fatalf("exact network cleanup: %v, %v", receipts, err)
	}
	if _, err := client.InspectNetwork(t.Context(), desired); err != nil {
		t.Fatalf("desired network disappeared: %v", err)
	}
}

func TestDockerVolumeGuardedCreateAndInventoryOnRealDocker(t *testing.T) {
	testDockerVolumeGuardedCreate(t, dockertest.Real(t))
}

func TestDockerVolumeGuardedCreateAndInventory(t *testing.T) {
	engine := dockertest.New(t)
	engine.Mountpoints = true
	client := engine.Client()
	t.Cleanup(client.Close)
	testDockerVolumeGuardedCreate(t, client)
}

func testDockerVolumeGuardedCreate(t *testing.T, client *docker.Client) {
	t.Helper()
	ref := resource.Ref{Kind: volume.Kind, Scope: dockertest.Scope(t), Name: "storage"}
	name := volume.TargetOf(ref).Name
	runtime := &volume.Docker{Client: client, Kept: &volume.Kept{Path: filepath.Join(t.TempDir(), "kept.json")}}
	driver := &volume.Driver{Runtime: runtime}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}, DockerStorageGuard: storageCreateGuard(name)}
	wanted := &spec.Volume{Driver: "local"}
	foreign, err := client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: name, Driver: "local"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := client.RemoveVolume(ctx, name); err != nil && !docker.IsNotFound(err) {
			t.Error(err)
		}
	})
	if err := driver.Apply(t.Context(), step, wanted); err == nil {
		t.Fatal("the create claimed a volume with the same name")
	}
	if err := client.RemoveVolume(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	if err := driver.Apply(t.Context(), step, wanted); err != nil {
		t.Fatal(err)
	}
	if err := driver.Apply(t.Context(), step, wanted); err != nil {
		t.Fatalf("retry of the same create: %v", err)
	}
	c := cleanupCalls(t, client)
	old, err := c.dockerVolumeDetail(t.Context(), docker.VolumeInventoryKey(foreign))
	if err != nil || old.GetObject() != nil || old.GetReplacementKey() == nil {
		t.Fatalf("stale key: %v, %v", old, err)
	}
	current, err := c.dockerVolumeDetail(t.Context(), old.GetReplacementKey().GetEngineId())
	if got := current.GetObject().GetVolume(); err != nil || got.GetDriver() != "local" || got.GetMountpoint() == "" {
		t.Fatalf("live volume: %v, %v", current, err)
	}
}

func TestDockerNetworkAttachmentRolloutOnRealDocker(t *testing.T) {
	testDockerNetworkAttachmentRollout(t, dockertest.Real(t))
}

func TestDockerNetworkAttachmentRollout(t *testing.T) {
	engine := dockertest.New(t)
	engine.AddLocalImage("busybox:1.37", dockertest.Image{})
	client := engine.Client()
	t.Cleanup(client.Close)
	testDockerNetworkAttachmentRollout(t, client)
}

func testDockerNetworkAttachmentRollout(t *testing.T, client *docker.Client) {
	t.Helper()
	scope := dockertest.Scope(t)
	ref := resource.Ref{Kind: service.Kind, Scope: scope, Name: "workload"}
	project := docker.Project(scope)
	baseName := docker.ScopedName(project, "default")
	addedName := "external-" + strings.ToLower(scope)
	makeStorageTestNetwork(t, client, baseName)
	added := makeStorageTestNetwork(t, client, addedName)
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		workloads, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + project}})
		if err != nil {
			t.Error(err)
		}
		for i := range workloads {
			if err := client.RemoveContainer(ctx, workloads[i].ID, true); err != nil && !docker.IsNotFound(err) {
				t.Error(err)
			}
		}
	})
	runtime := &container.Docker{Client: client, RunStateDir: t.TempDir(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: runtime}})}
	app := &spec.Container{Image: "busybox:1.37", Command: []string{"sleep", "600"}}
	wanted := &spec.Service{Runs: &spec.Service_App{App: app}}
	apply := func(ctx context.Context) {
		t.Helper()
		steps, err := local.Plan(ctx, []resource.Desired{{Ref: ref, Spec: wanted}})
		if err != nil {
			t.Fatal(err)
		}
		if err := local.Apply(ctx, steps[0], wanted); err != nil {
			t.Fatal(err)
		}
	}
	inspect := func() *docker.Container {
		t.Helper()
		running, err := client.ListContainers(t.Context(), map[string][]string{
			"label": {docker.LabelProject + "=" + project, docker.LabelService + "=" + ref.Name},
		})
		if err != nil || len(running) != 1 || running[0].State != "running" {
			t.Fatalf("expected one running workload: %v, %v", running, err)
		}
		ctr, err := client.InspectContainer(t.Context(), running[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return ctr
	}
	apply(t.Context())
	c := cleanupCalls(t, client)
	guard := &v1.DockerNetworkAttachmentGuard{
		NetworkName: addedName, NetworkId: added, ContainerId: inspect().ID,
		Action: v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT,
	}
	if err := c.verifyDockerAttachment(t.Context(), guard); err != nil {
		t.Fatal(err)
	}
	app.ExternalNetworks = []string{addedName}
	app.Networks = []string{"default"}
	apply(container.WithDockerAttachment(t.Context(), guard))
	joined := inspect()
	if joined.HostConfig.NetworkMode != baseName {
		t.Fatalf("the first network changed to %q while connecting the second", joined.HostConfig.NetworkMode)
	}
	ep := joined.NetworkSettings.Networks[addedName]
	if ep == nil || ep.NetworkID != added {
		t.Fatalf("workload joined %v, want %s", ep, added)
	}
	guard.ContainerId = joined.ID
	guard.Action = v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_DISCONNECT
	guard.ExpectedEndpointId = added
	if err := c.verifyDockerAttachment(t.Context(), guard); err != nil {
		t.Fatal(err)
	}
	app.ExternalNetworks = nil
	apply(container.WithDockerAttachment(t.Context(), guard))
	left := inspect()
	if left.NetworkSettings.Networks[addedName] != nil || left.NetworkSettings.Networks[baseName] == nil {
		t.Fatalf("wrong networks after disconnect: %v", left.NetworkSettings.Networks)
	}
}
