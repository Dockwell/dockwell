// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const (
	reasonDangling        = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING
	reasonUnused          = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED
	reasonIdle            = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_IDLE
	reasonDesired         = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DESIRED
	reasonTakeOn          = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_TAKE_ON
	reasonAnonymous       = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ANONYMOUS
	reasonBackup          = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_BACKUP
	reasonCompose         = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_COMPOSE
	reasonSwarmManager    = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_SWARM_MANAGER
	reasonRecentlyCreated = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_RECENTLY_CREATED
)

const (
	maxCleanupRows      = 1200
	minVolumeCleanupAge = 60 * time.Second
)

var errCleanupSize = errors.New("there are too many Docker objects to review in one snapshot")

type dockerOccupancy struct {
	images            map[string]bool
	networks          map[string]bool
	volumes           map[string]bool
	composeProjects   map[string]bool
	hasImageMounts    bool
	unknownImageMount bool
	stopped           []*v1.DockerCleanupTarget
}

func (c *calls) cleanupNow() time.Time {
	if c.cfg.disks != nil && c.cfg.disks.now != nil {
		return c.cfg.disks.now()
	}
	return time.Now()
}

func (c *calls) dockerOccupancy(ctx context.Context) (dockerOccupancy, error) {
	held := dockerOccupancy{
		images:          map[string]bool{},
		networks:        map[string]bool{},
		volumes:         map[string]bool{},
		composeProjects: map[string]bool{},
	}
	var count int
	err := c.cfg.Docker.EachCleanupContainer(ctx, func(row *docker.ContainerSummary) error {
		count++
		if count > maxCleanupRows {
			return errCleanupSize
		}
		held.images[row.ImageID] = true
		held.images[row.Image] = true
		if project := composeIdentity(row.Labels[docker.LabelProject]); project != "" && composeIdentity(row.Labels[docker.LabelService]) != "" && !strings.EqualFold(row.Labels[docker.LabelOneoff], "true") {
			held.composeProjects[project] = true
		}
		for name, endpoint := range row.NetworkSettings.Networks {
			held.networks[name] = true
			if endpoint != nil {
				held.networks[endpoint.NetworkID] = true
			}
		}
		for _, mount := range row.Mounts {
			if mount.Type == "image" {
				held.hasImageMounts = true
				// Engine can report a snapshot path here instead of the mounted image ID.
				if docker.IsImageID(mount.Source) {
					held.images[mount.Source] = true
				} else {
					held.unknownImageMount = true
				}
			}
			if mount.Type == "volume" && mount.Name != "" {
				held.volumes[mount.Name] = true
			}
		}
		if (row.State == "exited" || row.State == "dead" || row.State == "created") && !inventoryHelper(row.Labels) && !strings.HasPrefix(row.Labels[docker.LabelProject], "env_") {
			name := row.ID
			if len(row.Names) > 0 {
				name = strings.TrimPrefix(row.Names[0], "/")
			}
			held.stopped = append(held.stopped, &v1.DockerCleanupTarget{Kind: cleanupContainer, EngineId: row.ID, Name: bounded(name, 255), Reason: reasonTakeOn})
		}
		return nil
	})
	return held, err
}

func cacheTarget(record docker.DiskUsageCache, exact bool) *v1.DockerCleanupTarget {
	t := &v1.DockerCleanupTarget{Kind: cleanupCache, EngineId: bounded(record.ID, 80), Name: bounded(record.ID, 80), EstimatedBytes: measuredBytes(record.Size)}
	if record.LastUsedAt != nil {
		t.LastUsedAt = inventoryTime(*record.LastUsedAt)
	}
	t.Reason = cacheCleanupReason(record, exact)
	if t.Reason == reasonIdle {
		t.Evidence = cleanupEvidence(record.ID, record.CreatedAt, record.LastUsedAt, record.UsageCount, record.Size)
	}
	return t
}

func cacheCleanupReason(record docker.DiskUsageCache, exact bool) v1.DockerCleanupReason {
	switch {
	case record.InUse:
		return reasonInUse
	case record.Type == "internal" || record.Type == "frontend":
		return reasonDesired
	case record.Shared:
		return reasonShared
	case !exact || !docker.IsBuildCacheID(record.ID) || record.CreatedAt.IsZero() || record.Size < 0:
		return reasonUnavailable
	default:
		return reasonIdle
	}
}

