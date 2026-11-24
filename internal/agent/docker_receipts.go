// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	dwid "github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

const (
	receiptRemoved = v1.DockerReceiptState_DOCKER_RECEIPT_STATE_REMOVED
	receiptChanged = v1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED
	receiptFailed  = v1.DockerReceiptState_DOCKER_RECEIPT_STATE_FAILED
	receiptUnknown = v1.DockerReceiptState_DOCKER_RECEIPT_STATE_UNKNOWN

	receiptReasonInUse          = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IN_USE
	receiptReasonGone           = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_GONE
	receiptReasonReferenceMoved = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_REFERENCE_MOVED
	receiptReasonNewer          = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_NEWER
	receiptReasonProtected      = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_PROTECTED

	maxCleanupTargetsPerServer = 200
	legacyCleanupTargetCap     = 12
)

var (
	errCleanupSwarm      = errors.New("volumes can't be removed while Docker Swarm is active on this server. Review cleanup again")
	errLegacyCleanupPlan = errors.New("The cleanup was reviewed by an older agent. Review cleanup again") //nolint:staticcheck // Exact user-facing stale-plan sentence.
)

func (c *calls) checkDockerMaintenance(ref resource.Ref, maintenance *v1.DockerMaintenance) error {
	targets := maintenance.GetTargets()
	if c.cfg.Docker == nil || c.cfg.disks == nil || c.cfg.Dir == "" || c.cfg.Identity == nil ||
		ref.Kind != host.Kind || ref.Name != host.Name || ref.Scope != c.cfg.Identity.ServerID ||
		maintenance.GetChangeId() == "" || len(maintenance.GetChangeId()) > 80 || len(targets) > maxCleanupTargetsPerServer {
		return errors.New("this agent could not verify the cleanup scope")
	}
	now := c.cleanupNow()
	created, err := dwid.Parse(dwid.Change, maintenance.GetChangeId())
	if err != nil || now.Sub(created) >= cleanupReceiptRetention {
		return errors.New("this Docker cleanup plan has expired. Preview cleanup again")
	}
	if token := maintenance.GetResumeCleanupToken(); token != "" {
		if !docker.IsFullID(token) || len(targets) != 0 {
			return errors.New("re-enable Docker cleanup in its own plan")
		}
		return nil
	}
	if len(targets) == 0 {
		return errors.New("this cleanup plan has no targets")
	}
	seen := make(map[string]bool, len(targets))
	var hasLegacy bool
	for _, target := range targets {
		id := target.GetEngineId()
		tags, digests, records := target.GetImageTags(), target.GetImageDigests(), target.GetImageRecords()
		var valid bool
		switch target.GetKind() {
		case cleanupCache:
			valid = target.GetReason() == reasonIdle && docker.IsBuildCacheID(id) && len(tags)+len(digests)+len(records) == 0
		case cleanupImage:
			if isLegacyImageCleanupTarget(target) {
				valid = true
				hasLegacy = true
				break
			}
			baseOK := target.GetReason() == imageCleanupReason(tags, digests) && docker.IsImageID(id) && len(tags) <= 1 &&
				reviewableImageRefs(tags, digests) && oneImageRepository(tags, digests)
			switch {
			case !baseOK:
				valid = false
			case len(records) > 0:
				valid = reviewableImageRecords(records, tags, digests) && target.GetEvidence() == imageCleanupEvidence(id, tags, digests, records)
			case len(tags) == 1:
				// An older control plane can drop field 13 on JSON round-trip while keeping
				// the v2 evidence hash; the sole record is determined by the reviewed tag.
				valid = target.GetEvidence() == imageCleanupEvidence(id, tags, digests, []string{canonicalImageRecord(tags[0])})
			case len(digests) == 0:
				valid = target.GetEvidence() == imageCleanupEvidence(id, nil, nil, nil)
			default:
				// Pinned or digest-only image whose field 13 may have been dropped by an older
				// control plane. Apply rebuilds current records from Engine and matches the v2 hash.
				valid = isLowerHex64(target.GetEvidence())
			}
		case cleanupNetwork:
			valid = target.GetReason() == reasonUnused && docker.IsFullID(id) && target.GetName() != "" && len(tags)+len(digests)+len(records) == 0
		case cleanupVolume:
			name, ok := docker.VolumeInventoryName(id)
			previewAt, evOK := parseVolumeCleanupEvidence(target.GetEvidence(), id, name, target.GetReason())
			valid = (target.GetReason() == reasonAnonymous || target.GetReason() == reasonUnused) &&
				ok && name == target.GetName() && len(tags)+len(digests)+len(records) == 0 &&
				evOK && !previewAt.After(now) && now.Sub(previewAt) < cleanupReceiptRetention
		default:
		}
		if !valid || target.GetServerId() != ref.Scope ||
			(target.GetKind() != cleanupVolume && len(target.GetEvidence()) != 64) || seen[id] {
			return errors.New("the reviewed cleanup contains an unsupported target")
		}
		seen[id] = true
	}
	if hasLegacy {
		return errLegacyCleanupPlan
	}
	return nil
}

