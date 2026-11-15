// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/githooks"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/gittest"
	"github.com/dockwell/dockwell/internal/store"
)

// appPushBody is a push through a GitHub App: a push's payload, with the
// installation it came through.
func appPushBody(repo, ref, after string, installation int64) []byte {
	var m map[string]any
	_ = json.Unmarshal(pushBody(repo, ref, after), &m)
	m["installation"] = map[string]any{"id": installation}
	b, _ := json.Marshal(m)
	return b
}

// An app whose repository a GitHub App reaches turns deploy on push on with a
// passkey and nothing to paste, and a push through the App's one webhook, signed
// with the App's secret, deploys it as its maker, as slice 1's hooks do. A wrong
// signature and a webhook there isn't get the same answer; an installation that
// stops reaching the app's project, or is removed on GitHub, deploys nothing more.
func TestAPushThroughTheGitHubAppDeploysWhatItReaches(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	git := gittest.Start(t)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.GitHub = GitHubConfig{API: gh.srv.URL, Web: git.URL, Client: gh.srv.Client()}
	})
	b := signedIn(t, f)
	g := &gitBench{
		bench: &bench{
			f: f, b: b, e: f.addDockerServer(t), c: changeClient(b, b.origin),
			cc: dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()),
			s:  dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()),
		},
		git: git, builds: dockwellv1connect.NewBuildServiceClient(b, b.origin, connect.WithProtoJSON()),
	}
	g.repo = git.Repo("acme/web")
	// acme/web is private: every read and fetch needs the App's token for it.
	git.RequireAuth("Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_listing")))
	ctx := t.Context()

	// Connect the App, and add the installation on acme that reaches acme/web.
	gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())
	started, err := gp.StartGitHubApp(ctx, connect.NewRequest(&dockwellv1.StartGitHubAppRequest{
		Organization: "acme", Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CONNECT_GITHUB_APP, ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	action, _ := url.Parse(started.Msg.GetActionUrl())
	var manifest struct {
		Hook struct{ URL string } `json:"hook_attributes"`
	}
	_ = json.Unmarshal([]byte(started.Msg.GetManifest()), &manifest)
	hookURL, _ := url.Parse(manifest.Hook.URL)
	webhook := b.origin + hookURL.Path
	finished, err := gp.FinishGitHubApp(ctx, connect.NewRequest(&dockwellv1.FinishGitHubAppRequest{Code: "code-1", State: action.Query().Get("state")}))
	if err != nil {
		t.Fatal(err)
	}
	app := finished.Msg.GetApp()
	if _, err := gp.AddGitHubInstallation(ctx, connect.NewRequest(&dockwellv1.AddGitHubInstallationRequest{Id: app.GetId(), InstallationId: 7})); err != nil {
		t.Fatal(err)
	}

	// web builds acme/web; deploy on push on is a passkey and nothing more.
	g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile, "app.js": "1"})
	first := g.propose(t, g.buildEdit("main", nil))
	// The plan's build says the installation it fetches through.
	if login := buildStep(first).GetGitLogin(); login != "dockwell-deploy/7" {
		t.Errorf("the plan builds through %q, not the installation that reaches it", login)
	}
	g.done(t, first)
	web := g.service(t, "", "web")
	// The plan named the installation that reaches acme/web, as the edit didn't.
	if login := web.GetSpec().GetBuild().GetGitLogin(); login != "dockwell-deploy/7" {
		t.Fatalf("web's build fetches through %q, not the installation that reaches it", login)
	}
	named := g.buildEdit("main", nil)
	named.GetSpec().GetService().Build.GitLogin = "dockwell-deploy/8"
	if _, err := g.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Fetch elsewhere", Edits: []*dockwellv1.Edit{named}})); err == nil ||
		!strings.Contains(err.Error(), "dockwell-deploy/8 doesn't reach this project. The project's are on acme") {
		t.Errorf("a build naming an installation that doesn't reach the project = %v", err)
	}
	made, err := g.hooks().CreateGitHook(ctx, connect.NewRequest(&dockwellv1.CreateGitHookRequest{
		Id: web.GetId(), Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_HOOK, web.GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if h := made.Msg.GetHook(); h.GetGithubApp() != "dockwell-deploy" || h.GetUrl() != "" || made.Msg.GetSecret() != "" {
		t.Fatalf("deploy on push through the App made %v with secret %q", h, made.Msg.GetSecret())
	}

	// A push through the App deploys web as its maker.
	second := g.repo.Commit("main", "Second", map[string]string{"Dockerfile": webDockerfile, "app.js": "2"})
	push := func(id string, installation int64) (int, string) {
		code, text, _ := send(t, webhook, delivery{id: id, event: "push", secret: "first-webhook-secret", body: appPushBody(g.repo.URL, "refs/heads/main", second, installation)})
		return code, text
	}
	if code, text := push("a-1", 7); code != http.StatusAccepted || !strings.Contains(text, "deploys 1 app") {
		t.Fatalf("a push through the App: %d %q", code, text)
	}
	d := g.outcome(t, web.GetId(), "a-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
	c := g.ended(t, d.GetChangeId())
	if c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || c.GetAppliedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_HOOK {
		t.Fatalf("the push's deploy ended %v by %v: %s", c.GetState(), c.GetAppliedBy(), c.GetError())
	}
	if got := g.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got != second {
		t.Errorf("web builds %s, not the commit pushed, %s", got, second)
	}
	if code, text := push("a-1", 7); code != http.StatusAccepted || !strings.Contains(text, "already") {
		t.Errorf("the delivery again: %d %q", code, text)
	}

	// A wrong signature and a webhook there isn't get the same answer.
	body := appPushBody(g.repo.URL, "refs/heads/main", second, 7)
	wrongCode, wrongText, _ := send(t, webhook, delivery{id: "a-2", event: "push", secret: "not-the-secret", body: body})
	noneCode, noneText, _ := send(t, b.origin+githooks.AppPath+"ghw_0123456789abcdef0123", delivery{id: "a-3", event: "push", secret: "first-webhook-secret", body: body})
	if wrongCode != http.StatusUnauthorized || noneCode != wrongCode || noneText != wrongText {
		t.Errorf("a wrong signature: %d %q; a webhook there isn't: %d %q", wrongCode, wrongText, noneCode, noneText)
	}

	// A plan narrows the installation to another project: a push deploys nothing.
	narrow, err := g.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Narrow acme", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "github.app", Name: app.GetName()},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: &resourcev1.GitHubApp{
			Reaches: []*resourcev1.GitHubInstallationReach{{InstallationId: 7, Projects: []string{"prj_01JD6Z3R8M9QF2K7T4XW5BNCZZ"}}},
		}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	g.done(t, narrow.Msg.GetChange())
	if code, text := push("a-4", 7); code != http.StatusAccepted || !strings.Contains(text, "No app") {
		t.Errorf("a push through an installation that doesn't reach web's project: %d %q", code, text)
	}
	widen, err := g.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Widen acme", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "github.app", Name: app.GetName()},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: &resourcev1.GitHubApp{}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	g.done(t, widen.Msg.GetChange())

	// GitHub says the installation was removed: pushes through it deploy nothing.
	removed, _ := json.Marshal(map[string]any{"action": "deleted", "installation": map[string]any{"id": 7}})
	if code, _, _ := send(t, webhook, delivery{id: "a-5", event: "installation", secret: "first-webhook-secret", body: removed}); code != http.StatusAccepted {
		t.Fatalf("an installation's removal: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		listed, err := gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
		if err == nil && len(listed.Msg.GetApps()) == 1 && len(listed.Msg.GetApps()[0].GetInstallations()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the removed installation still lists: %v, %v", listed, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, text := push("a-6", 7); code != http.StatusAccepted || !strings.Contains(text, "No app") {
		t.Errorf("a push through a removed installation: %d %q", code, text)
	}
}
