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
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

var (
	errImageChanged        = errors.New("the image or tag changed. Review the image action again")
	errImageProtected      = errors.New("a kept workload uses this tag. Choose another tag")
	errImageAuthRefused    = errors.New("the registry login could not be used on this server")
	errImageInfrastructure = errors.New("Dockwell uses this infrastructure tag. Choose another tag") //nolint:staticcheck // It starts with the product name.
)

func imageActionReference(action *v1.DockerImageAction) (registry.Ref, error) {
	return registry.Parse(imageReference(action))
}

func imageReference(action *v1.DockerImageAction) string {
	if tag := action.GetTag(); tag != nil {
		return tag.GetDestination()
	}
	return action.GetPull().GetReference()
}

func (c *calls) imageActionScope(ref resource.Ref, plan *v1.DockerMaintenance) error {
	if c.cfg.Identity == nil || c.cfg.disks == nil || c.cfg.Docker == nil || c.cfg.images == nil || c.cfg.Dir == "" ||
		ref.Scope != c.cfg.Identity.ServerID || ref.Kind != host.Kind || ref.Name != host.Name ||
		len(plan.GetTargets()) != 0 || plan.GetResumeCleanupToken() != "" {
		return errors.New("the agent could not verify the image action's server")
	}
	a := plan.GetImageAction()
	r, err := imageActionReference(a)
	if (a.GetTag() == nil) == (a.GetPull() == nil) || err != nil || (r.Digest != "" && (r.Tag != "" || a.GetTag() != nil)) {
		return errors.New("enter an image tag or a repository digest")
	}
	if tag := a.GetTag(); tag != nil {
		key := tag.GetSourceKey()
		if key.GetServerId() != ref.Scope || !docker.IsImageID(key.GetImageId()) {
			return errors.New("select the full image ID on this server")
		}
	}
	if len(a.GetApprovedLogins()) > 1 || a.GetTag() != nil && len(a.GetApprovedLogins()) != 0 {
		return errImageAuthRefused
	}
	return nil
}

func (c *calls) imageActionLogin(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte) (context.Context, registry.Auth, error) {
	approved := plan.GetImageAction().GetApprovedLogins()
	if len(approved) == 0 && len(sealed) == 0 {
		return ctx, registry.Auth{}, nil
	}
	if len(approved) != 1 || len(sealed) == 0 {
		return ctx, registry.Auth{}, errImageAuthRefused
	}
	a, err := c.openLogin(resource.DockerImageAuthRef(ref, plan), sealed)
	if err != nil || a.Name != approved[0].GetName() || a.Registry != approved[0].GetHost() {
		return ctx, registry.Auth{}, errImageAuthRefused
	}
	target, err := imageActionReference(plan.GetImageAction())
	if err != nil || target.Registry != a.Registry {
		return ctx, registry.Auth{}, errImageAuthRefused
	}
	return registry.WithAuth(ctx, a), a, nil
}

func (c *calls) resolveDockerImage(ctx context.Context, r registry.Ref) (*v1.DockerRegistryDescriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, imageReadTimeout)
	defer cancel()
	im := c.cfg.images
	if im.o.Off != "" {
		return nil, errors.New("registry reads are disabled on this server")
	}
	client, platform := im.ready(ctx)
	release, err := im.turn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	descriptor, err := client.ResolveImage(ctx, r, platform)
	if err != nil {
		if errors.Is(err, registry.ErrMoved) || errors.Is(err, registry.ErrUnreadChallenge) {
			return nil, errImageAuthRefused
		}
		return nil, errors.New("the registry could not verify this image for the server's platform. Check the reference or choose a login")
	}
	return &v1.DockerRegistryDescriptor{Repository: r.Name(), IndexDigest: descriptor.Index, ManifestDigest: descriptor.Manifest, ConfigDigest: descriptor.Config, Platform: descriptor.Platform.String()}, nil
}

// imageBinding reads the alias or digest selected for review. A digest pull
// may already be present under the exact native manifest it would stage.
func (c *calls) imageBinding(ctx context.Context, action *v1.DockerImageAction, ref string) (*docker.Image, error) {
	local, err := c.localImage(ctx, ref)
	if err != nil || local != nil || !strings.Contains(ref, "@") {
		return local, err
	}
	d := action.GetDescriptor_()
	pinned := d.GetRepository() + "@" + d.GetManifestDigest()
	if pinned == ref {
		return nil, nil
	}
	return c.localImage(ctx, pinned)
}

