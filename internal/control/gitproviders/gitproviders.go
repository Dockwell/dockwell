// SPDX-License-Identifier: AGPL-3.0-only

// Package gitproviders implements GitProviderService: connecting GitHub as an App
// Dockwell makes through GitHub's manifest flow, its installations, and the
// repositories a project's apps may build from through them. The App's private
// key and webhook secret are opened here, on the control plane, and go no further:
// no answer, change's plan, event or log holds them, nor a token GitHub gives. See
// docs/adr/0098-connecting-github-as-an-app.md.
package gitproviders

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Bounds on connecting.
const (
	// stateLasts is how long a manifest's state may come back.
	stateLasts = 10 * time.Minute
	// maxStarting bounds the connections begun and not finished.
	maxStarting = 64
	// keepOldSecret is how long the secret before a new one still counts, for
	// deliveries GitHub sent before it.
	keepOldSecret = 10 * time.Minute
	// maxKey bounds a private key's file.
	maxKey = 16 << 10
)

// Config is what the handler needs.
type Config struct {
	Auth    *auth.Service
	DB      *store.DB
	Keeper  *secrets.Keeper
	Changes *changes.Handler
	Logger  *slog.Logger
	// API and Web are GitHub's API and pages: github.API and github.Web unless a
	// test's.
	API, Web string
	// Client reaches GitHub: github.NewClient unless a test's.
	Client github.Doer
	// Now is the clock: time.Now unless a test's.
	Now func() time.Time
}

// Handler implements GitProviderService.
type Handler struct {
	cfg Config

	mu sync.Mutex
	// starting is each connection begun, by the hash of its state.
	starting map[string]starting
	// apps is each App as it acts, by its resource's ID, for the revision it was
	// made from, so its tokens are kept in memory between calls.
	apps map[string]*actingApp
	// gone are Apps GitHub refused, by resource ID.
	gone map[string]bool
	// watch is whether each App's deliveries reach the panel, by resource ID;
	// branches what the last check of each hook's branch said, by hook ID; and
	// pusher what deploys a push polling finds.
	watch    map[string]*watching
	branches map[string]branch
	pusher   Pusher
}

// starting is a connection begun: for whom, and the webhook ID its manifest named.
type starting struct {
	user, session, org, hookID string
	at                         time.Time
}

// actingApp is an App as it acts, for one revision of its resource.
type actingApp struct {
	revision int
	app      *github.App
}

// New returns the handler.
func New(cfg Config) *Handler {
	if cfg.API == "" {
		cfg.API = github.API
	}
	if cfg.Web == "" {
		cfg.Web = github.Web
	}
	if cfg.Client == nil {
		cfg.Client = github.NewClient()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Handler{
		cfg: cfg, starting: map[string]starting{}, apps: map[string]*actingApp{}, gone: map[string]bool{},
		watch: map[string]*watching{}, branches: map[string]branch{},
	}
}

var _ dockwellv1connect.GitProviderServiceHandler = (*Handler)(nil)

func (h *Handler) now() time.Time {
	if h.cfg.Now != nil {
		return h.cfg.Now()
	}
	return time.Now()
}

func (h *Handler) internal(ctx context.Context, doing string, err error) error {
	h.cfg.Logger.ErrorContext(ctx, "the GitHub App's handler failed", slog.String("doing", doing), slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, fmt.Errorf("the control plane failed %s. Try again, and check its logs if it keeps failing", doing))
}

// said is a refusal in words a person reads, with its reason.
func said(code connect.Code, reason, format string, args ...any) error {
	return authapi.ReasonError(code, fmt.Errorf(format, args...), reason)
}

// fromGitHub says what GitHub said, by its status and message, and what to do.
func fromGitHub(doing string, err error) error {
	var e *github.Error
	switch {
	case errors.As(err, &e) && e.Gone():
		return said(connect.CodeFailedPrecondition, "github_app_gone", "GitHub refused the App %s: it was deleted on GitHub, or its key was. Connect it again from Settings, Integrations", doing)
	case errors.As(err, &e):
		return said(connect.CodeFailedPrecondition, "github_refused", "GitHub refused %s: %s", doing, e.Error())
	default:
		return said(connect.CodeUnavailable, "github_unreachable", "Dockwell couldn't reach GitHub %s: %s. Try again in a minute", doing, err.Error())
	}
}

// panel is the panel's address, as GitHub reaches it: its domain, or the address
// the person is using.
func (h *Handler) panel(ctx context.Context) (*url.URL, error) {
	origin := ""
	if domain := h.cfg.Auth.Sites().Domain; domain != "" {
		origin = "https://" + domain
	} else if client, ok := authapi.ClientFrom(ctx); ok {
		origin = client.Site.Origin
	}
	u, err := url.Parse(origin)
	if err != nil || origin == "" || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, said(connect.CodeFailedPrecondition, "no_panel_address", "Dockwell can't tell the panel's address to give GitHub. Open the panel by its address, or give it a domain")
	}
	return u, nil
}

