// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/build"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

func (c *calls) checkBuildDestination(ctx context.Context, output string, held *agentv1.DockerCleanupProtection) error {
	if c.infrastructureImageAlias(output) || resource.DockerAliasProtected(output, held.GetImages()) {
		return errImageProtected
	}
	if current, err := c.localImage(ctx, output); err != nil || current != nil {
		return errors.New("this output tag is in use. Review the build again and use a separate tag action to replace it")
	}
	return nil
}

func (c *calls) buildSettings(ctx context.Context, ref resource.Ref, desired *agentv1.DesiredResource) (service.BuildRequest, string, func(), error) {
	noop := func() {}
	plan := desired.GetDockerMaintenance()
	a := plan.GetImageAction()
	b := a.GetBuild()
	if c.cfg.artifacts == nil || c.cfg.Builds == nil || b == nil ||
		a.GetLoad() != nil || a.GetExport() != nil || a.GetTag() != nil || a.GetPull() != nil ||
		!artifact.ContextPath(b.GetDockerfilePath()) {
		return service.BuildRequest{}, "", noop, errors.New("this agent could not verify the reviewed build")
	}
	output, err := registry.Parse(b.GetOutput())
	if err != nil || output.Tag == "" || output.Digest != "" || output.String() != b.GetOutput() ||
		output.Registry == docker.BuiltRegistry || !strings.Contains(output.Tag, "dockwell-") {
		return service.BuildRequest{}, "", noop, errImageChanged
	}
	_, native := c.cfg.images.ready(ctx)
	if native.Architecture == "" {
		return service.BuildRequest{}, "", noop, errors.New("the Engine could not verify the server's build platform")
	}
	platform := native.String()
	if b.GetPlatform() != "" && b.GetPlatform() != platform {
		return service.BuildRequest{}, "", noop, errors.New("choose this server's native platform")
	}
	req := service.BuildRequest{Service: ref, Change: plan.GetChangeId(), Builder: resource.BuilderDockerfile, Image: b.GetOutput(), Dockerfile: b.GetDockerfilePath()}
	cleanup := noop
	if git := b.GetGit(); git != nil {
		commit := a.GetGitCommit()
		repo, err := service.CheckRepository(git.GetRepository())
		if err != nil || repo != git.GetRepository() || len(commit) != 40 {
			return req, "", noop, errImageChanged
		}
		if _, err := hex.DecodeString(commit); err != nil {
			return req, "", noop, errImageChanged
		}
		req.URL, req.Ref, req.Commit, req.Context = repo, git.GetRef(), commit, git.GetSubdirectory()
		if service.UsesGitPack(git.GetGitLogin()) {
			stream := desired.GetBuildGitBundleStream()
			if stream == 0 {
				return req, "", noop, errors.New("the reviewed Git pack could not be opened")
			}
			packDir, cleanupPack, err := c.receiveGitPack(ctx, stream)
			if err != nil {
				return req, "", noop, errors.New("the reviewed Git pack could not be received")
			}
			req.GitPackPath = packDir
			cleanup = cleanupPack
		} else if git.GetGitLogin() != "" {
			purpose := resource.DockerImageAuthRef(ref, plan)
			purpose.Name += "/git"
			req.GitAuth, err = c.openGitAuth(purpose, desired.GetBuildGitAuth())
			if err != nil {
				return req, "", noop, errors.New("the Git login could not be opened")
			}
		}
	} else {
		info, err := c.cfg.artifacts.Status(a.GetStagedSourceId(), "")
		if err != nil || info.GetKind() != artifact.BuildContext || info.GetState() != artifact.Ready || info.GetSha256() != a.GetSourceSha256() {
			return req, "", noop, artifact.ErrUnavailable
		}
	}
	if approved := a.GetApprovedLogins(); len(approved) != 0 {
		if c.delivery == nil {
			cleanup()
			return req, "", noop, errImageAuthRefused
		}
		values, err := c.delivery.Open(c.server, resource.DockerImageAuthRef(ref, plan), desired.GetDockerRegistryAuth())
		if err != nil {
			cleanup()
			return req, "", noop, errImageAuthRefused
		}
		defer clear(values)
		req.Logins, err = registry.AuthsFrom(values)
		if err != nil || len(req.Logins) != len(approved) {
			cleanup()
			return req, "", noop, errImageAuthRefused
		}
		for i, login := range req.Logins {
			if login.Registry != approved[i].GetHost() || login.Name != approved[i].GetName() {
				cleanup()
				return req, "", noop, errImageAuthRefused
			}
		}
	}
	return req, platform, cleanup, nil
}

