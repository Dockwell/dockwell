// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

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
