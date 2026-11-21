// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const dockerImageKind = v1.DockerObjectKind_DOCKER_OBJECT_KIND_IMAGE

// imageAliases omits the placeholders some Engine versions return for dangling
// images. Tags and digests are bounded separately from the size of the page.
func imageAliases(refs []string) []string {
	var out []string
	for _, ref := range refs {
		if len(out) == 24 {
			break
		}
		if ref == "" || strings.Contains(ref, "<none>") || len(ref) > 512 {
			continue
		}
		out = append(out, bounded(ref, 512))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func imageRow(measurement *v1.ServerDiskUse, row docker.DiskUsageImage) *v1.DockerObject {
	tags, digests := imageAliases(row.RepoTags), imageAliases(row.RepoDigests)
	name := row.ID
	if len(tags) > 0 {
		name = tags[0]
	}
	var state v1.DockerImageState
	switch {
	case row.Containers > 0:
		state = v1.DockerImageState_DOCKER_IMAGE_STATE_USED
	case len(imageTags(row.RepoTags))+len(imageTags(row.RepoDigests)) == 0:
		state = v1.DockerImageState_DOCKER_IMAGE_STATE_DANGLING
	default:
		state = v1.DockerImageState_DOCKER_IMAGE_STATE_UNUSED
	}
	image := &v1.DockerImage{
		Tags: tags, Digests: digests, TotalBytes: measuredBytes(row.Size), UniqueBytes: imageUnique(row),
		CreatedAt: imageDate(row.Created), State: state,
		DetailsTruncated: len(row.RepoTags) > 24 || len(row.RepoDigests) > 24,
	}
	return &v1.DockerObject{
		Key: &v1.DockerObjectKey{Kind: dockerImageKind, EngineId: row.ID}, Name: name,
		ObservedAt: measurement.GetTime(), Detail: &v1.DockerObject_Image{Image: image},
	}
}

func imagePlatform(target *v1.DockerImage, image *docker.Image) {
	if image.OS != "" && image.Architecture != "" {
		target.Platform = bounded(strings.TrimSuffix(image.OS+"/"+image.Architecture+"/"+image.Variant, "/"), 80)
	}
	if !image.Created.IsZero() {
		target.CreatedAt = inventoryTime(image.Created)
	}
}

func (c *calls) dockerImageList(ctx context.Context, ask *agentv1.DockerInventoryListCall) (*agentv1.DockerInventoryListResult, error) {
	use, df, err := c.cfg.disks.snapshot(ctx, false)
	if err != nil {
		return nil, errDockerInventory
	}
	search := strings.ToLower(ask.GetFilter().GetSearch())
	ids := make([]int, 0, len(df.Images))
	for i, row := range df.Images {
		if !docker.IsImageID(row.ID) {
			continue
		}
		if search == "" || strings.Contains(strings.ToLower(row.ID), search) ||
			slices.ContainsFunc(row.RepoTags, func(ref string) bool { return strings.Contains(strings.ToLower(ref), search) }) ||
			slices.ContainsFunc(row.RepoDigests, func(ref string) bool { return strings.Contains(strings.ToLower(ref), search) }) {
			ids = append(ids, i)
		}
	}
	slices.SortFunc(ids, func(a, b int) int { return strings.Compare(df.Images[a].ID, df.Images[b].ID) })
	hash := sha256.New()
	for _, i := range ids {
		row := &df.Images[i]
		fmt.Fprintln(hash, row.ID, row.RepoTags, row.RepoDigests, row.Containers)
	}
	out := &agentv1.DockerInventoryListResult{Snapshot: hex.EncodeToString(hash.Sum(nil))}
	if ask.GetSnapshot() != "" && ask.GetSnapshot() != out.Snapshot {
		out.Resync = true
		return out, nil
	}
	page := int(min(ask.GetPageSize(), 24))
	if page == 0 {
		page = 24
	}
	for _, i := range ids {
		row := df.Images[i]
		if row.ID <= ask.GetAfterId() {
			continue
		}
		if len(out.Objects) == page {
			out.NextId = out.Objects[page-1].GetKey().GetEngineId()
			break
		}
		item := imageRow(use, row)
		if image, err := c.cfg.Docker.InspectImage(ctx, row.ID); err == nil && image.ID == row.ID {
			imagePlatform(item.GetImage(), image)
		}
		out.Objects = append(out.Objects, item)
	}
	return out, nil
}

func (c *calls) dockerImageDetail(ctx context.Context, id string) (*agentv1.DockerInventoryDetailResult, error) {
	if c.cfg.Docker == nil || !docker.IsImageID(id) {
		return nil, errDockerInventory
	}
	out := &agentv1.DockerInventoryDetailResult{}
	image, err := c.cfg.Docker.InspectImage(ctx, id)
	if docker.IsNotFound(err) || err == nil && image.ID != id {
		return out, nil
	}
	if err != nil {
		return nil, errDockerInventory
	}
	use, df, err := c.cfg.disks.snapshot(ctx, false)
	if err != nil {
		return nil, errDockerInventory
	}
	row := docker.DiskUsageImage{ID: id, Size: image.Size, SharedSize: -1, Containers: 0, RepoTags: image.RepoTags, RepoDigests: image.RepoDigests}
	for _, measured := range df.Images {
		if measured.ID == id {
			row.SharedSize = measured.SharedSize
			if image.RepoTags == nil {
				row.RepoTags = measured.RepoTags
			}
			break
		}
	}
	var containers []*v1.DockerImageContainer
	if err := c.cfg.Docker.EachContainer(ctx, nil, func(container *docker.ContainerSummary) error {
		if container.ImageID == id {
			row.Containers++
			if len(containers) < 64 {
				name := container.ID
				if len(container.Names) > 0 {
					name = strings.TrimPrefix(container.Names[0], "/")
				}
				containers = append(containers, &v1.DockerImageContainer{Key: inventoryKey(container.ID), Name: bounded(name, 255)})
			}
		}
		return nil
	}); err != nil {
		return nil, errDockerInventory
	}
	out.Object = imageRow(use, row)
	detail := out.Object.GetImage()
	detail.Containers = containers
	imagePlatform(detail, image)
	for _, layer := range image.RootFS.Layers[:min(256, len(image.RootFS.Layers))] {
		if docker.IsImageID(layer) {
			detail.Layers = append(detail.Layers, &v1.DockerImageLayer{DiffId: layer})
		}
	}
	history, err := c.cfg.Docker.ImageHistory(ctx, id)
	detail.HistoryUnavailable = err != nil
	if err == nil {
		// Engine history is newest first and includes metadata-only entries.
		// Report sizes only when its nonempty layers line up with every DiffID.
		var layerSizes []*uint64
		for _, entry := range history[:min(256, len(history))] {
			detail.History = append(detail.History, &v1.DockerImageHistory{
				CreatedAt: imageDate(entry.Created), SizeBytes: measuredBytes(entry.Size),
			})
			if entry.Size > 0 {
				layerSizes = append(layerSizes, measuredBytes(entry.Size))
			}
		}
		if len(history) <= 256 && len(layerSizes) == len(detail.Layers) && len(detail.Layers) == len(image.RootFS.Layers) {
			for i, layer := range detail.Layers {
				layer.SizeBytes = layerSizes[len(layerSizes)-1-i]
			}
		}
	}
	detail.DetailsTruncated = detail.DetailsTruncated || len(image.RootFS.Layers) > 256 || len(history) > 256 || row.Containers > 64
	return out, nil
}

func imageDate(unix int64) *timestamppb.Timestamp {
	if unix <= 0 {
		return nil
	}
	return inventoryTime(time.Unix(unix, 0))
}