func networkTarget(network *docker.Network, held dockerOccupancy, protected []string) *v1.DockerCleanupTarget {
	t := &v1.DockerCleanupTarget{Kind: cleanupNetwork, EngineId: bounded(network.ID, 80), Name: bounded(network.Name, 255)}
	switch {
	case protectedNetwork(network, protected):
		t.Reason = reasonDesired
	case len(network.Containers) > 0 || held.networks[network.Name] || held.networks[network.ID]:
		t.Reason = reasonInUse
	case network.Scope != "local" || !docker.IsFullID(network.ID) || t.Name != network.Name:
		t.Reason = reasonUnavailable
	default:
		t.Reason = reasonUnused
		t.Evidence = cleanupEvidence(network.ID, network.Name)
	}
	return t
}

func protectedNetwork(network *docker.Network, protected []string) bool {
	if slices.Contains(protected, network.Name) || slices.Contains(protected, network.ID) {
		return true
	}
	switch network.Name {
	case "bridge", "none", "host", "ingress", "docker_gwbridge":
		return true
	}
	if strings.HasPrefix(network.Name, "dockwell") {
		return true
	}
	for label := range network.Labels {
		if strings.Contains(label, "dockwell") {
			return true
		}
	}
	return false
}

func protectedVolume(v *docker.Volume, protected []string) bool {
	if slices.Contains(protected, v.Name) || strings.HasPrefix(v.Name, "dockwell") {
		return true
	}
	if strings.HasPrefix(v.Labels[docker.LabelProject], "env_") && storageLabelMatches(v.Name, v.Labels, docker.LabelVolume) {
		return true
	}
	for label := range v.Labels {
		if strings.Contains(label, "dockwell") {
			return true
		}
	}
	return false
}

func composeVolumeProject(v *docker.Volume) (string, bool) {
	if project := strings.TrimSpace(v.Labels[docker.LabelProject]); project != "" {
		return project, true
	}
	if strings.TrimSpace(v.Labels[docker.LabelVolume]) != "" {
		return "", true
	}
	for label := range v.Labels {
		if strings.HasPrefix(label, docker.ComposePrefix) {
			return "", true
		}
	}
	return "", false
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func hasOrchestratorLabel(labels map[string]string) bool {
	for label := range labels {
		lower := strings.ToLower(label)
		if strings.Contains(lower, "dockwell") ||
			strings.HasPrefix(lower, "com.docker.compose.") ||
			strings.HasPrefix(lower, "com.docker.stack.") ||
			strings.HasPrefix(lower, "com.docker.swarm.") ||
			strings.HasPrefix(lower, "com.docker.ucp.") ||
			strings.HasPrefix(lower, "io.kubernetes.") ||
			strings.HasPrefix(lower, "app.kubernetes.io/") ||
			strings.HasPrefix(lower, "io.rancher.") ||
			strings.HasPrefix(lower, "com.hashicorp.nomad.") {
			return true
		}
	}
	return false
}

func isAnonymousVolume(v *docker.Volume) bool {
	if v == nil {
		return false
	}
	val, ok := v.Labels["com.docker.volume.anonymous"]
	return ok && val == "" && isLowerHex64(v.Name) && !hasOrchestratorLabel(v.Labels)
}

func parseVolumeCreatedAt(raw string) (time.Time, bool) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

func (c *calls) volumeKept(name string) bool {
	if c.cfg.DockerVolumeKept == nil {
		return false
	}
	kept, err := c.cfg.DockerVolumeKept(name)
	return kept || err != nil
}

func volumeCleanupEvidence(key, name string, reason v1.DockerCleanupReason, previewAt time.Time) string {
	return cleanupEvidence(key, name, int32(reason)) + "." + strconv.FormatInt(previewAt.UnixNano(), 10)
}

func parseVolumeCleanupEvidence(ev, key, name string, reason v1.DockerCleanupReason) (time.Time, bool) {
	hash, unixStr, ok := strings.Cut(ev, ".")
	if !ok || hash != cleanupEvidence(key, name, int32(reason)) {
		return time.Time{}, false
	}
	unixNano, err := strconv.ParseInt(unixStr, 10, 64)
	if err != nil || unixNano <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, unixNano).UTC(), true
}

func (c *calls) swarmBlocksVolumeCleanup(ctx context.Context) bool {
	info, err := c.cfg.Docker.Info(ctx)
	return err != nil || info.SwarmVolumeFallback()
}

