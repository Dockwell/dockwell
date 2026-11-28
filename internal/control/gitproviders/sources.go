// SPDX-License-Identifier: AGPL-3.0-only

package gitproviders

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/gitref"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/resource/gitlabconnection"
	"github.com/dockwell/dockwell/internal/resource/service"
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
	ctx, target, err := s.authed(ctx, url)
	if err != nil {
		return "", "", err
	}
	return s.Base.Resolve(ctx, target, ref)
}

// Branches lists the repository's branches, as Base does.
func (s *Sources) Branches(ctx context.Context, url string) (head string, branches []string, err error) {
	lister, ok := s.Base.(branchLister)
	if !ok {
		return "", nil, errors.New("this control plane can't read a repository's branches")
	}
	ctx, target, err := s.authed(ctx, url)
	if err != nil {
		return "", nil, err
	}
	return lister.Branches(ctx, target)
}

// authed is ctx with the Authorization the call's git login gives, if it names one,
// and the target URL to query (rewritten to TestBase for GitLab test stand-ins).
func (s *Sources) authed(ctx context.Context, url string) (context.Context, string, error) {
	login := revision.GitLoginOf(ctx)
	if login == "" {
		return ctx, url, nil
	}
	apps := s.apps.Load()
	if apps == nil {
		return ctx, url, errors.New("this control plane has no Git provider to read a private repository with")
	}
	if _, isGL := service.GitLabConnectionOf(login); isGL {
		target, header, connName, err := apps.GitLabGitTarget(ctx, login, url, revision.GitScopeOf(ctx))
		if err != nil {
			return ctx, url, err
		}
		ctx = gitref.WithAuth(ctx, header)
		ctx = gitref.WithAuthProvider(ctx, "GitLab connection "+connName)
		return ctx, target, nil
	}
	header, err := apps.Authorization(ctx, login, url)
	if err != nil {
		return ctx, url, err
	}
	return gitref.WithAuth(ctx, header), url, nil
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
// must be a GitHub App installation or GitLab connection that reaches the project
// and covers repoURL. One left out picks the first matching GitHub App
// installation or the most specific matching GitLab connection, or "" where none
// matches, which fetches the repository as the public do.
func (h *Handler) PinGitLogin(ctx context.Context, tx *store.Tx, project, repoURL, login string) (string, error) {
	if connName, isGL := service.GitLabConnectionOf(login); isGL {
		return h.pinGitLabLogin(ctx, tx, project, repoURL, connName)
	}
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
		glLogin, err := h.pinGitLabLogin(ctx, tx, project, repoURL, "")
		if err != nil {
			return "", err
		}
		return glLogin, nil
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

// pinGitLabLogin validates an explicit GitLab connection name for project and
// repoURL, or picks the most specific reachable GitLab connection when connName
// is empty ONLY if that connection lists the project OR an anonymous smart-HTTP
// probe of repoURL answers 401/403/404. Public repositories on a connected
// GitLab host keep git_login == "" and build directly on the agent with no token.
func (h *Handler) pinGitLabLogin(ctx context.Context, tx *store.Tx, project, repoURL, connName string) (string, error) {
	conns, err := h.allGitLab(ctx, tx)
	if err != nil {
		return "", err
	}
	if connName != "" {
		if project == "" {
			return "", fmt.Errorf("the GitLab connection %s reaches only the projects an admin lets it. Name the project the build belongs to", connName)
		}
		for _, k := range conns {
			if k.r.Name != connName {
				continue
			}
			if !gitlabconnection.Reaches(k.spec, project) {
				return "", fmt.Errorf("the GitLab connection %s doesn't reach this project. An admin says which projects each connection reaches in Settings, Integrations", connName)
			}
			projectPath := gitlab.ProjectPath(k.spec.GetInstanceUrl(), repoURL)
			if projectPath == "" {
				return "", fmt.Errorf("the GitLab connection %s is for %s, not %s. Dockwell never sends a GitLab token to another host", connName, k.spec.GetInstanceUrl(), shown(repoURL))
			}
			if !gitlab.MatchesPrefix(projectPath, k.spec.GetPathPrefix()) {
				return "", fmt.Errorf("the GitLab connection %s covers %s, which doesn't include %s", connName, k.spec.GetPathPrefix(), projectPath)
			}
			return service.GitLabLoginPrefix + k.r.Name, nil
		}
		return "", fmt.Errorf("there's no GitLab connection %s any more. Connect it again in Settings, Integrations", connName)
	}
	if project == "" {
		return "", nil
	}
	type candidate struct {
		k           keptGitLab
		projectPath string
	}
	var matches []candidate
	for i := range conns {
		k := conns[i]
		if !gitlabconnection.Reaches(k.spec, project) {
			continue
		}
		projectPath := gitlab.ProjectPath(k.spec.GetInstanceUrl(), repoURL)
		if projectPath == "" || !gitlab.MatchesPrefix(projectPath, k.spec.GetPathPrefix()) {
			continue
		}
		matches = append(matches, candidate{k: k, projectPath: projectPath})
	}
	if len(matches) == 0 {
		return "", nil
	}
	slices.SortFunc(matches, func(a, b candidate) int {
		if d := len(b.k.spec.GetPathPrefix()) - len(a.k.spec.GetPathPrefix()); d != 0 {
			return d
		}
		return strings.Compare(a.k.r.Name, b.k.r.Name)
	})
	// 1. Prefer a matching connection whose project list already includes repoURL.
	for i := range matches {
		m := &matches[i]
		if h.gitlabListsProject(ctx, m.k, m.projectPath) {
			return service.GitLabLoginPrefix + m.k.r.Name, nil
		}
	}
	// 2. Otherwise probe repoURL anonymously: pin the most specific connection
	// only if the repository refuses unauthenticated access (401/403/404).
	if h.probeAnonymousGitLabNeedsAuth(ctx, repoURL) {
		return service.GitLabLoginPrefix + matches[0].k.r.Name, nil
	}
	return "", nil
}

func (h *Handler) gitlabListsProject(ctx context.Context, k keptGitLab, projectPath string) bool {
	baseURL := strings.TrimRight(k.spec.GetInstanceUrl(), "/")
	want := strings.ToLower(strings.Trim(projectPath, "/"))
	has := func(list []gitlab.Project) bool {
		for _, p := range list {
			if strings.ToLower(strings.Trim(p.PathWithNamespace, "/")) == want {
				return true
			}
		}
		return false
	}
	projects, hasMore, err := h.listGitLabProjectsCached(ctx, k, baseURL, "")
	if err != nil {
		return false
	}
	if has(projects) {
		return true
	}
	if !hasMore {
		return false
	}
	seg := projectPath
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	if seg == "" {
		return false
	}
	more, _, err := h.listGitLabProjectsCached(ctx, k, baseURL, seg)
	return err == nil && has(more)
}

func (h *Handler) probeAnonymousGitLabNeedsAuth(ctx context.Context, repoURL string) bool {
	targetURL, err := h.cfg.GitLab.GitURL(repoURL)
	if err != nil {
		return false
	}
	orig, err := neturl.Parse(strings.TrimRight(strings.TrimSpace(targetURL), "/"))
	if err != nil || orig.Host == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var client interface {
		Do(*http.Request) (*http.Response, error)
	}
	if hc := h.cfg.GitLab.HTTPClient(); hc != nil {
		c := *hc
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &c
	} else {
		client = netguard.Client(netguard.Options{Private: true, MaxBody: 64 << 10, Timeout: 5 * time.Second})
	}
	origRepo := strings.TrimSuffix(strings.TrimRight(orig.Path, "/"), ".git")
	cur := *orig
	for hop := range 2 {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, cur.String()+"/info/refs?service=git-upload-pack", http.NoBody)
		if err != nil {
			return false
		}
		req.Header.Set("User-Agent", "git/dockwell")
		req.Header.Set("Git-Protocol", "version=2")
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := strings.TrimSpace(resp.Header.Get("Location"))
			if loc == "" || hop == 1 {
				return false
			}
			next, err := req.URL.Parse(loc)
			if err != nil || next.User != nil || !strings.EqualFold(next.Scheme, orig.Scheme) || !strings.EqualFold(next.Host, orig.Host) {
				return false
			}
			p := strings.TrimRight(strings.TrimSuffix(next.Path, "/info/refs"), "/")
			if strings.TrimSuffix(p, ".git") != origRepo {
				return false
			}
			cur.Path = p
		default:
			return false
		}
	}
	return false
}

