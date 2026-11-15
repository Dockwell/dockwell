// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A new webhook secret is GitHub's at once, and the one before still counts for 10
// minutes, for deliveries GitHub sent before it had the new one; after that, only
// the new one does.
func TestAWebhookSecretGivenAgainTakesTheOldOneForTenMinutes(t *testing.T) {
	t.Parallel()
	var ahead atomic.Int64
	a := newAppBenchWith(t, func(c *GitHubConfig) {
		c.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})
	ctx := t.Context()
	ping, _ := json.Marshal(map[string]any{"zen": "Design for failure.", "hook_id": 1})
	sent := 0
	send1 := func(secret string) int {
		t.Helper()
		sent++
		code, _, _ := send(t, a.webhook, delivery{id: "s-" + strconv.Itoa(sent), event: "ping", secret: secret, body: ping})
		return code
	}

	if _, err := a.gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{
		Id: a.app.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
	})); err != nil {
		t.Fatal(err)
	}
	a.gh.mu.Lock()
	given := a.gh.secrets[len(a.gh.secrets)-1]
	a.gh.mu.Unlock()
	if given == "" || given == "first-webhook-secret" {
		t.Fatalf("GitHub was given %q as the new secret", given)
	}
	if code := send1(given); code != http.StatusAccepted {
		t.Errorf("a delivery signed with the new secret: %d", code)
	}
	if code := send1("first-webhook-secret"); code != http.StatusAccepted {
		t.Errorf("a delivery signed with the old secret, within 10 minutes: %d", code)
	}

	ahead.Store(int64(11 * time.Minute))
	if code := send1("first-webhook-secret"); code != http.StatusUnauthorized {
		t.Errorf("a delivery signed with the old secret, 11 minutes on: %d", code)
	}
	if code := send1(given); code != http.StatusAccepted {
		t.Errorf("a delivery signed with the new secret, 11 minutes on: %d", code)
	}
}

// GitHub refusing a new webhook secret loses nothing: Dockwell kept the new one
// first, the old one counting, and keeps the old one as its own again, so a push
// GitHub signs with the secret it still has is taken, then and later.
func TestAWebhookSecretGitHubRefusesLeavesTheOldOneWorking(t *testing.T) {
	t.Parallel()
	var ahead atomic.Int64
	a := newAppBenchWith(t, func(c *GitHubConfig) {
		c.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})
	ctx := t.Context()
	inner := a.gh.srv.Config.Handler
	a.gh.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/hook/config" && r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		inner.ServeHTTP(w, r)
	})
	if _, err := a.gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{
		Id: a.app.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
	})); err == nil {
		t.Fatal("a new webhook secret GitHub refused was taken")
	}
	ping, _ := json.Marshal(map[string]any{"zen": "Keep it logically awesome.", "hook_id": 1})
	if code, _, _ := send(t, a.webhook, delivery{id: "r-1", event: "ping", secret: "first-webhook-secret", body: ping}); code != http.StatusAccepted {
		t.Errorf("a push signed with the secret GitHub kept, just after: %d", code)
	}
	ahead.Store(int64(11 * time.Minute))
	if code, _, _ := send(t, a.webhook, delivery{id: "r-2", event: "ping", secret: "first-webhook-secret", body: ping}); code != http.StatusAccepted {
		t.Errorf("a push signed with the secret GitHub kept, 11 minutes on: %d", code)
	}
}

// Two admins giving the App a new webhook secret at once leave Dockwell with the
// one GitHub has: the changes go one at a time, each from the secret the last one
// left.
func TestTwoNewWebhookSecretsAtOnceLeaveDockwellWithGitHubs(t *testing.T) {
	t.Parallel()
	var ahead atomic.Int64
	a := newAppBenchWith(t, func(c *GitHubConfig) {
		c.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})
	ctx := t.Context()
	proofs := []string{
		stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
		stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
	}
	errs := make(chan error, 2)
	for _, proof := range proofs {
		go func() {
			_, err := a.gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{Id: a.app.GetId(), Proof: proof}))
			errs <- err
		}()
	}
	for range proofs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	a.gh.mu.Lock()
	onGitHub := a.gh.secrets[len(a.gh.secrets)-1]
	a.gh.mu.Unlock()
	// Past every window, only the secret Dockwell keeps as its own counts.
	ahead.Store(int64(11 * time.Minute))
	ping, _ := json.Marshal(map[string]any{"zen": "Half measures are as bad as nothing at all.", "hook_id": 1})
	if code, _, _ := send(t, a.webhook, delivery{id: "c-1", event: "ping", secret: onGitHub, body: ping}); code != http.StatusAccepted {
		t.Errorf("a push signed with the secret GitHub has after two new ones at once: %d", code)
	}
}

// A change that acts on GitHub offers no undo, as GitHub keeps what it did: a new
// webhook secret, connecting the App. Undoing one is refused with what to do
// instead. Which projects an installation reaches is Dockwell's alone, and undoes.
func TestAChangeThatActsOnGitHubOffersNoUndo(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	rotated, err := a.gp.RotateGitHubWebhookSecret(ctx, connect.NewRequest(&dockwellv1.RotateGitHubWebhookSecretRequest{
		Id: a.app.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GITHUB_APP, a.app.GetId()),
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: rotated.Msg.GetChangeId()}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetChange().GetUndoDeadline() != nil {
		t.Errorf("a new webhook secret offers an undo until %v", got.Msg.GetChange().GetUndoDeadline().AsTime())
	}
	if _, err := a.c.UndoChange(ctx, connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: rotated.Msg.GetChangeId()})); err == nil ||
		!strings.Contains(err.Error(), "GitHub has the App's new webhook secret, which an undo can't take back") {
		t.Errorf("undoing a new webhook secret: %v", err)
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
	applied := a.done(t, narrow.Msg.GetChange())
	if applied.GetUndoDeadline() == nil {
		t.Errorf("narrowing an installation offers no undo")
	}
	if _, err := a.c.UndoChange(ctx, connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: applied.GetId()})); err != nil {
		t.Errorf("undoing a narrowing: %v", err)
	}
}