func (c *calls) applyDockerMaintenance(ctx context.Context, ref resource.Ref, maintenance *v1.DockerMaintenance, protection *agentv1.DockerCleanupProtection) error {
	if err := c.checkDockerMaintenance(ref, maintenance); err != nil {
		return err
	}
	if protection == nil && slices.ContainsFunc(maintenance.GetTargets(), func(t *v1.DockerCleanupTarget) bool {
		return t.GetKind() != cleanupCache
	}) {
		return errors.New("the agent did not receive current Docker cleanup protections")
	}
	c.cfg.disks.cleanupMu.Lock()
	defer c.cfg.disks.cleanupMu.Unlock()
	if token := maintenance.GetResumeCleanupToken(); token != "" {
		return c.resumeDockerCleanup(token)
	}

	dir := filepath.Join(c.cfg.Dir, "docker-cleanup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("the agent could not open its Docker cleanup receipts")
	}
	changeHash := cleanupEvidence(maintenance.GetChangeId())
	journalFile := filepath.Join(dir, changeHash+".ndjson")
	legacyFile := filepath.Join(dir, changeHash+".json")
	targets := maintenance.GetTargets()
	receipts, restoreJournal, err := readDockerReceipts(journalFile, legacyFile, targets)
	if err != nil {
		return err
	}
	defer func() { resource.Report(ctx, resource.Outcome{DockerReceipts: receipts}) }()

	_, df, err := c.cfg.disks.snapshot(ctx, true)
	if err != nil {
		return err
	}
	// Mirror baseline-compatible changes as arrays so a rolled-back agent can
	// replay their receipts, including any ambiguous removal.
	mirror := ""
	if len(targets) <= legacyCleanupTargetCap && !slices.ContainsFunc(targets, func(t *v1.DockerCleanupTarget) bool {
		return t.GetKind() == cleanupVolume
	}) {
		mirror = legacyFile
	}
	// Observe interrupted removals without dispatching them again.
	var journal *dockerReceiptJournal
	defer func() {
		if journal != nil {
			_ = journal.close()
		}
	}()
	openJournal := func() error {
		if journal == nil {
			journal, err = openDockerReceiptJournal(journalFile, mirror, receipts, restoreJournal)
			return err
		}
		return nil
	}
	var incident *cleanupPauseRecord
	for i, receipt := range receipts {
		if receipt.GetState() != receiptUnknown {
			continue
		}
		target := receipt.GetTarget()
		if target.GetKind() != cleanupVolume {
			receipt.AfterBytes = cleanupObjectBytes(df, target)
			continue
		}
		observed := proto.CloneOf(receipt)
		c.reconcileUnknownVolumeReceipt(ctx, df, observed)
		if !proto.Equal(receipt, observed) {
			if err := openJournal(); err != nil {
				return err
			}
			if err := journal.append(i, observed); err != nil {
				return err
			}
			receipts[i] = observed
		}
		resumed, err := c.isIncidentResumed(maintenance.GetChangeId(), target.GetEngineId())
		if err != nil {
			return err
		}
		if !resumed && incident == nil {
			incident = newVolumeCleanupPause(maintenance.GetChangeId(), target.GetEngineId())
			incident.Reason = observed.GetReason()
		}
	}
	if incident != nil {
		if !c.cleanupDisabled() {
			return errors.Join(errCleanupDisabled, c.saveCleanupPause(incident))
		}
		return errCleanupDisabled
	}

	if c.cleanupDisabled() {
		return errCleanupDisabled
	}
	if len(receipts) < len(targets) && c.cfg.disks.isFailedCleanupChange(maintenance.GetChangeId()) {
		return errors.New("earlier Docker cleanup receipts could not be saved. Review cleanup again")
	}
	if slices.ContainsFunc(receipts, func(r *v1.DockerReceipt) bool {
		return r.GetState() != receiptRemoved && r.GetState() != receiptChanged
	}) {
		return errors.New("some Docker artifacts could not be confirmed removed. Review their receipts and preview cleanup again")
	}
	if len(receipts) < len(targets) && slices.ContainsFunc(receipts, func(r *v1.DockerReceipt) bool {
		return r.GetState() == receiptChanged
	}) {
		return errors.New("earlier Docker cleanup targets changed before the batch finished. Review cleanup again")
	}
	if len(receipts) == len(targets) {
		return nil
	}

	// Recheck the untouched part before making any new removals. Each object
	// is checked again immediately before dispatch using filtered Engine queries.
	// A target that changed since review gets a Changed receipt and is skipped,
	// while the rest of the batch continues.
	var batchHeld *dockerOccupancy
	if slices.ContainsFunc(targets[len(receipts):], func(t *v1.DockerCleanupTarget) bool {
		return t.GetKind() != cleanupCache
	}) {
		held, err := c.dockerOccupancy(ctx)
		if err != nil {
			return err
		}
		batchHeld = &held
	}
	hasImageMounts := batchHeld != nil && batchHeld.hasImageMounts
	if slices.ContainsFunc(targets[len(receipts):], func(t *v1.DockerCleanupTarget) bool {
		return t.GetKind() == cleanupVolume
	}) && c.swarmBlocksVolumeCleanup(ctx) {
		return errCleanupSwarm
	}
	var pending []*v1.DockerReceipt
	for _, target := range targets[len(receipts):] {
		identity, state, reason := c.recheckCleanupWithHeld(ctx, df, batchHeld, target, protection, false, hasImageMounts)
		pending = append(pending, &v1.DockerReceipt{
			Target: target, State: state, Reason: reason, RecheckedEngineId: identity,
			BeforeBytes: cleanupObjectBytes(df, target),
		})
	}
	if err := openJournal(); err != nil {
		if slices.ContainsFunc(pending, func(r *v1.DockerReceipt) bool { return r.GetState() == receiptChanged }) {
			c.cfg.disks.markFailedCleanupChange(maintenance.GetChangeId())
		}
		return err
	}
	recordReceipt := func(idx int, r *v1.DockerReceipt) error {
		if err := journal.append(idx, r); err != nil {
			c.cfg.disks.markFailedCleanupChange(maintenance.GetChangeId())
			return err
		}
		saved := proto.CloneOf(r)
		if idx == len(receipts) {
			receipts = append(receipts, saved)
		} else {
			receipts[idx] = saved
		}
		return nil
	}

	var operationErr error
	var dispatched bool
	first := len(receipts)
	for _, receipt := range pending {
		if ctx.Err() != nil {
			operationErr = ctx.Err()
			break
		}
		idx := len(receipts)
		writeReceipt := func() error { return recordReceipt(idx, receipt) }
		if receipt.GetState() == receiptChanged {
			if err := writeReceipt(); err != nil {
				operationErr = err
				break
			}
			continue
		}
		if receipt.GetState() == receiptFailed {
			writeErr := writeReceipt()
			operationErr = errors.Join(errors.New("some Docker artifacts could not be checked. Review their receipts and preview cleanup again"), writeErr)
			break
		}
		if receipt.GetTarget().GetKind() == cleanupVolume && c.swarmBlocksVolumeCleanup(ctx) {
			operationErr = errCleanupSwarm
			break
		}
		dispatched = true
		var prepareFailed bool
		prepare := func() error {
			if err := writeReceipt(); err != nil {
				prepareFailed = true
				return err
			}
			return nil
		}
		var pause *cleanupPauseRecord
		var paused bool
		if target := receipt.GetTarget(); target.GetKind() == cleanupVolume {
			pause = newVolumeCleanupPause(maintenance.GetChangeId(), target.GetEngineId())
			prepare = func() error {
				// A resume token always corresponds to a persisted receipt.
				if err := writeReceipt(); err != nil {
					prepareFailed = true
					return err
				}
				paused = true
				return c.saveCleanupPause(pause)
			}
		}
		operationErr = c.removeCleanupTarget(ctx, receipt, protection, hasImageMounts, pause, prepare)
		if !prepareFailed {
			writeErr := writeReceipt()
			operationErr = errors.Join(operationErr, writeErr)
			if paused && receipt.GetState() != receiptUnknown && writeErr == nil {
				operationErr = errors.Join(operationErr, c.clearCleanupPause())
			}
		}
		if operationErr != nil || receipt.GetState() == receiptFailed {
			break
		}
	}
	if dispatched && len(receipts) > first {
		if _, after, err := c.cfg.disks.snapshot(ctx, true); err == nil {
			for i := first; i < len(receipts); i++ {
				updated := proto.CloneOf(receipts[i])
				if updated.GetTarget().GetKind() == cleanupVolume && updated.GetState() == receiptUnknown {
					c.reconcileUnknownVolumeReceipt(ctx, after, updated)
				} else {
					updated.AfterBytes = cleanupObjectBytes(after, updated.GetTarget())
				}
				operationErr = errors.Join(operationErr, recordReceipt(i, updated))
			}
		}
	}
	if operationErr != nil {
		return operationErr
	}
	if slices.ContainsFunc(receipts, func(r *v1.DockerReceipt) bool {
		return r.GetState() != receiptRemoved && r.GetState() != receiptChanged
	}) {
		return errors.New("some Docker artifacts could not be confirmed removed. Review their receipts and preview cleanup again")
	}
	return nil
}

