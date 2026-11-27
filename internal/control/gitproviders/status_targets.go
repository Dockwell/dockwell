// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/resource/gitlabconnection"
	"github.com/dockwell/dockwell/internal/store"
)

// GitHubStatusTarget is the resolved GitHub App installation and repository for
// reporting a CI deploy's check run.
type GitHubStatusTarget struct {
	App            *github.App
	AppID          string
	AppName        string
	InstallationID int64
	RepositoryID   int64
	FullName       string
	CredentialRef  string
}

// GitLabStatusTarget is the resolved GitLab connection and project path for
// reporting a CI deploy's commit status.
type GitLabStatusTarget struct {
	ConnectionID  string
	Revision      int
	Name          string
	BaseURL       string
	Token         string
	ProjectPath   string
	CredentialRef string
}

// githubFullName extracts owner/repo for GitHub CI status delivery, matching
// either the configured web base (for test stand-ins) or github.com.
func githubFullName(web, repoURL string) string {
	if full := fullName(web, repoURL); full != "" {
		return full
	}
	return fullName(github.Web, repoURL)
}

// FindGitHubStatusTarget resolves the GitHub App installation that reaches
// serviceID's project and repository repoURL for posting Check Runs.
// Returns (nil, skipReason, nil) when no installation can report for this deploy.
func (h *Handler) FindGitHubStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitHubStatusTarget, string, error) {
	full := githubFullName(h.cfg.Web, repoURL)
	if full == "" {
		return nil, "This deploy's repository isn't on GitHub.", nil
	}
	var (
		chosen        *GitHubStatusTarget
		chosenApp     kept
		sawApp        bool
		sawMissingChk bool
		sawSuspended  bool
		sawOutOfReach bool
	)
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		project, err := projectOf(ctx, tx, serviceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		sawApp = len(apps) > 0
		for _, a := range apps {
			ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
			if err != nil {
				return err
			}
			for i := range ins {
				in := &ins[i]
				if in.RemovedAt != nil {
					continue
				}
				var repoID int64
				for _, r := range in.Repositories {
					if strings.EqualFold(r.FullName, full) {
						repoID = r.ID
						break
					}
				}
				if repoID == 0 {
					continue
				}
				if in.SuspendedAt != nil {
					sawSuspended = true
					continue
				}
				if !githubapp.Reaches(a.spec, in.ID, project) {
					sawOutOfReach = true
					continue
				}
				if len(in.Permissions) > 0 && !github.HasChecksWrite(in.Permissions) {
					sawMissingChk = true
					continue
				}
				chosenApp = a
				chosen = &GitHubStatusTarget{
					AppID:          a.r.ID,
					AppName:        a.r.Name,
					InstallationID: in.ID,
					RepositoryID:   repoID,
					FullName:       full,
					CredentialRef:  "github_app:" + a.r.ID + "/" + strconv.FormatInt(in.ID, 10),
				}
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if chosen != nil {
		app, err := h.acting(chosenApp)
		if err != nil {
			return nil, "", err
		}
		chosen.App = app
		return chosen, "", nil
	}
	switch {
	case sawMissingChk:
		return nil, "GitHub App lacks Checks permission. Grant Checks (read and write) in Settings, Integrations, then retry.", nil
	case sawOutOfReach:
		return nil, "No GitHub App installation reaches this project. Allow the installation for this project in Settings, Integrations.", nil
	case sawSuspended:
		return nil, "The GitHub App installation for this repository is suspended on GitHub.", nil
	case !sawApp:
		return nil, "No GitHub App is connected. Connect GitHub in Settings, Integrations to report deploy status.", nil
	default:
		return nil, fmt.Sprintf("No GitHub App installation reaches %s. Add the repository to the installation on GitHub.", full), nil
	}
}

// MarkChecksPermissionMissing records that a GitHub App installation refused
// checks: write so Settings, Integrations surfaces the missing permission.
func (h *Handler) MarkChecksPermissionMissing(ctx context.Context, appID string, installationID int64) {
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		in, err := tx.GetGitHubInstallation(ctx, installationID)
		if err != nil {
			return err
		}
		if in.Permissions == nil {
			in.Permissions = map[string]string{}
		}
		in.Permissions[string(github.Checks)] = "none"
		in.UpdatedAt = h.now()
		return tx.SetGitHubInstallation(ctx, in)
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.cfg.Logger.WarnContext(ctx, "can't record missing GitHub Checks permission", slog.String("error", err.Error()))
	}
}

// FindGitLabStatusTarget resolves the GitLab connection matching repoURL and
// serviceID's project (most specific path_prefix wins).
// Returns (nil, skipReason, nil) when no connection matches.
func (h *Handler) FindGitLabStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitLabStatusTarget, string, error) {
	repoParsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || repoParsed.Host == "" {
		return nil, "This deploy's repository URL isn't valid.", nil //nolint:nilerr // invalid repo URL is a skip reason, not a store error
	}
	var (
		best          *GitLabStatusTarget
		bestPrefixLen = -1
		sawHost       bool
		sawOutOfReach bool
		sawAny        bool
	)
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		project, err := projectOf(ctx, tx, serviceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		all, err := h.allGitLab(ctx, tx)
		if err != nil {
			return err
		}
		sawAny = len(all) > 0
		for _, k := range all {
			projectPath := gitlab.ProjectPath(k.spec.GetInstanceUrl(), repoURL)
			if projectPath == "" {
				continue
			}
			sawHost = true
			if !gitlab.MatchesPrefix(projectPath, k.spec.GetPathPrefix()) {
				continue
			}
			if !gitlabconnection.Reaches(k.spec, project) {
				sawOutOfReach = true
				continue
			}
			prefixLen := len(k.spec.GetPathPrefix())
			if best == nil || prefixLen > bestPrefixLen {
				bestPrefixLen = prefixLen
				best = &GitLabStatusTarget{
					ConnectionID:  k.r.ID,
					Revision:      k.r.Revision,
					Name:          k.r.Name,
					BaseURL:       k.spec.GetInstanceUrl(),
					Token:         k.spec.GetToken(),
					ProjectPath:   projectPath,
					CredentialRef: "gitlab_connection:" + k.r.ID,
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if best != nil {
		return best, "", nil
	}
	switch {
	case sawOutOfReach:
		return nil, "No GitLab connection reaches this project. Allow the connection for this project in Settings, Integrations.", nil
	case sawHost:
		return nil, fmt.Sprintf("No GitLab connection's path prefix matches %s.", strings.Trim(repoParsed.Path, "/")), nil
	case !sawAny:
		return nil, "No GitLab connection is configured. Connect GitLab in Settings, Integrations to report deploy status.", nil
	default:
		return nil, fmt.Sprintf("No GitLab connection matches %s. Add one in Settings, Integrations.", repoParsed.Host), nil
	}
}