// random is n random bytes, as base64url.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// randomHex is n random bytes, as hex: what a webhook secret is made of.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// StartGitHubApp begins connecting GitHub.
func (h *Handler) StartGitHubApp(ctx context.Context, req *connect.Request[dockwellv1.StartGitHubAppRequest]) (*connect.Response[dockwellv1.StartGitHubAppResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	org := strings.TrimSpace(req.Msg.GetOrganization())
	panel, err := h.panel(ctx)
	if err != nil {
		return nil, err
	}
	state := random(32)
	action, err := github.NewAppURL(h.cfg.Web, org, state)
	if err != nil {
		return nil, said(connect.CodeInvalidArgument, "invalid", "%s", err.Error())
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpConnectGitHubApp, p.User.ID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	hookID := "ghw_" + strings.NewReplacer("-", "", "_", "").Replace(random(24))
	now := h.now()
	h.mu.Lock()
	for k, s := range h.starting {
		if now.Sub(s.at) > stateLasts {
			delete(h.starting, k)
		}
	}
	if len(h.starting) >= maxStarting {
		h.mu.Unlock()
		giveBack()
		return nil, said(connect.CodeResourceExhausted, "too_many", "%d connections to GitHub are begun and not finished. Finish one, or wait 10 minutes", maxStarting)
	}
	h.starting[hashOf(state)] = starting{user: p.User.ID, session: p.Session.ID, org: org, hookID: hookID, at: now}
	h.mu.Unlock()
	return connect.NewResponse(&dockwellv1.StartGitHubAppResponse{
		ActionUrl: action,
		Manifest:  github.NewManifest(panel, hookID).JSON(),
	}), nil
}

// take uses a connection's state once: it was begun in this session, by this
// person, in the last 10 minutes.
func (h *Handler) take(p auth.Principal, state string) (starting, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := hashOf(state)
	s, ok := h.starting[key]
	if !ok {
		return starting{}, false
	}
	delete(h.starting, key)
	fresh := h.now().Sub(s.at) <= stateLasts
	return s, fresh && s.user == p.User.ID && s.session == p.Session.ID
}

// FinishGitHubApp finishes connecting GitHub.
func (h *Handler) FinishGitHubApp(ctx context.Context, req *connect.Request[dockwellv1.FinishGitHubAppRequest]) (*connect.Response[dockwellv1.FinishGitHubAppResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Session.ID == "" {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	s, ok := h.take(p, req.Msg.GetState())
	if !ok {
		return nil, said(connect.CodePermissionDenied, "github_state", "this page's link from GitHub isn't for your session, was used, or is past 10 minutes. Connect GitHub again")
	}
	conv, err := github.Convert(ctx, h.cfg.Client, h.cfg.API, req.Msg.GetCode())
	if err != nil {
		return nil, fromGitHub("making the App", err)
	}
	key, err := github.ParseKey([]byte(conv.PEM))
	if err != nil {
		return nil, said(connect.CodeFailedPrecondition, "github_refused", "GitHub's key for the App can't be read: %s. Connect GitHub again", err.Error())
	}
	fp, err := github.Fingerprint(key)
	if err != nil {
		return nil, h.internal(ctx, "reading the App's key", err)
	}
	spec := &resourcev1.GitHubApp{
		AppId: conv.ID, Slug: conv.Slug, Owner: conv.Owner.Login, OwnerIsOrganization: conv.Owner.Type == "Organization",
		ClientId: conv.ClientID, PrivateKey: conv.PEM, WebhookSecret: conv.WebhookSecret, KeyFingerprint: fp, HookId: s.hookID,
	}
	changeID, err := h.change(ctx, "Connect the GitHub App "+conv.Slug, conv.Slug, spec)
	if err != nil {
		return nil, err
	}
	app, err := h.view(ctx, conv.Slug)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.FinishGitHubAppResponse{
		App: app, InstallUrl: github.InstallURL(h.cfg.Web, conv.Slug), ChangeId: changeID,
	}), nil
}

// change keeps spec as the App named name, as a change the caller proposes and
// applies, held to what ChangeService holds them to; nil deletes it.
func (h *Handler) change(ctx context.Context, summary, name string, spec *resourcev1.GitHubApp) (string, error) {
	edit := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(githubapp.Kind), Name: name}}
	if spec == nil {
		edit.Delete = true
	} else {
		edit.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: spec}}
	}
	propose := &dockwellv1.ProposeChangeRequest{Summary: summary, Edits: []*dockwellv1.Edit{edit}}
	pctx, err := authapi.Check(ctx, h.cfg.Auth, h.cfg.Logger, dockwellv1connect.ChangeServiceProposeChangeProcedure, propose)
	if err != nil {
		return "", err
	}
	planned, err := h.cfg.Changes.ProposeChange(pctx, connect.NewRequest(propose))
	if err != nil {
		return "", err
	}
	c := planned.Msg.GetChange()
	apply := &dockwellv1.ApplyChangeRequest{Id: c.GetId(), Fingerprint: c.GetPlan().GetFingerprint()}
	actx, err := authapi.Check(ctx, h.cfg.Auth, h.cfg.Logger, dockwellv1connect.ChangeServiceApplyChangeProcedure, apply)
	if err != nil {
		return c.GetId(), err
	}
	if _, err := h.cfg.Changes.ApplyChange(actx, connect.NewRequest(apply)); err != nil {
		return c.GetId(), err
	}
	return c.GetId(), nil
}

