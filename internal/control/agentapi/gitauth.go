// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// GitPackFetcher fetches a verified Git pack on the control plane for a build
// or detection that uses an SSH deploy key (ADR 0113). The returned cleanup
// closes and removes the temporary pack file.
type GitPackFetcher func(ctx context.Context, login, repoURL, ref, commit, scopeID string) (io.ReaderAt, int64, func(), error)

// SendGitPack has a build or detection whose git_login names an SSH deploy key
// fetch the verified Git pack on the control plane and stream it to the agent
// over ReceiveFile (ADR 0113).
func (a *Agents) SendGitPack(fn GitPackFetcher) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gitPack = fn
}

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
	if _, isKey := service.DeployKeyOf(b.GetGitLogin()); isKey {
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
	if _, isKey := service.DeployKeyOf(b.GetGitLogin()); isKey {
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

func (s *Session) openGitPackTransfer(r io.ReaderAt, size int64, onRead func()) *transfer {
	s.agents.mu.Lock()
	t := s.agents.transferLocked(s.server)
	t.control = &controlBackupTransfer{session: s, file: io.NewSectionReader(r, 0, size), onRead: onRead}
	s.agents.mu.Unlock()
	return t
}

func (s *Session) buildGitPack(ctx context.Context, step resource.Step, spec proto.Message) (io.ReaderAt, int64, func(), error) {
	svc, ok := spec.(*resourcev1.Service)
	b := svc.GetBuild()
	if step.Build == nil || !ok {
		return nil, 0, nil, nil
	}
	if _, isKey := service.DeployKeyOf(b.GetGitLogin()); !isKey {
		return nil, 0, nil, nil
	}
	if !s.handsGitBundle || !s.transfersFiles {
		return nil, 0, nil, fmt.Errorf("the agent on %s can't build with an SSH deploy key yet. Update the agent on %s, then deploy again", s.name, s.name)
	}
	s.agents.mu.Lock()
	fn := s.agents.gitPack
	s.agents.mu.Unlock()
	if fn == nil {
		return nil, 0, nil, errors.New("this control plane can't fetch a repository with an SSH deploy key")
	}
	g := b.GetGit()
	return fn(ctx, b.GetGitLogin(), g.GetUrl(), g.GetRef(), g.GetCommit(), step.Ref.Scope)
}

func (s *Session) detectGitPack(ctx context.Context, call *agentv1.DetectBuildCall) (func(*Session) error, func(), error) {
	b := call.GetBuild()
	if _, isKey := service.DeployKeyOf(b.GetGitLogin()); !isKey {
		return nil, nil, nil
	}
	s.agents.mu.Lock()
	fn := s.agents.gitPack
	s.agents.mu.Unlock()
	if fn == nil {
		return nil, nil, errors.New("this control plane can't read a repository with an SSH deploy key")
	}
	g := b.GetGit()
	r, size, cleanupFile, err := fn(ctx, b.GetGitLogin(), g.GetUrl(), g.GetRef(), g.GetCommit(), "")
	if err != nil {
		return nil, nil, err
	}
	var active *transfer
	cleanup := func() {
		if active != nil {
			s.agents.closeTransfer(active)
		}
		if cleanupFile != nil {
			cleanupFile()
		}
	}
	prepare := func(on *Session) error {
		if !on.handsGitBundle || !on.transfersFiles {
			return fmt.Errorf("the agent on %s can't read a repository with an SSH deploy key yet. Update the agent on %s, then read again", on.name, on.name)
		}
		if active != nil {
			s.agents.closeTransfer(active)
		}
		active = on.openGitPackTransfer(r, size, cleanupFile)
		call.GitBundleStream = active.token
		return nil
	}
	return prepare, cleanup, nil
}
