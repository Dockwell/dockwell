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

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/resource/gitlabconnection"
	"github.com/dockwell/dockwell/internal/scope"
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
// can grant or accept missing Checks write permission for the App whose
// installation reaches serviceID and repoURL (N12: never falls back to another
// App).
func (h *Handler) GitHubPermissionsURL(ctx context.Context, serviceID, repoURL string) string {
	full := githubFullName(h.cfg.Web, repoURL)
	if full == "" || serviceID == "" {
		return ""
	}
	var out string
	_ = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		project, err := projectOf(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if project == "" {
			return nil
		}
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
			if err != nil {
				continue
			}
			view := h.appProto(a, ins)
			if !view.GetChecksPermissionMissing() || view.GetPermissionsUrl() == "" {
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
		return nil
	})
	return out
}

// canonicalRepo normalizes raw to a canonical HTTPS repository URL, translating
// test stand-in GitHub loopback URLs (e.g. http://127.0.0.1:PORT/owner/repo) to
// https://github.com/owner/repo so build sources and deploy tokens compare alike.
func (h *Handler) canonicalRepo(raw string) string {
	if norm, err := store.NormalizeDeployRepository(raw); err == nil && norm != "" {
		return norm
	}
	if full := githubFullName(h.cfg.Web, raw); full != "" {
		return "https://github.com/" + full
	}
	return ""
}

// serviceGitRepoInTx returns the canonical Git build source URL of serviceID,
// or "" if the service does not build from Git.
func (h *Handler) serviceGitRepoInTx(ctx context.Context, tx *store.Tx, serviceID string) (string, error) {
	r, err := tx.GetResourceByID(ctx, serviceID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (r.Kind != "service" || r.Spec == nil)) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	decoded, unreadable, err := h.cfg.Keeper.DecodeEach(ctx, tx, []store.StoredSpec{{
		At:   store.InRevision(r.ID, r.Kind, r.Revision),
		Spec: r.Spec,
	}})
	if err != nil {
		return "", err
	}
	if len(unreadable) > 0 && unreadable[0] != nil {
		return "", nil //nolint:nilerr // unreadable service spec falls back to non-Git ownership check
	}
	spec, ok := decoded[0].(*resourcev1.Service)
	if !ok {
		return "", nil
	}
	return h.canonicalRepo(spec.GetBuild().GetGit().GetUrl()), nil
}

// claimedByOtherProjectSet returns the lowercase canonical HTTPS repository URLs
// used by any project other than projectID, either as a service's Git build
// source or as a live deploy token's bound status repository (N6).
func (h *Handler) claimedByOtherProjectSet(ctx context.Context, tx *store.Tx, projectID string) (map[string]bool, error) {
	claimed := map[string]bool{}
	tokens, err := tx.LiveDeployTokenRepositories(ctx, h.now())
	if err != nil {
		return nil, err
	}
	for _, b := range tokens {
		if b.ProjectID != "" && b.ProjectID != projectID {
			if norm := h.canonicalRepo(b.Repository); norm != "" {
				claimed[strings.ToLower(norm)] = true
			}
		}
	}
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	envProject := map[string]string{}
	for _, p := range tree.Projects {
		for _, env := range p.Environments {
			envProject[env.ID] = p.ID
		}
	}
	services, err := tx.ListKind(ctx, "service")
	if err != nil {
		return nil, err
	}
	var others []store.Resource
	var specs []store.StoredSpec
	for i := range services {
		r := services[i]
		if r.Spec == nil {
			continue
		}
		if p := envProject[r.Scope]; p != "" && p != projectID {
			others = append(others, r)
			specs = append(specs, store.StoredSpec{
				At:   store.InRevision(r.ID, r.Kind, r.Revision),
				Spec: r.Spec,
			})
		}
	}
	if len(specs) > 0 {
		decoded, unreadable, err := h.cfg.Keeper.DecodeEach(ctx, tx, specs)
		if err != nil {
			return nil, err
		}
		for i := range others {
			if unreadable[i] != nil {
				continue
			}
			spec, ok := decoded[i].(*resourcev1.Service)
			if !ok {
				continue
			}
			if norm := h.canonicalRepo(spec.GetBuild().GetGit().GetUrl()); norm != "" {
				claimed[strings.ToLower(norm)] = true
			}
		}
	}
	return claimed, nil
}

// canReachDeployRepositoryInTx reports whether any usable GitHub App
// installation or GitLab connection reaching projectID covers normRepo (N7).
func (h *Handler) canReachDeployRepositoryInTx(ctx context.Context, tx *store.Tx, projectID, normRepo string) (bool, error) {
	if full := githubFullName(h.cfg.Web, normRepo); full != "" {
		apps, err := h.all(ctx, tx)
		if err != nil {
			return false, err
		}
		for _, a := range apps {
			ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
			if err != nil {
				return false, err
			}
			for i := range ins {
				in := &ins[i]
				if in.RemovedAt != nil || in.SuspendedAt != nil {
					continue
				}
				if !githubapp.Reaches(a.spec, in.ID, projectID) {
					continue
				}
				for _, r := range in.Repositories {
					if strings.EqualFold(r.FullName, full) {
						return true, nil
					}
				}
			}
		}
	}
	conns, err := h.allGitLab(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, k := range conns {
		if !gitlabconnection.Reaches(k.spec, projectID) {
			continue
		}
		projectPath := gitlab.ProjectPath(k.spec.GetInstanceUrl(), normRepo)
		if projectPath == "" {
			continue
		}
		if gitlab.MatchesPrefix(projectPath, k.spec.GetPathPrefix()) {
			return true, nil
		}
	}
	return false, nil
}

// ValidateDeployRepositoryInTx enforces N7 (project reach without repository
// existence disclosure) and N6 (project ownership / install-admin gating) when
// creating or rebinding a deploy token's status repository.
func (h *Handler) ValidateDeployRepositoryInTx(ctx context.Context, tx *store.Tx, serviceID, projectID, normRepo string, isInstallAdmin bool) error {
	if normRepo == "" {
		return nil
	}
	reached, err := h.canReachDeployRepositoryInTx(ctx, tx, projectID, normRepo)
	if err != nil {
		return err
	}
	if !reached {
		return fmt.Errorf("%w: no GitHub App or GitLab connection connected to this project reaches that repository", auth.ErrInvalid)
	}
	if isInstallAdmin {
		return nil
	}
	appRepo, err := h.serviceGitRepoInTx(ctx, tx, serviceID)
	if err != nil {
		return err
	}
	if appRepo != "" {
		if store.SameDeployRepository(appRepo, normRepo) {
			return nil
		}
		return auth.ErrDeployRepoAdminOnly
	}
	claimed, err := h.claimedByOtherProjectSet(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if claimed[strings.ToLower(normRepo)] {
		return auth.ErrDeployRepoAdminOnly
	}
	return nil
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