// localImage reads only the binding. Engine's own response text can contain values.
func (c *calls) localImage(ctx context.Context, target string) (*docker.Image, error) {
	image, err := c.cfg.Docker.InspectImage(ctx, target)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	if err != nil || !docker.IsImageID(image.ID) {
		return nil, errors.New("the Engine could not verify the local image")
	}
	return image, nil
}

func imageID(image *docker.Image) string {
	if image == nil {
		return ""
	}
	return image.ID
}

func (c *calls) previewDockerImage(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte) error {
	a := plan.GetImageAction()
	target := docker.WithTag(imageReference(a))
	containerd := false
	if pull := a.GetPull(); pull != nil {
		r, _ := imageActionReference(a)
		target = r.String()
		if r.Digest == "" {
			target = docker.WithTag(target)
		}
		info, err := c.imageStore(ctx)
		if err != nil {
			return err
		}
		containerd = info.ContainerdImageStore()
		scoped, auth, err := c.imageActionLogin(ctx, ref, plan, sealed)
		if err != nil {
			return err
		}
		if auth.Registry != "" {
			if auth.Registry == registry.Hub && info.RegistryConfig != nil && len(info.RegistryConfig.Mirrors) != 0 {
				return resource.PlainlyBecause("registry_login_mirrors", "Docker Hub mirrors are enabled on this server. A saved login can't be sent through a mirror. Pull a public image or remove the mirrors before using this login.", errImageAuthRefused)
			}
			if ok, err := c.cfg.images.challengeFor(ctx, auth, false); err != nil || !ok {
				return errImageAuthRefused
			}
		}
		if a.Descriptor_, err = c.resolveDockerImage(scoped, r); err != nil {
			return err
		}
	} else {
		source := a.GetTag().GetSourceKey().GetImageId()
		image, err := c.localImage(ctx, source)
		if err != nil || imageID(image) != source {
			return errImageChanged
		}
	}
	local, err := c.imageBinding(ctx, a, target)
	if err != nil {
		return err
	}
	found := imageID(local)
	if pull := a.GetPull(); pull != nil && pull.ExpectedLocalImageId != nil && found != pull.GetExpectedLocalImageId() {
		return errImageChanged
	}
	if tag := a.GetTag(); tag != nil && found != "" && found != tag.GetSourceKey().GetImageId() && !tag.GetMoveExisting() {
		return errors.New("this tag points to another image. Choose another tag or review moving it")
	}
	binding := &v1.DockerImageBinding{Reference: target, ImageId: found, ExpectAbsent: found == ""}
	if local != nil && a.GetPull() != nil {
		binding.Local = c.localPullIdentity(ctx, local, a.GetDescriptor_(), containerd)
	}
	a.Bindings = []*v1.DockerImageBinding{binding}
	if resource.DockerImageRetargetsAlias(a) && c.infrastructureImageAlias(target) {
		return errImageInfrastructure
	}
	return nil
}

func (c *calls) imageStore(ctx context.Context) (*docker.Info, error) {
	info, err := c.cfg.Docker.Info(ctx)
	if err != nil {
		return nil, errors.New("the Engine could not verify its image store")
	}
	return info, nil
}

func (c *calls) localPullIdentity(ctx context.Context, image *docker.Image, candidate *v1.DockerRegistryDescriptor, containerd bool) *v1.DockerRegistryDescriptor {
	local := &v1.DockerRegistryDescriptor{Repository: candidate.GetRepository()}
	if image.OS != "" && image.Architecture != "" {
		local.Platform = (registry.Platform{OS: image.OS, Architecture: image.Architecture, Variant: image.Variant}).String()
	}
	if d := image.Descriptor; d != nil {
		if d.Digest == image.ID {
			switch d.MediaType {
			case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
				local.IndexDigest = d.Digest
			case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
				local.ManifestDigest = d.Digest
			}
		}
		return local
	}
	if containerd {
		// Older APIs can omit Descriptor. Their ID still names the OCI target.
		switch image.ID {
		case candidate.GetIndexDigest():
			local.IndexDigest = image.ID
		case candidate.GetManifestDigest():
			local.ManifestDigest = image.ID
		}
		return local
	}
	local.ConfigDigest = image.ID
	if image.ID == candidate.GetConfigDigest() {
		local.Platform = candidate.GetPlatform() // The reviewed config proves the platform.
		if c.hasLocalDigest(ctx, image, local.GetRepository(), candidate.GetIndexDigest()) {
			local.IndexDigest = candidate.GetIndexDigest()
		}
		if local.GetIndexDigest() == "" && c.hasLocalDigest(ctx, image, local.GetRepository(), candidate.GetManifestDigest()) {
			local.ManifestDigest = candidate.GetManifestDigest()
		}
	}
	return local
}

