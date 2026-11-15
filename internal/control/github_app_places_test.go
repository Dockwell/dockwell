// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"slices"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// A hook whose pushes come through the GitHub App is narrowed as slice 1's are
// (ADR 0071): to its app's project, as a developer at most, so an admin's push into
// protected production plans its deploy and waits for a person. Its maker, an admin
// there, lets it deploy without waiting with a passkey, and the next push does.
func TestAnAdminsAppHookPushIntoProtectedProductionWaits(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	production := a.web.GetResource().GetScope()
	project := envProject(t, a.f, production)
	if err := a.f.store.View(ctx, func(tx *store.Tx) error {
		h, err := tx.GitHookOf(ctx, a.web.GetId())
		if err == nil && (!slices.Equal(h.Projects, []string{project}) || h.Role != store.RoleDeveloper || h.Via != store.ViaGitHub) {
			t.Errorf("the owner's App-backed hook acts in %v as %q, want web's project %s, as a developer at most", h.Projects, h.Role, project)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	protectEnv(t, a.c, a.f, production, resourcev1.Protection_PROTECTION_PROTECTED)

	push := func(id, commit string) {
		t.Helper()
		if code, _, _ := send(t, a.webhook, delivery{id: id, event: "push", secret: "first-webhook-secret", body: appPushBody(a.repo.URL, "refs/heads/main", commit, 7)}); code != http.StatusAccepted {
			t.Fatalf("a push through the App: %d", code)
		}
	}
	second := a.commit("Second")
	push("p-1", second)
	d := a.outcome(t, a.web.GetId(), "p-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON)
	got, err := a.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.GetChangeId()}))
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Msg.GetChange(); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED || !c.GetApproves() {
		t.Errorf("the owner's push through the App into protected production is %v, approves %v; want planned, for a person", c.GetState(), c.GetApproves())
	}

	// Letting it deploy without waiting takes a passkey, and the next push deploys.
	if _, err := a.hooks().SetGitHookRole(ctx, connect.NewRequest(&dockwellv1.SetGitHookRoleRequest{Id: a.web.GetId(), Role: dockwellv1.Role_ROLE_ADMIN})); reasonOf(err) != "needs_step_up" {
		t.Errorf("letting the App's hook deploy without waiting, with no passkey: %v", err)
	}
	if _, err := a.hooks().SetGitHookRole(ctx, connect.NewRequest(&dockwellv1.SetGitHookRoleRequest{
		Id: a.web.GetId(), Role: dockwellv1.Role_ROLE_ADMIN, Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_WIDEN_GIT_HOOK, a.web.GetId()),
	})); err != nil {
		t.Fatal(err)
	}
	third := a.commit("Third")
	push("p-2", third)
	d = a.outcome(t, a.web.GetId(), "p-2", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
	if c := a.ended(t, d.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("the push after Deploy without waiting ended %v: %s", c.GetState(), c.GetError())
	}
}

// A hook through the GitHub App gets no secret of its own, so its own URL never
// takes pushes: asking for a new one is refused (re-check N3).
func TestAnAppHookGetsNoSecretOfItsOwn(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	_, err := a.hooks().RotateGitHookSecret(t.Context(), connect.NewRequest(&dockwellv1.RotateGitHookSecretRequest{
		Id: a.web.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GIT_HOOK, a.web.GetId()),
	}))
	if reasonOf(err) != "hook_through_app" {
		t.Errorf("a new secret for a hook through the App: %v", err)
	}
}