// kept is an App as it's kept: its resource, and its spec, opened.
type kept struct {
	r    store.Resource
	spec *resourcev1.GitHubApp
}

// all opens every App, by name.
func (h *Handler) all(ctx context.Context, tx *store.Tx) ([]kept, error) {
	stored, err := tx.ListKind(ctx, string(githubapp.Kind))
	if err != nil {
		return nil, err
	}
	var out []kept
	for i := range stored {
		r := stored[i]
		if r.Spec == nil {
			continue
		}
		spec, err := h.cfg.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read the GitHub App %s: %w", r.Name, err)
		}
		if a, ok := spec.(*resourcev1.GitHubApp); ok {
			out = append(out, kept{r: r, spec: a})
		}
	}
	slices.SortFunc(out, func(a, b kept) int { return strings.Compare(a.r.Name, b.r.Name) })
	return out, nil
}

// one opens the App whose ID or name is ref.
func (h *Handler) one(ctx context.Context, ref string) (kept, error) {
	var found kept
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			// Or by its webhook's ID, which GitHub's setup URL names, as the manifest
			// is made before the App has an ID of its own.
			if a.r.ID == ref || a.r.Name == ref || ref != "" && a.spec.GetHookId() == ref {
				found = a
				return nil
			}
		}
		return said(connect.CodeNotFound, "not_found", "there's no GitHub App %s. Connect one from Settings, Integrations", ref)
	})
	return found, err
}

