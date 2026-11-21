// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const dockerNetworkKind = v1.DockerObjectKind_DOCKER_OBJECT_KIND_NETWORK

const (
	maxStorageRows        = 1200
	maxStorageAttachments = 64
)

func storageRows(ask *agentv1.DockerInventoryListCall, rows []*v1.DockerObject) *agentv1.DockerInventoryListResult {
	slices.SortFunc(rows, func(a, b *v1.DockerObject) int {
		return strings.Compare(a.GetKey().GetEngineId(), b.GetKey().GetEngineId())
	})
	hash := sha256.New()
	for _, row := range rows {
		fmt.Fprintln(hash, row.GetKey().GetEngineId(), row.GetName())
	}
	out := &agentv1.DockerInventoryListResult{Snapshot: hex.EncodeToString(hash.Sum(nil))}
	if ask.GetSnapshot() != "" && out.Snapshot != ask.GetSnapshot() {
		out.Resync = true
		return out
	}
	page := int(min(ask.GetPageSize(), 24))
	if page == 0 {
		page = 24
	}
	for _, row := range rows {
		if row.GetKey().GetEngineId() <= ask.GetAfterId() {
			continue
		}
		if len(out.Objects) == page {
			out.NextId = out.Objects[page-1].GetKey().GetEngineId()
			break
		}
		out.Objects = append(out.Objects, row)
	}
	return out
}

func storageMatches(filter *v1.DockerObjectFilter, name, driver string) bool {
	return (filter.GetDriver() == "" || filter.GetDriver() == driver) &&
		strings.Contains(strings.ToLower(name), strings.ToLower(strings.TrimSpace(filter.GetSearch())))
}

func networkProtection(n *docker.Network) v1.DockerStorageProtection {
	switch n.Name {
	case "bridge", "none", "host", "ingress", "docker_gwbridge":
		return v1.DockerStorageProtection_DOCKER_STORAGE_PROTECTION_BUILT_IN
	}
	if strings.HasPrefix(n.Name, "dockwell") {
		return v1.DockerStorageProtection_DOCKER_STORAGE_PROTECTION_INFRASTRUCTURE
	}
	if projected := network.Project(n); projected.GetSubnet() != "" {
		subnet, err := netip.ParsePrefix(projected.GetSubnet())
		if err == nil && subnet.Bits() == 29 && network.LinkRange.Contains(subnet.Addr()) {
			return v1.DockerStorageProtection_DOCKER_STORAGE_PROTECTION_SHARED_LINK
		}
	}
	return v1.DockerStorageProtection_DOCKER_STORAGE_PROTECTION_UNSPECIFIED
}

