// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Deploy on push is narrowed as an API token is (ADR 0071): a hook acts in its
// app's project alone, as a developer at most.

// pushTo sends a push of a new commit on main to a hook, and waits for it to come
// to want.
func (g *gitBench) pushTo(t *testing.T, url, secret, deliveryID, want string) store.GitHookDelivery {
	t.Helper()
	commit := g.repo.Commit("main", "Push "+deliveryID, map[string]string{"Dockerfile": webDockerfile, "app.js": deliveryID})
	if code, text, _ := send(t, url, delivery{id: deliveryID, event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", commit)}); code != http.StatusAccepted {
		t.Fatalf("push: %d %q", code, text)
	}
	return g.stored(t, url, deliveryID, want)
}

// approved applies a hook's plan as the person signed in to g, whose applying it
// approves it, as a hook's change is the hook's alone, whoever it acts for (ADR
// 0071): asked for a passkey with no proof, and applied with one.
func (g *gitBench) approved(t *testing.T, c *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	fingerprint := c.GetPlan().GetFingerprint()
	_, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: c.GetId(), Fingerprint: fingerprint}))
	if asked := stepUpAsked(err); asked.GetAction() != dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE || asked.GetTarget() != c.GetId()+":"+fingerprint {
		t.Fatalf("applying a hook's plan with no passkey: %v, asking %v; want approving %s:%s", err, asked, c.GetId(), fingerprint)
	}
	w := watch(t, g.c, c.GetId())
	proof := stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, c.GetId()+":"+fingerprint)
	if _, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: c.GetId(), Fingerprint: fingerprint, Proof: proof})); err != nil {
		t.Fatalf("approving a hook's plan with the fingerprint and a passkey: %v", err)
	}
	return waitWatch(t, w).last()
}

// An admin's hook acts as a developer in its app's project: its push into a
// protected environment plans and waits for a person, where the admin's own token
// would apply it (ADR 0071).
func TestAnAdminsHookPushIntoAProtectedEnvironmentWaits(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
		production := made.GetPlan().GetResources()[0].GetResource().GetScope()
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		project := envProject(t, g.f, production)
		if err := g.f.store.View(t.Context(), func(tx *store.Tx) error {
			h, err := tx.GitHookOf(t.Context(), web.GetId())
			if err == nil && (!slices.Equal(h.Projects, []string{project}) || h.Role != store.RoleDeveloper) {
				t.Errorf("the owner's hook acts in %v as %q, want web's project %s, as a developer at most", h.Projects, h.Role, project)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_PROTECTED)

		d := g.pushTo(t, url, secret, "d-1", store.HookNeedsPerson)
		got, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.ChangeID}))
		if err != nil {
			t.Fatal(err)
		}
		if state := got.Msg.GetChange().GetState(); state != dockwellv1.ChangeState_CHANGE_STATE_PLANNED || !got.Msg.GetChange().GetApproves() {
			t.Errorf("the owner's push into protected production is %v, approves %v to them; want planned, for them to approve",
				state, got.Msg.GetChange().GetApproves())
		}
		// The owner, whom the hook acts for, approves it with a passkey.
		if c := g.approved(t, got.Msg.GetChange()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the owner approving their hook's plan: %v %s", c.GetState(), c.GetError())
		}
	})
}

// A push reaches its hook's projects alone: a hook narrowed to a project its app
// isn't in deploys nothing, and says why (ADR 0071).
func TestAPushPastItsHooksProjectIsRefused(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		first := g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		blog := g.done(t, g.propose(t, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})).GetPlan().GetResources()[0].GetId()
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		// Nothing makes such a hook but an edit by hand, as the API makes each for its
		// app's project.
		if err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
			h, err := tx.GitHookOf(t.Context(), web.GetId())
			if err != nil {
				return err
			}
			if err := tx.DeleteGitHook(t.Context(), h.ID); err != nil {
				return err
			}
			h.Projects = []string{blog}
			return tx.CreateGitHook(t.Context(), h)
		}); err != nil {
			t.Fatal(err)
		}

		d := g.pushTo(t, url, secret, "d-1", store.HookFailed)
		if !strings.Contains(d.Detail, "This hook may not deploy it now") || !strings.Contains(d.Detail, "not in a project you hold") || d.ChangeID != "" {
			t.Errorf("a push past its hook's project came to %+v, want it refused, saying why", d)
		}
		if got := g.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got != first {
			t.Errorf("web builds %s after a push past its hook's project, want %s still", got, first)
		}
	})
}