func (c *calls) reconcileUnknownVolumeReceipt(ctx context.Context, df *docker.DiskUsage, receipt *v1.DockerReceipt) {
	target := receipt.GetTarget()
	v, err := c.cfg.Docker.InspectVolume(ctx, target.GetName())
	switch {
	case docker.IsNotFound(err):
		zero := uint64(0)
		receipt.AfterBytes = &zero
		if receipt.GetReason() == 0 {
			receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED
		}
	case err != nil:
		receipt.AfterBytes = nil
		if receipt.GetReason() == 0 {
			receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED
		}
	case docker.VolumeInventoryKey(v) != target.GetEngineId():
		receipt.AfterBytes = nil
		receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_REPLACED
	default:
		receipt.AfterBytes = cleanupVolumeBytesMatching(df, v)
		if receipt.GetReason() == 0 {
			receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED
		}
	}
}

func cleanupVolumeBytesMatching(df *docker.DiskUsage, v *docker.Volume) *uint64 {
	if df == nil || v == nil {
		return nil
	}
	for _, row := range df.Volumes {
		if row.Name != v.Name || row.UsageData == nil {
			continue
		}
		if row.Mountpoint != "" && v.Mountpoint != "" && row.Mountpoint != v.Mountpoint {
			continue
		}
		if row.CreatedAt != "" && v.CreatedAt != "" && row.CreatedAt != v.CreatedAt {
			continue
		}
		return measuredBytes(row.UsageData.Size)
	}
	return nil
}