// acting is the App k as it acts, made once for its revision.
func (h *Handler) acting(k kept) (*github.App, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a, ok := h.apps[k.r.ID]; ok && a.revision == k.r.Revision {
		return a.app, nil
	}
	key, err := github.ParseKey([]byte(k.spec.GetPrivateKey()))
	if err != nil {
		return nil, fmt.Errorf("the GitHub App %s's key can't be read: %w", k.r.Name, err)
	}
	app := &github.App{ID: k.spec.GetAppId(), Key: key, Base: h.cfg.API, Client: h.cfg.Client, Now: h.cfg.Now}
	h.apps[k.r.ID] = &actingApp{revision: k.r.Revision, app: app}
	return app, nil
}

// noted keeps whether GitHub refused the App.
func (h *Handler) noted(appID string, err error) {
	var e *github.Error
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case err == nil:
		delete(h.gone, appID)
	case errors.As(err, &e) && e.Gone():
		h.gone[appID] = true
	}
}

// AddGitHubInstallation records an installation GitHub sent the browser back with.
func (h *Handler) AddGitHubInstallation(ctx context.Context, req *connect.Request[dockwellv1.AddGitHubInstallationRequest]) (*connect.Response[dockwellv1.AddGitHubInstallationResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.one(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	installation := req.Msg.GetInstallationId()
	if installation <= 0 {
		return nil, said(connect.CodeInvalidArgument, "invalid", "an installation's ID is a positive number, as GitHub sends it")
	}
	in, err := h.Refresh(ctx, k, installation)
	if err != nil {
		return nil, err
	}
	err = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "github_app.install", k.r.ID, fmt.Sprintf("installed on %s, installation %d, %d repositories", in.Account, in.ID, len(in.Repositories)))
	})
	if err != nil {
		return nil, h.internal(ctx, "recording an installation", err)
	}
	return connect.NewResponse(&dockwellv1.AddGitHubInstallationResponse{Installation: installationProto(k.spec, &in, h.cfg.Web)}), nil
}

// Refresh reads installation of the App k from GitHub, as the App, which says
// it's the App's, with its repositories, and keeps what GitHub said.
func (h *Handler) Refresh(ctx context.Context, k kept, installation int64) (store.GitHubInstallation, error) {
	app, err := h.acting(k)
	if err != nil {
		return store.GitHubInstallation{}, h.internal(ctx, "opening the App's key", err)
	}
	got, err := app.Installation(ctx, installation)
	h.noted(k.r.ID, err)
	var e *github.Error
	if errors.As(err, &e) && e.NotFound() {
		return store.GitHubInstallation{}, said(connect.CodeNotFound, "not_found", "GitHub says installation %d isn't the App %s's. Install it from GitHub's page for the App", installation, k.r.Name)
	}
	if err != nil {
		return store.GitHubInstallation{}, fromGitHub("reading the installation", err)
	}
	in := store.GitHubInstallation{
		ID: got.ID, AppID: k.r.ID, Account: got.Account.Login, Organization: got.Account.Type == "Organization",
		Selection: got.Selection, SuspendedAt: got.Suspended, UpdatedAt: h.now(),
	}
	if got.Suspended == nil {
		repos, err := app.Repositories(ctx, installation)
		if err != nil {
			return store.GitHubInstallation{}, fromGitHub("listing the installation's repositories", err)
		}
		for _, r := range repos {
			in.Repositories = append(in.Repositories, store.GitHubRepository{ID: r.ID, FullName: r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch})
		}
	}
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error { return tx.SetGitHubInstallation(ctx, in) }); err != nil {
		return store.GitHubInstallation{}, h.internal(ctx, "keeping an installation", err)
	}
	return in, nil
}

// audit records what was done to an App, on its row in the audit log.
func (h *Handler) audit(ctx context.Context, tx *store.Tx, actor, action, appID, detail string) error {
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: h.now(), Actor: actor, Surface: store.SurfaceOf(actor), Action: action, Subject: appID, Target: appID, Detail: detail,
	})
}

