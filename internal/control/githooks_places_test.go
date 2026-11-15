// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
		if state := got.Msg.GetChange().GetState(); state != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Errorf("the owner's push into protected production is %v, want planned, for a person", state)
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