func (c *calls) volumeTarget(v *docker.Volume, measured *docker.DiskUsageVolume, held dockerOccupancy, protected, backups, rollback []string, refTime time.Time, swarmBlocked bool) *v1.DockerCleanupTarget {
	key := docker.VolumeInventoryKey(v)
	_, validKey := docker.VolumeInventoryName(key)
	var size *uint64
	var refCount int64
	if measured != nil && measured.UsageData != nil &&
		(measured.Mountpoint == "" || v.Mountpoint == "" || measured.Mountpoint == v.Mountpoint) &&
		(measured.CreatedAt == "" || v.CreatedAt == "" || measured.CreatedAt == v.CreatedAt) {
		size = measuredBytes(measured.UsageData.Size)
		refCount = measured.UsageData.RefCount
	}
	t := &v1.DockerCleanupTarget{
		Kind:           cleanupVolume,
		EngineId:       key,
		Name:           bounded(v.Name, 255),
		EstimatedBytes: size,
	}
	composeProject, isCompose := composeVolumeProject(v)
	createdAt, validCreatedAt := parseVolumeCreatedAt(v.CreatedAt)
	switch {
	case c.volumeKept(v.Name):
		t.Reason = reasonRetained
	case protectedVolume(v, protected):
		t.Reason = reasonDesired
	case isCompose:
		t.Reason = reasonCompose
		t.ComposeProject = bounded(composeProject, 255)
		if composeProject != "" && held.composeProjects[composeProject] {
			if strings.HasPrefix(composeProject, "env_") {
				t.ComposeId = composeProject
			} else {
				t.ComposeId = "outside:" + bounded(composeProject, 255)
			}
		}
	case slices.Contains(backups, v.Name):
		t.Reason = reasonBackup
	case slices.Contains(rollback, v.Name):
		t.Reason = reasonRollback
	case held.volumes[v.Name] || refCount > 0:
		t.Reason = reasonInUse
	case swarmBlocked:
		t.Reason = reasonSwarmManager
	case v.Driver != "local" || v.Scope != "local" || !validKey || !validCreatedAt || t.Name != v.Name:
		t.Reason = reasonUnavailable
	case refTime.Sub(createdAt) < minVolumeCleanupAge:
		t.Reason = reasonRecentlyCreated
	default:
		if isAnonymousVolume(v) {
			t.Reason = reasonAnonymous
		} else {
			t.Reason = reasonUnused
		}
		t.Evidence = volumeCleanupEvidence(key, v.Name, t.Reason, refTime)
	}
	return t
}

func wantsCleanupKind(filters *v1.DockerCleanupFilters, kind v1.DockerCleanupKind) bool {
	kinds := filters.GetKinds()
	return len(kinds) == 0 || slices.Contains(kinds, kind)
}

