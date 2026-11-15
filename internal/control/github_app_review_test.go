// SPDX-License-Identifier: AGPL-3.0-only

package control

// The review of #253's slice 2 (dockwell-briefs/reviews/github-app-slice2-review.md),
// each test named for the finding it holds.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/store"
)

// Review M1: a branch pushed back to a commit polling deployed before is a push
// too, as a rollback by force-push is, and deploys.
func TestReviewPollingDeploysABranchMovedBackToAnEarlierCommit(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	apps := a.f.server.apps
	a.gh.mu.Lock()
	a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":0}]`
	a.gh.mu.Unlock()
	apps.CheckDeliveries(ctx)

	second := a.commit("Second")
	apps.Poll(ctx)
	a.ended(t, a.outcome(t, a.web.GetId(), "poll-"+second, dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING).GetChangeId())
	third := a.commit("Third")
	apps.Poll(ctx)
	a.ended(t, a.outcome(t, a.web.GetId(), "poll-"+third, dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING).GetChangeId())
	if got := a.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got != third {
		t.Fatalf("web builds %s, not %s", got, third)
	}

	// Third was bad: main is force-pushed back to Second.
	a.repo.Ref("refs/heads/main", second)
	a.mu.Lock()
	a.head = second
	a.mu.Unlock()
	apps.Poll(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit() == second {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("main names %s again, and web still builds %s: polling took the rollback for a commit it had deployed", second, third)
}

// Review M2: a delivery the panel refused (401, after a webhook secret GitHub and
// Dockwell don't share) didn't deliver anything, so Dockwell should check for pushes.
func TestReviewARefusedDeliveryIsntOneThatReachedThePanel(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	a.gh.mu.Lock()
	a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":401}]`
	a.gh.mu.Unlock()
	a.f.server.apps.CheckDeliveries(t.Context())
	if d := a.delivery(t); d.GetState() == dockwellv1.GitHubDelivery_STATE_REACHED {
		t.Errorf("the panel refused GitHub's newest delivery, and Dockwell says %v, so nothing checks for pushes", d.GetState())
	}
}

// Review M3: GitHub signs with the new webhook secret as soon as it takes it, so a
// push it sends before Dockwell's change keeps that secret is refused, and lost.
func TestReviewADeliverySignedWithTheNewSecretMidRotationIsTaken(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	ping, _ := json.Marshal(map[string]any{"zen": "Design for failure.", "hook_id": 1})
	inner := a.gh.srv.Config.Handler
	var during atomic.Int64
	a.gh.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/hook/config" || r.Method != http.MethodPatch {
			inner.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct{ Secret string }
		_ = json.Unmarshal(raw, &body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		inner.ServeHTTP(w, r)
		if during.Load() == 0 {
			// GitHub has the new secret: the next delivery is signed with it.
			code, _, _ := send(t, a.webhook, delivery{id: "mid-rotation", event: "ping", secret: body.Secret, body: ping})
			during.Store(int64(code))
		}
	})
	if _, err := a.gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{
		Id: a.app.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
	})); err != nil {
		t.Fatal(err)
	}
	if code := during.Load(); code != http.StatusAccepted {
		t.Errorf("a delivery GitHub signed with the new secret before Dockwell kept it was answered %d", code)
	}
}

// Review M4: undoing a new webhook secret puts the old one back in Dockwell alone:
// GitHub keeps signing with the new one, so every delivery is refused from then on.
func TestReviewUndoingANewWebhookSecretKeepsDeliveriesWorking(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	rotated, err := a.gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{
		Id: a.app.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	a.gh.mu.Lock()
	onGitHub := a.gh.secrets[len(a.gh.secrets)-1]
	a.gh.mu.Unlock()
	undo, err := a.c.UndoChange(ctx, connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: rotated.Msg.GetChangeId()}))
	if err != nil {
		t.Logf("undo refused: %v", err)
		return
	}
	a.done(t, undo.Msg.GetChange())
	a.gh.mu.Lock()
	after := a.gh.secrets[len(a.gh.secrets)-1]
	a.gh.mu.Unlock()
	ping, _ := json.Marshal(map[string]any{"zen": "Approachable is better than simple.", "hook_id": 1})
	if code, _, _ := send(t, a.webhook, delivery{id: "after-undo", event: "ping", secret: after, body: ping}); code != http.StatusAccepted {
		t.Errorf("after the undo, GitHub signs with %s (rotated to %s), and Dockwell answers %d", short(after), short(onGitHub), code)
	}
}

func short(s string) string {
	if len(s) > 6 {
		return s[:6] + "…"
	}
	return s
}

