// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const dockerVolumeKind = v1.DockerObjectKind_DOCKER_OBJECT_KIND_VOLUME

func (c *calls) projectedVolume(v *docker.Volume, at *timestamppb.Timestamp) *v1.DockerObject {
	created, _ := time.Parse(time.RFC3339Nano, v.CreatedAt)
	_, anonymous := v.Labels["com.docker.volume.anonymous"]
	facts := &v1.DockerVolume{
		Driver: bounded(v.Driver, 80), CreatedAt: inventoryTime(created), Anonymous: anonymous,
		LabelKeys:  storageKeys(v.Labels, docker.LabelProject, docker.LabelVolume, "com.docker.volume.anonymous"),
		OptionKeys: storageKeys(v.Options, "type", "device", "o", "size"),
	}
	if filepath.IsAbs(v.Mountpoint) && filepath.Clean(v.Mountpoint) == v.Mountpoint && len(v.Mountpoint) <= 4096 && !strings.ContainsAny(v.Mountpoint, "\x00\r\n") {
		facts.Mountpoint = v.Mountpoint
	}
	if c.cfg.DockerVolumeKept != nil {
		if kept, err := c.cfg.DockerVolumeKept(v.Name); kept || err != nil {
			facts.Protection = v1.DockerStorageProtection_DOCKER_STORAGE_PROTECTION_RETAINED_VOLUME
		}
	}
	row := &v1.DockerObject{
		Key: &v1.DockerObjectKey{Kind: dockerVolumeKind, EngineId: docker.VolumeInventoryKey(v)}, Name: v.Name,
		ObservedAt: at, Detail: &v1.DockerObject_Volume{Volume: facts},
	}
	if facts.Protection != 0 {
		row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM
	} else if storageLabelMatches(v.Name, v.Labels, docker.LabelVolume) {
		row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED
	}
	return row
}

