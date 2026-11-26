// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// CIDigestReader resolves a moving tag on the app's server, with its registry login.
type CIDigestReader interface {
	DeployDigest(context.Context, string, resource.Ref, *resourcev1.Service) (string, error)
}

var commitSHA = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func checkedDeployOrigin(in *v1.DeployOrigin, image string) (*v1.DeployOrigin, error) {
	if in == nil {
		return nil, nil
	}
	if strings.TrimSpace(image) != image || len(image) > 512 || image == "" {
		return nil, errors.New("supply the image reference that CI built")
	}
	switch in.GetProvider() {
	case v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, v1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, v1.DeployProvider_DEPLOY_PROVIDER_OTHER:
	default:
		return nil, errors.New("choose the CI provider")
	}
	if !commitSHA.MatchString(in.GetCommit()) {
		return nil, errors.New("supply the full Git commit SHA")
	}
	ref := in.GetRef()
	if ref == "" || len(ref) > 512 || strings.ContainsFunc(ref, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) {
		return nil, errors.New("supply the Git ref that CI built")
	}
	if !ciURL(in.GetRepository()) || !ciURL(in.GetRunUrl()) {
		return nil, errors.New("supply HTTPS links to the repository and CI run without credentials, query parameters or fragments")
	}
	commit := strings.ToLower(in.GetCommit())
	commitURL := in.GetCommitUrl()
	if in.GetProvider() == v1.DeployProvider_DEPLOY_PROVIDER_OTHER {
		if !ciURL(commitURL) {
			return nil, errors.New("supply an HTTPS commit link for this CI provider")
		}
	} else {
		repo, _ := url.Parse(in.GetRepository())
		path := strings.TrimSuffix(strings.TrimRight(repo.Path, "/"), ".git")
		if in.GetProvider() == v1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI {
			path += "/-"
		}
		repo.Path, repo.RawPath = path+"/commit/"+commit, ""
		commitURL = repo.String()
	}
	return &v1.DeployOrigin{
		Provider: in.GetProvider(), Repository: in.GetRepository(), Commit: commit, CommitUrl: commitURL,
		Ref: ref, RunUrl: in.GetRunUrl(), Image: image,
	}, nil
}

func ciURL(raw string) bool {
	if len(raw) > 2048 || strings.ContainsAny(raw, "\\#?") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		strings.Trim(u.Path, "/") != "" && !strings.ContainsFunc(u.Path, unicode.IsControl)
}

func (h *Handler) pinCIDeploy(ctx context.Context, r store.Resource, ref resource.Ref, spec *resourcev1.Service, origin *v1.DeployOrigin) (*v1.Change, error) {
	image, err := registry.Parse(origin.GetImage())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	digest := image.Digest
	if digest != "" {
		origin.Digest = digest
		change, err := h.existingCIDeploy(ctx, r.ID, origin)
		if err != nil || change != nil {
			return change, err
		}
	}
	switch resource.RunIntent(spec) {
	case resourcev1.DesiredRunState_DESIRED_RUN_STATE_PAUSED:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("unpause this app before deploying from CI"))
	case resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("start this app before deploying from CI"))
	case resourcev1.DesiredRunState_DESIRED_RUN_STATE_RUNNING, resourcev1.DesiredRunState_DESIRED_RUN_STATE_UNSPECIFIED:
	}
	var servers []string
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		servers, err = tx.Placement(ctx, r.ID, r.Revision)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "reading where a CI deploy runs", err)
	}
	if len(servers) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("place this app on a server before deploying from CI"))
	}
	if digest == "" {
		if h.CIImages == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the image tag can't be pinned here. Pass the image by digest"))
		}
		digest, err = h.CIImages.DeployDigest(ctx, servers[0], ref, spec)
		if err != nil {
			return nil, err
		}
		matched, err := registry.Parse(resource.PinnedImage(image.String(), digest))
		if err != nil || matched.Digest == "" {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("the registry returned no usable digest. Try again or pass the image by digest"))
		}
	}
	origin.Digest = digest
	spec.GetApp().Image = resource.PinnedImage(image.String(), digest)
	if image.Digest == "" {
		return h.existingCIDeploy(ctx, r.ID, origin)
	}
	return nil, nil
}

func (h *Handler) existingCIDeploy(ctx context.Context, service string, origin *v1.DeployOrigin) (*v1.Change, error) {
	var id string
	err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		id, err = tx.CIDeployChange(ctx, store.CIDeployKey{Service: service, Digest: origin.GetDigest(), Commit: origin.GetCommit()})
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, h.internal(ctx, "reading a CI deploy", err)
	default:
		return h.changes.ExistingCIDeploy(ctx, id)
	}
}
