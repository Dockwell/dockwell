// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const (
	cleanupImage     = v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE
	cleanupCache     = v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE
	cleanupNetwork   = v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK
	cleanupContainer = v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_CONTAINER
	cleanupVolume    = v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME
	cleanupLog       = v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_LOG

	reasonInUse       = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_IN_USE
	reasonRollback    = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ROLLBACK
	reasonRetained    = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_RETAINED
	reasonUnavailable = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_EXACT_REMOVAL_UNAVAILABLE
	reasonShared      = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_SHARED
)

func measuredBytes(n int64) *uint64 {
	if n < 0 {
		return nil
	}
	size := uint64(n)
	return &size
}

func imageUnique(image docker.DiskUsageImage) *uint64 {
	if image.SharedSize < 0 || image.SharedSize > image.Size {
		return nil
	}
	return measuredBytes(image.Size - image.SharedSize)
}

func (d *diskUse) dockerBreakdown(out *v1.ServerDiskUse, df *docker.DiskUsage) {
	var otherMounts uint64
	volumePartial := false
	for _, volume := range df.Volumes {
		if volume.UsageData == nil || volume.UsageData.Size < 0 {
			volumePartial = true
			continue
		}
		if filepath.IsAbs(volume.Mountpoint) {
			mount, _, _, known := d.collector.DiskOf(volume.Mountpoint)
			if known && out.MountPoint != "" && mount != out.MountPoint {
				otherMounts += positive(volume.UsageData.Size)
			}
		}
	}
	var containerPartial bool
	var cacheReclaim uint64
	for _, container := range df.Containers {
		containerPartial = containerPartial || container.SizeRw < 0
	}
	for _, record := range df.BuildCache {
		if cacheCleanupReason(record, d.docker.ExactCacheRemoval()) == reasonIdle {
			cacheReclaim += positive(record.Size)
		}
	}
	// Dockwell's standalone builders report no Engine df records, so build cache is partial.
	zero := uint64(0)
	out.DockerBreakdown = []*v1.DockerDiskCategory{
		{Kind: cleanupImage, UsedBytes: measuredBytes(df.LayersSize), Blockers: []v1.DockerCleanupReason{reasonInUse, reasonRollback, reasonShared}},
		{Kind: cleanupContainer, UsedBytes: &out.ContainersBytes, ReclaimableBytes: &zero, Partial: containerPartial, Blockers: []v1.DockerCleanupReason{reasonInUse, v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_TAKE_ON}},
		{Kind: cleanupVolume, UsedBytes: &out.VolumesBytes, ReclaimableBytes: &zero, Partial: volumePartial, OtherMountsBytes: &otherMounts, Blockers: []v1.DockerCleanupReason{reasonRetained}},
		{Kind: cleanupCache, UsedBytes: &out.BuildCacheBytes, ReclaimableBytes: &cacheReclaim, Partial: true, Blockers: []v1.DockerCleanupReason{reasonInUse, reasonUnavailable}},
		{Kind: cleanupLog, UsedBytes: &out.LogsBytes, ReclaimableBytes: &zero, Blockers: []v1.DockerCleanupReason{reasonRetained}},
	}
	for _, category := range out.DockerBreakdown {
		category.ObservedAt = out.GetTime()
	}
}

// dockerDiskUse computes the default image estimate against current desired
// state. Each request annotates its own copy of the cached measurement.
func (c *calls) dockerDiskUse(ctx context.Context, protection *agentv1.DockerCleanupProtection) (*v1.ServerDiskUse, error) {
	use, df, err := c.cfg.disks.cached(ctx)
	switch {
	case errors.Is(err, errDiskMeasuring):
		use = &v1.ServerDiskUse{}
	case err != nil:
		return nil, err
	default:
		use = proto.CloneOf(use)
	}
	live, err := c.cfg.Docker.VolumeInventory(ctx)
	if err != nil {
		return nil, err
	}
	use.VolumeUse = liveVolumeUse(live, df)
	use.Volumes = uint32(len(use.VolumeUse)) //nolint:gosec // bounded Docker inventory fits in uint32.
	if use.GetTime() == nil {
		return use, nil
	}
	if c.cleanupDisabled() {
		for _, row := range use.DockerBreakdown {
			if row.Kind == cleanupImage || row.Kind == cleanupCache {
				row.ReclaimableBytes = measuredBytes(0)
				row.Blockers = append(row.Blockers, v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DISABLED)
			}
		}
		return use, nil
	}
	if protection == nil {
		return use, nil
	}
	held, err := c.dockerOccupancy(ctx)
	for _, row := range use.DockerBreakdown {
		if row.Kind != cleanupImage {
			continue
		}
		if err != nil {
			row.Partial = true
			break
		}
		var reclaim uint64
		for _, image := range df.Images {
			if !docker.IsImageID(image.ID) || len(imageTags(image.RepoTags))+len(imageTags(image.RepoDigests)) > 0 {
				continue
			}
			target := c.imageCleanupTarget(image, held, protection.GetImages())
			if target.GetReason() == reasonDangling {
				reclaim += target.GetEstimatedBytes()
				row.Partial = row.Partial || target.EstimatedBytes == nil
			}
		}
		row.ReclaimableBytes = &reclaim
	}
	return use, nil
}

// liveVolumeUse takes presence and mountpoints from the Engine inventory.
// A count can add measurements to those rows, including a real zero, but
// cannot make a disappeared volume reappear.
func liveVolumeUse(live []docker.VolumeIdentity, df *docker.DiskUsage) []*v1.VolumeDiskUse {
	measured := make(map[string]docker.DiskUsageVolume)
	if df != nil {
		for _, v := range df.Volumes {
			measured[v.Name] = v
		}
	}
	out := make([]*v1.VolumeDiskUse, 0, len(live))
	for _, v := range live {
		row := &v1.VolumeDiskUse{Name: v.Name, Mountpoint: v.Mountpoint, SizeBytes: -1}
		if count := measured[v.Name]; count.UsageData != nil && count.Mountpoint == v.Mountpoint &&
			(count.CreatedAt == "" || v.CreatedAt == "" || count.CreatedAt == v.CreatedAt) {
			row.SizeBytes = count.UsageData.Size
			row.Containers = uint32(max(count.UsageData.RefCount, 0)) //nolint:gosec // container counts fit in uint32.
		}
		out = append(out, row)
	}
	return out
}