func (c *calls) buildText(ctx context.Context, a *v1.DockerImageAction, req service.BuildRequest) ([]byte, error) {
	b := a.GetBuild()
	if override := b.GetDockerfileOverrideArtifactId(); override != "" {
		info, err := c.cfg.artifacts.Status(override, "")
		if err != nil || info.GetKind() != artifact.Dockerfile {
			return nil, artifact.ErrUnavailable
		}
		return c.cfg.artifacts.BuildText(ctx, override, "", "")
	}
	if b.GetGit() != nil {
		d, err := c.cfg.Builds.Detect(ctx, req)
		if err != nil {
			return nil, err
		}
		if d == nil || d.Builder != resource.BuilderDockerfile || len(d.Text) > artifact.DockerfileSize {
			return nil, errors.New("the reviewed commit did not yield a Dockerfile within the 1 MiB limit")
		}
		return []byte(d.Text), nil
	}
	return c.cfg.artifacts.BuildText(ctx, a.GetStagedSourceId(), "", b.GetDockerfilePath())
}

func (c *calls) standaloneLogins(ctx context.Context, logins []registry.Auth, refs []registry.Ref) ([]registry.Auth, error) {
	for _, login := range logins {
		if !slices.ContainsFunc(refs, func(ref registry.Ref) bool { return ref.Registry == login.Registry }) {
			return nil, errors.New("a selected login does not match a registry in this Dockerfile")
		}
	}
	if len(logins) == 0 {
		return nil, nil
	}
	checked, err := c.checkBuildLogins(ctx, logins)
	if err != nil || len(checked) != len(logins) {
		return nil, errImageAuthRefused
	}
	client, _ := c.cfg.images.ready(ctx)
	for _, login := range checked {
		check, done := context.WithTimeout(ctx, registry.RequestTimeout)
		err := client.CheckBuildSignIn(check, login)
		done()
		if err != nil {
			return nil, errImageAuthRefused
		}
	}
	return checked, nil
}

func (c *calls) previewImageBuild(ctx context.Context, ref resource.Ref, desired *agentv1.DesiredResource) error {
	req, platform, cleanup, err := c.buildSettings(ctx, ref, desired)
	if err != nil {
		return err
	}
	defer cleanup()
	a := desired.GetDockerMaintenance().GetImageAction()
	if err := c.checkBuildDestination(ctx, req.Image, nil); err != nil {
		return err
	}
	if desired.GetDockerMaintenance().GetChangeId() != "" {
		return nil
	}
	text, err := c.buildText(ctx, a, req)
	if err != nil {
		return err
	}
	if len(text) == 0 {
		return errors.New("the selected Dockerfile is empty")
	}
	refs, err := build.StandaloneReferences(text)
	if err != nil {
		return err
	}
	logins, err := c.standaloneLogins(ctx, req.Logins, refs)
	if err != nil {
		return err
	}
	for _, rr := range refs {
		scoped := ctx
		for _, login := range logins {
			if login.Registry == rr.Registry {
				scoped = registry.WithAuth(ctx, login)
				break
			}
		}
		descriptor, err := c.resolveDockerImage(scoped, rr)
		if err != nil {
			return err
		}
		// Resolution already verified the config against the native platform.
		// Record the requested platform consistently when the config spells out
		// a default variant (linux/arm64/v8) or leaves it implicit.
		descriptor.Platform = platform
		a.BaseDescriptors = append(a.BaseDescriptors, descriptor)
	}
	a.Build.Platform = platform
	a.DockerfileSha256 = fmtHash(text)
	return nil
}

func fmtHash(text []byte) string {
	digest := sha256.Sum256(text)
	return hex.EncodeToString(digest[:])
}