func cleanupObjectBytes(df *docker.DiskUsage, target *v1.DockerCleanupTarget) *uint64 {
	zero := uint64(0)
	switch target.GetKind() {
	case cleanupImage:
		for _, image := range df.Images {
			if image.ID == target.GetEngineId() {
				return imageUnique(image)
			}
		}
		return &zero
	case cleanupCache:
		if record := cacheRecord(df, target.GetEngineId()); record != nil {
			return measuredBytes(record.Size)
		}
		return &zero
	case cleanupVolume:
		for _, vol := range df.Volumes {
			if vol.Name == target.GetName() {
				if vol.UsageData == nil {
					return nil
				}
				return measuredBytes(vol.UsageData.Size)
			}
		}
		return &zero
	default:
		return nil
	}
}

func (c *calls) recheckCleanupWithHeld(ctx context.Context, df *docker.DiskUsage, preloaded *dockerOccupancy, target *v1.DockerCleanupTarget, protection *agentv1.DockerCleanupProtection, swarmBlocked, hasImageMounts bool) (string, v1.DockerReceiptState, v1.DockerReceiptReason) {
	id := target.GetEngineId()
	if target.GetKind() == cleanupCache {
		record := cacheRecord(df, id)
		if record == nil {
			return "", receiptChanged, receiptReasonGone
		}
		current := cacheTarget(*record, c.cfg.Docker.ExactCacheRemoval())
		if sameCleanupTarget(target, current) {
			return id, receiptUnknown, 0
		}
		switch current.GetReason() {
		case reasonInUse:
			return id, receiptChanged, receiptReasonInUse
		case reasonIdle:
			if cacheIsNewer(target, current) {
				return id, receiptChanged, receiptReasonNewer
			}
			return id, receiptChanged, 0
		default:
			return id, receiptChanged, receiptReasonProtected
		}
	}
	if target.GetKind() == cleanupImage {
		return c.recheckCleanupImage(ctx, preloaded, target, protection.GetImages(), hasImageMounts)
	}
	if target.GetKind() == cleanupVolume {
		return c.recheckCleanupVolume(ctx, preloaded, target, protection, swarmBlocked)
	}
	return c.recheckCleanupNetwork(ctx, preloaded, target, protection.GetNetworks())
}

func (c *calls) recheckCleanupNetwork(ctx context.Context, preloaded *dockerOccupancy, target *v1.DockerCleanupTarget, protected []string) (string, v1.DockerReceiptState, v1.DockerReceiptReason) {
	id := target.GetEngineId()
	network, err := c.cfg.Docker.InspectNetwork(ctx, id)
	if err != nil {
		state, reason := cleanupCheckFailure(err)
		return "", state, reason
	}
	var held dockerOccupancy
	if preloaded != nil {
		held = *preloaded
	} else {
		held = dockerOccupancy{networks: map[string]bool{}}
		users, err := c.cfg.Docker.ListContainers(ctx, map[string][]string{"network": {id}})
		if err != nil {
			return network.ID, receiptFailed, 0
		}
		if len(users) > 0 {
			held.networks[id] = true
		}
	}
	current := networkTarget(network, held, protected)
	if !sameCleanupTarget(target, current) {
		if current.GetEngineId() != target.GetEngineId() {
			return network.ID, receiptChanged, receiptReasonReferenceMoved
		}
		switch current.GetReason() {
		case reasonInUse:
			return network.ID, receiptChanged, receiptReasonInUse
		case reasonDesired, reasonUnavailable:
			return network.ID, receiptChanged, receiptReasonProtected
		default:
			return network.ID, receiptChanged, receiptReasonReferenceMoved
		}
	}
	return network.ID, receiptUnknown, 0
}

func (c *calls) recheckCleanupVolume(ctx context.Context, preloaded *dockerOccupancy, target *v1.DockerCleanupTarget, protection *agentv1.DockerCleanupProtection, swarmBlocked bool) (string, v1.DockerReceiptState, v1.DockerReceiptReason) {
	name, ok := docker.VolumeInventoryName(target.GetEngineId())
	if !ok || name != target.GetName() {
		return "", receiptChanged, receiptReasonProtected
	}
	previewAt, evOK := parseVolumeCleanupEvidence(target.GetEvidence(), target.GetEngineId(), name, target.GetReason())
	if !evOK || previewAt.After(c.cleanupNow()) {
		return target.GetEngineId(), receiptChanged, receiptReasonNewer
	}
	if swarmBlocked {
		return target.GetEngineId(), receiptChanged, receiptReasonProtected
	}
	v, err := c.cfg.Docker.InspectVolume(ctx, name)
	if err != nil {
		state, reason := cleanupCheckFailure(err)
		return "", state, reason
	}
	users, err := c.cfg.Docker.ListContainers(ctx, map[string][]string{"volume": {name}})
	if err != nil {
		return docker.VolumeInventoryKey(v), receiptFailed, 0
	}
	if len(users) > 0 {
		return docker.VolumeInventoryKey(v), receiptChanged, receiptReasonInUse
	}
	var held dockerOccupancy
	if preloaded != nil {
		held = *preloaded
	}
	current := c.volumeTarget(v, nil, held, protection.GetVolumes(), protection.GetBackupVolumes(), protection.GetRollbackVolumes(), previewAt, swarmBlocked)
	if !sameCleanupTarget(target, current) {
		if current.GetEngineId() != target.GetEngineId() {
			return current.GetEngineId(), receiptChanged, v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_REPLACED
		}
		switch current.GetReason() {
		case reasonInUse:
			return current.GetEngineId(), receiptChanged, receiptReasonInUse
		case reasonRecentlyCreated:
			return current.GetEngineId(), receiptChanged, receiptReasonNewer
		case reasonRetained, reasonDesired, reasonCompose, reasonBackup, reasonRollback, reasonSwarmManager, reasonUnavailable:
			return current.GetEngineId(), receiptChanged, receiptReasonProtected
		default:
			return current.GetEngineId(), receiptChanged, receiptReasonReferenceMoved
		}
	}
	return current.GetEngineId(), receiptUnknown, 0
}