func cleanupEvidence(fields ...any) string {
	data, _ := json.Marshal(fields)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (c *calls) dockerCleanupPreview(ctx context.Context, ask *agentv1.DockerMaintenancePreviewCall) (*agentv1.DockerMaintenancePreviewResult, error) {
	if c.cfg.Docker == nil || c.cfg.disks == nil {
		return nil, errDockerInventory
	}
	if ask.GetFresh() {
		c.cfg.disks.cleanupMu.Lock()
		defer c.cfg.disks.cleanupMu.Unlock()
	}
	pause, err := c.readCleanupPause()
	if err != nil || pause != nil {
		return &agentv1.DockerMaintenancePreviewResult{Pause: pause}, err
	}
	var use *v1.ServerDiskUse
	var df *docker.DiskUsage
	if ask.GetFresh() {
		use, df, err = c.cfg.disks.snapshotMode(ctx, true, true)
	} else {
		use, df, err = c.cfg.disks.cached(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("the Engine did not provide a fresh disk count for cleanup: %w", err)
	}
	held, err := c.dockerOccupancy(ctx)
	if err != nil {
		return nil, err
	}
	filters := ask.GetFilters()
	var targets []*v1.DockerCleanupTarget
	if wantsCleanupKind(filters, cleanupContainer) {
		targets = append(targets, held.stopped...)
	}
	if len(df.Images)+len(df.Volumes)+len(df.BuildCache)+len(targets) > maxCleanupRows {
		return nil, errCleanupSize
	}
	if wantsCleanupKind(filters, cleanupImage) {
		for _, raw := range df.Images {
			if !docker.IsImageID(raw.ID) {
				continue
			}
			target := c.imageCleanupTarget(raw, held, ask.GetProtectedImages())
			if target.GetReason() == reasonUnused && target.GetEvidence() != "" && !filters.GetTaggedImages() {
				continue
			}
			targets = append(targets, target)
		}
	}
	if wantsCleanupKind(filters, cleanupCache) {
		for _, record := range df.BuildCache {
			targets = append(targets, cacheTarget(record, c.cfg.Docker.ExactCacheRemoval()))
		}
	}
	if wantsCleanupKind(filters, cleanupVolume) {
		if filters.GetVolumes() {
			live, err := c.cfg.Docker.InventoryVolumes(ctx, maxCleanupRows)
			if err != nil || len(live)+len(targets) > maxCleanupRows {
				return nil, errCleanupSize
			}
			swarmBlocked := c.swarmBlocksVolumeCleanup(ctx)
			previewAt := c.cleanupNow()
			measured := make(map[string]*docker.DiskUsageVolume, len(df.Volumes))
			for i := range df.Volumes {
				v := &df.Volumes[i]
				measured[v.Name] = v
			}
			for i := range live {
				v := &live[i]
				targets = append(targets, c.volumeTarget(v, measured[v.Name], held,
					ask.GetProtectedVolumes(), ask.GetBackupVolumes(), ask.GetRollbackVolumes(), previewAt, swarmBlocked))
			}
		} else {
			for _, volume := range df.Volumes {
				target := &v1.DockerCleanupTarget{Kind: cleanupVolume, EngineId: bounded(volume.Name, 255), Name: bounded(volume.Name, 255), Reason: reasonRetained}
				if volume.UsageData != nil {
					target.EstimatedBytes = measuredBytes(volume.UsageData.Size)
				}
				if target.EstimatedBytes == nil || target.GetEstimatedBytes() > 0 {
					targets = append(targets, target)
				}
			}
		}
	}
	if wantsCleanupKind(filters, cleanupLog) && use.GetLogsBytes() > 0 {
		targets = append(targets, &v1.DockerCleanupTarget{Kind: cleanupLog, Reason: reasonRetained, EstimatedBytes: &use.LogsBytes})
	}
	if wantsCleanupKind(filters, cleanupNetwork) && filters.GetNetworks() {
		list, err := c.cfg.Docker.ListCleanupNetworks(ctx, maxCleanupRows-len(targets))
		if err != nil || len(list)+len(targets) > maxCleanupRows {
			return nil, errDockerInventory
		}
		for i := range list {
			targets = append(targets, networkTarget(&list[i], held, ask.GetProtectedNetworks()))
		}
	}
	if len(targets) > maxCleanupRows {
		return nil, errCleanupSize
	}

	targets = slices.DeleteFunc(targets, func(t *v1.DockerCleanupTarget) bool {
		return t.GetKind() != cleanupVolume && t.GetEvidence() == "" && t.EstimatedBytes != nil && t.GetEstimatedBytes() == 0
	})
	return &agentv1.DockerMaintenancePreviewResult{Targets: targets}, nil
}

// Keep the whole set of tags in the reviewed plan. An object with aliases
// beyond these bounds is protected instead of being reviewed in part.
func reviewableImageTags(tags []string) bool {
	if len(tags) > 16 || !slices.IsSorted(tags) {
		return false
	}
	for i, tag := range tags {
		colon := strings.LastIndex(tag, ":")
		if len(tag) > 255 || colon <= strings.LastIndex(tag, "/")+1 || colon == len(tag)-1 ||
			strings.ContainsAny(tag, "@<>?% \r\n\t") || docker.IsImageID(tag) ||
			i > 0 && tags[i-1] == tag {
			return false
		}
	}
	return true
}

func imageTags(raw []string) []string {
	tags := slices.Clone(raw)
	tags = slices.DeleteFunc(tags, func(ref string) bool { return ref == "" || ref == "<none>:<none>" || ref == "<none>@<none>" })
	slices.Sort(tags)
	return tags
}

func (c *calls) imageCleanupTarget(raw docker.DiskUsageImage, held dockerOccupancy, protected []string) *v1.DockerCleanupTarget {
	// The containerd store can include a canonical digest in RepoTags as well.
	// Count it as a digest exactly once, while verifying it at apply like any other reference.
	references := slices.Compact(imageTags(raw.RepoTags))
	var tags, digests []string
	digests = append(digests, raw.RepoDigests...)
	for _, ref := range references {
		if strings.Contains(ref, "@") {
			digests = append(digests, ref)
		} else {
			tags = append(tags, ref)
		}
	}
	digests = slices.Compact(imageTags(digests))
	raw.RepoTags, raw.RepoDigests = tags, digests
	name := raw.ID
	if len(tags) > 0 {
		name = tags[0]
	}
	t := &v1.DockerCleanupTarget{Kind: cleanupImage, EngineId: raw.ID, Name: bounded(name, 255), EstimatedBytes: imageUnique(raw)}
	switch {
	case containsProtectedImage(raw, protected):
		t.Reason = reasonRollback
	case c.ownedImage(raw):
		t.Reason = reasonDesired
	case held.images[raw.ID] || raw.Containers > 0 || slices.ContainsFunc(tags, func(tag string) bool { return held.images[tag] }) ||
		slices.ContainsFunc(raw.RepoDigests, func(digest string) bool { return held.images[digest] }):
		t.Reason = reasonInUse
	case held.unknownImageMount:
		t.Reason = reasonUnavailable
	case !oneImageRepository(tags, digests):
		t.Reason = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_REPOSITORIES
	case len(tags) > 1:
		t.Reason = v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_TAGS
	case len(references) > 1:
		// The snapshotter's canonical references are independent records. Its
		// non-forced delete by ID refuses when several records name the target.
		t.Reason = reasonUnavailable
	case !reviewableImageRefs(tags, digests):
		t.Reason = reasonUnavailable
	default:
		t.Reason = imageCleanupReason(tags, digests)
		t.ImageTags, t.ImageDigests = tags, digests
		t.Evidence = imageCleanupEvidence(raw.ID, tags, digests)
	}
	return t
}

func imageCleanupEvidence(id string, tags, digests []string) string {
	// Empty repeated fields are equivalent before and after wire decoding.
	return cleanupEvidence(id, strings.Join(tags, "\n"), strings.Join(digests, "\n"))
}

func imageCleanupReason(tags, digests []string) v1.DockerCleanupReason {
	if len(tags)+len(digests) == 0 {
		return reasonDangling
	}
	return reasonUnused
}

func oneImageRepository(tags, digests []string) bool {
	var repo string
	for _, refs := range [][]string{tags, digests} {
		for _, ref := range refs {
			name := docker.Repository(ref)
			if repo != "" && name != repo {
				return false
			}
			repo = name
		}
	}
	return true
}

func reviewableImageRefs(tags, digests []string) bool {
	return len(tags)+len(digests) <= 16 && reviewableImageTags(tags) && slices.IsSorted(digests) &&
		!slices.ContainsFunc(digests, func(ref string) bool {
			name, digest, found := strings.Cut(ref, "@")
			return !found || name == "" || len(ref) > 512 || !docker.IsImageID(digest)
		})
}

func containsProtectedImage(image docker.DiskUsageImage, protected []string) bool {
	return slices.ContainsFunc(protected, func(want string) bool {
		name, digest, hasDigest := strings.Cut(want, "@")
		if !hasDigest {
			digest = want
		}
		if image.ID == digest || slices.ContainsFunc(image.RepoDigests, func(ref string) bool {
			_, found, _ := strings.Cut(ref, "@")
			return found != "" && found == digest
		}) {
			return true
		}
		if docker.IsImageID(name) || hasDigest && docker.WithTag(name) != name {
			return false
		}
		tag := canonicalTag(name)
		return slices.ContainsFunc(image.RepoTags, func(ref string) bool { return tag == canonicalTag(ref) })
	})
}

func canonicalTag(ref string) string {
	ref = docker.WithTag(ref)
	colon := strings.LastIndex(ref, ":")
	if colon < 0 {
		return ref
	}
	return docker.Repository(ref) + ref[colon:]
}

func sameCleanupTarget(reviewed, current *v1.DockerCleanupTarget) bool {
	return reviewed.GetEvidence() != "" && current.GetEvidence() == reviewed.GetEvidence() &&
		current.GetReason() == reviewed.GetReason() && current.GetEngineId() == reviewed.GetEngineId()
}
