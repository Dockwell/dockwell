// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/gitref"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// branchLister lists a repository's branches, as gitref.Resolver does.
type branchLister interface {
	Branches(ctx context.Context, url string) (head string, branches []string, err error)
}

// Sources reads repositories as Base does, with a GitHub App installation's token
// for the repository alone where the call's build names one (revision.GitLoginOf).
// A plan checked the login against the project; a read only uses it.
type Sources struct {
	Base revision.Sources
	apps atomic.Pointer[Handler]
}

// SetApps has it ask apps for tokens, once the handler is made.
func (s *Sources) SetApps(apps *Handler) { s.apps.Store(apps) }

var _ revision.Sources = (*Sources)(nil)

// Resolve gives what ref names in the repository at url, as Base does.
func (s *Sources) Resolve(ctx context.Context, url, ref string) (commit, full string, err error) {
	ctx, err = s.authed(ctx, url)
	if err != nil {
		return "", "", err
	}
	return s.Base.Resolve(ctx, url, ref)
}

// Branches lists the repository's branches, as Base does.
func (s *Sources) Branches(ctx context.Context, url string) (head string, branches []string, err error) {
	lister, ok := s.Base.(branchLister)
	if !ok {
		return "", nil, errors.New("this control plane can't read a repository's branches")
	}
	ctx, err = s.authed(ctx, url)
	if err != nil {
		return "", nil, err
	}
	return lister.Branches(ctx, url)
}

// authed is ctx with the Authorization the call's git login gives, if it names one.
func (s *Sources) authed(ctx context.Context, url string) (context.Context, error) {
	login := revision.GitLoginOf(ctx)
	if login == "" {
		return ctx, nil
	}
	apps := s.apps.Load()
	if apps == nil {
		return ctx, errors.New("this control plane has no GitHub App to read a private repository with")
	}
	header, err := apps.Authorization(ctx, login, url)
	if err != nil {
		return ctx, err
	}
	return gitref.WithAuth(ctx, header), nil
}

// installationOf reads the App and installation a git login names, as
// "<App name>/<installation ID>".
func (h *Handler) installationOf(ctx context.Context, login string) (kept, store.GitHubInstallation, error) {
	name, idText, ok := strings.Cut(login, "/")
	installation, err := strconv.ParseInt(idText, 10, 64)
	if !ok || err != nil || installation <= 0 {
		return kept{}, store.GitHubInstallation{}, fmt.Errorf("%q isn't a GitHub App's installation, as <App name>/<installation ID> names it", login)
	}
	k, err := h.one(ctx, name)
	if err != nil {
		return kept{}, store.GitHubInstallation{}, fmt.Errorf("there's no GitHub App %s any more. Connect it again from Settings, Integrations", name)
	}
	var in store.GitHubInstallation
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		in, err = tx.GetGitHubInstallation(ctx, installation)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && in.AppID != k.r.ID:
		return kept{}, store.GitHubInstallation{}, fmt.Errorf("the GitHub App %s has no installation %d. Install it from GitHub's page for the App", name, installation)
	case err != nil:
		return kept{}, store.GitHubInstallation{}, err
	case in.RemovedAt != nil:
		return kept{}, store.GitHubInstallation{}, fmt.Errorf("the GitHub App %s was removed from %s on GitHub. Install it there again", name, in.Account)
	case in.SuspendedAt != nil:
		return kept{}, store.GitHubInstallation{}, fmt.Errorf("the GitHub App %s is suspended on %s. Unsuspend it in the installation's settings on GitHub", name, in.Account)
	}
	return k, in, nil
}

// Authorization is the header that reads the repository at url through the
// installation login names: Basic, with a token for that repository alone, which
// may read its contents and nothing else. The token is asked for as needed, kept
// in memory, and ends within the hour.
func (h *Handler) Authorization(ctx context.Context, login, url string) (string, error) {
	k, in, err := h.installationOf(ctx, login)
	if err != nil {
		return "", err
	}
	full := fullName(h.cfg.Web, url)
	var repo int64
	for _, r := range in.Repositories {
		if full != "" && strings.EqualFold(r.FullName, full) {
			repo = r.ID
		}
	}
	if repo == 0 {
		return "", fmt.Errorf("the GitHub App %s's installation on %s doesn't reach %s. Add it to the installation on GitHub", k.r.Name, in.Account, shown(url))
	}
	app, err := h.acting(k)
	if err != nil {
		return "", fmt.Errorf("the GitHub App %s's key can't be read: %w", k.r.Name, err)
	}
	tok, err := app.Token(ctx, in.ID, []int64{repo}, github.Contents)
	h.noted(k.r.ID, err)
	if err != nil {
		var e *github.Error
		if errors.As(err, &e) && e.Gone() {
			return "", fmt.Errorf("GitHub refused the App %s: it was deleted on GitHub, or its key was. Connect it again from Settings, Integrations", k.r.Name)
		}
		return "", fmt.Errorf("GitHub gave no token to read %s: %w", shown(url), err)
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok)), nil
}

