// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/gittest"
	"github.com/dockwell/dockwell/internal/store"
)

// appBench is a control plane with a GitHub App connected, installed on acme, and
// web, which builds acme/web, a private repository, with deploy on push on through
// the App.
type appBench struct {
	*gitBench
	gh      *fakeGitHub
	gp      dockwellv1connect.GitProviderServiceClient
	app     *dockwellv1.GitHubAppInfo
	web     *dockwellv1.ServiceInfo
	webhook string
	mu      sync.Mutex
	// head is the commit main names now, as GitHub's commits API says it.
	head string
}

func newAppBench(t *testing.T) *appBench {
	t.Helper()
	return newAppBenchWith(t, nil)
}

// newAppBenchWith is newAppBench with more of the control plane's config.
func newAppBenchWith(t *testing.T, more func(*GitHubConfig)) *appBench {
	t.Helper()
	gh := newFakeGitHub(t)
	git := gittest.Start(t)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.GitHub = GitHubConfig{API: gh.srv.URL, Web: git.URL, Client: gh.srv.Client()}
		if more != nil {
			more(&c.GitHub)
		}
	})
	b := signedIn(t, f)
	a := &appBench{gh: gh, gp: dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())}
	a.gitBench = &gitBench{
		bench: &bench{
			f: f, b: b, e: f.addDockerServer(t), c: changeClient(b, b.origin),
			cc: dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()),
			s:  dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()),
		},
		git: git, builds: dockwellv1connect.NewBuildServiceClient(b, b.origin, connect.WithProtoJSON()),
	}
	a.repo = git.Repo("acme/web")
	git.RequireAuth("Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_listing")))
	gh.head = func() string {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.head
	}
	ctx := t.Context()
	started, err := a.gp.StartGitHubApp(ctx, connect.NewRequest(&dockwellv1.StartGitHubAppRequest{
		Organization: "acme", Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CONNECT_GITHUB_APP, ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	action, _ := url.Parse(started.Msg.GetActionUrl())
	var manifest struct {
		Hook  struct{ URL string } `json:"hook_attributes"`
		Setup string               `json:"setup_url"`
	}
	_ = json.Unmarshal([]byte(started.Msg.GetManifest()), &manifest)
	hookURL, _ := url.Parse(manifest.Hook.URL)
	a.webhook = b.origin + hookURL.Path
	finished, err := a.gp.FinishGitHubApp(ctx, connect.NewRequest(&dockwellv1.FinishGitHubAppRequest{Code: "code-1", State: action.Query().Get("state")}))
	if err != nil {
		t.Fatal(err)
	}
	a.app = finished.Msg.GetApp()
	// GitHub sends the browser to the setup URL, which names the App by its webhook's
	// ID, as the manifest was made before the App had one of its own.
	setup, _ := url.Parse(manifest.Setup)
	named := strings.TrimSuffix(strings.TrimPrefix(setup.Path, "/settings/integrations/github/"), "/installed")
	if !strings.HasPrefix(named, "ghw_") {
		t.Fatalf("the setup URL is %s", manifest.Setup)
	}
	if _, err := a.gp.AddGitHubInstallation(ctx, connect.NewRequest(&dockwellv1.AddGitHubInstallationRequest{Id: named, InstallationId: 7})); err != nil {
		t.Fatal(err)
	}
	a.commit("First")
	// Production is protected by its name, where a hook's push waits for a person
	// (ADR 0071): web's environment is open, so its pushes deploy.
	a.openWhere(t, a.done(t, a.propose(t, a.buildEdit("main", nil))))
	a.web = a.service(t, "", "web")
	if _, err := a.hooks().CreateGitHook(ctx, connect.NewRequest(&dockwellv1.CreateGitHookRequest{
		Id: a.web.GetId(), Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_HOOK, a.web.GetId()),
	})); err != nil {
		t.Fatal(err)
	}
	return a
}

// commit commits to main, and has GitHub's commits API say it.
func (a *appBench) commit(subject string) string {
	sha := a.repo.Commit("main", subject, map[string]string{"Dockerfile": webDockerfile, "app.js": subject})
	a.mu.Lock()
	a.head = sha
	a.mu.Unlock()
	return sha
}

// delivery is what Dockwell says of the App's deliveries.
func (a *appBench) delivery(t *testing.T) *dockwellv1.GitHubDelivery {
	t.Helper()
	listed, err := a.gp.ListGitProviders(t.Context(), connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
	if err != nil || len(listed.Msg.GetApps()) != 1 {
		t.Fatalf("the Apps listed = %v, %v", listed, err)
	}
	return listed.Msg.GetApps()[0].GetDelivery()
}

func (a *appBench) heads() int {
	a.gh.mu.Lock()
	defer a.gh.mu.Unlock()
	return a.gh.heads
}

// While GitHub's deliveries don't reach the panel, Dockwell checks web's branch
// itself, and deploys a commit it moved to as a push would, once; an unmoved branch
// costs an ETag's 304. When GitHub asks it to slow down, it waits as long as asked,
// checking nothing; and once a delivery reaches the panel, it stops checking.
func TestWhenGitHubCantReachThePanelDockwellChecksForPushes(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	apps := a.f.server.apps

	// The newest delivery didn't reach the panel: polling, each minute.
	a.gh.mu.Lock()
	a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":0},{"id":1,"guid":"g1","event":"ping","status_code":202}]`
	a.gh.mu.Unlock()
	apps.CheckDeliveries(ctx)
	if d := a.delivery(t); d.GetState() != dockwellv1.GitHubDelivery_STATE_POLLING || d.GetCheckEverySeconds() != 60 {
		t.Fatalf("GitHub's deliveries don't reach the panel, and Dockwell says %v", d)
	}

	// The branch hasn't moved past what web runs: nothing deploys.
	apps.Poll(ctx)
	if a.heads() != 1 {
		t.Fatalf("one poll asked GitHub %d times", a.heads())
	}
	second := a.commit("Second")
	apps.Poll(ctx)
	d := a.outcome(t, a.web.GetId(), "poll-"+second, dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
	if c := a.ended(t, d.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the deploy polling found ended %v: %s", c.GetState(), c.GetError())
	}
	if got := a.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got != second {
		t.Errorf("web builds %s, not the commit polling found, %s", got, second)
	}
	apps.Poll(ctx) // an unmoved branch: GitHub's 304
	hooks, err := a.hooks().GetGitHook(ctx, connect.NewRequest(&dockwellv1.GetGitHookRequest{Id: a.web.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, dl := range hooks.Msg.GetHook().GetDeliveries() {
		if strings.HasPrefix(dl.GetDeliveryId(), "poll-") {
			found++
		}
	}
	if found != 1 {
		t.Errorf("polling deployed %d times for one push", found)
	}

	// The installation is narrowed to another project: web's branch isn't checked.
	reach := func(summary string, reaches ...*resourcev1.GitHubInstallationReach) {
		t.Helper()
		p, err := a.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: summary, Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "github.app", Name: a.app.GetName()},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: &resourcev1.GitHubApp{Reaches: reaches}}},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		a.done(t, p.Msg.GetChange())
	}
	reach("Narrow acme", &resourcev1.GitHubInstallationReach{InstallationId: 7, Projects: []string{"prj_01JD6Z3R8M9QF2K7T4XW5BNCZZ"}})
	before := a.heads()
	a.commit("Narrowed")
	apps.Poll(ctx)
	if a.heads() != before {
		t.Errorf("Dockwell checked web's branch through an installation that no longer reaches its project")
	}
	reach("Widen acme")

	// GitHub asks Dockwell to slow down: it waits, checking nothing.
	a.gh.mu.Lock()
	a.gh.retryAfter = 120
	a.gh.mu.Unlock()
	a.commit("Third")
	apps.Poll(ctx)
	asked := a.heads()
	if d := a.delivery(t); d.GetState() != dockwellv1.GitHubDelivery_STATE_BACKING_OFF ||
		d.GetBackoffUntil().AsTime().Before(time.Now().Add(110*time.Second)) {
		t.Fatalf("GitHub asked for 2 minutes, and Dockwell says %v", d)
	}
	apps.Poll(ctx)
	if a.heads() != asked {
		t.Errorf("Dockwell asked GitHub while backing off")
	}

	// A delivery reaches the panel: polling stops.
	a.gh.mu.Lock()
	a.gh.retryAfter = 0
	a.gh.mu.Unlock()
	ping, _ := json.Marshal(map[string]any{"zen": "Keep it logically awesome.", "hook_id": 1})
	if code, _, _ := send(t, a.webhook, delivery{id: "g3", event: "ping", secret: "first-webhook-secret", body: ping}); code != http.StatusAccepted {
		t.Fatalf("a ping: %d", code)
	}
	if d := a.delivery(t); d.GetState() != dockwellv1.GitHubDelivery_STATE_REACHED {
		t.Errorf("a delivery reached the panel, and Dockwell says %v", d)
	}
	apps.Poll(ctx)
	if a.heads() != asked {
		t.Errorf("Dockwell checked for pushes once deliveries reached the panel")
	}
}

// What the panel answered GitHub's newest delivery says whether it reached it: a
// 2xx did; a 401 means the secret GitHub signs with isn't Dockwell's, which the
// line says, while Dockwell checks for pushes itself; anything else, as a proxy's
// 502, delivered nothing either.
func TestOnlyADeliveryThePanelTookReachedIt(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	apps := a.f.server.apps
	for _, c := range []struct {
		status  int
		state   dockwellv1.GitHubDelivery_State
		refused bool
	}{
		{401, dockwellv1.GitHubDelivery_STATE_POLLING, true},
		{502, dockwellv1.GitHubDelivery_STATE_POLLING, false},
		{202, dockwellv1.GitHubDelivery_STATE_REACHED, false},
	} {
		a.gh.mu.Lock()
		a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":` + strconv.Itoa(c.status) + `}]`
		a.gh.mu.Unlock()
		apps.CheckDeliveries(ctx)
		if d := a.delivery(t); d.GetState() != c.state || d.GetSecretRefused() != c.refused {
			t.Errorf("GitHub's newest delivery was answered %d, and Dockwell says %v, secret refused %v", c.status, d.GetState(), d.GetSecretRefused())
		}
	}
}