// Only known names are projected. Arbitrary driver keys and their values can
// carry operator-supplied strings.
func storageKeys(values map[string]string, known ...string) []string {
	var keys []string
	for _, key := range known {
		if _, ok := values[key]; ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// The control plane also requires a matching placement before it calls the object managed.
func storageLabelMatches(name string, labels map[string]string, key string) bool {
	project, label := labels[docker.LabelProject], labels[key]
	return project != "" && label != "" && name == docker.ScopedName(project, label)
}

func projectedNetwork(n *docker.Network, at *timestamppb.Timestamp) *v1.DockerObject {
	ipv6 := n.EnableIPv6
	facts := &v1.DockerNetwork{
		Driver: bounded(n.Driver, 80), Scope: bounded(n.Scope, 32), Ipv6Enabled: &ipv6,
		Internal: n.Internal, CreatedAt: inventoryTime(n.Created), Protection: networkProtection(n),
		LabelKeys:  storageKeys(n.Labels, docker.LabelProject, docker.LabelNetwork),
		OptionKeys: storageKeys(n.Options, "parent", "com.docker.network.bridge.name", "com.docker.network.driver.mtu"),
	}
	for _, p := range n.IPAM.Config[:min(len(n.IPAM.Config), maxStorageAttachments)] {
		if prefix, err := netip.ParsePrefix(p.Subnet); err == nil {
			facts.Pools = append(facts.Pools, &spec.NetworkIPAMPool{Subnet: prefix.String(), Gateway: inventoryAddress(p.Gateway)})
		}
	}
	row := &v1.DockerObject{
		Key:  &v1.DockerObjectKey{Kind: dockerNetworkKind, EngineId: n.ID},
		Name: bounded(n.Name, 255), ObservedAt: at, Detail: &v1.DockerObject_Network{Network: facts},
	}
	if facts.Protection != 0 {
		row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM
	} else if storageLabelMatches(n.Name, n.Labels, docker.LabelNetwork) {
		row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED
	}
	return row
}

func containerSummaryName(c *docker.ContainerSummary) string {
	if len(c.Names) > 0 {
		return bounded(strings.TrimPrefix(c.Names[0], "/"), 255)
	}
	return c.ID
}

func (c *calls) eachStorageContainer(ctx context.Context, visit func(*docker.ContainerSummary)) bool {
	count := 0
	err := c.cfg.Docker.EachCleanupContainer(ctx, func(row *docker.ContainerSummary) error {
		count++
		if count > maxStorageRows {
			return errCleanupSize
		}
		if docker.IsFullID(row.ID) {
			visit(row)
		}
		return nil
	})
	return err != nil
}

func addNetworkAttachment(facts *v1.DockerNetwork, ctr *docker.ContainerSummary, ep *docker.EndpointSettings, detail bool) {
	count := facts.GetAttachmentCount() + 1
	facts.AttachmentCount = &count
	if detail && len(facts.Attachments) < maxStorageAttachments {
		facts.Attachments = append(facts.Attachments, &v1.DockerNetworkEndpoint{
			ContainerId: ctr.ID, Name: containerSummaryName(ctr), Ipv4: inventoryAddress(ep.IPAddress),
			Ipv6: inventoryAddress(ep.GlobalIPv6Address), State: containerState(ctr.State),
		})
	}
}

func (c *calls) dockerNetworkList(ctx context.Context, ask *agentv1.DockerInventoryListCall) (*agentv1.DockerInventoryListResult, error) {
	live, err := c.cfg.Docker.ListCleanupNetworks(ctx, maxStorageRows)
	if err != nil {
		return nil, err
	}
	at := timestamppb.Now()
	rows := make([]*v1.DockerObject, 0, len(live))
	byID := make(map[string]*v1.DockerNetwork, len(live))
	byName := make(map[string]*v1.DockerNetwork, len(live))
	for i := range live {
		n := &live[i]
		if docker.IsFullID(n.ID) && storageMatches(ask.GetFilter(), n.Name, n.Driver) {
			row := projectedNetwork(n, at)
			rows = append(rows, row)
			byID[n.ID] = row.GetNetwork()
			if _, duplicate := byName[n.Name]; duplicate {
				byName[n.Name] = nil
			} else {
				byName[n.Name] = row.GetNetwork()
			}
		}
	}
	out := storageRows(ask, rows)
	if out.Resync || len(out.Objects) == 0 {
		return out, nil
	}
	keep := make(map[*v1.DockerNetwork]bool, len(out.Objects))
	for _, row := range out.Objects {
		count := uint32(0)
		row.GetNetwork().AttachmentCount = &count
		keep[row.GetNetwork()] = true
	}
	partial := c.eachStorageContainer(ctx, func(ctr *docker.ContainerSummary) {
		seen := make(map[*v1.DockerNetwork]bool)
		for name, ep := range ctr.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			facts := byID[ep.NetworkID]
			if ep.NetworkID == "" {
				facts = byID[name]
				if facts == nil {
					facts = byName[name]
				}
			}
			if keep[facts] && !seen[facts] {
				addNetworkAttachment(facts, ctr, ep, false)
				seen[facts] = true
			}
		}
	})
	for facts := range keep {
		facts.AttachmentsPartial = partial
		if partial && facts.GetAttachmentCount() == 0 {
			facts.AttachmentCount = nil
		}
	}
	return out, nil
}

func (c *calls) dockerNetworkDetail(ctx context.Context, engineID string) (*agentv1.DockerInventoryDetailResult, error) {
	if !docker.IsFullID(engineID) {
		return nil, errDockerInventory
	}
	out := &agentv1.DockerInventoryDetailResult{}
	n, err := c.cfg.Docker.InspectNetwork(ctx, engineID)
	if docker.IsNotFound(err) || err == nil && n.ID != engineID {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Object = projectedNetwork(n, timestamppb.Now())
	facts := out.Object.GetNetwork()
	count := uint32(0)
	facts.AttachmentCount = &count
	seen := make(map[string]bool)
	facts.AttachmentsPartial = c.eachStorageContainer(ctx, func(ctr *docker.ContainerSummary) {
		ep := ctr.NetworkSettings.Networks[n.Name]
		if ep == nil {
			ep = ctr.NetworkSettings.Networks[n.ID]
		}
		if ep != nil && (ep.NetworkID == n.ID || ep.NetworkID == "") {
			seen[ctr.ID] = true
			addNetworkAttachment(facts, ctr, ep, true)
		}
	})
	for _, ctrID := range slices.Sorted(maps.Keys(n.Containers)) {
		if !docker.IsFullID(ctrID) || seen[ctrID] {
			continue
		}
		ep := n.Containers[ctrID]
		ip4, _, _ := strings.Cut(ep.IPv4Address, "/")
		ip6, _, _ := strings.Cut(ep.IPv6Address, "/")
		addNetworkAttachment(facts, &docker.ContainerSummary{ID: ctrID, Names: []string{ep.Name}},
			&docker.EndpointSettings{IPAddress: ip4, GlobalIPv6Address: ip6}, true)
	}
	slices.SortFunc(facts.Attachments, func(a, b *v1.DockerNetworkEndpoint) int {
		return strings.Compare(a.GetName(), b.GetName())
	})
	facts.AttachmentsPartial = facts.AttachmentsPartial || facts.GetAttachmentCount() > uint32(len(facts.Attachments)) //nolint:gosec // bounded above
	return out, nil
}

func (c *calls) checkDockerNetwork(ctx context.Context, ask *agentv1.CheckDockerNetworkCall) (*agentv1.CheckDockerNetworkResult, error) {
	if c.cfg.Docker == nil || composeIdentity(ask.GetEngineName()) == "" || ask.GetSpec() == nil {
		return nil, errDockerInventory
	}
	n := ask.GetSpec()
	out := &agentv1.CheckDockerNetworkResult{}
	if n.GetDriver() != "" && n.GetDriver() != "bridge" {
		out.Findings = append(out.Findings, &v1.DockerNetworkFinding{Code: v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_DRIVER_UNSUPPORTED, FieldPath: "driver"})
		return out, nil
	}
	var bad *network.IPAMError
	if err := network.CheckIPAM(n); errors.As(err, &bad) {
		out.Findings = append(out.Findings, &v1.DockerNetworkFinding{Code: bad.Code, FieldPath: bad.Path})
		return out, nil
	}
	live, err := c.cfg.Docker.ListCleanupNetworks(ctx, maxStorageRows)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(live, func(n docker.Network) bool { return n.Name == ask.GetEngineName() }) {
		out.Findings = append(out.Findings, &v1.DockerNetworkFinding{Code: v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_NAME_IN_USE, FieldPath: "name"})
	}
	if conflict := network.PoolConflict(n, live); conflict != nil {
		conflict.ConflictingNetwork = bounded(conflict.GetConflictingNetwork(), 255)
		out.Findings = append(out.Findings, conflict)
	} else {
		conflict, err := network.HostPoolConflict(ctx, n)
		if err != nil {
			return nil, err
		}
		if conflict != nil {
			out.Findings = append(out.Findings, conflict)
		}
	}
	return out, nil
}

func (c *calls) checkDockerAttachmentApply(ctx context.Context, step resource.Step, guard *v1.DockerNetworkAttachmentGuard) error {
	if !step.Finishing {
		return c.verifyDockerAttachment(ctx, guard)
	}
	if c.cfg.Docker == nil || !docker.IsFullID(guard.GetNetworkId()) {
		return errDockerInventory
	}
	t := container.HostTarget(step.Ref)
	if step.Ref.Kind == service.Kind {
		t = service.TargetIn(ctx, step.Ref)
	}
	return (&container.Docker{Client: c.cfg.Docker}).VerifyFinishedAttachment(ctx, t, guard)
}

func (c *calls) verifyDockerAttachment(ctx context.Context, guard *v1.DockerNetworkAttachmentGuard) error {
	if c.cfg.Docker == nil || !docker.IsFullID(guard.GetNetworkId()) || !docker.IsFullID(guard.GetContainerId()) {
		return errDockerInventory
	}
	n, err := c.cfg.Docker.InspectNetwork(ctx, guard.GetNetworkId())
	if err != nil || n.ID != guard.GetNetworkId() || n.Name != guard.GetNetworkName() ||
		n.Driver != "bridge" || n.Scope != "local" || networkProtection(n) != 0 {
		return errors.New("the reviewed network changed. Review the attachment again")
	}
	ctr, err := c.cfg.Docker.InspectContainer(ctx, guard.GetContainerId())
	if err != nil || ctr.ID != guard.GetContainerId() {
		return errors.New("the reviewed container changed. Review the attachment again")
	}
	ep := ctr.NetworkEndpoint(n.Name, n.ID)
	switch guard.GetAction() {
	case v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT:
		if ep == nil && guard.GetExpectedEndpointId() == "" {
			return nil
		}
	case v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_DISCONNECT:
		if ep != nil && (ep.NetworkID == n.ID || ep.NetworkID == "") &&
			n.ID == guard.GetExpectedEndpointId() {
			return nil
		}
	default:
	}
	return errors.New("the reviewed network attachment changed. Review the attachment again")
}
