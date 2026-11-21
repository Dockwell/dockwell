// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"net/netip"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
)

func TestDockerNetworkChecksTheControlConnectionBeforeCreate(t *testing.T) {
	engine := dockertest.New(t)
	client := engine.Client()
	t.Cleanup(client.Close)
	c := &calls{cfg: Config{Docker: client}}
	ctx := network.WithControlPeer(t.Context(), netip.MustParseAddr("192.0.2.37"))
	ref := resource.Ref{Kind: network.Kind, Scope: dockertest.Scope(t), Name: "fixture"}
	name := network.TargetOf(ref).Name
	wanted := &spec.Network{Ipam: &spec.NetworkIPAM{Pools: []*spec.NetworkIPAMPool{{Subnet: "192.0.2.0/24"}}}}
	out, err := c.checkDockerNetwork(ctx, &agentv1.CheckDockerNetworkCall{EngineName: name, Spec: wanted})
	if err != nil || len(out.GetFindings()) != 1 || out.GetFindings()[0].GetConflictingNetwork() != "the control connection" ||
		out.GetFindings()[0].GetConflictingSubnet() != "192.0.2.37/32" {
		t.Fatalf("check = %v, %v", out, err)
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}, DockerStorageGuard: storageCreateGuard(name)}
	driver := &network.Driver{Client: client}
	if err := driver.Apply(ctx, step, wanted); err == nil || !strings.Contains(err.Error(), "the control connection") {
		t.Fatalf("apply = %v, want the control connection named", err)
	}
	if _, err := client.NetworkNamed(ctx, name); !docker.IsNotFound(err) {
		t.Fatalf("the conflicting network was created: %v", err)
	}
}

func TestDockerAttachmentRecoveryChecksTheApprovedOutcome(t *testing.T) {
	engine := dockertest.New(t)
	client := engine.Client()
	t.Cleanup(client.Close)
	c := &calls{cfg: Config{Docker: client}}
	engine.AddNetwork(docker.Network{Name: "fixture", Driver: "bridge", Scope: "local"})
	id := engine.Network("fixture").ID
	ref := resource.Ref{Kind: container.Kind, Scope: dockertest.Scope(t), Name: "workload"}
	deploy := resource.Deploy{Change: "chg_fixture", Revision: 3}
	held := engine.AddContainer(docker.Container{Name: "/workload", Config: docker.Config{Labels: map[string]string{
		container.LabelChange: deploy.Change, container.LabelRevision: "3",
	}}, NetworkSettings: docker.ContainerNetworkSettings{Networks: map[string]*docker.EndpointSettings{
		"fixture": {NetworkID: id, IPAddress: "192.0.2.12"},
	}}})
	guard := &v1.DockerNetworkAttachmentGuard{
		NetworkId: id, NetworkName: "fixture", ContainerId: strings.Repeat("a", 64),
		Action: v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT,
	}
	step := resource.Step{Ref: ref, Finishing: true}
	ctx := resource.WithDeploy(t.Context(), deploy)
	if err := c.checkDockerAttachmentApply(ctx, step, guard); err != nil {
		t.Fatal(err)
	}
	for _, other := range []resource.Deploy{{Change: "another_change", Revision: 3}, {Change: deploy.Change, Revision: 4}} {
		if err := c.checkDockerAttachmentApply(resource.WithDeploy(ctx, other), step, guard); err == nil {
			t.Fatal("another deploy's replacement was accepted")
		}
	}
	if err := client.DisconnectNetwork(ctx, id, held.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.checkDockerAttachmentApply(ctx, step, guard); err == nil {
		t.Fatal("the replacement lacks the approved attachment")
	}
	guard.Action = v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_DISCONNECT
	guard.ExpectedEndpointId = id
	if err := c.checkDockerAttachmentApply(ctx, step, guard); err != nil {
		t.Fatal(err)
	}
}

func TestDockerGuardedNetworkCreateResumesAllMembers(t *testing.T) {
	engine := dockertest.New(t)
	client := engine.Client()
	t.Cleanup(client.Close)
	ref := resource.Ref{Kind: network.Kind, Scope: dockertest.Scope(t), Name: "fixture"}
	target := network.TargetOf(ref)
	memberScope := dockertest.Scope(t)
	labels := map[string]string{docker.LabelProject: docker.Project(memberScope), docker.LabelService: "web"}
	first := engine.AddContainer(docker.Container{Name: "fixture-first", Config: docker.Config{Labels: labels}})
	second := engine.AddContainer(docker.Container{Name: "fixture-second", Config: docker.Config{Labels: labels}})
	guard := storageCreateGuard(target.Name)
	marked := network.Labels(target, &spec.Network{Driver: "bridge"})
	marked[docker.LabelStorageCreation] = guard.GetCreate().GetCreationToken()
	id, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: target.Name, Driver: "bridge", Labels: marked})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ConnectNetwork(t.Context(), id, first.ID, []string{"fixture-alias"}); err != nil {
		t.Fatal(err)
	}
	n := &spec.Network{Driver: "bridge", Members: []*spec.NetworkMember{{Environment: memberScope, Service: "web", Alias: "fixture-alias"}}}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}, DockerStorageGuard: guard}
	driver := &network.Driver{Client: client}
	for range 2 {
		if err := driver.Apply(t.Context(), step, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, ctr := range []string{first.ID, second.ID} {
		held, err := client.InspectContainer(t.Context(), ctr)
		if err != nil || held.NetworkEndpoint(target.Name, id) == nil {
			t.Fatalf("member %s was not joined behind the existing member: %v", ctr, err)
		}
	}
}