func (c *calls) hasLocalDigest(ctx context.Context, image *docker.Image, repo, digest string) bool {
	if digest == "" {
		return false
	}
	want := docker.Repository(repo)
	if slices.ContainsFunc(image.RepoDigests, func(ref string) bool {
		_, known, ok := strings.Cut(ref, "@")
		return ok && known == digest && docker.Repository(ref) == want
	}) {
		return true
	}
	pinned, err := c.localImage(ctx, repo+"@"+digest)
	return err == nil && imageID(pinned) == image.ID
}

func (c *calls) planImageAction(ctx context.Context, desired *agentv1.DesiredResource) (*agentv1.CallResult, bool) {
	plan := proto.CloneOf(desired.GetDockerMaintenance())
	ref := resourcepb.FromRef(desired.GetResource())
	if err := c.imageActionScope(ref, plan); err != nil {
		return callFailed(err), true
	}
	if plan.GetChangeId() == "" {
		if err := c.previewDockerImage(ctx, ref, plan, desired.GetDockerRegistryAuth()); err != nil {
			return callFailed(err), true
		}
	} else if _, err := imageActionBinding(plan); err != nil {
		return callFailed(err), true
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionUpdate, DockerMaintenance: plan}}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Plan{Plan: &agentv1.PlanResult{Steps: []*v1.PlanStep{resourcepb.Step(step, "")}}}}, true
}

func imageActionBinding(plan *v1.DockerMaintenance) (*v1.DockerImageBinding, error) {
	a := plan.GetImageAction()
	if len(a.GetBindings()) != 1 {
		return nil, errors.New("the reviewed image binding is missing")
	}
	b := a.GetBindings()[0]
	target := imageReference(a)
	r, err := imageActionReference(a)
	if err != nil {
		return nil, errImageChanged
	}
	if r.Digest == "" {
		target = docker.WithTag(target)
	}
	if b.GetReference() != target || b.GetExpectAbsent() != (b.GetImageId() == "") || b.GetImageId() != "" && !docker.IsImageID(b.GetImageId()) {
		return nil, errImageChanged
	}
	if tag := a.GetTag(); tag != nil && b.GetImageId() != "" &&
		b.GetImageId() != tag.GetSourceKey().GetImageId() && !tag.GetMoveExisting() {
		return nil, errImageChanged
	}
	if a.GetPull() != nil {
		d := a.GetDescriptor_()
		if d.GetRepository() != r.Name() || !registry.IsDigest(d.GetManifestDigest()) || !docker.IsImageID(d.GetConfigDigest()) ||
			d.GetPlatform() == "" || d.GetIndexDigest() != "" && !registry.IsDigest(d.GetIndexDigest()) {
			return nil, errImageChanged
		}
	}
	return b, nil
}

func (c *calls) runImageAction(ctx context.Context, callID uint64, ref resource.Ref, desired *agentv1.DesiredResource, protected *agentv1.DockerCleanupProtection) *agentv1.CallResult {
	plan := desired.GetDockerMaintenance()
	if err := c.imageActionScope(ref, plan); err != nil {
		return callFailed(err)
	}
	receipt, err := c.applyImageAction(ctx, ref, plan, desired.GetDockerRegistryAuth(), protected)
	if receipt != nil {
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{Id: callID, Outcome: &agentv1.StepOutcome{DockerImageReceipt: []*v1.DockerImageReceipt{receipt}}}}})
	}
	if err != nil {
		return callFailed(err)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}}
}

