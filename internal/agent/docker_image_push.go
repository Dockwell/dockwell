// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

func (c *calls) prepareImagePush(ctx context.Context, owner string, selection *v1.DockerImageExport) (*v1.DockerArtifactInfo, *v1.DockerRegistryDescriptor, error) {
	key := selection.GetSourceKey()
	if key.GetServerId() != c.server || !docker.IsImageID(key.GetImageId()) || c.cfg.disks == nil || c.cfg.Docker == nil {
		return nil, nil, artifact.ErrUnavailable
	}
	release, err := c.cfg.artifacts.TakeValidation(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	transfer, err := c.cfg.takeArtifactTransfer(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer transfer()
	disks := c.cfg.disks
	disks.cleanupMu.Lock()
	image, err := c.localImage(ctx, key.GetImageId())
	if err != nil || imageID(image) != key.GetImageId() {
		disks.cleanupMu.Unlock()
		return nil, nil, errImageChanged
	}
	hold := disks.holdPulledImage(key.GetImageId())
	disks.cleanupMu.Unlock()
	defer func() { disks.cleanupMu.Lock(); hold(); disks.cleanupMu.Unlock() }()
	platform := strings.TrimSuffix(image.OS+"/"+image.Architecture+"/"+image.Variant, "/")
	if image.OS == "" || image.Architecture == "" || selection.GetPlatform() != "" && selection.GetPlatform() != platform {
		return nil, nil, artifact.ErrArchive
	}
	engine, err := c.imageStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	var manifests []string
	if engine.ContainerdImageStore() {
		manifest, err := c.cfg.Docker.ImagePlatformManifest(ctx, image, platform)
		if err != nil {
			return nil, nil, artifact.ErrArchive
		}
		manifests = append(manifests, manifest)
	}
	src, err := c.cfg.Docker.SaveImage(ctx, key.GetImageId(), platform)
	if err != nil {
		return nil, nil, artifact.ErrArchive
	}
	defer src.Close()
	return c.cfg.artifacts.PreparePush(ctx, owner, src, func(a *artifact.Archive, config string) bool {
		got, layers, ok := a.Identity(config)
		return ok && got == platform && slices.Equal(layers, image.RootFS.Layers) && (engine.ContainerdImageStore() || config == key.GetImageId())
	}, manifests...)
}

func (c *calls) pushPublisher(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte, write bool) (*registry.Publisher, registry.Ref, error) {
	_, login, err := c.imageActionLogin(ctx, ref, plan, sealed)
	if err != nil || login.Registry == "" {
		return nil, registry.Ref{}, errImageAuthRefused
	}
	target, err := imageActionReference(plan.GetImageAction())
	if err != nil {
		return nil, registry.Ref{}, errImageChanged
	}
	client, _ := c.cfg.images.ready(ctx)
	pub, err := client.PublisherFor(target, login, write)
	return pub, target, err
}

func (c *calls) previewImagePush(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte) error {
	a := plan.GetImageAction()
	if a.GetPush().GetSourceKey() != nil {
		if err := c.checkPushSource(a); err != nil {
			return err
		}
	}
	pub, target, err := c.pushPublisher(ctx, ref, plan, sealed, a.GetPush().GetSourceKey() != nil)
	if err != nil {
		return pushFailure(err)
	}
	got, err := pub.ManifestDigest(ctx, target.Reference())
	if err != nil {
		return pushFailure(err)
	}
	if a.GetPush().GetSourceKey() != nil && got != "" && got != a.GetDescriptor_().GetManifestDigest() {
		return pushFailure(registry.ErrPushConflict)
	}
	if a.GetPush().GetSourceKey() == nil && got != "" {
		a.Descriptor_ = &v1.DockerRegistryDescriptor{Repository: target.Name(), ManifestDigest: got}
	}
	a.Bindings = []*v1.DockerImageBinding{{Reference: target.String(), ExpectAbsent: got == ""}}
	return nil
}

func (c *calls) checkPushSource(a *v1.DockerImageAction) error {
	d := a.GetDescriptor_()
	target, err := imageActionReference(a)
	if err != nil || c.cfg.artifacts == nil || !artifact.ValidID(a.GetStagedSourceId()) || d.GetRepository() != target.Name() ||
		!docker.IsImageID(d.GetManifestDigest()) || !docker.IsImageID(d.GetConfigDigest()) || d.GetPlatform() == "" ||
		a.GetPush().GetDigestOnly() != (target.Digest != "") || target.Digest != "" && target.Digest != d.GetManifestDigest() {
		return errImageChanged
	}
	facts, err := c.cfg.artifacts.Status(a.GetStagedSourceId(), "")
	if err != nil || facts.GetKind() != artifact.PushSource || facts.GetState() != artifact.Ready || facts.GetSha256() != a.GetSourceSha256() {
		return artifact.ErrUnavailable
	}
	return nil
}

func pushFailure(err error) error {
	switch {
	case errors.Is(err, registry.ErrPushTimeout), errors.Is(err, context.DeadlineExceeded):
		return resource.PlainlyBecause("docker_push_not_published", "Not published. The push timed out before sending the manifest", registry.ErrPushTimeout)
	case errors.Is(err, registry.ErrPushRefused):
		return resource.PlainlyBecause("docker_push_registry_refused", "The registry refused the upload", err)
	case errors.Is(err, registry.ErrPushConflict):
		return resource.PlainlyBecause("docker_push_conflict", "This destination serves a different digest. Choose another tag", err)
	case errors.Is(err, registry.ErrMoved), errors.Is(err, registry.ErrUnreadChallenge):
		return resource.PlainlyBecause("docker_push_realm", "The registry's sign-in changed. Check the registry login", err)
	case errors.Is(err, registry.ErrSignIn), errors.Is(err, errImageAuthRefused):
		return resource.PlainlyBecause("docker_push_login", "The registry login could not be used for this repository", err)
	default:
		return err
	}
}

func (c *calls) applyImagePush(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, sealed []byte) (*v1.DockerImageReceipt, error) {
	a := plan.GetImageAction()
	created, err := id.Parse(id.Change, plan.GetChangeId())
	if err != nil || time.Since(created) >= cleanupReceiptRetention || a.GetPush().GetSourceKey() == nil || len(a.GetBindings()) != 1 || c.cfg.artifacts == nil {
		return nil, errImageChanged
	}
	b, d := a.GetBindings()[0], a.GetDescriptor_()
	digest := d.GetManifestDigest()
	if b.GetReference() != imageReference(a) || b.GetImageId() != "" || b.GetLocal() != nil {
		return nil, errImageChanged
	}
	receipt := &v1.DockerImageReceipt{Reference: b.GetReference(), Digest: digest, ImageKey: a.GetPush().GetSourceKey()}
	dir := filepath.Join(c.cfg.Dir, "docker-image-actions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, registry.ErrPushRefused
	}
	file := filepath.Join(dir, plan.GetChangeId()+".json")
	if deadline, ok := ctx.Deadline(); ok {
		// Leave time to deliver a definite receipt before the call expires.
		grace := min(3*time.Second, time.Until(deadline)/10)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-grace))
		defer cancel()
	}
	if saved, err := readPushReceipt(file, receipt); saved != nil || err != nil {
		return saved, pushFailure(err)
	}
	if err := c.checkPushSource(a); err != nil {
		return nil, err
	}
	receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED
	transfer, err := c.cfg.artifacts.TakePush(ctx, func() { resource.Progress(ctx, "Waiting for a transfer slot") })
	if err != nil {
		return receipt, pushFailure(err)
	}
	defer transfer()
	resource.Progress(ctx, "Publishing image")
	pub, target, err := c.pushPublisher(ctx, ref, plan, sealed, true)
	if err != nil {
		return receipt, pushFailure(err)
	}
	sending, usedJournal := false, false
	err = c.cfg.artifacts.WithPush(ctx, a.GetStagedSourceId(), plan.GetChangeId(), a.GetSourceSha256(), func(source *artifact.Distribution) error {
		if saved, err := readPushReceipt(file, receipt); saved != nil || err != nil {
			if saved != nil {
				receipt = saved
			}
			usedJournal = true
			// Only one apply can hold the stage lease. Check again in case an
			// earlier call wrote a journal after we first looked.
			return err
		}
		if source.Digest != digest || source.Config != d.GetConfigDigest() || source.Platform != d.GetPlatform() {
			return errImageChanged
		}
		got, err := pub.ManifestDigest(ctx, target.Reference())
		if err != nil {
			return err
		}
		if got == digest {
			receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNCHANGED
			return saveImageJournal(file, receipt)
		}
		if got != "" || !b.GetExpectAbsent() {
			return registry.ErrPushConflict
		}
		resource.Progress(ctx, "Uploading image blobs")
		for _, blob := range source.Blobs {
			if err := pub.PutBlob(ctx, blob.Digest, blob.Size, blob.Reader); err != nil {
				return err
			}
		}
		err = pub.PutManifest(ctx, target.Reference(), digest, bytes.NewReader(source.Manifest), int64(len(source.Manifest)), func() error {
			receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN
			receipt.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
			if err := saveImageJournal(file, receipt); err != nil {
				return registry.ErrPushRefused
			}
			sending = true
			return nil
		})
		if err != nil {
			return err
		}
		receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNCHANGED
		if sending {
			receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED
		}
		receipt.Reason = 0
		if err := saveImageJournal(file, receipt); err != nil {
			return registry.ErrPushUnknown
		}
		return nil
	})
	if usedJournal {
		return receipt, pushFailure(err)
	}
	switch {
	case err == nil:
		_, _ = c.cfg.artifacts.Abort(a.GetStagedSourceId(), "")
	case errors.Is(err, registry.ErrPushConflict):
		receipt.Outcome, receipt.Reason = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED, v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE
	case errors.Is(err, registry.ErrPushUnknown):
		receipt.Outcome, receipt.Reason = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
	case errors.Is(err, registry.ErrMoved), errors.Is(err, registry.ErrUnreadChallenge):
		receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED
		receipt.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_REALM_CHANGED
	case errors.Is(err, registry.ErrSignIn):
		receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED
		receipt.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_CREDENTIAL_REFUSED
	case errors.Is(err, registry.ErrPushRefused):
		receipt.Outcome, receipt.Reason = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
	default:
		receipt.Outcome, receipt.Reason = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED, v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
	}
	if sending && err != nil && !errors.Is(err, registry.ErrPushUnknown) {
		if saveImageJournal(file, receipt) != nil {
			receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN
			return receipt, registry.ErrPushUnknown
		}
	}
	if err != nil && !sending && ctx.Err() != nil && !errors.Is(err, registry.ErrPushUnknown) {
		err = ctx.Err()
	}
	return receipt, pushFailure(err)
}

func readPushReceipt(file string, expected *v1.DockerImageReceipt) (*v1.DockerImageReceipt, error) {
	data, err := os.ReadFile(file) //nolint:gosec // private state and a validated change ID
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var saved v1.DockerImageReceipt
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.GetReference() != expected.GetReference() ||
		saved.GetDigest() != expected.GetDigest() || !proto.Equal(saved.GetImageKey(), expected.GetImageKey()) {
		return nil, registry.ErrPushUnknown
	}
	switch saved.GetOutcome() {
	case v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED, v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNCHANGED:
		return &saved, nil
	case v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED:
		return &saved, registry.ErrPushRefused
	case v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED:
		return &saved, registry.ErrPushTimeout
	default:
		return &saved, registry.ErrPushUnknown
	}
}
