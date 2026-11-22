// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

func isImageArchive(a *v1.DockerImageAction) bool { return a.GetLoad() != nil || a.GetExport() != nil }

func (c *calls) checkArchiveAction(ctx context.Context, ref resource.Ref, a *v1.DockerImageAction, protection *agentv1.DockerCleanupProtection) error {
	if c.cfg.artifacts == nil || !artifact.ValidID(a.GetStagedSourceId()) || a.GetTag() != nil || a.GetPull() != nil || len(a.GetApprovedLogins()) != 0 ||
		(a.GetLoad() == nil) == (a.GetExport() == nil) {
		return artifact.ErrUnavailable
	}
	facts, err := c.cfg.artifacts.Status(a.GetStagedSourceId(), "")
	if err != nil {
		return err
	}
	if into := a.GetLoad(); into != nil {
		if facts.GetKind() != artifact.Import || facts.GetState() != artifact.Ready || facts.GetSha256() != a.GetSourceSha256() ||
			into.GetArtifactId() != facts.GetArtifactId() || len(into.GetSelectedImageIds()) == 0 || len(into.GetSelectedImageIds()) > 64 || len(into.GetTags()) > 128 {
			return artifact.ErrArchive
		}
		selected := make(map[string]bool)
		for _, image := range into.GetSelectedImageIds() {
			if selected[image] || !slices.ContainsFunc(facts.GetImages(), func(row *v1.DockerArchiveImage) bool { return row.GetImageId() == image }) {
				return artifact.ErrArchive
			}
			selected[image] = true
		}
		seen := make(map[string]bool)
		for _, tag := range into.GetTags() {
			alias := tag.GetAlias()
			name, err := registry.Parse(alias)
			if !selected[tag.GetImageId()] || err != nil || name.Tag == "" || name.Digest != "" || name.Registry == docker.BuiltRegistry || name.String() != alias || seen[alias] {
				return artifact.ErrArchive
			}
			seen[alias] = true
			if c.infrastructureImageAlias(alias) || protection != nil && resource.DockerAliasProtected(alias, protection.GetImages()) {
				return errImageProtected
			}
			if current, err := c.localImage(ctx, alias); err != nil || current != nil {
				return errImageChanged
			}
		}
		return nil
	}
	out := a.GetExport()
	key := out.GetSourceKey()
	if key.GetServerId() != ref.Scope || !docker.IsImageID(key.GetImageId()) || len(out.GetTags()) > 128 || facts.GetKind() != artifact.Export {
		return artifact.ErrArchive
	}
	image, err := c.localImage(ctx, key.GetImageId())
	if err != nil || imageID(image) != key.GetImageId() {
		return errImageChanged
	}
	platform := strings.TrimSuffix(image.OS+"/"+image.Architecture+"/"+image.Variant, "/")
	if out.GetPlatform() != "" && out.GetPlatform() != platform {
		return artifact.ErrArchive
	}
	if out.Platform == "" {
		out.Platform = platform
	}
	seen := make(map[string]bool)
	for _, alias := range out.GetTags() {
		name, err := registry.Parse(alias)
		if err != nil || name.Tag == "" || name.Digest != "" || name.String() != alias || seen[alias] {
			return artifact.ErrArchive
		}
		seen[alias] = true
		if current, err := c.localImage(ctx, alias); err != nil || imageID(current) != key.GetImageId() {
			return errImageChanged
		}
	}
	return nil
}