func (c *calls) applyImageAction(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte, protected *agentv1.DockerCleanupProtection) (*v1.DockerImageReceipt, error) {
	b, err := imageActionBinding(plan)
	if err != nil {
		return nil, err
	}
	created, err := dwid.Parse(dwid.Change, plan.GetChangeId())
	if err != nil || time.Since(created) >= cleanupReceiptRetention {
		return nil, errors.New("this image review expired. Review it again")
	}
	a := plan.GetImageAction()
	target := b.GetReference()
	want := a.GetTag().GetSourceKey().GetImageId()
	containerd := false
	if a.GetPull() != nil {
		info, err := c.imageStore(ctx)
		if err != nil {
			return nil, err
		}
		containerd = info.ContainerdImageStore()
		want = a.GetDescriptor_().GetConfigDigest()
		if containerd {
			want = a.GetDescriptor_().GetManifestDigest()
		}
	}
	receipt := &v1.DockerImageReceipt{Reference: target, PreviousImageId: b.GetImageId(), Digest: a.GetDescriptor_().GetManifestDigest(), ImageKey: &v1.DockerImageSourceKey{ServerId: ref.Scope, ImageId: want}}
	fail := func(outcome v1.DockerImageOutcome, reason v1.DockerImageReason, err error) (*v1.DockerImageReceipt, error) {
		receipt.Outcome, receipt.Reason = outcome, reason
		return receipt, err
	}
	// The journal shares Cleanup's expiry patrol but has a separate name and shape.
	dir := filepath.Join(c.cfg.Dir, "docker-image-actions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("the agent could not create an image action receipt")
	}
	file := filepath.Join(dir, plan.GetChangeId()+".json")
	disks := c.cfg.disks
	disks.cleanupMu.Lock()
	defer disks.cleanupMu.Unlock()
	if previous, err := c.savedImageOutcome(ctx, file, receipt); previous != nil || err != nil {
		return previous, err
	}
	recheck := func() error {
		current, err := c.imageBinding(ctx, a, target)
		if err != nil || imageID(current) != b.GetImageId() {
			return errImageChanged
		}
		if protected == nil {
			return errImageProtected
		}
		if b.GetImageId() != want && !strings.Contains(target, "@") {
			if c.infrastructureImageAlias(target) {
				return errImageInfrastructure
			}
			if resource.DockerAliasProtected(target, protected.GetImages()) {
				return errImageProtected
			}
			if b.GetImageId() != "" && !resource.DockerImageNeedsPasskey(a) {
				return errImageChanged
			}
		}
		return nil
	}
	refuseBinding := func(err error) (*v1.DockerImageReceipt, error) {
		reason := v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE
		if errors.Is(err, errImageProtected) || errors.Is(err, errImageInfrastructure) {
			reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_PROTECTED
		}
		return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED, reason, err)
	}
	if err := recheck(); err != nil {
		return refuseBinding(err)
	}
	if a.GetPull() != nil {
		release := disks.holdPulledImage(want)
		defer release()
		disks.cleanupMu.Unlock()
		pinned, pullErr := c.stageReviewedImage(ctx, ref, plan, sealed)
		disks.cleanupMu.Lock()
		if pullErr != nil {
			switch {
			case errors.Is(pullErr, errImageChanged):
				return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED, v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE, pullErr)
			case errors.Is(pullErr, errImageAuthRefused):
				return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED, v1.DockerImageReason_DOCKER_IMAGE_REASON_CREDENTIAL_REFUSED, pullErr)
			default:
				return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED, pullErr)
			}
		}
		if previous, err := c.savedImageOutcome(ctx, file, receipt); previous != nil || err != nil {
			return previous, err
		}
		loaded, err := c.localImage(ctx, pinned)
		if err != nil || imageID(loaded) != want || containerd &&
			c.localPullIdentity(ctx, loaded, a.GetDescriptor_(), true).GetManifestDigest() != a.GetDescriptor_().GetManifestDigest() {
			return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED, errors.New("the Engine returned a different image than the registry described"))
		}
		r, _ := imageActionReference(a)
		if r.Digest != "" {
			return saveImageOutcome(file, receipt)
		}
	} else if current, err := c.localImage(ctx, want); err != nil || imageID(current) != want {
		return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED, v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE, errImageChanged)
	}
	if err := recheck(); err != nil {
		return refuseBinding(err)
	}
	if b.GetImageId() == want {
		return saveImageOutcome(file, receipt)
	}
	// Write intent before touching an alias. A reconnect checks the result and
	// never repeats a write whose response was lost.
	if err := saveImageJournal(file, receipt); err != nil {
		return nil, errors.New("the agent could not record the image action")
	}
	tagErr := c.cfg.Docker.TagImage(ctx, want, target)
	disks.invalidateImages()
	current, readErr := c.localImage(ctx, target)
	if readErr != nil || imageID(current) != want {
		if tagErr != nil && readErr == nil && imageID(current) == b.GetImageId() {
			return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED, errors.New("the Engine did not set the reviewed tag"))
		}
		return fail(v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED, errors.New("the resulting tag could not be verified. Check the image before reviewing again"))
	}
	return saveImageOutcome(file, receipt)
}

