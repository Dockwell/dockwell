// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

// A whole restore of a server another project's app shares through a link takes
// back what that app wrote, so the engine asks for a role in the app's environment
// too, wherever the restore comes from: as it's planned, applied, undone and
// discarded by someone else, and an admin's where that environment is protected and
// it applies (ADR 0084).
func TestAWholeRestoreOfAServerAnotherProjectsAppSharesTakesARoleThere(t *testing.T) {
	x := withVolume(t, store.SQLite)
	e, ref, env, db := x.e, x.ref, x.env, x.db
	e.kinds = e.kinds.With(link.Type{})
	e.sealer = secretstest.Keeper(db, e.kinds)
	withData(t, e, env)
	data := envProjectOf(t, db, env)
	open := func(project string, p resourcev1.Protection) {
		t.Helper()
		apply(t, e, propose(t, e, "Protect", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "production"}, Spec: &resourcev1.Environment{Protection: p}}))
	}
	open(data, resourcev1.Protection_PROTECTION_OPEN)
	apply(t, e, propose(t, e, "Add blog", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: tree(t, db).Organization.ID, Name: "blog"}, Spec: &resourcev1.Project{}}))
	_, blogEnv, err := tree(t, db).Resolve("blog", "")
	if err != nil {
		t.Fatal(err)
	}
	blog := envProjectOf(t, db, blogEnv.ID)
	apply(t, e, propose(t, e, "Link blog", Edit{Ref: resource.Ref{Kind: link.Kind, Scope: blogEnv.ID, Name: "db"}, Spec: &resourcev1.Link{
		Label: "Database", Service: "web", MeetsEnvironment: env, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL,
		Accepts: []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
		Sets:    []*resourcev1.LinkSet{{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
	}}))
	e.RequireRoles()

	as := func(user string, projects ...string) (context.Context, By) {
		var grants []store.Grant
		for _, p := range projects {
			grants = append(grants, store.Grant{UserID: user, ProjectID: p, Role: store.RoleDeveloper})
		}
		return place.WithCaller(t.Context(), place.Caller{UserID: user, Grants: grants}), By{Actor: "user:" + user, Via: ViaPanel}
	}
	dataDev, dataBy := as("usr_dana", data)
	both, bothBy := as("usr_bo", data, blog)
	whole := resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}
	refused := func(what string, err error, words string) {
		t.Helper()
		if !errors.Is(err, ErrNeedsRole) || !strings.Contains(err.Error(), words) {
			t.Errorf("%s = %v, want a refusal saying %q", what, err, words)
		}
	}

	// Data's developer alone can't plan it.
	_, err = e.Restore(dataDev, ref, whole, dataBy, "")
	refused("data's developer planning it", err, "takes back what an app in another project wrote to it since")

	// A developer in both plans it; with the blog's environment protected, applying
	// it is an admin's, and once that's open, theirs.
	plan, err := e.Restore(both, ref, whole, bothBy, "")
	if err != nil {
		t.Fatal(err)
	}
	refused("applying it with the blog's environment protected", e.ApplyBy(both, plan.Change, bothBy), "an app in a protected environment")
	refused("data's developer discarding it", e.Discard(dataDev, plan.Change, dataBy), "takes back what an app in a protected environment wrote")
	open(blog, resourcev1.Protection_PROTECTION_OPEN)
	if err := e.ApplyBy(both, plan.Change, bothBy); err != nil {
		t.Fatal(err)
	}

	// Its undo takes back what the blog wrote since too.
	_, err = e.Undo(dataDev, plan.Change, dataBy)
	refused("data's developer undoing it", err, "takes back what an app in another project wrote")
	undo, err := e.Undo(both, plan.Change, bothBy)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ApplyBy(both, undo.Change, bothBy); err != nil {
		t.Fatal(err)
	}
	if len(x.r.ran) != 2 || x.r.ran[1].Undoes != plan.Change {
		t.Errorf("restores ran %+v", x.r.ran)
	}
}