// An admin lets their hook deploy without waiting where its app's environment is
// protected: one step away, with a passkey, so a push there applies with no
// person, and narrowed back with none, so it waits again. It's the maker's
// choice: a hook acting for someone else isn't widened, a new secret from
// someone else makes it wait again, and a developer, though it acts for them,
// can't widen it, where the environment is open or protected (ADR 0071).
func TestAnAdminLetsTheirHookDeployWithoutWaiting(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
		production := made.GetPlan().GetResources()[0].GetResource().GetScope()
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		setRole := func(role dockwellv1.Role, proof string) (*dockwellv1.GitHook, error) {
			t.Helper()
			res, err := g.hooks().SetGitHookRole(t.Context(), connect.NewRequest(&dockwellv1.SetGitHookRoleRequest{Id: web.GetId(), Role: role, Proof: proof}))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetHook(), nil
		}
		widen := func() string {
			t.Helper()
			return stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_WIDEN_GIT_HOOK, web.GetId())
		}

		// Without a passkey, or with one for another service, widening asks for one.
		_, err := setRole(dockwellv1.Role_ROLE_ADMIN, "")
		if reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("widening a hook with no passkey: %v, want %s", err, authapi.ReasonNeedsStepUp)
		}
		elsewhere := stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_WIDEN_GIT_HOOK, "svc_elsewhere")
		if _, err := setRole(dockwellv1.Role_ROLE_ADMIN, elsewhere); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("widening a hook with a passkey for another service: %v, want %s", err, authapi.ReasonNeedsStepUp)
		}
		// With one, a push into protected production applies with no person.
		if hook, err := setRole(dockwellv1.Role_ROLE_ADMIN, widen()); err != nil || hook.GetRole() != dockwellv1.Role_ROLE_ADMIN {
			t.Fatalf("widening the owner's hook: %v, %v", hook, err)
		}
		d := g.pushTo(t, url, secret, "w-1", store.HookDeploying)
		if c := g.ended(t, d.ChangeID); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("a widened hook's push into protected production ended %v: %s", c.GetState(), c.GetError())
		}
		// Narrowed back, with no passkey, its push waits again.
		if hook, err := setRole(dockwellv1.Role_ROLE_DEVELOPER, ""); err != nil || hook.GetRole() != dockwellv1.Role_ROLE_DEVELOPER {
			t.Fatalf("narrowing the hook back: %v, %v", hook, err)
		}
		g.pushTo(t, url, secret, "w-2", store.HookNeedsPerson)

		// A hook acting for someone else is theirs to widen, and a new secret from
		// someone else makes a widened one wait again.
		if err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
			if err := tx.CreateUser(t.Context(), store.User{ID: "usr_dev", Email: "dev@example.com", Name: "Dev", WebAuthnHandle: []byte("dev"), CreatedAt: time.Now()}); err != nil {
				return err
			}
			h, err := tx.GitHookOf(t.Context(), web.GetId())
			if err != nil {
				return err
			}
			if err := tx.DeleteGitHook(t.Context(), h.ID); err != nil {
				return err
			}
			h.UserID, h.Role = "usr_dev", store.RoleAdmin
			return tx.CreateGitHook(t.Context(), h)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := setRole(dockwellv1.Role_ROLE_ADMIN, widen()); connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "hook_not_yours" {
			t.Errorf("widening a hook that acts for someone else: %v, want hook_not_yours", err)
		}
		rotated, err := g.hooks().RotateGitHookSecret(t.Context(), connect.NewRequest(&dockwellv1.RotateGitHookSecretRequest{
			Id: web.GetId(), Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GIT_HOOK, web.GetId()),
		}))
		if err != nil || rotated.Msg.GetHook().GetRole() != dockwellv1.Role_ROLE_DEVELOPER {
			t.Errorf("a new secret from someone the widened hook didn't act for: %v, %v; want it waiting again", rotated, err)
		}
		url, secret = rotated.Msg.GetHook().GetUrl(), rotated.Msg.GetSecret()

		// A developer can't widen their own hook, where it's open or protected.
		hold := aMember(t, g.f)
		hold(onInstall(store.RoleDeveloper))
		_, err = setRole(dockwellv1.Role_ROLE_ADMIN, widen())
		needsRole(t, "a developer widening their hook where it's protected", err)
		hold(onInstall(store.RoleAdmin))
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_OPEN)
		hold(onInstall(store.RoleDeveloper))
		_, err = setRole(dockwellv1.Role_ROLE_ADMIN, widen())
		if reasonOf(err) != "needs_role" || !strings.Contains(err.Error(), "only an admin where the app is lets its pushes deploy without waiting") {
			t.Errorf("a developer widening their hook where it's open: %v, want it refused, saying why", err)
		}
		if url == "" || secret == "" {
			t.Error("the new secret's hook has no URL or secret")
		}
	})
}

// A push that came while its hook could deploy without waiting, and waited behind
// another change while its maker set the hook to wait again, waits for a person
// too: it applies with the hook's rights now, not those it came with (ADR 0071).
func TestAPushQueuedBeforeItsHookWaitsAgainWaitsForAPerson(t *testing.T) {
	t.Parallel()
	g := newGitBench(t, store.SQLite)
	g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
	made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
	production := made.GetPlan().GetResources()[0].GetResource().GetScope()
	protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
	web := g.service(t, "", "web")
	url, secret := g.hook(t, web.GetId())
	setRole := func(role dockwellv1.Role, proof string) {
		t.Helper()
		if _, err := g.hooks().SetGitHookRole(t.Context(), connect.NewRequest(&dockwellv1.SetGitHookRoleRequest{Id: web.GetId(), Role: role, Proof: proof})); err != nil {
			t.Fatal(err)
		}
	}
	setRole(dockwellv1.Role_ROLE_ADMIN, stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_WIDEN_GIT_HOOK, web.GetId()))
	release := g.holdApp(t, web.GetId())
	commit := g.repo.Commit("main", "Pushed", map[string]string{"Dockerfile": webDockerfile, "app.js": "pushed"})
	if code, text, _ := send(t, url, delivery{id: "q-1", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", commit)}); code != 202 {
		t.Fatalf("push: %d %q", code, text)
	}
	g.plannedByHook(t)
	setRole(dockwellv1.Role_ROLE_DEVELOPER, "")
	release()
	g.stored(t, url, "q-1", store.HookNeedsPerson)
}

