// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"errors"
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
// Returns (nil, reasonCode, nil) when no installation can report for this deploy.
// Installations that do not reach serviceID's project are skipped before
// inspecting repository lists so callers cannot enumerate repositories across
// projects.
func (h *Handler) FindGitHubStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitHubStatusTarget, string, error) {
	full := githubFullName(h.cfg.Web, repoURL)
	if full == "" {
		return nil, store.CIStatusReasonGitHubNotReached, nil
	}
	var (
		chosen        *GitHubStatusTarget
		chosenApp     kept
		sawApp        bool
		sawMissingChk bool
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
				if in.RemovedAt != nil || in.SuspendedAt != nil {
					continue
				}
				if !githubapp.Reaches(a.spec, in.ID, project) {
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
		return nil, store.CIStatusReasonMissingChecksPermission, nil
	case !sawApp:
		return nil, store.CIStatusReasonNoGitHubApp, nil
	default:
		return nil, store.CIStatusReasonGitHubNotReached, nil
	}
}

// VerifyChecksPermission re-reads the installation's live permissions from
// GitHub before concluding that checks: write is missing, so a repo-local 403
// (such as an archived repository) never disables reporting for the whole
// installation. Returns true only when GitHub confirms checks: write is absent.
func (h *Handler) VerifyChecksPermission(ctx context.Context, appID string, installationID int64) bool {
	k, err := h.one(ctx, appID)
	if err != nil {
		return false
	}
	app, err := h.acting(k)
	if err != nil {
		return false
	}
	got, err := app.Installation(ctx, installationID)
	if err != nil {
		return false
	}
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		in, err := tx.GetGitHubInstallation(ctx, installationID)
		if err != nil {
			return err
		}
		in.Permissions = got.Permissions
		in.SuspendedAt = got.Suspended
		in.UpdatedAt = h.now()
		return tx.SetGitHubInstallation(ctx, in)
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.cfg.Logger.WarnContext(ctx, "can't update GitHub installation permissions", slog.String("error", err.Error()))
	}
	return !github.HasChecksWrite(got.Permissions)
}

// GitHubPermissionsURL returns the HTTPS link on GitHub where an install admin
// can grant or accept missing Checks write permission for serviceID/repoURL.
func (h *Handler) GitHubPermissionsURL(ctx context.Context, serviceID, repoURL string) string {
	full := githubFullName(h.cfg.Web, repoURL)
	var out string
	_ = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		project, _ := projectOf(ctx, tx, serviceID)
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		var fallback string
		for _, a := range apps {
			ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
			if err != nil {
				continue
			}
			view := h.appProto(a, ins)
			if !view.GetChecksPermissionMissing() || view.GetPermissionsUrl() == "" {
				continue
			}
			if fallback == "" {
				fallback = view.GetPermissionsUrl()
			}
			if full == "" || project == "" {
				continue
			}
			for i := range ins {
				in := &ins[i]
				if in.RemovedAt != nil || in.SuspendedAt != nil || !githubapp.Reaches(a.spec, in.ID, project) {
					continue
				}
				for _, r := range in.Repositories {
					if strings.EqualFold(r.FullName, full) {
						out = view.GetPermissionsUrl()
						return nil
					}
				}
			}
		}
		if out == "" {
			out = fallback
		}
		return nil
	})
	return out
}

// FindGitLabStatusTarget resolves the GitLab connection matching repoURL and
// serviceID's project (most specific path_prefix wins).
// Returns (nil, reasonCode, nil) when no connection matches.
func (h *Handler) FindGitLabStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitLabStatusTarget, string, error) {
	repoParsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || repoParsed.Host == "" {
		return nil, store.CIStatusReasonGitLabNotReached, nil //nolint:nilerr // invalid repo URL is a skip reason, not a store error
	}
	var (
		best          *GitLabStatusTarget
		bestPrefixLen = -1
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
			if !gitlabconnection.Reaches(k.spec, project) {
				continue
			}
			projectPath := gitlab.ProjectPath(k.spec.GetInstanceUrl(), repoURL)
			if projectPath == "" {
				continue
			}
			if !gitlab.MatchesPrefix(projectPath, k.spec.GetPathPrefix()) {
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
	if !sawAny {
		return nil, store.CIStatusReasonNoGitLabConnection, nil
	}
	return nil, store.CIStatusReasonGitLabNotReached, nil
}
