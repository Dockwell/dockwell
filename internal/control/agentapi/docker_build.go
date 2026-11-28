// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

type dockerBuildLogins interface {
	DockerBuildLogin(context.Context, string, string) (*v1.DockerImageLoginRef, registry.Auth, error)
}

// DockerBuildLogin reopens one selected login subject to the current project grant.
func (s *Session) DockerBuildLogin(ctx context.Context, project, id string) (*v1.DockerImageLoginRef, registry.Auth, error) {
	if l, ok := s.agents.logins.(dockerBuildLogins); ok && id != "" {
		return l.DockerBuildLogin(ctx, project, id)
	}
	return nil, registry.Auth{}, ErrNoLogins
}

// SendDockerBuildGitAuth sets the GitHub App credential provider for image builds.
func (a *Agents) SendDockerBuildGitAuth(fn GitAuth) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dockerBuildGitAuth = fn
}

func (s *Session) sealDockerBuild(ctx context.Context, ref resource.Ref, wanted *agentv1.DesiredResource) error {
	if !s.ArtifactTransfers() || !s.DockerImageOperations() {
		return ErrOlderAgent
	}
	plan := wanted.GetDockerMaintenance()
	action := plan.GetImageAction()
	binding := resource.DockerImageAuthRef(ref, plan)
	var auths []registry.Auth
	for _, pick := range action.GetApprovedLogins() {
		current, auth, err := s.DockerBuildLogin(ctx, action.GetCredentialProject(), pick.GetId())
		if err != nil || !proto.Equal(current, pick) {
			return errors.New("a registry login changed. Review the image build again")
		}
		auths = append(auths, auth)
	}
	if len(auths) != 0 {
		values := registry.AuthsValues(auths)
		defer clear(values)
		data, err := s.seal(binding, values)
		if err != nil {
			return err
		}
		wanted.DockerRegistryAuth = data
	}
	if git := action.GetBuild().GetGit(); git.GetGitLogin() != "" && !service.UsesGitPack(git.GetGitLogin()) {
		s.agents.mu.Lock()
		fn := s.agents.dockerBuildGitAuth
		s.agents.mu.Unlock()
		if fn == nil {
			return errors.New("this control plane has no GitHub login for this build")
		}
		header, err := fn(ctx, git.GetGitLogin(), git.GetRepository(), git.GetProject())
		if err != nil {
			return errors.New("the reviewed Git login could not be used")
		}
		binding.Name += "/git"
		sealed, err := s.seal(binding, map[string]string{"authorization": header})
		if err != nil {
			return err
		}
		wanted.BuildGitAuth = sealed
	}
	return nil
}