func (h *Handler) gitlabForOp(ctx context.Context, login, repoURL, scopeID string) (keptGitLab, string, error) {
	connName, ok := service.GitLabConnectionOf(login)
	if !ok {
		return keptGitLab{}, "", fmt.Errorf("%q isn't a GitLab connection login", login)
	}
	if scopeID == "" {
		scopeID = revision.GitScopeOf(ctx)
	}
	if scopeID == "" {
		return keptGitLab{}, "", fmt.Errorf("the GitLab connection %s reaches only the projects an admin lets it, and this request named none", connName)
	}
	var found keptGitLab
	var projectID string
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if p.ID == scopeID || p.Name == scopeID {
				projectID = p.ID
				break
			}
			for _, e := range p.Environments {
				if e.ID == scopeID {
					projectID = p.ID
					break
				}
			}
			if projectID != "" {
				break
			}
		}
		if projectID == "" {
			return fmt.Errorf("the GitLab connection %s doesn't reach this project any more. Check Settings, Integrations", connName)
		}
		conns, err := h.allGitLab(ctx, tx)
		if err != nil {
			return err
		}
		for _, k := range conns {
			if k.r.Name == connName {
				found = k
				return nil
			}
		}
		return fmt.Errorf("there's no GitLab connection %s any more. Connect it again in Settings, Integrations", connName)
	})
	if err != nil {
		return keptGitLab{}, "", err
	}
	if !gitlabconnection.Reaches(found.spec, projectID) {
		return keptGitLab{}, "", fmt.Errorf("the GitLab connection %s doesn't reach this project any more, so its build can't fetch %s. An admin lets the connection reach the project in Settings, Integrations", connName, shown(repoURL))
	}
	projectPath := gitlab.ProjectPath(found.spec.GetInstanceUrl(), repoURL)
	if projectPath == "" {
		return keptGitLab{}, "", fmt.Errorf("the GitLab connection %s is for %s, not %s. Dockwell never sends a GitLab token to another host", connName, found.spec.GetInstanceUrl(), shown(repoURL))
	}
	if !gitlab.MatchesPrefix(projectPath, found.spec.GetPathPrefix()) {
		return keptGitLab{}, "", fmt.Errorf("the GitLab connection %s covers %s, which doesn't include %s", connName, found.spec.GetPathPrefix(), projectPath)
	}
	return found, projectID, nil
}

