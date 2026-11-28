// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// GitLogins checks the GitHub App installation, GitLab connection, or SSH deploy
// key a reading fetches a repository through, as a plan does, and picks one
// where none is named (ADR 0098, ADR 0113). Runs outside any store transaction.
type GitLogins interface {
	PinGitLogin(ctx context.Context, project, repoURL, login string) (string, error)
}

// SetGitLogins has readings of a repository fetch it through the login g says
// reaches the project they're for.
func (h *Handler) SetGitLogins(g GitLogins) { h.gitLogins = g }

// loginFor is the git login a reading of the repository at url, for project,
// fetches through. A login or SSH repository named for no project is refused.
func (h *Handler) loginFor(ctx context.Context, project, url, login string) (string, error) {
	if h.gitLogins == nil {
		if login != "" || service.IsSSHRepository(url) {
			return "", connect.NewError(connect.CodeUnavailable, errors.New("this control plane has no Git provider or deploy keys to read a private repository with"))
		}
		return "", nil
	}
	if project == "" {
		if _, isKey := service.DeployKeyOf(login); isKey {
			return "", authapi.Invalid(errors.New("an SSH deploy key belongs to one project. Name the project that holds the deploy key"))
		}
		if _, isGL := service.GitLabConnectionOf(login); isGL {
			return "", authapi.Invalid(fmt.Errorf("%s reads through a GitLab connection, which reaches only the projects an admin lets it. Name the project the build belongs to", login))
		}
		if login != "" {
			return "", authapi.Invalid(fmt.Errorf("%s reads through a GitHub App's installation, which reaches only the projects an admin lets it. Name the project the app builds in", login))
		}
		if service.IsSSHRepository(url) {
			return "", authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("an SSH repository needs a project's deploy key. Choose a project and set up a deploy key"), "unreadable")
		}
		return "", nil
	}
	pinned, err := h.gitLogins.PinGitLogin(ctx, project, url, login)
	if err != nil {
		var refusal *connect.Error
		if errors.As(err, &refusal) {
			return "", err
		}
		return "", authapi.ReasonError(connect.CodeFailedPrecondition, err, "unreadable")
	}
	return pinned, nil
}

// projectNamed is the project a request names by ID or name, or the one of the
// environment it names, by ID, or by name in that project; "" where it names
// neither.
func (h *Handler) projectNamed(ctx context.Context, project, environment string) (string, error) {
	if project == "" && environment == "" {
		return "", nil
	}
	var found string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if project != "" && p.ID != project && p.Name != project {
				continue
			}
			if environment == "" {
				found = p.ID
				return nil
			}
			for _, e := range p.Environments {
				if e.ID == environment || e.Name == environment {
					found = p.ID
					return nil
				}
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return "", err
	case found == "" && environment != "":
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no environment %s", environment))
	case found == "":
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no project %s", project))
	}
	return found, nil
}

// projectOfScope is the project environment env is in, or env itself when it
// names a project, or "" for none.
func (h *Handler) projectOfScope(ctx context.Context, env string) (string, error) {
	found := ""
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if p.ID == env {
				found = p.ID
			}
			for _, e := range p.Environments {
				if e.ID == env {
					found = p.ID
				}
			}
		}
		return nil
	})
	return found, err
}
