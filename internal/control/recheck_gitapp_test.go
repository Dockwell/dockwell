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

// Re-check of H1: an App-backed hook made by an admin of one project, and by a
// developer of one project, waits for a person in protected production.
func TestRecheckAppHookOfAOneProjectMemberWaits(t *testing.T) {
	t.Parallel()
	for _, role := range []string{store.RoleAdmin, store.RoleDeveloper} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			a := newAppBench(t)
			ctx := t.Context()
			production := a.web.GetResource().GetScope()
			project := envProject(t, a.f, production)
			if _, err := a.hooks().DeleteGitHook(ctx, connect.NewRequest(&dockwellv1.DeleteGitHookRequest{Id: a.web.GetId()})); err != nil {
				t.Fatal(err)
			}
			hold := aMember(t, a.f)
			grant := store.Grant{ProjectID: project, Role: role}
			if role == store.RoleAdmin {
				grant = onInstall(role)
			}
			hold(grant)
			if _, err := a.hooks().CreateGitHook(ctx, connect.NewRequest(&dockwellv1.CreateGitHookRequest{
				Id: a.web.GetId(), Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_HOOK, a.web.GetId()),
			})); err != nil {
				t.Fatal(err)
			}
			hold(onInstall(store.RoleAdmin))
			protectEnv(t, a.c, a.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
			hold(grant)
			if err := a.f.store.View(ctx, func(tx *store.Tx) error {
				h, err := tx.GitHookOf(ctx, a.web.GetId())
				if err == nil && (!slices.Equal(h.Projects, []string{project}) || h.Role != store.RoleDeveloper || h.Via != store.ViaGitHub) {
					t.Errorf("hook acts in %v as %q via %q", h.Projects, h.Role, h.Via)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			push := func(id, commit string) {
				t.Helper()
				if code, _, _ := send(t, a.webhook, delivery{id: id, event: "push", secret: "first-webhook-secret", body: appPushBody(a.repo.URL, commit, 7)}); code != http.StatusAccepted {
					t.Fatalf("push: %d", code)
				}
			}
			push("r-1", a.commit("Second"))
			d := a.outcome(t, a.web.GetId(), "r-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON)
			got, err := a.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.GetChangeId()}))
			if err != nil {
				t.Fatal(err)
			}
			if c := got.Msg.GetChange(); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
				t.Errorf("%s's App push into protected production is %v", role, c.GetState())
			}
			_, err = a.hooks().SetGitHookRole(ctx, connect.NewRequest(&dockwellv1.SetGitHookRoleRequest{
				Id: a.web.GetId(), Role: dockwellv1.Role_ROLE_ADMIN, Proof: stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_WIDEN_GIT_HOOK, a.web.GetId()),
			}))
			if role == store.RoleDeveloper {
				needsRole(t, "a developer widening their App hook", err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			push("r-2", a.commit("Third"))
			d = a.outcome(t, a.web.GetId(), "r-2", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
			if c := a.ended(t, d.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Errorf("project admin's widened App push ended %v: %s", c.GetState(), c.GetError())
			}
			// Demoted to developer, the widened hook's next push waits: the role is
			// capped by what its maker holds now.
			hold(onInstall(store.RoleDeveloper))
			push("r-3", a.commit("Fourth"))
			d = a.outcome(t, a.web.GetId(), "r-3", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON)
			_ = d
		})
	}
}

// Re-check of H1: a push through the App that came while its hook could deploy
// without waiting, and waited behind another deploy while its maker set it to
// wait again, waits for a person (#709's stillOn).
func TestRecheckAppPushQueuedBeforeNarrowingWaits(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	production := a.web.GetResource().GetScope()
	protectEnv(t, a.c, a.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
	setRole := func(role dockwellv1.Role, proof string) {
		t.Helper()
		if _, err := a.hooks().SetGitHookRole(ctx, connect.NewRequest(&dockwellv1.SetGitHookRoleRequest{Id: a.web.GetId(), Role: role, Proof: proof})); err != nil {
			t.Fatal(err)
		}
	}
	setRole(dockwellv1.Role_ROLE_ADMIN, stepUpFor(t, a.b, dockwellv1.StepUpAction_STEP_UP_ACTION_WIDEN_GIT_HOOK, a.web.GetId()))
	release := a.holdApp(t, a.web.GetId())
	if code, _, _ := send(t, a.webhook, delivery{id: "q-1", event: "push", secret: "first-webhook-secret", body: appPushBody(a.repo.URL, a.commit("Pushed"), 7)}); code != http.StatusAccepted {
		t.Fatalf("push: %d", code)
	}
	a.plannedByHook(t)
	setRole(dockwellv1.Role_ROLE_DEVELOPER, "")
	release()
	a.outcome(t, a.web.GetId(), "q-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON)
}