func (c *calls) recheckCleanupImage(ctx context.Context, preloaded *dockerOccupancy, target *v1.DockerCleanupTarget, protected []string, hasImageMounts bool) (string, v1.DockerReceiptState, v1.DockerReceiptReason) {
	id := target.GetEngineId()
	if c.cfg.disks.imagePulls[id] > 0 {
		return id, receiptChanged, receiptReasonInUse
	}
	checked := make(map[string]bool, len(target.GetImageTags())+len(target.GetImageDigests()))
	for _, refs := range [][]string{target.GetImageTags(), target.GetImageDigests()} {
		for _, ref := range refs {
			if checked[ref] {
				continue
			}
			checked[ref] = true
			image, err := c.cfg.Docker.InspectImage(ctx, ref)
			if err != nil {
				state, reason := cleanupCheckFailure(err)
				if reason == receiptReasonGone {
					if _, idErr := c.cfg.Docker.InspectImage(ctx, id); idErr == nil {
						reason = receiptReasonReferenceMoved
					}
				}
				return "", state, reason
			}
			if image.ID != id {
				return image.ID, receiptChanged, receiptReasonReferenceMoved
			}
		}
	}
	image, err := c.cfg.Docker.InspectImage(ctx, id)
	if err != nil {
		state, reason := cleanupCheckFailure(err)
		return "", state, reason
	}
	var held dockerOccupancy
	switch {
	case preloaded != nil:
		held = *preloaded
	case hasImageMounts:
		// Engine ancestor= filters do not cover type=image mounts; fail closed
		// with a full container scan when any container mounts an image.
		full, err := c.dockerOccupancy(ctx)
		if err != nil {
			return image.ID, receiptFailed, 0
		}
		held = full
	default:
		held = dockerOccupancy{images: map[string]bool{}}
		users, err := c.cfg.Docker.ListContainers(ctx, map[string][]string{"ancestor": {id}})
		if err != nil {
			return image.ID, receiptFailed, 0
		}
		if len(users) > 0 {
			held.images[id] = true
		}
	}
	current := c.imageCleanupTarget(docker.DiskUsageImage{
		ID: image.ID, RepoTags: image.RepoTags, RepoDigests: image.RepoDigests, Labels: image.Config.Labels,
	}, held, protected)
	if !sameCleanupTarget(target, current) {
		switch current.GetReason() {
		case reasonInUse:
			return image.ID, receiptChanged, receiptReasonInUse
		case reasonRollback, reasonDesired, reasonUnavailable,
			v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_REPOSITORIES,
			v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_TAGS:
			return image.ID, receiptChanged, receiptReasonProtected
		default:
			return image.ID, receiptChanged, receiptReasonReferenceMoved
		}
	}
	return image.ID, receiptUnknown, 0
}

func cleanupCheckFailure(err error) (v1.DockerReceiptState, v1.DockerReceiptReason) {
	if docker.IsNotFound(err) {
		return receiptChanged, receiptReasonGone
	}
	return receiptFailed, 0
}

func cacheIsNewer(reviewed, current *v1.DockerCleanupTarget) bool {
	rev, cur := reviewed.GetLastUsedAt(), current.GetLastUsedAt()
	if rev == nil || cur == nil {
		return false
	}
	return cur.AsTime().After(rev.AsTime())
}

func (c *calls) removeCleanupTarget(ctx context.Context, receipt *v1.DockerReceipt, protection *agentv1.DockerCleanupProtection, hasImageMounts bool, pause *cleanupPauseRecord, journal func() error) error {
	target := receipt.GetTarget()
	if target.GetKind() == cleanupCache {
		return c.removeCleanupCache(ctx, receipt, journal)
	}
	// The pending receipt is durable before the final check. There is no
	// filesystem write between the last inspect and the Engine delete.
	if err := journal(); err != nil {
		return err
	}
	if target.GetKind() == cleanupVolume && c.swarmBlocksVolumeCleanup(ctx) {
		receipt.RecheckedEngineId = target.GetEngineId()
		receipt.State = receiptFailed
		receipt.Reason = receiptReasonProtected
		return errCleanupSwarm
	}
	receipt.RecheckedEngineId, receipt.State, receipt.Reason = c.recheckCleanupWithHeld(ctx, nil, nil, target, protection, false, hasImageMounts)
	if receipt.State != receiptUnknown {
		return nil
	}
	switch target.GetKind() {
	case cleanupNetwork:
		err := c.cfg.Docker.RemoveNetwork(ctx, target.GetEngineId())
		receipt.State, receipt.Reason = removalOutcome(err, err == nil)
	case cleanupVolume:
		err := c.cfg.Docker.RemoveVolume(ctx, target.GetName())
		receipt.State, receipt.Reason = removalOutcome(err, err == nil)
		if receipt.State == receiptUnknown {
			c.reconcileUnknownVolumeReceipt(ctx, nil, receipt)
			reason := receipt.GetReason()
			if reason == 0 {
				reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED
				receipt.Reason = reason
			}
			pause.Reason = reason
			return errors.Join(errCleanupDisabled, c.saveCleanupPause(pause))
		}
	case cleanupImage:
		err := c.cfg.Docker.RemoveReviewedImage(ctx, target.GetEngineId())
		receipt.State, receipt.Reason = removalOutcome(err, err == nil)
		if docker.IsConflict(err) {
			receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IMAGE_ID_REFUSED
		}
	default:
	}
	return nil
}

