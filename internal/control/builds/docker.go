// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

const imageResourcePrefix = "docker-image/"

// ResolveDockerGit authorizes the selected repository through the existing
// GitHub installation and resolves an immutable commit for the server.
func (h *Handler) ResolveDockerGit(ctx context.Context, input *v1.DockerImageGitSource, dockerfile string) (*v1.DockerImageGitSource, string, error) {
	g := proto.CloneOf(input)
	spec := &resourcev1.ImageBuild{Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: g.GetRepository(), Ref: g.GetRef()}}, Context: g.GetSubdirectory(), Dockerfile: dockerfile}
	if err := service.CheckBuild(spec); err != nil {
		return nil, "", authapi.Invalid(err)
	}
	if h.sources == nil {
		return nil, "", connect.NewError(connect.CodeUnavailable, errors.New("repository sources are unavailable"))
	}
	project, err := h.projectNamed(ctx, g.GetProject(), "")
	if err != nil {
		return nil, "", err
	}
	g.Project, g.Repository, g.Subdirectory = project, spec.GetGit().GetUrl(), spec.GetContext()
	g.GitLogin, err = h.loginFor(ctx, project, g.GetRepository(), g.GetGitLogin())
	if err != nil {
		return nil, "", err
	}
	commit, ref, err := h.sources.Resolve(revision.WithGitLogin(ctx, g.GetGitLogin()), g.GetRepository(), spec.GetGit().GetRef())
	if err != nil {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the selected Git ref could not be resolved"))
	}
	if ref == "" {
		ref = commit
	}
	spec.GetGit().Ref, spec.GetGit().Commit = ref, commit
	if err := service.CheckBuild(spec); err != nil || len(spec.GetGit().GetCommit()) != 40 {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the repository did not return a full commit"))
	}
	g.Ref = spec.GetGit().GetRef()
	return g, spec.GetGit().GetCommit(), nil
}

func imagePlan(c store.Change, server string) (*v1.DockerImageAction, error) {
	var plan revision.Plan
	if err := json.Unmarshal(c.Plan, &plan); err != nil {
		return nil, err
	}
	for _, sp := range plan.Servers {
		if sp.Server != server {
			continue
		}
		for i := range sp.Steps {
			step := &sp.Steps[i]
			if step.Ref == (resource.Ref{Kind: host.Kind, Scope: server, Name: host.Name}) && step.DockerMaintenance.GetImageAction().GetBuild() != nil {
				return step.DockerMaintenance.GetImageAction(), nil
			}
		}
	}
	return nil, errNotPlanned
}

func (h *Handler) newImageBuild(ctx context.Context, tx *store.Tx, change, server string) (store.Build, error) {
	c, err := tx.GetChange(ctx, change)
	if err != nil {
		return store.Build{}, err
	}
	if c.State != store.ChangeApplying {
		return store.Build{}, errNotPlanned
	}
	a, err := imagePlan(c, server)
	if err != nil {
		return store.Build{}, err
	}
	g := a.GetBuild().GetGit()
	b := store.Build{
		ID: resource.DockerBuildID(change), ChangeID: change,
		ResourceID: imageResourcePrefix + server, ServerID: server, Service: host.Name,
		State: store.BuildQueued, Image: a.GetBuild().GetOutput(), CreatedAt: h.now(),
		URL: g.GetRepository(), Ref: g.GetRef(), Commit: a.GetGitCommit(),
	}
	return b, tx.CreateBuild(ctx, b)
}

func (h *Handler) getImage(ctx context.Context, change, server string) (store.Build, *v1.Build, error) {
	if _, err := id.Parse(id.Change, change); err != nil || server == "" {
		return store.Build{}, nil, authapi.Invalid(errors.New("choose a server and the build's change"))
	}
	var b store.Build
	var p *v1.Build
	err := h.db.View(ctx, func(tx *store.Tx) error {
		c, err := tx.GetChange(ctx, change)
		if err != nil {
			return err
		}
		a, err := imagePlan(c, server)
		if err != nil {
			return err
		}
		b, err = tx.BuildOf(ctx, change, imageResourcePrefix+server, server)
		if errors.Is(err, store.ErrNotFound) {
			b = store.Build{
				ID: resource.DockerBuildID(change), ChangeID: change, ServerID: server, ResourceID: imageResourcePrefix + server, Service: host.Name,
				CreatedAt: c.CreatedAt, State: store.BuildQueued, Image: a.GetBuild().GetOutput(),
			}
			if c.State != store.ChangeApplying && c.State != store.ChangePlanned {
				b.State = store.BuildLost
			}
			p = toProto(b, "")
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := h.protos(ctx, tx, []store.Build{b})
		if err == nil {
			p = rows[0]
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, errNotPlanned) {
		return b, nil, connect.NewError(connect.CodeNotFound, errors.New("this change has no image build on that server"))
	}
	return b, p, err
}

// WatchImage follows a standalone build, including the time before its first agent update.
func (h *Handler) WatchImage(ctx context.Context, req *v1.WatchDockerImageBuildRequest, send func(*v1.WatchBuildResponse) error) error {
	if req.GetAfterLine() < 0 {
		return authapi.Invalid(errors.New("choose a nonnegative log cursor"))
	}
	tick := time.NewTicker(lostRecheck)
	defer tick.Stop()
	var sentState bool
	for {
		b, p, err := h.getImage(ctx, req.GetChangeId(), req.GetServerId())
		if err != nil {
			return h.internal(ctx, "following an image build", err)
		}
		if b.Lines != 0 || b.StartedAt != nil || b.EndedAt != nil {
			return h.watchBuild(ctx, b, req.GetAfterLine(), send)
		}
		ended := p.GetState() != v1.BuildState_BUILD_STATE_QUEUED && p.GetState() != v1.BuildState_BUILD_STATE_BUILDING
		if !sentState || ended {
			if err := send(&v1.WatchBuildResponse{Build: p}); err != nil || ended {
				return err
			}
			sentState = true
		}
		updates, stop := h.watch(b.ID)
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-updates:
		case <-tick.C:
		}
		stop()
	}
}

// CancelImage reuses the authenticated cancellation and audit event.
func (h *Handler) CancelImage(ctx context.Context, req *v1.CancelDockerImageBuildRequest) (*connect.Response[v1.CancelBuildResponse], error) {
	b, p, err := h.getImage(ctx, req.GetChangeId(), req.GetServerId())
	if err != nil {
		return nil, h.internal(ctx, "cancelling an image build", err)
	}
	return h.cancelBuild(ctx, b, p)
}

func imageBuild(b store.Build) bool { return strings.HasPrefix(b.ResourceID, imageResourcePrefix) }
