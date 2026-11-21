// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
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
)

func (c *calls) checkDockerMaintenance(ref resource.Ref, maintenance *v1.DockerMaintenance) error {
	targets := maintenance.GetTargets()
	if c.cfg.Docker == nil || c.cfg.disks == nil || c.cfg.Dir == "" || c.cfg.Identity == nil ||
		ref.Kind != host.Kind || ref.Name != host.Name || ref.Scope != c.cfg.Identity.ServerID ||
		maintenance.GetChangeId() == "" || len(maintenance.GetChangeId()) > 80 || len(targets) > 12 {
		return errors.New("this agent could not verify the cleanup scope")
	}
	created, err := dwid.Parse(dwid.Change, maintenance.GetChangeId())
	if err != nil || time.Since(created) >= cleanupReceiptRetention {
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
	for _, target := range targets {
		id := target.GetEngineId()
		tags, digests := target.GetImageTags(), target.GetImageDigests()
		var valid bool
		switch target.GetKind() {
		case cleanupCache:
			valid = target.GetReason() == reasonIdle && docker.IsBuildCacheID(id) && len(tags)+len(digests) == 0
		case cleanupImage:
			valid = target.GetReason() == imageCleanupReason(tags, digests) && docker.IsImageID(id) && len(tags) <= 1 && reviewableImageRefs(tags, digests) && oneImageRepository(tags, digests) && target.GetEvidence() == imageCleanupEvidence(id, tags, digests)
		case cleanupNetwork:
			valid = target.GetReason() == reasonUnused && docker.IsFullID(id) && target.GetName() != "" && len(tags)+len(digests) == 0
		default:
		}
		if !valid || target.GetServerId() != ref.Scope ||
			len(target.GetEvidence()) != 64 || seen[id] {
			return errors.New("the reviewed cleanup contains an unsupported target")
		}
		seen[id] = true
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
	file := filepath.Join(dir, cleanupEvidence(maintenance.GetChangeId())+".json")
	targets := maintenance.GetTargets()
	receipts, err := readDockerReceipts(file, targets)
	if err != nil {
		return err
	}
	defer func() { resource.Report(ctx, resource.Outcome{DockerReceipts: receipts}) }()

	_, df, err := c.cfg.disks.snapshot(ctx, true)
	if err != nil {
		return err
	}
	// Observe interrupted removals without dispatching them again.
	for _, receipt := range receipts {
		if receipt.GetState() == receiptUnknown {
			receipt.AfterBytes = cleanupObjectBytes(df, receipt.GetTarget())
		}
	}

	if c.cleanupDisabled() {
		return errCleanupDisabled
	}
	if slices.ContainsFunc(receipts, func(r *v1.DockerReceipt) bool { return r.GetState() != receiptRemoved }) {
		return errors.New("some Docker artifacts could not be confirmed removed. Review their receipts and preview cleanup again")
	}

	// Recheck the untouched part before making any new removals. Each object
	// is checked again immediately before dispatch.
	var pending []*v1.DockerReceipt
	drift := false
	for _, target := range targets[len(receipts):] {
		identity, state := c.recheckCleanup(ctx, df, target, protection)
		drift = drift || state != receiptUnknown
		pending = append(pending, &v1.DockerReceipt{
			Target: target, State: state, RecheckedEngineId: identity,
			BeforeBytes: cleanupObjectBytes(df, target),
		})
	}
	if drift {
		for _, receipt := range pending {
			if receipt.State == receiptUnknown {
				receipt.State = receiptChanged
			}
		}
		receipts = append(receipts, pending...)
		if err := saveDockerReceipts(file, receipts); err != nil {
			return err
		}
		return errors.New("the Docker cleanup candidates changed. Review cleanup again")
	}
	var operationErr error
	first := len(receipts)
	for _, receipt := range pending {
		if ctx.Err() != nil {
			operationErr = ctx.Err()
			break
		}
		receipts = append(receipts, receipt)
		journal := func() error { return saveDockerReceipts(file, receipts) }
		operationErr = c.removeCleanupTarget(ctx, receipt, protection, journal)
		if operationErr = errors.Join(operationErr, journal()); operationErr != nil {
			break
		}
	}
	if len(receipts) > first {
		if _, after, err := c.cfg.disks.snapshot(ctx, true); err == nil {
			for _, receipt := range receipts[first:] {
				receipt.AfterBytes = cleanupObjectBytes(after, receipt.GetTarget())
			}
		}
		operationErr = errors.Join(operationErr, saveDockerReceipts(file, receipts))
	}
	if operationErr != nil {
		return operationErr
	}
	if slices.ContainsFunc(receipts, func(r *v1.DockerReceipt) bool { return r.GetState() != receiptRemoved }) {
		return errors.New("some Docker artifacts could not be confirmed removed. Review their receipts and preview cleanup again")
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
	default:
		return nil
	}
}

func (c *calls) recheckCleanup(ctx context.Context, df *docker.DiskUsage, target *v1.DockerCleanupTarget, protection *agentv1.DockerCleanupProtection) (string, v1.DockerReceiptState) {
	id := target.GetEngineId()
	if target.GetKind() == cleanupCache {
		if record := cacheRecord(df, id); record != nil {
			if sameCleanupTarget(target, cacheTarget(*record, c.cfg.Docker.ExactCacheRemoval())) {
				return id, receiptUnknown
			}
			return id, receiptChanged
		}
		return "", receiptChanged
	}
	if target.GetKind() == cleanupImage {
		return c.recheckCleanupImage(ctx, target, protection.GetImages())
	}
	held, err := c.dockerOccupancy(ctx)
	if err != nil {
		return "", receiptFailed
	}
	network, err := c.cfg.Docker.InspectNetwork(ctx, id)
	if err != nil {
		return "", cleanupCheckFailure(err)
	}
	if !sameCleanupTarget(target, networkTarget(network, held, protection.GetNetworks())) {
		return network.ID, receiptChanged
	}
	return network.ID, receiptUnknown
}

func (c *calls) recheckCleanupImage(ctx context.Context, target *v1.DockerCleanupTarget, protected []string) (string, v1.DockerReceiptState) {
	id := target.GetEngineId()
	if c.cfg.disks.imagePulls[id] > 0 {
		return id, receiptChanged
	}
	held, err := c.dockerOccupancy(ctx)
	if err != nil {
		return "", receiptFailed
	}
	for _, refs := range [][]string{target.GetImageTags(), target.GetImageDigests()} {
		for _, ref := range refs {
			image, err := c.cfg.Docker.InspectImage(ctx, ref)
			if err != nil {
				return "", cleanupCheckFailure(err)
			}
			if image.ID != id {
				return image.ID, receiptChanged
			}
		}
	}
	image, err := c.cfg.Docker.InspectImage(ctx, id)
	if err != nil {
		return "", cleanupCheckFailure(err)
	}
	current := c.imageCleanupTarget(docker.DiskUsageImage{
		ID: image.ID, RepoTags: image.RepoTags, RepoDigests: image.RepoDigests, Labels: image.Config.Labels,
	}, held, protected)
	if !sameCleanupTarget(target, current) {
		return image.ID, receiptChanged
	}
	return image.ID, receiptUnknown
}

func cleanupCheckFailure(err error) v1.DockerReceiptState {
	if docker.IsNotFound(err) {
		return receiptChanged
	}
	return receiptFailed
}

func (c *calls) removeCleanupTarget(ctx context.Context, receipt *v1.DockerReceipt, protection *agentv1.DockerCleanupProtection, journal func() error) error {
	target := receipt.GetTarget()
	if target.GetKind() == cleanupCache {
		return c.removeCleanupCache(ctx, receipt, journal)
	}
	// The pending receipt is durable before the final check. There is no
	// filesystem write between the last inspect and the Engine delete.
	if err := journal(); err != nil {
		return err
	}
	receipt.RecheckedEngineId, receipt.State = c.recheckCleanup(ctx, nil, target, protection)
	if receipt.State != receiptUnknown {
		return nil
	}
	switch target.GetKind() {
	case cleanupNetwork:
		err := c.cfg.Docker.RemoveNetwork(ctx, target.GetEngineId())
		receipt.State = removalState(err, err == nil)
	case cleanupImage:
		err := c.cfg.Docker.RemoveReviewedImage(ctx, target.GetEngineId())
		receipt.State = removalState(err, err == nil)
		if docker.IsConflict(err) {
			receipt.Reason = v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IMAGE_ID_REFUSED
		}
	default:
	}
	return nil
}

const cleanupReceiptRetention = 30 * 24 * time.Hour

var errCleanupDisabled = errors.New("cleanup is disabled on this server after an unverified build cache removal")

func (c *calls) cleanupBlockFile() string {
	return filepath.Join(c.cfg.Dir, "docker-cleanup", "disabled")
}

func (c *calls) readCleanupPause() (*v1.DockerCleanupPause, error) {
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
	pause := &v1.DockerCleanupPause{}
	if json.Unmarshal(data, pause) != nil || !docker.IsFullID(pause.GetToken()) {
		// The first version wrote a plain-text marker. Keep the server paused.
		pause = &v1.DockerCleanupPause{
			Token:  cleanupEvidence(string(data)),
			Reason: v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_CACHE_UNVERIFIED,
		}
	}
	return pause, nil
}

func (c *calls) cleanupDisabled() bool {
	pause, err := c.readCleanupPause()
	return err != nil || pause != nil
}

func (c *calls) resumeDockerCleanup(token string) error {
	pause, err := c.readCleanupPause()
	if err != nil || pause == nil {
		return err
	}
	if pause.GetToken() != token {
		return errors.New("the Docker cleanup pause changed. Review it again")
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
	receipt.RecheckedEngineId, receipt.State = c.recheckCleanup(ctx, df, receipt.GetTarget(), nil)
	if receipt.State != receiptUnknown {
		return nil
	}
	record := cacheRecord(df, id)
	if err := journal(); err != nil {
		return err
	}
	pause := &v1.DockerCleanupPause{
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
	receipt.State = removalState(removeErr, !remaining[id])
	if !remaining[id] && !slices.Contains(deleted, id) {
		receipt.State = receiptUnknown
	}
	return c.clearCleanupPause()
}

func (c *calls) saveCleanupPause(pause *v1.DockerCleanupPause) error {
	data, err := json.Marshal(pause)
	if err != nil {
		return err
	}
	return writeDockerCleanupFile(c.cleanupBlockFile(), data)
}

func removalState(err error, removed bool) v1.DockerReceiptState {
	switch {
	case docker.IsInUse(err) || docker.IsNotFound(err):
		return receiptChanged
	case docker.IsBadRequest(err):
		return receiptFailed
	case removed:
		return receiptRemoved
	case err == nil:
		return receiptChanged
	default:
		return receiptUnknown
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

func readDockerReceipts(file string, targets []*v1.DockerCleanupTarget) ([]*v1.DockerReceipt, error) {
	data, err := os.ReadFile(file) //nolint:gosec // Agent data path with a hashed change ID.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var receipts []*v1.DockerReceipt
	if err != nil || json.Unmarshal(data, &receipts) != nil || len(receipts) > len(targets) {
		return nil, errors.New("the agent could not verify its saved Docker cleanup receipts")
	}
	for i, r := range receipts {
		if !proto.Equal(r.GetTarget(), targets[i]) || r.GetState() == 0 {
			return nil, errors.New("the saved Docker cleanup receipts have a different scope")
		}
	}
	return receipts, nil
}

func saveDockerReceipts(file string, receipts []*v1.DockerReceipt) error {
	data, err := json.Marshal(receipts)
	if err != nil {
		return err
	}
	return writeDockerCleanupFile(file, data)
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
			digest, ok := strings.CutSuffix(entry.Name(), ".json")
			if !ok || !docker.IsFullID(digest) || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err == nil && now.Sub(info.ModTime()) >= cleanupReceiptRetention {
				_ = os.Remove(filepath.Join(files.Name(), entry.Name()))
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