const cleanupReceiptRetention = 30 * 24 * time.Hour

var errCleanupDisabled = errors.New("cleanup is disabled on this server after an unverified Docker removal")

type cleanupPauseRecord struct {
	Token    string                 `json:"token,omitempty"`
	Reason   v1.DockerReceiptReason `json:"reason,omitempty"`
	ChangeID string                 `json:"change_id,omitempty"`
	TargetID string                 `json:"target_id,omitempty"`
}

func newVolumeCleanupPause(changeID, targetID string) *cleanupPauseRecord {
	return &cleanupPauseRecord{
		Token:    cleanupEvidence(dwid.New(dwid.Change)),
		Reason:   v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED,
		ChangeID: changeID,
		TargetID: targetID,
	}
}

type resumedCleanupIncident struct {
	ChangeID string `json:"change_id"`
	TargetID string `json:"target_id"`
}

func (c *calls) cleanupBlockFile() string {
	return filepath.Join(c.cfg.Dir, "docker-cleanup", "disabled")
}

func (c *calls) cleanupResumedFile() string {
	return filepath.Join(c.cfg.Dir, "docker-cleanup", "resumed.json")
}

func (c *calls) readCleanupPauseRecord() (*cleanupPauseRecord, error) {
	if c.cfg.Dir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(c.cleanupBlockFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := &cleanupPauseRecord{}
	if json.Unmarshal(data, rec) != nil || !docker.IsFullID(rec.Token) {
		// The first version wrote a plain-text marker. Keep the server paused.
		rec = &cleanupPauseRecord{
			Token:  cleanupEvidence(string(data)),
			Reason: v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_CACHE_UNVERIFIED,
		}
	}
	return rec, nil
}

func (c *calls) readCleanupPause() (*v1.DockerCleanupPause, error) {
	rec, err := c.readCleanupPauseRecord()
	if err != nil || rec == nil {
		return nil, err
	}
	return &v1.DockerCleanupPause{
		Token:  rec.Token,
		Reason: rec.Reason,
	}, nil
}

func (c *calls) cleanupDisabled() bool {
	pause, err := c.readCleanupPause()
	return err != nil || pause != nil
}

func (c *calls) readResumedIncidents() ([]resumedCleanupIncident, error) {
	if c.cfg.Dir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(c.cleanupResumedFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []resumedCleanupIncident
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (c *calls) isIncidentResumed(changeID, targetID string) (bool, error) {
	list, err := c.readResumedIncidents()
	if err != nil {
		return false, fmt.Errorf("can't read the Docker cleanup resume acknowledgments: %w", err)
	}
	return slices.Contains(list, resumedCleanupIncident{ChangeID: changeID, TargetID: targetID}), nil
}

func (c *calls) recordResumedIncident(changeID, targetID string) error {
	if changeID == "" || targetID == "" || c.cfg.Dir == "" {
		return nil
	}
	list, err := c.readResumedIncidents()
	if err != nil {
		return fmt.Errorf("can't read the Docker cleanup resume acknowledgments: %w", err)
	}
	now := c.cleanupNow()
	list = slices.DeleteFunc(list, func(item resumedCleanupIncident) bool {
		created, err := dwid.Parse(dwid.Change, item.ChangeID)
		return err == nil && now.Sub(created) >= cleanupReceiptRetention
	})
	incident := resumedCleanupIncident{ChangeID: changeID, TargetID: targetID}
	if slices.Contains(list, incident) {
		return nil
	}
	data, err := json.Marshal(append(list, incident))
	if err != nil {
		return err
	}
	return writeDockerCleanupFile(c.cleanupResumedFile(), data)
}

func (c *calls) resumeDockerCleanup(token string) error {
	pause, err := c.readCleanupPauseRecord()
	if err != nil || pause == nil {
		return err
	}
	if pause.Token != token {
		return errors.New("the Docker cleanup pause changed. Review it again")
	}
	if err := c.recordResumedIncident(pause.ChangeID, pause.TargetID); err != nil {
		return err
	}
	return c.clearCleanupPause()
}

func (c *calls) clearCleanupPause() error {
	file := c.cleanupBlockFile()
	if err := os.Remove(file); err != nil {
		return err
	}
	return syncCleanupDir(filepath.Dir(file))
}

func (c *calls) removeCleanupCache(ctx context.Context, receipt *v1.DockerReceipt, journal func() error) error {
	before, err := c.cfg.Docker.BuildCacheUsage(ctx)
	if err != nil {
		receipt.State = receiptFailed
		return err
	}
	id := receipt.GetTarget().GetEngineId()
	df := &docker.DiskUsage{BuildCache: before}
	receipt.RecheckedEngineId, receipt.State, receipt.Reason = c.recheckCleanupWithHeld(ctx, df, nil, receipt.GetTarget(), nil, false, false)
	if receipt.State != receiptUnknown {
		return nil
	}
	record := cacheRecord(df, id)
	if err := journal(); err != nil {
		return err
	}
	pause := &cleanupPauseRecord{
		Token:  cleanupEvidence(dwid.New(dwid.Change)),
		Reason: v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_CACHE_UNVERIFIED,
	}
	if err := c.saveCleanupPause(pause); err != nil {
		return err
	}
	deleted, removeErr := c.cfg.Docker.RemoveBuildCache(ctx, id, record.LastUsedAt)
	after, readErr := c.cfg.Docker.BuildCacheUsage(ctx)
	reportedExtra := slices.ContainsFunc(deleted, func(gone string) bool { return gone != id })
	remaining := make(map[string]bool, len(after))
	for _, row := range after {
		remaining[row.ID] = true
	}
	cutoff := docker.BuildCacheCutoff(record.LastUsedAt)
	missingExtra := readErr == nil && slices.ContainsFunc(before, func(row docker.DiskUsageCache) bool {
		return row.ID != id && !remaining[row.ID] && !row.InUse && !row.Shared &&
			row.Type != "internal" && row.Type != "frontend" &&
			(row.LastUsedAt == nil || !row.LastUsedAt.After(cutoff))
	})
	switch {
	case reportedExtra || missingExtra:
		receipt.State = receiptFailed
		receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_EXTRA_CACHE_REMOVED
		pause.Reason = receipt.Reason
		return errors.Join(errCleanupDisabled, c.saveCleanupPause(pause))
	case readErr != nil:
		receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_CACHE_UNVERIFIED
		return errCleanupDisabled
	}
	receipt.State, receipt.Reason = removalOutcome(removeErr, !remaining[id])
	if !remaining[id] && !slices.Contains(deleted, id) {
		receipt.State = receiptUnknown
	}
	return c.clearCleanupPause()
}

func (c *calls) saveCleanupPause(rec *cleanupPauseRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeDockerCleanupFile(c.cleanupBlockFile(), data)
}

func removalOutcome(err error, removed bool) (v1.DockerReceiptState, v1.DockerReceiptReason) {
	switch {
	case docker.IsInUse(err):
		return receiptChanged, receiptReasonInUse
	case docker.IsNotFound(err):
		return receiptChanged, receiptReasonGone
	case docker.IsBadRequest(err):
		return receiptFailed, 0
	case removed:
		return receiptRemoved, 0
	case err == nil:
		return receiptChanged, 0
	default:
		return receiptUnknown, 0
	}
}

func cacheRecord(df *docker.DiskUsage, id string) *docker.DiskUsageCache {
	for i := range df.BuildCache {
		if df.BuildCache[i].ID == id {
			return &df.BuildCache[i]
		}
	}
	return nil
}

type dockerReceiptRecord struct {
	Index   int               `json:"index"`
	Receipt *v1.DockerReceipt `json:"receipt"`
}

type dockerReceiptJournal struct {
	file   *os.File
	mirror string
	last   []*v1.DockerReceipt
	err    error
}

// openDockerReceiptJournal repairs a torn tail before opening it for appends.
// When a legacy agent has advanced the change, seed the journal from its array.
func openDockerReceiptJournal(path, mirror string, existing []*v1.DockerReceipt, restore bool) (*dockerReceiptJournal, error) {
	if restore {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		for i, receipt := range existing {
			if err := encoder.Encode(dockerReceiptRecord{Index: i, Receipt: receipt}); err != nil {
				return nil, err
			}
		}
		if err := writeDockerCleanupFile(path, buf.Bytes()); err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(path) //nolint:gosec // Agent data path with a hashed change ID.
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // Agent data path with a hashed change ID.
	if err != nil {
		return nil, err
	}
	if validLen := bytes.LastIndexByte(data, '\n') + 1; validLen < len(data) {
		err = errors.Join(f.Truncate(int64(validLen)), f.Sync())
	}
	if created {
		err = syncCleanupDir(filepath.Dir(path))
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	last := make([]*v1.DockerReceipt, len(existing))
	for i, receipt := range existing {
		last[i] = proto.CloneOf(receipt)
	}
	return &dockerReceiptJournal{file: f, mirror: mirror, last: last}, nil
}

func (j *dockerReceiptJournal) append(index int, receipt *v1.DockerReceipt) error {
	if j.err != nil {
		return j.err
	}
	if index < len(j.last) && proto.Equal(j.last[index], receipt) {
		return nil
	}
	// After a partial write no caller may append again until the tail is repaired.
	j.err = j.write(index, receipt)
	return j.err
}

func (j *dockerReceiptJournal) write(index int, receipt *v1.DockerReceipt) error {
	saved := proto.CloneOf(receipt)
	nextLast := slices.Clone(j.last)
	if index == len(nextLast) {
		nextLast = append(nextLast, saved)
	} else {
		nextLast[index] = saved
	}
	if j.mirror != "" {
		// Put the checkpoint on disk before the append so the baseline agent
		// can see every attempted deletion if it takes over after a crash.
		data, err := json.Marshal(nextLast)
		if err != nil {
			return err
		}
		if err := writeDockerCleanupFile(j.mirror, data); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(j.file).Encode(dockerReceiptRecord{Index: index, Receipt: receipt}); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.last = nextLast
	return nil
}

func (j *dockerReceiptJournal) close() error {
	return j.file.Close()
}

func readDockerReceipts(journalFile, legacyFile string, targets []*v1.DockerCleanupTarget) ([]*v1.DockerReceipt, bool, error) {
	data, err := os.ReadFile(journalFile) //nolint:gosec // Agent data path with a hashed change ID.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, errors.New("the agent could not verify its saved Docker cleanup receipts")
	}
	receipts, err := parseNDJSONReceipts(data, targets)
	if err != nil {
		return nil, false, err
	}
	legacyData, err := os.ReadFile(legacyFile) //nolint:gosec // Agent data path with a hashed change ID.
	if errors.Is(err, os.ErrNotExist) {
		return receipts, false, nil
	}
	var legacy []*v1.DockerReceipt
	if err != nil || json.Unmarshal(legacyData, &legacy) != nil || len(legacy) > len(targets) {
		return nil, false, errors.New("the agent could not verify its saved Docker cleanup receipts")
	}
	if _, err := validateDockerReceipts(legacy, targets); err != nil {
		return nil, false, err
	}
	if len(legacy) > len(receipts) {
		return legacy, true, nil
	}
	return receipts, false, nil
}

func parseNDJSONReceipts(data []byte, targets []*v1.DockerCleanupTarget) ([]*v1.DockerReceipt, error) {
	if len(data) == 0 {
		return nil, nil
	}
	// Every synced record ends with '\n'. Trailing bytes after the last newline
	// (including a torn first record with no newline yet) never finished Sync()
	// before a crash and will be durably truncated before any append.
	idx := bytes.LastIndexByte(data, '\n')
	if idx < 0 {
		return nil, nil
	}
	var receipts []*v1.DockerReceipt
	scanner := bufio.NewScanner(bytes.NewReader(data[:idx+1]))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec dockerReceiptRecord
		if json.Unmarshal(line, &rec) != nil || rec.Receipt == nil || rec.Index < 0 || rec.Index >= len(targets) {
			return nil, errors.New("the agent could not verify its saved Docker cleanup receipts")
		}
		if rec.Index > len(receipts) {
			return nil, errors.New("the agent could not verify its saved Docker cleanup receipts")
		}
		if rec.Index == len(receipts) {
			receipts = append(receipts, rec.Receipt)
		} else {
			receipts[rec.Index] = rec.Receipt
		}
	}
	if scanner.Err() != nil {
		return nil, errors.New("the agent could not verify its saved Docker cleanup receipts")
	}
	return validateDockerReceipts(receipts, targets)
}

func validateDockerReceipts(receipts []*v1.DockerReceipt, targets []*v1.DockerCleanupTarget) ([]*v1.DockerReceipt, error) {
	for i, r := range receipts {
		if !proto.Equal(r.GetTarget(), targets[i]) || r.GetState() == 0 {
			return nil, errors.New("the saved Docker cleanup receipts have a different scope")
		}
	}
	return receipts, nil
}

func writeDockerCleanupFile(file string, data []byte) error {
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, "receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	syncErr, closeErr := tmp.Sync(), tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	return syncCleanupDir(dir)
}

func syncCleanupDir(dir string) error {
	parent, err := os.Open(dir) //nolint:gosec // Agent data directory holding cleanup receipts.
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func pruneDockerReceipts(dir string, now time.Time) {
	files, err := os.Open(filepath.Join(dir, "docker-cleanup")) //nolint:gosec // Agent data directory.
	if err != nil {
		return
	}
	defer files.Close()
	for {
		entries, err := files.ReadDir(64)
		for _, entry := range entries {
			name := entry.Name()
			digest, ok := strings.CutSuffix(name, ".ndjson")
			if !ok {
				digest, ok = strings.CutSuffix(name, ".json")
			}
			if !ok || !docker.IsFullID(digest) || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err == nil && now.Sub(info.ModTime()) >= cleanupReceiptRetention {
				_ = os.Remove(filepath.Join(files.Name(), name))
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *calls) planDockerMaintenance(ctx context.Context, ask *agentv1.PlanCall) (*agentv1.CallResult, bool) {
	for _, desired := range ask.GetDesired() {
		plan := desired.GetDockerMaintenance()
		if plan == nil {
			continue
		}
		if len(ask.GetDesired()) != 1 {
			return callFailed(errors.New("plan Docker cleanup as a separate operation")), true
		}
		if plan.GetImageAction() != nil {
			return c.planImageAction(ctx, desired)
		}
		ref := resourcepb.FromRef(desired.GetResource())
		if err := c.checkDockerMaintenance(ref, plan); err != nil {
			return callFailed(err), true
		}
		step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionUpdate, Destroys: plan.GetResumeCleanupToken() == "", DockerMaintenance: plan}}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Plan{Plan: &agentv1.PlanResult{Steps: []*v1.PlanStep{resourcepb.Step(step, "")}}}}, true
	}
	return nil, false
}

func (c *calls) runDockerMaintenance(ctx context.Context, callID uint64, ref resource.Ref, plan *v1.DockerMaintenance, protection *agentv1.DockerCleanupProtection) *agentv1.CallResult {
	ctx = resource.WithOutcome(ctx, func(out resource.Outcome) {
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{
			Id: callID, Outcome: &agentv1.StepOutcome{DockerReceipt: out.DockerReceipts},
		}}})
	})
	if err := c.applyDockerMaintenance(ctx, ref, plan, protection); err != nil {
		return callFailed(err)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}}
}