// holdPulledImage is called with the cleanup slot held, both to acquire and release.
func (d *diskUse) holdPulledImage(imageID string) func() {
	if d.imagePulls == nil {
		d.imagePulls = make(map[string]int)
	}
	d.imagePulls[imageID]++
	return func() {
		d.imagePulls[imageID]--
		if d.imagePulls[imageID] == 0 {
			delete(d.imagePulls, imageID)
		}
	}
}

func (c *calls) stageReviewedImage(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte) (string, error) {
	a := plan.GetImageAction()
	scoped, auth, err := c.imageActionLogin(ctx, ref, plan, sealed)
	if err != nil {
		return "", err
	}
	r, _ := imageActionReference(a)
	fresh, err := c.resolveDockerImage(scoped, r)
	if errors.Is(err, errImageAuthRefused) {
		return "", err
	}
	if err != nil || !proto.Equal(fresh, a.GetDescriptor_()) {
		return "", errImageChanged
	}
	if auth.Registry != "" {
		ok, err := c.cfg.images.challenge(ctx, auth)
		if err != nil || !ok {
			return "", errImageAuthRefused
		}
		ctx = docker.WithRegistryAuth(ctx, auth.Registry, auth.Header())
	}
	pinned := fresh.GetRepository() + "@" + fresh.GetManifestDigest()
	resource.Stage(ctx, resource.PhasePulling, "Pulling image")
	defer c.cfg.disks.invalidateImages()
	if err := c.cfg.Docker.PullImageBytes(ctx, pinned, func(done, total int64) {
		resource.Tell(ctx, resource.Activity{Message: "Pulling image", Phase: resource.PhasePulling, Done: done, Total: total})
	}); err != nil {
		return "", errors.New("the Engine could not pull the reviewed image")
	}
	return pinned, nil
}

func (c *calls) savedImageOutcome(ctx context.Context, file string, receipt *v1.DockerImageReceipt) (*v1.DockerImageReceipt, error) {
	data, err := os.ReadFile(file) //nolint:gosec // Agent data path with a validated change ID.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var saved v1.DockerImageReceipt
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.GetReference() != receipt.GetReference() ||
		!proto.Equal(saved.GetImageKey(), receipt.GetImageKey()) || saved.GetPreviousImageId() != receipt.GetPreviousImageId() ||
		saved.GetDigest() != receipt.GetDigest() {
		return nil, errors.New("the agent could not verify its saved image action receipt")
	}
	if saved.GetOutcome() == v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED {
		return &saved, nil
	}
	current, err := c.localImage(ctx, saved.GetReference())
	if err == nil && imageID(current) == saved.GetImageKey().GetImageId() {
		return saveImageOutcome(file, receipt)
	}
	saved.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN
	saved.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
	return &saved, errors.New("the earlier image action's result could not be verified. Check the image before reviewing again")
}

func saveImageOutcome(file string, receipt *v1.DockerImageReceipt) (*v1.DockerImageReceipt, error) {
	receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED
	if err := saveImageJournal(file, receipt); err != nil {
		receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN
		receipt.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
		return receipt, errors.New("the result was seen but its receipt could not be saved")
	}
	return receipt, nil
}

func saveImageJournal(file string, receipt *v1.DockerImageReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return writeDockerCleanupFile(file, data)
}

func pruneDockerImageActions(dir string, now time.Time) {
	files, err := os.Open(filepath.Join(dir, "docker-image-actions")) //nolint:gosec // Agent data directory.
	if err != nil {
		return
	}
	defer files.Close()
	for {
		entries, err := files.ReadDir(64)
		for _, entry := range entries {
			name, ok := strings.CutSuffix(entry.Name(), ".json")
			if !ok || !entry.Type().IsRegular() {
				continue
			}
			if created, parseErr := dwid.Parse(dwid.Change, name); parseErr == nil && now.Sub(created) >= cleanupReceiptRetention {
				_ = os.Remove(filepath.Join(files.Name(), entry.Name()))
			}
		}
		if err != nil {
			return
		}
	}
}