func (c *calls) applyImageBuild(ctx context.Context, ref resource.Ref, desired *agentv1.DesiredResource, protected *agentv1.DockerCleanupProtection) (*v1.DockerImageReceipt, error) {
	plan := desired.GetDockerMaintenance()
	a := plan.GetImageAction()
	change := plan.GetChangeId()
	if _, err := id.Parse(id.Change, change); err != nil || protected == nil {
		return nil, errImageChanged
	}
	builder, ok := c.cfg.Builds.(service.Builder)
	if !ok {
		return nil, errors.New("this server cannot build images")
	}
	req, platform, cleanup, err := c.buildSettings(ctx, ref, desired)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	receipt := &v1.DockerImageReceipt{
		BuildId: resource.DockerBuildID(change), Reference: req.Image,
		Outcome: v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED,
	}
	dir := filepath.Join(c.cfg.Dir, "docker-image-actions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return receipt, err
	}
	journal := filepath.Join(dir, change+".json")
	if data, err := os.ReadFile(journal); err == nil { //nolint:gosec // private state, validated change ID
		var saved v1.DockerImageReceipt
		if decodeBuildReceipt(data, &saved, receipt) {
			return &saved, nil
		}
		receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN
		return receipt, errors.New("the earlier build result could not be verified. Check the Images tab before trying again")
	} else if !errors.Is(err, os.ErrNotExist) {
		return receipt, err
	}
	for _, lease := range resource.DockerBuildLeases(a) {
		if err := c.cfg.artifacts.CheckPin(lease, change); err != nil {
			return receipt, err
		}
	}
	ctx = resource.WithBuilds(ctx, c.buildUpdates(change, ref))
	req.Standalone = &service.StandaloneBuild{
		Platform: platform, CacheScope: buildCacheScope(a.GetApprovedLogins()), Scratch: dir,
		Prepare: func(ctx context.Context, req *service.BuildRequest, run func() error) error {
			text, err := c.buildText(ctx, a, *req)
			if err != nil {
				return err
			}
			if fmtHash(text) != a.GetDockerfileSha256() {
				return errors.New("the reviewed Dockerfile could not be verified")
			}
			refs, err := build.StandaloneReferences(text)
			if err != nil {
				return err
			}
			req.Standalone.Policy, err = build.StandalonePolicy(refs, a.GetBaseDescriptors(), platform)
			if err != nil {
				return err
			}
			req.Logins, err = c.standaloneLogins(ctx, req.Logins, refs)
			if err != nil {
				return err
			}
			req.Standalone.Dockerfile = string(text)
			if a.GetBuild().GetGit() != nil {
				return run()
			}
			release, err := c.cfg.takeArtifactTransfer(ctx)
			if err != nil {
				return err
			}
			finish := sync.OnceFunc(release)
			defer finish()
			return c.cfg.artifacts.WithBuildContext(ctx, a.GetStagedSourceId(), a.GetSourceSha256(), func(dir string) error {
				req.Standalone.Context = dir
				finish()
				return run()
			})
		},
		Publish: func(ctx context.Context, begin func() error, load func(context.Context) error) error {
			// Once publication starts, resolve its outcome under a bounded context
			// that survives viewer disconnect and cancellation of the solve.
			ctx, cancel := resource.Finishing(ctx, 2*time.Minute)
			defer cancel()
			disks := c.cfg.disks
			disks.cleanupMu.Lock()
			defer disks.cleanupMu.Unlock()
			if err := c.checkBuildDestination(ctx, req.Image, protected); err != nil {
				return err
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				return errors.New("this build already attempted publication. Check the Images tab before trying again")
			}
			if err := begin(); err != nil {
				return err
			}
			if err := saveImageJournal(journal, receipt); err != nil {
				return err
			}
			loadErr := load(ctx)
			disks.invalidateImages()
			verify, end := resource.Finishing(ctx, 10*time.Second)
			defer end()
			image, err := c.localImage(verify, req.Image)
			if err != nil || image == nil {
				receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN
				if loadErr != nil {
					return loadErr
				}
				return errors.New("the Engine could not verify the published image")
			}
			receipt.ImageKey = &v1.DockerImageSourceKey{ServerId: ref.Scope, ImageId: image.ID}
			_, err = saveImageOutcome(journal, receipt)
			return err
		},
	}
	err = builder.Build(ctx, req)
	if resource.ReasonOf(err) == resource.ReasonCancelled {
		receipt.Outcome = v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_CANCELLED
	}
	return receipt, err
}

func decodeBuildReceipt(data []byte, saved, proposed *v1.DockerImageReceipt) bool {
	return json.Unmarshal(data, saved) == nil && saved.GetOutcome() == v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED &&
		saved.GetBuildId() == proposed.GetBuildId() && saved.GetReference() == proposed.GetReference() &&
		docker.IsImageID(saved.GetImageKey().GetImageId())
}

func buildCacheScope(refs []*v1.DockerImageLoginRef) string {
	if len(refs) == 0 {
		return "public"
	}
	data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&v1.DockerImageAction{ApprovedLogins: refs})
	return fmtHash(data)[:20]
}