// AuthorizationIn is Authorization for a build of an app in environment, as its
// change applies: the installation must still reach the environment's project, as
// an admin may have narrowed it since the plan was made (review L3). An empty
// environment checks no project.
func (h *Handler) AuthorizationIn(ctx context.Context, login, url, environment string) (string, error) {
	if environment != "" {
		k, in, err := h.installationOf(ctx, login)
		if err != nil {
			return "", err
		}
		project := ""
		if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
			tree, err := scope.Load(ctx, tx)
			project = projectIn(tree, environment)
			return err
		}); err != nil {
			return "", err
		}
		if project == "" || !githubapp.Reaches(k.spec, in.ID, project) {
			return "", fmt.Errorf("the GitHub App installation %s doesn't reach this app's project any more, so its build can't fetch %s. An admin lets the installation reach the project in Settings, Integrations, then deploy again", login, shown(url))
		}
	}
	return h.Authorization(ctx, login, url)
}

// shown is a repository's URL as an error says it: no user, no query.
func shown(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		rest := url[i+3:]
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			url = url[:i+3] + rest[at+1:]
		}
	}
	url, _, _ = strings.Cut(url, "?")
	return strings.TrimSuffix(url, ".git")
}

var _ changes.GitLogins = (*Handler)(nil)

// PinGitLogin checks a build's git login for project, as a plan is made: one named
// must be an installation that reaches the project and lists the repository at
// repoURL. One left out is the first installation, by App and ID, that does, or ""
// where none does, which fetches the repository as the public do.
func (h *Handler) PinGitLogin(ctx context.Context, tx *store.Tx, project, repoURL, login string) (string, error) {
	full := fullName(h.cfg.Web, repoURL)
	apps, err := h.all(ctx, tx)
	if err != nil {
		return "", err
	}
	type reaching struct {
		login, account string
		listed         bool
	}
	var reach []reaching
	for _, a := range apps {
		ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
		if err != nil {
			return "", err
		}
		for i := range ins {
			in := &ins[i]
			if !in.Usable() || !githubapp.Reaches(a.spec, in.ID, project) {
				continue
			}
			r := reaching{login: a.r.Name + "/" + strconv.FormatInt(in.ID, 10), account: in.Account}
			for _, repo := range in.Repositories {
				if full != "" && strings.EqualFold(repo.FullName, full) {
					r.listed = true
				}
			}
			reach = append(reach, r)
		}
	}
	if login == "" {
		for _, r := range reach {
			if r.listed {
				return r.login, nil
			}
		}
		return "", nil
	}
	accounts := make([]string, 0, len(reach))
	for _, r := range reach {
		if r.login != login {
			accounts = append(accounts, r.account)
			continue
		}
		if !r.listed {
			return "", fmt.Errorf("the GitHub App's installation on %s doesn't reach %s. Add the repository to the installation on GitHub", r.account, shown(repoURL))
		}
		return login, nil
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("the GitHub App installation %s doesn't reach this project, and none does. An admin says which projects each installation reaches in Settings, Integrations", login)
	}
	return "", fmt.Errorf("the GitHub App installation %s doesn't reach this project. The project's are on %s", login, strings.Join(accounts, ", "))
}

// DockerBuildAuthorization rechecks the reviewed project's access before a
// standalone builder fetches a private repository.
func (h *Handler) DockerBuildAuthorization(ctx context.Context, login, url, project string) (string, error) {
	if project == "" {
		return "", errors.New("a private repository requires a credential project")
	}
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		_, err := h.PinGitLogin(ctx, tx, project, url, login)
		return err
	})
	if err != nil {
		return "", err
	}
	return h.Authorization(ctx, login, url)
}