// ListGitProviders returns the connected Apps.
func (h *Handler) ListGitProviders(ctx context.Context, _ *connect.Request[dockwellv1.ListGitProvidersRequest]) (*connect.Response[dockwellv1.ListGitProvidersResponse], error) {
	out := &dockwellv1.ListGitProvidersResponse{}
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		apps, err := h.all(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			ins, err := tx.GitHubInstallationsOf(ctx, a.r.ID)
			if err != nil {
				return err
			}
			out.Apps = append(out.Apps, h.appProto(a, ins))
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "listing the GitHub Apps", err)
	}
	return connect.NewResponse(out), nil
}

// view is the App named name, as the API shows it.
func (h *Handler) view(ctx context.Context, name string) (*dockwellv1.GitHubAppInfo, error) {
	k, err := h.one(ctx, name)
	if err != nil {
		return nil, err
	}
	var ins []store.GitHubInstallation
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		ins, err = tx.GitHubInstallationsOf(ctx, k.r.ID)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "reading the App's installations", err)
	}
	return h.appProto(k, ins), nil
}

// appProto is an App as anyone may see it: nothing of its key or secrets.
func (h *Handler) appProto(k kept, ins []store.GitHubInstallation) *dockwellv1.GitHubAppInfo {
	a := k.spec
	h.mu.Lock()
	gone := h.gone[k.r.ID]
	h.mu.Unlock()
	out := &dockwellv1.GitHubAppInfo{
		Id: k.r.ID, Name: k.r.Name, AppId: a.GetAppId(), Owner: a.GetOwner(), OwnerIsOrganization: a.GetOwnerIsOrganization(),
		HtmlUrl: strings.TrimSuffix(h.cfg.Web, "/") + "/apps/" + url.PathEscape(a.GetSlug()), SettingsUrl: settingsURL(h.cfg.Web, a),
		KeyFingerprint: a.GetKeyFingerprint(), Gone: gone, Delivery: h.deliveryProto(k.r.ID),
	}
	for i := range ins {
		if ins[i].RemovedAt != nil {
			continue
		}
		out.Installations = append(out.Installations, installationProto(a, &ins[i], h.cfg.Web))
	}
	return out
}

// settingsURL is the App's settings on GitHub.
func settingsURL(web string, a *resourcev1.GitHubApp) string {
	base := strings.TrimSuffix(web, "/")
	if a.GetOwnerIsOrganization() {
		return base + "/organizations/" + url.PathEscape(a.GetOwner()) + "/settings/apps/" + url.PathEscape(a.GetSlug())
	}
	return base + "/settings/apps/" + url.PathEscape(a.GetSlug())
}

func installationProto(a *resourcev1.GitHubApp, in *store.GitHubInstallation, web string) *dockwellv1.GitHubInstallation {
	out := &dockwellv1.GitHubInstallation{
		Id: in.ID, Account: in.Account, Organization: in.Organization, AllRepositories: in.Selection == "all",
		RepositoryCount: int32(min(len(in.Repositories), 1<<30)),
		Suspended:       in.SuspendedAt != nil, Removed: in.RemovedAt != nil, EveryProject: true,
	}
	base := strings.TrimSuffix(web, "/")
	if in.Organization {
		out.SettingsUrl = base + "/organizations/" + url.PathEscape(in.Account) + "/settings/installations/" + strconv.FormatInt(in.ID, 10)
	} else {
		out.SettingsUrl = base + "/settings/installations/" + strconv.FormatInt(in.ID, 10)
	}
	for _, r := range a.GetReaches() {
		if r.GetInstallationId() == in.ID {
			out.EveryProject, out.Projects = false, r.GetProjects()
		}
	}
	return out
}