func (d *diskUse) volumeMeasurement() (time.Time, []docker.DiskUsageVolume) {
	if d == nil {
		return time.Time{}, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastDF == nil || d.now().Sub(d.at) >= diskUseFor {
		return time.Time{}, nil
	}
	return d.at, d.lastDF.Volumes
}

func (c *calls) volumeSizes(rows []*v1.DockerObject) {
	at, measurements := c.cfg.disks.volumeMeasurement()
	if len(measurements) == 0 {
		return
	}
	byName := make(map[string]*docker.DiskUsageVolume, len(measurements))
	for i := range measurements {
		v := &measurements[i]
		byName[v.Name] = v
	}
	for _, row := range rows {
		facts := row.GetVolume()
		if facts.GetDriver() != "local" || facts.GetCreatedAt() == nil || at.Before(facts.GetCreatedAt().AsTime()) {
			continue
		}
		measured := byName[row.GetName()]
		if measured == nil || measured.UsageData == nil || measured.Mountpoint != facts.GetMountpoint() {
			continue
		}
		if measured.CreatedAt != "" {
			created, err := time.Parse(time.RFC3339Nano, measured.CreatedAt)
			if err != nil || !created.Equal(facts.GetCreatedAt().AsTime()) {
				continue
			}
		}
		facts.SizeBytes = measuredBytes(measured.UsageData.Size)
		if facts.SizeBytes != nil {
			facts.SizeObservedAt = inventoryTime(at)
		}
	}
}

func (c *calls) volumeAttachments(ctx context.Context, rows []*v1.DockerObject, detail bool) {
	byName := make(map[string]*v1.DockerVolume, len(rows))
	for _, row := range rows {
		facts := row.GetVolume()
		count := uint32(0)
		facts.AttachmentCount = &count
		byName[row.GetName()] = facts
	}
	partial := c.eachStorageContainer(ctx, func(ctr *docker.ContainerSummary) {
		seen := make(map[string]bool)
		for _, mount := range ctr.Mounts {
			facts := byName[mount.Name]
			if mount.Type != "volume" || facts == nil {
				continue
			}
			if !seen[mount.Name] {
				*facts.AttachmentCount = facts.GetAttachmentCount() + 1
				seen[mount.Name] = true
			}
			if detail {
				if len(facts.Attachments) == maxStorageAttachments {
					facts.AttachmentsPartial = true
					continue
				}
				facts.Attachments = append(facts.Attachments, &v1.DockerVolumeAttachment{
					ContainerId: ctr.ID, Name: containerSummaryName(ctr), MountTarget: bounded(mount.Destination, 512),
					ReadOnly: !mount.RW, State: containerState(ctr.State),
				})
			}
		}
	})
	for _, facts := range byName {
		facts.AttachmentsPartial = facts.AttachmentsPartial || partial
		if partial && facts.GetAttachmentCount() == 0 {
			facts.AttachmentCount = nil
		}
		slices.SortFunc(facts.Attachments, func(a, b *v1.DockerVolumeAttachment) int {
			return strings.Compare(a.GetName()+a.GetMountTarget(), b.GetName()+b.GetMountTarget())
		})
	}
}

func (c *calls) dockerVolumeList(ctx context.Context, ask *agentv1.DockerInventoryListCall) (*agentv1.DockerInventoryListResult, error) {
	live, err := c.cfg.Docker.InventoryVolumes(ctx, maxStorageRows)
	if err != nil {
		return nil, err
	}
	at := timestamppb.Now()
	rows := make([]*v1.DockerObject, 0, len(live))
	for i := range live {
		v := &live[i]
		if storageMatches(ask.GetFilter(), v.Name, v.Driver) {
			if _, ok := docker.VolumeInventoryName(docker.VolumeInventoryKey(v)); ok {
				rows = append(rows, c.projectedVolume(v, at))
			}
		}
	}
	out := storageRows(ask, rows)
	if !out.Resync && len(out.Objects) > 0 {
		c.volumeAttachments(ctx, out.Objects, false)
		c.volumeSizes(out.Objects)
	}
	return out, nil
}

func (c *calls) dockerVolumeDetail(ctx context.Context, key string) (*agentv1.DockerInventoryDetailResult, error) {
	name, ok := docker.VolumeInventoryName(key)
	if !ok {
		return nil, errDockerInventory
	}
	out := &agentv1.DockerInventoryDetailResult{}
	v, err := c.cfg.Docker.InspectVolume(ctx, name)
	if docker.IsNotFound(err) || err == nil && v.Name != name {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	row := c.projectedVolume(v, timestamppb.Now())
	if row.Key.EngineId != key {
		out.ReplacementKey = row.Key
		return out, nil
	}
	out.Object = row
	c.volumeAttachments(ctx, []*v1.DockerObject{row}, true)
	c.volumeSizes([]*v1.DockerObject{row})
	return out, nil
}

func (c *calls) volumeLinks(ctx context.Context, ctr *v1.DockerContainer) {
	if !slices.ContainsFunc(ctr.GetMounts(), func(m *v1.ContainerMount) bool { return m.GetVolume() != "" }) {
		return
	}
	all, err := c.cfg.Docker.InventoryVolumes(ctx, maxStorageRows)
	if err != nil {
		return
	}
	byName := make(map[string]string, len(all))
	for i := range all {
		key := docker.VolumeInventoryKey(&all[i])
		if _, ok := docker.VolumeInventoryName(key); ok {
			byName[all[i].Name] = key
		}
	}
	for _, m := range ctr.GetMounts() {
		if key := byName[m.GetVolume()]; key != "" {
			ctr.VolumeMounts = append(ctr.VolumeMounts, &v1.DockerVolumeMountRef{
				MountTarget: m.GetTarget(), VolumeKey: &v1.DockerObjectKey{Kind: dockerVolumeKind, EngineId: key},
			})
		}
	}
}
