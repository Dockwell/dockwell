// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// GitAuth is the Authorization a build fetches its private repository with: a token
// the GitHub App's installation login names asks GitHub for, for the repository at
// url alone (ADR 0098). For a build of an app in environment, the installation must
// still reach the environment's project, as a narrowing since its plan may have
// taken it away; a reading names none, as its project was checked as it was asked.
type GitAuth func(ctx context.Context, login, url, environment string) (string, error)

// SendGitAuth has a step whose build names a git login carry what fetches its
// repository, asked for as the step runs, sealed to the stream it goes out on.
// Without it, a build that names one fails.
func (a *Agents) SendGitAuth(fn GitAuth) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gitAuth = fn
}

// buildGitAuth is what a step's build fetches its private repository with, or ""
// for a step that builds nothing, or a build that names no git login.
func (s *Session) buildGitAuth(ctx context.Context, step resource.Step, spec proto.Message) (string, error) {
	svc, ok := spec.(*resourcev1.Service)
	b := svc.GetBuild()
	if step.Build == nil || !ok || b.GetGitLogin() == "" {
		return "", nil
	}
	if !s.handsGitAuth {
		return "", fmt.Errorf("the agent on %s can't fetch a private repository for a build yet. Update the agent on %s, then deploy again", s.name, s.name)
	}
	if len(s.deliveryKey) == 0 {
		return "", s.noDeliveryKey(step.Ref)
	}
	s.agents.mu.Lock()
	fn := s.agents.gitAuth
	s.agents.mu.Unlock()
	if fn == nil {
		return "", errors.New("this control plane has no GitHub App to fetch a private repository with")
	}
	return fn(ctx, b.GetGitLogin(), b.GetGit().GetUrl(), step.Ref.Scope)
}

// sealGitAuth seals what fetches a build's repository for ref to this stream's
// delivery key, apart from its values and its registry logins.
func (s *Session) sealGitAuth(ref resource.Ref, header string) ([]byte, error) {
	return s.seal(resource.BuildGitRef(ref), map[string]string{"authorization": header})
}

// detectGitAuth readies a reading whose build names a git login to carry what
// fetches its repository, sealed for each stream it goes out on, as a build's is:
// nil for a build that names none.
func (s *Session) detectGitAuth(ctx context.Context, call *agentv1.DetectBuildCall) (func(*Session) error, error) {
	b := call.GetBuild()
	if b.GetGitLogin() == "" {
		return nil, nil
	}
	s.agents.mu.Lock()
	fn := s.agents.gitAuth
	s.agents.mu.Unlock()
	if fn == nil {
		return nil, errors.New("this control plane has no GitHub App to read a private repository with")
	}
	header, err := fn(ctx, b.GetGitLogin(), b.GetGit().GetUrl(), "")
	if err != nil {
		return nil, err
	}
	return func(on *Session) error {
		if !on.handsGitAuth {
			return fmt.Errorf("the agent on %s can't read a private repository yet. Update the agent on %s, then read again", on.name, on.name)
		}
		sealed, err := on.seal(resource.DetectGitRef, map[string]string{"authorization": header})
		if err != nil {
			return err
		}
		call.GitAuth = sealed
		return nil
	}, nil
}