// A developer's hook plans into a protected environment and waits; its maker may
// discard what it planned, as discarding runs nothing, though applying it needs an
// admin's approval (ADR 0071, #711).
func TestAHooksMakerDiscardsWhatItPlanned(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
		production := made.GetPlan().GetResources()[0].GetResource().GetScope()
		project := envProject(t, g.f, production)
		web := g.service(t, "", "web")
		// A developer turns deploy on push on while production is open, then an admin
		// protects it.
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_OPEN)
		hold := aMember(t, g.f)
		hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
		url, secret := g.hook(t, web.GetId())
		hold(store.Grant{ProjectID: store.Install, Role: store.RoleAdmin})
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
		hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})

		d := g.pushTo(t, url, secret, "d-1", store.HookNeedsPerson)
		// Lowered to viewer since, the maker may not discard it, so isn't told they may.
		hold(store.Grant{ProjectID: project, Role: store.RoleViewer})
		lowered, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.ChangeID}))
		if err != nil || lowered.Msg.GetChange().GetDiscards() {
			t.Errorf("to its maker lowered to viewer, the hook's waiting plan reads %v, %v; want not theirs to discard", lowered, err)
		}
		if _, err := g.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: d.ChangeID})); err == nil {
			t.Fatal("a viewer discarded the hook's plan")
		}
		hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})

		read, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.ChangeID}))
		if err != nil || !read.Msg.GetChange().GetDiscards() || read.Msg.GetChange().GetWaits() == "" {
			t.Errorf("to its maker, the hook's waiting plan reads %v, %v; want theirs to discard, waiting for an admin", read, err)
		}
		if _, err := g.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: d.ChangeID})); err != nil {
			t.Fatalf("the hook's maker discarding what it planned: %v", err)
		}
		got, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.ChangeID}))
		if err != nil || got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_DISCARDED {
			t.Errorf("after its maker discarded it, the hook's plan is %v, %v", got, err)
		}
	})
}

// A hook left in no project, as migration 0043 left one whose app was in no
// environment then, deploys nothing, and says so in its own words, with its fix:
// a new secret, which takes its app's project as it is now (ADR 0071, #711).
func TestANewSecretGivesAHookItsAppsProject(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
		project := envProject(t, g.f, made.GetPlan().GetResources()[0].GetResource().GetScope())
		web := g.service(t, "", "web")
		url, _ := g.hook(t, web.GetId())
		if err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
			h, err := tx.GitHookOf(t.Context(), web.GetId())
			if err != nil {
				return err
			}
			if err := tx.DeleteGitHook(t.Context(), h.ID); err != nil {
				return err
			}
			h.Projects = []string{}
			return tx.CreateGitHook(t.Context(), h)
		}); err != nil {
			t.Fatal(err)
		}
		got, err := g.hooks().RotateGitHookSecret(t.Context(), connect.NewRequest(&dockwellv1.RotateGitHookSecretRequest{
			Id: web.GetId(), Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GIT_HOOK, web.GetId()),
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := g.f.store.View(t.Context(), func(tx *store.Tx) error {
			h, err := tx.GitHookOf(t.Context(), web.GetId())
			if err == nil && !slices.Equal(h.Projects, []string{project}) {
				t.Errorf("after a new secret, the hook acts in %v, want web's project %s", h.Projects, project)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// Production waits for a person, so a push it may make plans and waits.
		g.pushTo(t, url, got.Msg.GetSecret(), "d-1", store.HookNeedsPerson)

		// Left in no project again, it says so, naming the hook and the fix.
		if err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
			h, err := tx.GitHookOf(t.Context(), web.GetId())
			if err != nil {
				return err
			}
			if err := tx.DeleteGitHook(t.Context(), h.ID); err != nil {
				return err
			}
			h.Projects = []string{}
			return tx.CreateGitHook(t.Context(), h)
		}); err != nil {
			t.Fatal(err)
		}
		d := g.pushTo(t, url, got.Msg.GetSecret(), "d-2", store.HookFailed)
		if !strings.Contains(d.Detail, "deploy-on-push hook's app is in no project it acts in") || !strings.Contains(d.Detail, "Give the hook a new secret") ||
			strings.Contains(d.Detail, "API token") {
			t.Errorf("a push through a hook in no project came to %+v, want it refused in the hook's words", d)
		}
	})
}
