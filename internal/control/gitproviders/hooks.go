// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/dockwell/dockwell/internal/control/githooks"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

var _ githooks.Apps = (*Handler)(nil)

// Webhook is the App whose webhook ID is hookID, with the secrets a delivery may be
// signed with now: its webhook secret, and the one before until it stops counting.
func (h *Handler) Webhook(ctx context.Context, hookID string) (githooks.App, bool, error) {
	var out githooks.App
	found := false
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			if hookID == "" || a.spec.GetHookId() != hookID {
				continue
			}
			out.ID, found = a.r.ID, true
			out.Secrets = [][]byte{[]byte(a.spec.GetWebhookSecret())}
			if prev := a.spec.GetPreviousWebhookSecret(); prev != "" && h.now().Unix() < a.spec.GetPreviousUntil() {
				out.Secrets = append(out.Secrets, []byte(prev))
			}
		}
		return nil
	})
	return out, found, err
}

// Installed reads installation of App appID again from GitHub, as an event of it
// says it changed, or keeps that GitHub removed it. One that's new, or back, and
// one removed, go in the audit log, as Dockwell heard them from GitHub.
func (h *Handler) Installed(ctx context.Context, appID string, installation int64, removed bool) error {
	k, err := h.one(ctx, appID)
	if err != nil {
		return err
	}
	var was store.GitHubInstallation
	known := true
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		was, err = tx.GetGitHubInstallation(ctx, installation)
		if errors.Is(err, store.ErrNotFound) {
			known = false
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if removed {
		// Only this App's own installation, and only once GitHub, asked as the App,
		// says it's gone: an event names it, and isn't the last word (review L2).
		if !known || was.AppID != k.r.ID || was.RemovedAt != nil {
			return nil
		}
		if app := h.app(k); app != nil {
			_, err := app.Installation(ctx, installation)
			var e *github.Error
			switch {
			case err == nil:
				return nil // still there
			case !errors.As(err, &e) || !e.NotFound():
				return fmt.Errorf("can't ask GitHub whether installation %d was removed: %w", installation, err)
			}
			app.Forget(installation)
		}
		now := h.now()
		was.ID, was.AppID, was.RemovedAt, was.UpdatedAt = installation, k.r.ID, &now, now
		return h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			if err := tx.SetGitHubInstallation(ctx, was); err != nil {
				return err
			}
			return h.audit(ctx, tx, revision.SystemActor, "github_app.uninstall", k.r.ID,
				fmt.Sprintf("GitHub says installation %d on %s was removed", installation, was.Account))
		})
	}
	in, err := h.Refresh(ctx, k, installation)
	if err != nil {
		return err
	}
	if known && was.RemovedAt == nil {
		return nil
	}
	return h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, revision.SystemActor, "github_app.install", k.r.ID,
			fmt.Sprintf("GitHub says it was installed on %s, installation %d, %d repositories", in.Account, in.ID, len(in.Repositories)))
	})
}

// app is the App k as it acts, or nil where its key can't be read.
func (h *Handler) app(k kept) *github.App {
	a, err := h.acting(k)
	if err != nil {
		return nil
	}
	return a
}

// projectOf is the project a service's environment is in.
func projectOf(ctx context.Context, tx *store.Tx, serviceID string) (string, error) {
	r, err := tx.GetResourceByID(ctx, serviceID)
	if err != nil {
		return "", err
	}
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return "", err
	}
	if p := projectIn(tree, r.Scope); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("service %s: %w", serviceID, store.ErrNotFound)
}

// projectIn is the project environment env is in, or "" for none.
func projectIn(tree scope.Tree, env string) string {
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			if e.ID == env {
				return p.ID
			}
		}
	}
	return ""
}

// Reaches says whether installation of App appID reaches the project of service
// serviceID, and is neither suspended nor removed.
func (h *Handler) Reaches(ctx context.Context, appID string, installation int64, serviceID string) (bool, error) {
	k, err := h.one(ctx, appID)
	if err != nil {
		return false, err
	}
	reach := false
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		in, err := tx.GetGitHubInstallation(ctx, installation)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		project, err := projectOf(ctx, tx, serviceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		reach = in.AppID == k.r.ID && in.Usable() && githubapp.Reaches(k.spec, installation, project)
		return nil
	})
	return reach, err
}

// Serves names the App one of whose installations reaches the project of service
// serviceID and lists the repository repoURL, or "" for none.
func (h *Handler) Serves(ctx context.Context, repoURL, serviceID string) (string, error) {
	full := fullName(h.cfg.Web, repoURL)
	if full == "" {
		return "", nil
	}
	name := ""
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		project, err := projectOf(ctx, tx, serviceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
			if err != nil {
				return err
			}
			for i := range ins {
				in := &ins[i]
				if !in.Usable() || !githubapp.Reaches(a.spec, in.ID, project) {
					continue
				}
				for _, r := range in.Repositories {
					if strings.EqualFold(r.FullName, full) && name == "" {
						name = a.r.Name
					}
				}
			}
		}
		return nil
	})
	return name, err
}

// fullName is a GitHub repository's owner and name from its URL, as "acme/web", or
// "" for one that isn't on GitHub, whose pages are at web.
func fullName(web, repoURL string) string {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	base, berr := url.Parse(web)
	if err != nil || berr != nil || !strings.EqualFold(u.Host, base.Host) {
		return ""
	}
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return ""
	}
	return owner + "/" + repo
}