// ListGitRepositories returns what a project's apps may build from through an App.
func (h *Handler) ListGitRepositories(ctx context.Context, req *connect.Request[dockwellv1.ListGitRepositoriesRequest]) (*connect.Response[dockwellv1.ListGitRepositoriesResponse], error) {
	out := &dockwellv1.ListGitRepositoriesResponse{}
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		project := ""
		for _, p := range tree.Projects {
			if p.ID == req.Msg.GetProject() || p.Name == req.Msg.GetProject() {
				project = p.ID
			}
		}
		if project == "" {
			return said(connect.CodeNotFound, "not_found", "there's no project %s", req.Msg.GetProject())
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
					out.Repositories = append(out.Repositories, &dockwellv1.GitRepository{
						FullName: r.FullName, Url: strings.TrimSuffix(h.cfg.Web, "/") + "/" + r.FullName, Private: r.Private,
						DefaultBranch: r.DefaultBranch, AppId: a.r.ID, InstallationId: in.ID, Account: in.Account,
					})
				}
			}
		}
		return nil
	})
	var ce *connect.Error
	if errors.As(err, &ce) {
		return nil, err
	}
	if err != nil {
		return nil, h.internal(ctx, "listing a project's repositories", err)
	}
	slices.SortFunc(out.Repositories, func(a, b *dockwellv1.GitRepository) int { return strings.Compare(a.GetFullName(), b.GetFullName()) })
	return connect.NewResponse(out), nil
}

// RotateGitHubWebhookSecret gives an App a new webhook secret.
func (h *Handler) RotateGitHubWebhookSecret(ctx context.Context, req *connect.Request[dockwellv1.RotateGitHubWebhookSecretRequest]) (*connect.Response[dockwellv1.RotateGitHubWebhookSecretResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.one(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpRotateGitHubApp, k.r.ID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	app, err := h.acting(k)
	if err != nil {
		giveBack()
		return nil, h.internal(ctx, "opening the App's key", err)
	}
	secret := randomHex(32)
	err = app.SetHook(ctx, "", secret)
	h.noted(k.r.ID, err)
	if err != nil {
		giveBack()
		return nil, fromGitHub("the new webhook secret", err)
	}
	spec := &resourcev1.GitHubApp{
		WebhookSecret: secret, PreviousWebhookSecret: k.spec.GetWebhookSecret(), PreviousUntil: h.now().Add(keepOldSecret).Unix(),
		Reaches: k.spec.GetReaches(),
	}
	changeID, err := h.change(ctx, "Give the GitHub App "+k.r.Name+" a new webhook secret", k.r.Name, spec)
	if err != nil {
		// GitHub has the new secret, and Dockwell still the old: put it back.
		if back := app.SetHook(ctx, "", k.spec.GetWebhookSecret()); back != nil {
			h.cfg.Logger.ErrorContext(ctx, "GitHub kept the App's new webhook secret, which Dockwell didn't", slog.String("app", k.r.Name), slog.String("error", back.Error()))
		}
		return nil, err
	}
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "github_app.rotate_secret", k.r.ID, "a new webhook secret, the old one counting 10 minutes more")
	}); err != nil {
		return nil, h.internal(ctx, "recording a new webhook secret", err)
	}
	return connect.NewResponse(&dockwellv1.RotateGitHubWebhookSecretResponse{ChangeId: changeID}), nil
}