func (c *calls) applyArchiveAction(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, protection *agentv1.DockerCleanupProtection) ([]*v1.DockerImageReceipt, error) {
	a := plan.GetImageAction()
	if _, err := id.Parse(id.Change, plan.GetChangeId()); err != nil {
		return nil, errImageChanged
	}
	if c.cfg.artifacts == nil {
		return nil, artifact.ErrUnavailable
	}
	validation, err := c.cfg.artifacts.TakeValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer validation()
	transfer, err := c.cfg.takeArtifactTransfer(ctx)
	if err != nil {
		return nil, err
	}
	defer transfer()
	disks := c.cfg.disks
	if out := a.GetExport(); out != nil {
		if err := c.cfg.artifacts.CheckPin(a.GetStagedSourceId(), plan.GetChangeId()); err != nil {
			return nil, err
		}
		disks.cleanupMu.Lock()
		if err := c.checkArchiveAction(ctx, ref, a, protection); err != nil {
			disks.cleanupMu.Unlock()
			return nil, err
		}
		key := out.GetSourceKey()
		release := disks.holdPulledImage(key.GetImageId())
		disks.cleanupMu.Unlock()
		defer func() { disks.cleanupMu.Lock(); release(); disks.cleanupMu.Unlock() }()
		engine, err := c.imageStore(ctx)
		if err != nil {
			return nil, err
		}
		image, err := c.localImage(ctx, key.GetImageId())
		if err != nil || imageID(image) != key.GetImageId() {
			return nil, errImageChanged
		}
		var manifests []string
		if engine.ContainerdImageStore() {
			manifest, err := c.cfg.Docker.ImagePlatformManifest(ctx, image, out.GetPlatform())
			if err != nil {
				return nil, errors.New("the Engine could not identify the reviewed platform manifest")
			}
			manifests = append(manifests, manifest)
		}
		input, err := c.cfg.Docker.SaveImage(ctx, key.GetImageId(), out.GetPlatform())
		if err != nil {
			return nil, errors.New("the Engine could not save the reviewed image")
		}
		defer input.Close()
		verify := func(ar *artifact.Archive, config string) bool {
			platform, layers, ok := ar.Identity(config)
			return ok && (engine.ContainerdImageStore() || config == key.GetImageId()) &&
				platform == out.GetPlatform() && slices.Equal(layers, image.RootFS.Layers)
		}
		ready, err := c.cfg.artifacts.Publish(ctx, a.GetStagedSourceId(), input, out.GetTags(), verify, manifests...)
		if err != nil {
			return nil, err
		}
		return []*v1.DockerImageReceipt{{Outcome: v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED, ImageKey: key, ArtifactId: ready.GetArtifactId(), SizeBytes: ready.GetSizeBytes(), Sha256: ready.GetSha256()}}, nil
	}

	if protection == nil {
		return nil, errImageProtected
	}
	dir := filepath.Join(c.cfg.Dir, "docker-image-actions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file := filepath.Join(dir, plan.GetChangeId()+".archive.json")
	var saved struct {
		Completed bool
		Receipts  []*v1.DockerImageReceipt
	}
	if data, err := os.ReadFile(file); err == nil { //nolint:gosec // the validated change ID names a journal in the agent data directory
		if json.Unmarshal(data, &saved) != nil || !saved.Completed {
			return nil, errors.New("the previous image load result is unknown. Check the images before reviewing again")
		}
		return saved.Receipts, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := c.cfg.artifacts.CheckPin(a.GetStagedSourceId(), plan.GetChangeId()); err != nil {
		return nil, err
	}
	var receipts []*v1.DockerImageReceipt
	err = c.cfg.artifacts.WithArchive(ctx, a.GetStagedSourceId(), a.GetSourceSha256(), func(ar *artifact.Archive) error {
		disks.cleanupMu.Lock()
		defer disks.cleanupMu.Unlock()
		// The lease lock held by WithArchive freezes the checked bytes. Check
		// the tag preconditions under the same mutation slot as cleanup.
		tags := a.GetLoad().GetTags()
		for _, t := range tags {
			name := t.GetAlias()
			if c.infrastructureImageAlias(name) || resource.DockerAliasProtected(name, protection.GetImages()) {
				return errImageProtected
			}
			if current, err := c.localImage(ctx, name); err != nil || current != nil {
				return errImageChanged
			}
		}
		engine, err := c.imageStore(ctx)
		if err != nil {
			return err
		}
		if err := ar.RoomForLoad(engine.DockerRootDir); err != nil {
			return err
		}
		if err := writeDockerCleanupFile(file, []byte("{}")); err != nil {
			return err
		}
		defer disks.invalidateImages()
		for _, image := range a.GetLoad().GetSelectedImageIds() {
			actual, err := c.loadArchiveImage(ctx, ar, image)
			if err != nil {
				return err
			}
			key := &v1.DockerImageSourceKey{ServerId: ref.Scope, ImageId: actual}
			receipts = append(receipts, &v1.DockerImageReceipt{Outcome: v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED, ImageKey: key})
			for _, t := range tags {
				if t.GetImageId() != image {
					continue
				}
				alias := t.GetAlias()
				if current, err := c.localImage(ctx, alias); err != nil || current != nil {
					return errImageChanged
				}
				tagErr := c.cfg.Docker.TagImage(ctx, actual, alias)
				current, err := c.localImage(ctx, alias)
				if tagErr != nil || err != nil || imageID(current) != actual {
					return errors.New("the resulting alias could not be verified")
				}
				receipts = append(receipts, &v1.DockerImageReceipt{Outcome: v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED, ImageKey: key, Reference: alias})
			}
		}
		data, err := json.Marshal(struct {
			Completed bool
			Receipts  []*v1.DockerImageReceipt
		}{true, receipts})
		if err != nil {
			return err
		}
		return writeDockerCleanupFile(file, data)
	})
	if err == nil {
		_, _ = c.cfg.artifacts.Abort(a.GetStagedSourceId(), "")
	}
	return receipts, err
}

func (c *calls) loadArchiveImage(ctx context.Context, ar *artifact.Archive, config string) (string, error) {
	platform, layers, ok := ar.Identity(config)
	if !ok {
		return "", artifact.ErrArchive
	}
	pipe, write := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := ar.WriteImage(ctx, write, config, nil)
		_ = write.CloseWithError(err)
		done <- err
	}()
	loaded, err := c.cfg.Docker.LoadImage(ctx, pipe)
	_ = pipe.CloseWithError(err)
	if writeErr := <-done; err != nil || writeErr != nil {
		return "", errors.New("the Engine did not load the reviewed image")
	}
	info, err := c.imageStore(ctx)
	if err != nil {
		return "", err
	}
	actual := config
	if info.ContainerdImageStore() {
		if len(loaded) != 1 || !docker.IsImageID(loaded[0]) {
			return "", errors.New("the Engine did not identify the loaded image")
		}
		actual = loaded[0]
	}
	image, err := c.localImage(ctx, actual)
	if err != nil || imageID(image) != actual || !slices.Equal(image.RootFS.Layers, layers) ||
		strings.TrimSuffix(image.OS+"/"+image.Architecture+"/"+image.Variant, "/") != platform {
		return "", errors.New("the Engine returned a different image than the archive described")
	}
	return actual, nil
}