// Review L2: an installation event saying one was removed is checked with GitHub,
// as the App, before Dockwell keeps it: one GitHub still has stays, and one Dockwell
// never knew, or another App's, is no row of this App's.
func TestReviewARemovalEventIsCheckedWithGitHub(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	removal := func(id string, installation int64) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": "deleted", "installation": map[string]any{"id": installation}})
		if code, _, _ := send(t, a.webhook, delivery{id: id, event: "installation", secret: "first-webhook-secret", body: body}); code != http.StatusAccepted {
			t.Fatalf("an installation event: %d", code)
		}
	}
	installed := func() int {
		t.Helper()
		listed, err := a.gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return len(listed.Msg.GetApps()[0].GetInstallations())
	}
	removal("l2-1", 7) // GitHub still has it
	removal("l2-2", 9) // nobody's
	time.Sleep(500 * time.Millisecond)
	if n := installed(); n != 1 {
		t.Errorf("after removals GitHub didn't make, the App lists %d installations", n)
	}
	if err := a.f.store.View(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetGitHubInstallation(ctx, 9); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an event for an installation nobody knew made a row: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Review L3: a plan made through an installation that an admin narrows away from
// the app's project before it applies doesn't fetch through it: the token is asked
// for as the build runs, from the installation's reach then.
func TestReviewABuildPlannedBeforeANarrowingDoesntFetchThroughIt(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	a.commit("Second")
	planned := a.propose(t, a.buildEdit("main", nil))
	if login := buildStep(planned).GetGitLogin(); login != "dockwell-deploy/7" {
		t.Fatalf("the plan builds through %q", login)
	}
	narrow, err := a.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Narrow acme", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "github.app", Name: a.app.GetName()},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: &resourcev1.GitHubApp{
			Reaches: []*resourcev1.GitHubInstallationReach{{InstallationId: 7, Projects: []string{"prj_01JD6Z3R8M9QF2K7T4XW5BNCZZ"}}},
		}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	a.done(t, narrow.Msg.GetChange())
	w := watch(t, a.c, planned.GetId())
	if _, err := applyChange(t, a.c, &dockwellv1.ApplyChangeRequest{Id: planned.GetId()}); err != nil {
		if !strings.Contains(err.Error(), "doesn't reach") {
			t.Errorf("applying a build through an installation narrowed since: %v", err)
		}
		return
	}
	c := waitWatch(t, w).last()
	if c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || !strings.Contains(c.GetError(), "doesn't reach this app's project any more") {
		t.Errorf("a build through an installation narrowed since its plan ended %v: %s", c.GetState(), c.GetError())
	}
}

// Review L4: GitHub failing a branch's check backs polling off, as asking too
// often does, rather than ask every branch again each minute.
func TestReviewGitHubFailingBacksPollingOff(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	apps := a.f.server.apps
	a.gh.mu.Lock()
	a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":0}]`
	a.gh.headStatus = http.StatusBadGateway
	a.gh.mu.Unlock()
	apps.CheckDeliveries(ctx)
	apps.Poll(ctx)
	if d := a.delivery(t); d.GetState() != dockwellv1.GitHubDelivery_STATE_BACKING_OFF {
		t.Errorf("GitHub answered 502, and polling is %v", d.GetState())
	}
}

// Review L6: GitHub out of reach as the browser comes back spends neither the code
// nor the state: the same page tries again, rather than connect anew and make a
// second App on GitHub, which no API can delete.
func TestReviewFinishingTriesAgainWhenGitHubWasOutOfReach(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.GitHub = GitHubConfig{API: gh.srv.URL, Client: gh.srv.Client()}
	})
	b := signedIn(t, f)
	ctx := t.Context()
	gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())
	inner := gh.srv.Config.Handler
	var failed atomic.Bool
	gh.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app-manifests/") && failed.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"Bad Gateway"}`))
			return
		}
		inner.ServeHTTP(w, r)
	})
	started, err := gp.StartGitHubApp(ctx, connect.NewRequest(&dockwellv1.StartGitHubAppRequest{
		Organization: "acme", Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CONNECT_GITHUB_APP, ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	action, _ := url.Parse(started.Msg.GetActionUrl())
	finish := &dockwellv1.FinishGitHubAppRequest{Code: "code-1", State: action.Query().Get("state")}
	if _, err := gp.FinishGitHubApp(ctx, connect.NewRequest(finish)); reasonOf(err) != "github_unreachable" {
		t.Fatalf("finishing while GitHub fails: %v", err)
	}
	if res, err := gp.FinishGitHubApp(ctx, connect.NewRequest(finish)); err != nil || res.Msg.GetApp().GetName() != "dockwell-deploy" {
		t.Errorf("finishing again, with the same code and state: %v, %v", res, err)
	}
}

// Review L7: a delivery sent again, as a signed body caught before, doesn't say
// GitHub reaches the panel: polling goes on until a new one does.
func TestReviewAReplayedDeliveryDoesntEndPolling(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	ping, _ := json.Marshal(map[string]any{"zen": "Mind your words, they are important.", "hook_id": 1})
	if code, _, _ := send(t, a.webhook, delivery{id: "l7-1", event: "ping", secret: "first-webhook-secret", body: ping}); code != http.StatusAccepted {
		t.Fatalf("a ping: %d", code)
	}
	a.gh.mu.Lock()
	a.gh.deliveries = `[{"id":2,"guid":"g2","event":"push","status_code":0}]`
	a.gh.mu.Unlock()
	a.f.server.apps.CheckDeliveries(ctx)
	if code, _, _ := send(t, a.webhook, delivery{id: "l7-1", event: "ping", secret: "first-webhook-secret", body: ping}); code != http.StatusAccepted {
		t.Fatalf("the ping again: %d", code)
	}
	if d := a.delivery(t); d.GetState() != dockwellv1.GitHubDelivery_STATE_POLLING {
		t.Errorf("a replayed delivery left polling %v", d.GetState())
	}
}
