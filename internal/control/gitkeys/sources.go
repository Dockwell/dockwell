// SPDX-License-Identifier: AGPL-3.0-only

package gitkeys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/gitssh"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

type branchLister interface {
	Branches(ctx context.Context, url string) (head string, branches []string, err error)
}

// Sources wraps a Base revision.Sources (such as gitproviders.Sources) and
// routes SSH repositories or ssh:<name> deploy-key logins through internal/gitssh
// on the control plane (ADR 0113).
type Sources struct {
	Base revision.Sources
	keys atomic.Pointer[Handler]
}

// SetKeys attaches the GitDeployKeyService handler once constructed.
func (s *Sources) SetKeys(h *Handler) { s.keys.Store(h) }

var _ revision.Sources = (*Sources)(nil)

// Resolve resolves ref in repoURL using a project-scoped deploy key when
// git_login names one or repoURL is SSH, otherwise delegates to Base.
func (s *Sources) Resolve(ctx context.Context, repoURL, ref string) (commit, full string, err error) {
	login := revision.GitLoginOf(ctx)
	if _, isKey := service.DeployKeyOf(login); isKey || service.IsSSHRepository(repoURL) {
		h := s.keys.Load()
		if h == nil {
			return "", "", errors.New("this control plane has no SSH deploy keys configured")
		}
		return h.ResolveSSH(ctx, login, repoURL, ref)
	}
	return s.Base.Resolve(ctx, repoURL, ref)
}

// Branches lists branches in repoURL using a project-scoped deploy key when
// git_login names one or repoURL is SSH, otherwise delegates to Base.
func (s *Sources) Branches(ctx context.Context, repoURL string) (head string, branches []string, err error) {
	login := revision.GitLoginOf(ctx)
	if _, isKey := service.DeployKeyOf(login); isKey || service.IsSSHRepository(repoURL) {
		h := s.keys.Load()
		if h == nil {
			return "", nil, errors.New("this control plane has no SSH deploy keys configured")
		}
		return h.BranchesSSH(ctx, login, repoURL)
	}
	lister, ok := s.Base.(branchLister)
	if !ok {
		return "", nil, errors.New("this control plane can't read a repository's branches")
	}
	return lister.Branches(ctx, repoURL)
}

// MultiGitLogins combines project-scoped SSH deploy keys with a fallback
// GitLogins provider (GitHub Apps) so plans and detections resolve both cleanly.
type MultiGitLogins struct {
	Keys     *Handler
	Fallback changes.GitLogins
}

var _ changes.GitLogins = (*MultiGitLogins)(nil)

// PinGitLogin checks or picks the git_login for project and repoURL.
func (m *MultiGitLogins) PinGitLogin(ctx context.Context, tx *store.Tx, project, repoURL, login string) (string, error) {
	if _, isKey := service.DeployKeyOf(login); isKey {
		if m.Keys == nil {
			return "", errors.New("this control plane doesn't support SSH deploy keys")
		}
		return m.Keys.PinDeployKey(ctx, tx, project, repoURL, login)
	}
	if login == "" && service.IsSSHRepository(repoURL) {
		if m.Keys == nil {
			return "", errors.New("this control plane doesn't support SSH deploy keys")
		}
		return m.Keys.PinDeployKey(ctx, tx, project, repoURL, "")
	}
	if m.Fallback != nil {
		return m.Fallback.PinGitLogin(ctx, tx, project, repoURL, login)
	}
	if login != "" {
		return "", fmt.Errorf("this control plane can't use git login %s", login)
	}
	return "", nil
}

// PinDeployKey validates a named ssh:<name> deploy key in project (or finds a
// matching pinned deploy key in project for an SSH repoURL). Cross-project
// deploy keys are always refused.
func (h *Handler) PinDeployKey(ctx context.Context, tx *store.Tx, project, repoURL, login string) (string, error) {
	if project == "" {
		return "", errors.New("a deploy key belongs to one project. Name the project the app builds in")
	}
	if keyName, ok := service.DeployKeyOf(login); ok {
		k, err := h.oneByNameTx(ctx, tx, project, keyName)
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("deploy key %s doesn't belong to this project. Create a deploy key in this project for %s", keyName, repoURL)
		}
		if err != nil {
			return "", err
		}
		if !gitssh.SameRepo(k.spec.GetRepository(), repoURL) {
			return "", fmt.Errorf("deploy key %s is for %s, not %s. Use a deploy key for this repository", keyName, k.spec.GetRepository(), repoURL)
		}
		if k.spec.GetHostKeyFingerprint() == "" {
			return "", fmt.Errorf("deploy key %s hasn't pinned its host's SSH key yet. Test the deploy key and accept the host key first", keyName)
		}
		if k.spec.GetObservedHostKeyFingerprint() != "" {
			return "", fmt.Errorf("the SSH host key for %s changed since deploy key %s pinned it, so fetches are stopped. Ask an admin to accept the new host key", k.spec.GetHost(), keyName)
		}
		return login, nil
	}
	keys, err := h.allInProject(ctx, tx, project)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if !gitssh.SameRepo(k.spec.GetRepository(), repoURL) {
			continue
		}
		if k.spec.GetHostKeyFingerprint() == "" {
			return "", fmt.Errorf("deploy key %s hasn't pinned its host's SSH key yet. Test the deploy key and accept the host key first", k.r.Name)
		}
		if k.spec.GetObservedHostKeyFingerprint() != "" {
			return "", fmt.Errorf("the SSH host key for %s changed since deploy key %s pinned it, so fetches are stopped. Ask an admin to accept the new host key", k.spec.GetHost(), k.r.Name)
		}
		return service.SSHLoginPrefix + k.r.Name, nil
	}
	return "", fmt.Errorf("%s requires an SSH deploy key in this project. Create a deploy key and test it first", repoURL)
}