// GitLabGitTarget verifies that login covers repoURL and scopeID's project and
// returns the smart-HTTP target URL, the Basic oauth2 header (never leaving the
// control plane), and the connection name.
func (h *Handler) GitLabGitTarget(ctx context.Context, login, repoURL, scopeID string) (targetURL, authHeader, connName string, err error) {
	k, _, err := h.gitlabForOp(ctx, login, repoURL, scopeID)
	if err != nil {
		return "", "", "", err
	}
	targetURL, err = h.cfg.GitLab.GitURL(repoURL)
	if err != nil {
		return "", "", "", err
	}
	authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+k.spec.GetToken()))
	return targetURL, authHeader, k.r.Name, nil
}

// PreparedGitLabPack is a validated GitLab pack-fetch target ready to run
// inside the control-plane pack concurrency gate.
type PreparedGitLabPack struct {
	ProjectID  string
	TargetURL  string
	AuthHeader string
	ConnName   string
}

// PrepareGitLabPackIn validates the GitLab connection, project reach, host and
// path prefix before any concurrency gate slot is taken.
func (h *Handler) PrepareGitLabPackIn(ctx context.Context, login, repoURL, scopeID string) (PreparedGitLabPack, error) {
	k, projectID, err := h.gitlabForOp(ctx, login, repoURL, scopeID)
	if err != nil {
		return PreparedGitLabPack{}, err
	}
	targetURL, err := h.cfg.GitLab.GitURL(repoURL)
	if err != nil {
		return PreparedGitLabPack{}, err
	}
	return PreparedGitLabPack{
		ProjectID:  projectID,
		TargetURL:  targetURL,
		AuthHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+k.spec.GetToken())),
		ConnName:   k.r.Name,
	}, nil
}

// FetchPreparedGitLabPack fetches and verifies a shallow Git pack over HTTPS on
// the control plane for a previously validated PreparedGitLabPack.
func (h *Handler) FetchPreparedGitLabPack(ctx context.Context, prep PreparedGitLabPack, ref, commit string) (io.ReaderAt, int64, func(), error) {
	f, size, err := gitfetch.FetchVerifiedPack(ctx, gitfetch.PackFetchOptions{
		RepoURL:         prep.TargetURL,
		Ref:             ref,
		Commit:          commit,
		AuthHeader:      prep.AuthHeader,
		CredentialLabel: fmt.Sprintf("the GitLab connection %s's token", prep.ConnName),
		Client:          h.cfg.GitLab.HTTPClient(),
		AllowPrivate:    true,
		PackTempDir:     h.cfg.PackTempDir,
	})
	if err != nil {
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

// FetchGitLabPackIn fetches and verifies a shallow Git pack over HTTPS on the
// control plane using the GitLab connection's token (never sending the token to
// any agent or to another host) (ADR 0113).
func (h *Handler) FetchGitLabPackIn(ctx context.Context, login, repoURL, ref, commit, scopeID string) (io.ReaderAt, int64, func(), error) {
	prep, err := h.PrepareGitLabPackIn(ctx, login, repoURL, scopeID)
	if err != nil {
		return nil, 0, nil, err
	}
	return h.FetchPreparedGitLabPack(ctx, prep, ref, commit)
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