// ReplaceGitHubPrivateKey keeps a new private key for an App.
func (h *Handler) ReplaceGitHubPrivateKey(ctx context.Context, req *connect.Request[dockwellv1.ReplaceGitHubPrivateKeyRequest]) (*connect.Response[dockwellv1.ReplaceGitHubPrivateKeyResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.one(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	text := req.Msg.GetPrivateKey()
	if len(text) > maxKey {
		return nil, said(connect.CodeInvalidArgument, "invalid", "that file is past %d KiB, and a GitHub App's private key is under 4. Pick the .pem file GitHub downloaded", maxKey>>10)
	}
	key, err := github.ParseKey([]byte(text))
	if err != nil {
		return nil, said(connect.CodeInvalidArgument, "invalid", "%s. Pick the .pem file GitHub downloaded", err.Error())
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpRotateGitHubApp, k.r.ID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	trial := &github.App{ID: k.spec.GetAppId(), Key: key, Base: h.cfg.API, Client: h.cfg.Client, Now: h.cfg.Now}
	if _, err := trial.Self(ctx); err != nil {
		giveBack()
		var e *github.Error
		if errors.As(err, &e) && e.Gone() {
			return nil, said(connect.CodeInvalidArgument, "github_key_refused", "GitHub refused that key for the App %s: it's another App's, or one deleted. Generate a key on the App's page, and pick that file", k.r.Name)
		}
		return nil, fromGitHub("checking the new key", err)
	}
	fp, err := github.Fingerprint(key)
	if err != nil {
		giveBack()
		return nil, h.internal(ctx, "reading the new key", err)
	}
	changeID, err := h.change(ctx, "Give the GitHub App "+k.r.Name+" a new private key", k.r.Name,
		&resourcev1.GitHubApp{PrivateKey: text, KeyFingerprint: fp, Reaches: k.spec.GetReaches()})
	if err != nil {
		return nil, err
	}
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return h.audit(ctx, tx, p.Actor(), "github_app.replace_key", k.r.ID, "a new private key "+fp+" in place of "+k.spec.GetKeyFingerprint())
	}); err != nil {
		return nil, h.internal(ctx, "recording a new private key", err)
	}
	return connect.NewResponse(&dockwellv1.ReplaceGitHubPrivateKeyResponse{
		Fingerprint: fp, OldFingerprint: k.spec.GetKeyFingerprint(), KeysUrl: settingsURL(h.cfg.Web, k.spec), ChangeId: changeID,
	}), nil
}

// RemoveGitHubApp uninstalls an App and forgets it.
func (h *Handler) RemoveGitHubApp(ctx context.Context, req *connect.Request[dockwellv1.RemoveGitHubAppRequest]) (*connect.Response[dockwellv1.RemoveGitHubAppResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	k, err := h.one(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	client, _ := authapi.ClientFrom(ctx)
	giveBack, err := h.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpRemoveGitHubApp, k.r.ID)
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	var ins []store.GitHubInstallation
	if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		ins, err = tx.GitHubInstallationsOf(ctx, k.r.ID)
		return err
	}); err != nil {
		giveBack()
		return nil, h.internal(ctx, "reading the App's installations", err)
	}
	uninstalled := int32(0)
	if app, err := h.acting(k); err == nil {
		for i := range ins {
			in := &ins[i]
			if in.RemovedAt != nil {
				continue
			}
			err := app.Uninstall(ctx, in.ID)
			var e *github.Error
			switch {
			case errors.As(err, &e) && e.Gone():
				// Deleted on GitHub already: its installations went with it.
			case err != nil:
				giveBack()
				return nil, fromGitHub("uninstalling it from "+in.Account, err)
			default:
				uninstalled++
			}
			app.Forget(in.ID)
		}
	}
	changeID, err := h.change(ctx, "Remove the GitHub App "+k.r.Name, k.r.Name, nil)
	if err != nil {
		return nil, err
	}
	err = h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.DeleteGitHubInstallationsOf(ctx, k.r.ID); err != nil {
			return err
		}
		return h.audit(ctx, tx, p.Actor(), "github_app.remove", k.r.ID, fmt.Sprintf("uninstalled from %d installations; its key and secret forgotten", uninstalled))
	})
	if err != nil {
		return nil, h.internal(ctx, "forgetting the App's installations", err)
	}
	h.mu.Lock()
	delete(h.apps, k.r.ID)
	delete(h.gone, k.r.ID)
	h.mu.Unlock()
	return connect.NewResponse(&dockwellv1.RemoveGitHubAppResponse{
		Uninstalled: uninstalled, DeleteUrl: settingsURL(h.cfg.Web, k.spec) + "/advanced", ChangeId: changeID,
	}), nil
}