func (h *Handler) projectForScope(ctx context.Context, tx *store.Tx, scopeID string) (string, error) {
	if scopeID == "" {
		return "", nil
	}
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return "", err
	}
	for _, p := range tree.Projects {
		if p.ID == scopeID || p.Name == scopeID {
			return p.ID, nil
		}
		for _, e := range p.Environments {
			if e.ID == scopeID {
				return p.ID, nil
			}
		}
	}
	return "", nil
}

func (h *Handler) loadKeyForOp(ctx context.Context, login, repoURL, scopeID string) (kept, gitssh.Endpoint, error) {
	if scopeID == "" {
		scopeID = revision.GitScopeOf(ctx)
	}
	var found kept
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		projectID, err := h.projectForScope(ctx, tx, scopeID)
		if err != nil {
			return err
		}
		if projectID == "" {
			return errors.New("an SSH deploy key belongs to one project, and this request named none")
		}
		pinned, err := h.PinDeployKey(ctx, tx, projectID, repoURL, login)
		if err != nil {
			return err
		}
		keyName, _ := service.DeployKeyOf(pinned)
		found, err = h.oneByNameTx(ctx, tx, projectID, keyName)
		return err
	})
	if err != nil {
		return kept{}, gitssh.Endpoint{}, err
	}
	ep, err := gitssh.ParseSSH(found.spec.GetRepository())
	if err != nil {
		return kept{}, gitssh.Endpoint{}, err
	}
	return found, ep, nil
}

// ResolveSSH resolves a ref over SSH using the project's pinned deploy key.
func (h *Handler) ResolveSSH(ctx context.Context, login, repoURL, ref string) (commit, full string, err error) {
	k, ep, err := h.loadKeyForOp(ctx, login, repoURL, "")
	if err != nil {
		return "", "", err
	}
	release, err := h.gate.acquireList(ctx, k.r.Scope, k.r.ID)
	if err != nil {
		return "", "", err
	}
	defer release()
	commit, full, err = gitssh.Resolve(ctx, h.sshConfig(k, ep), h.cfg.Dial, ref)
	if err != nil {
		h.maybeNoteHostKeyChange(ctx, k.r.ID, err)
		return "", "", err
	}
	return commit, full, nil
}

// BranchesSSH lists branches over SSH using the project's pinned deploy key.
func (h *Handler) BranchesSSH(ctx context.Context, login, repoURL string) (head string, branches []string, err error) {
	k, ep, err := h.loadKeyForOp(ctx, login, repoURL, "")
	if err != nil {
		return "", nil, err
	}
	release, err := h.gate.acquireList(ctx, k.r.Scope, k.r.ID)
	if err != nil {
		return "", nil, err
	}
	defer release()
	head, branches, _, err = gitssh.Branches(ctx, h.sshConfig(k, ep), h.cfg.Dial)
	if err != nil {
		h.maybeNoteHostKeyChange(ctx, k.r.ID, err)
		return "", nil, err
	}
	return head, branches, nil
}

// FetchPackIn fetches and verifies a shallow Git pack over SSH on the control
// plane for a build or detection step in scopeID (environment or project ID).
func (h *Handler) FetchPackIn(ctx context.Context, login, repoURL, ref, commit, scopeID string) (io.ReaderAt, int64, func(), error) {
	k, ep, err := h.loadKeyForOp(ctx, login, repoURL, scopeID)
	if err != nil {
		return nil, 0, nil, err
	}
	release, err := h.gate.acquireFetch(ctx, k.r.Scope, k.r.ID)
	if err != nil {
		return nil, 0, nil, err
	}
	defer release()
	f, size, err := gitssh.FetchPack(ctx, h.sshConfig(k, ep), h.cfg.Dial, ref, commit, gitssh.DefaultPackLimits())
	if err != nil {
		h.maybeNoteHostKeyChange(ctx, k.r.ID, err)
		return nil, 0, nil, err
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = f.Close()
			_ = os.Remove(f.Name())
		})
	}
	return f, size, cleanup, nil
}

func (h *Handler) sshConfig(k kept, ep gitssh.Endpoint) gitssh.Config {
	return gitssh.Config{
		Endpoint:         ep,
		HostKeyAlgorithm: k.spec.GetHostKeyAlgorithm(),
		HostKeyPin:       k.spec.GetHostKeyFingerprint(),
		PrivateKeyPEM:    k.spec.GetPrivateKey(),
		Local:            h.cfg.Local,
		PackTempDir:      h.cfg.PackTempDir,
	}
}

func (h *Handler) maybeNoteHostKeyChange(ctx context.Context, keyID string, err error) {
	var changed *gitssh.HostKeyChange
	if errors.As(err, &changed) {
		h.recordObservedHostKey(ctx, keyID, changed.Algorithm, changed.Fingerprint)
	}
}

// ShownLogin formats a git_login for CLI or plan display.
func ShownLogin(login string) string {
	if name, ok := service.DeployKeyOf(login); ok {
		return "deploy key " + name
	}
	if strings.Contains(login, "/") {
		return "GitHub App " + login
	}
	return login
}
