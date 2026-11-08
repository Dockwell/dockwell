// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The engine checks each item of a change against the role of whoever acts,
// wherever the change came from: planning needs a developer in reach, applying
// in a protected environment needs an admin, discarding someone else's plan
// needs what applying would, and what belongs to no project, a project
// included, is an admin's (ADR 0048, check 2; ADR 0049).
func TestTheEngineChecksTheRolesOfEachItem(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		e.RequireRoles()
		ctx := t.Context()
		var org, project, production string
		_ = db.View(ctx, func(tx *store.Tx) error {
			tr, err := scope.Load(ctx, tx)
			if err != nil || len(tr.Projects) == 0 {
				t.Fatalf("the default project: %v", err)
			}
			org, project, production = tr.Organization.ID, tr.Projects[0].ID, tr.Projects[0].Environments[0].ID
			return nil
		})
		as := func(user string, grants ...store.Grant) context.Context {
			return place.WithCaller(ctx, place.Caller{UserID: user, Grants: grants})
		}
		mira := as("usr_mira", store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleOwner})
		ada := as("usr_ada", store.Grant{UserID: "usr_ada", ProjectID: project, Role: store.RoleDeveloper})
		stranger := as("usr_sam", store.Grant{UserID: "usr_sam", ProjectID: "prj_01JD6Z3R8M9QF2K7T4XW5BNCAA", Role: store.RoleDeveloper})
		value := func(env, name string) Edit {
			return Edit{Ref: resource.Ref{Kind: "variable", Scope: env, Name: name}, Spec: &resourcev1.Variable{Value: "1"}}
		}
		plan := func(who context.Context, actor string, edits ...Edit) (*Plan, error) {
			return e.Propose(who, Proposal{Summary: "Change", Actor: actor, Edits: edits})
		}
		mustPlan := func(who context.Context, actor string, edits ...Edit) *Plan {
			t.Helper()
			p, err := plan(who, actor, edits...)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		staging := mustPlan(mira, "user:usr_mira", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{}})
		if err := e.ApplyBy(mira, staging.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatal(err)
		}
		var stagingID string
		for _, r := range staging.Resources {
			stagingID = r.ID
		}

		// Planning in protected production, a developer's change waits: applying it is an admin's.
		waits := mustPlan(ada, "user:usr_ada", value(production, "A"))
		if err := e.ApplyBy(ada, waits.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer applying in protected production: %v, want ErrNeedsRole", err)
		}
		// In open staging, theirs applies.
		open := mustPlan(ada, "user:usr_ada", value(stagingID, "A"))
		if err := e.ApplyBy(ada, open.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer applying in open staging: %v", err)
		}
		// Someone else's plan in production a developer can't discard; their own, they can.
		miras := mustPlan(mira, "user:usr_mira", value(production, "B"))
		if err := e.Discard(ada, miras.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer discarding the owner's plan in production: %v, want ErrNeedsRole", err)
		}
		if err := e.Discard(ada, waits.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer discarding their own plan in production: %v", err)
		}
		if err := e.ApplyBy(mira, miras.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Errorf("the owner applying their own change in production: %v", err)
		}

		// A plan gone stale can still be discarded.
		stale := mustPlan(ada, "user:usr_ada", value(stagingID, "S"))
		moved := mustPlan(mira, "user:usr_mira", value(stagingID, "S"))
		if err := e.ApplyBy(mira, moved.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatal(err)
		}
		if err := e.Discard(ada, stale.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer discarding their plan that went stale: %v", err)
		}

		// Out of reach is not found; a project, or a container on a server alone, is an admin's.
		if _, err := plan(stranger, "user:usr_sam", value(production, "C")); !errors.Is(err, ErrOutOfReach) {
			t.Errorf("someone of another project planning in default: %v, want ErrOutOfReach", err)
		}
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer making a project: %v, want ErrNeedsRole", err)
		}
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "tool"}, Spec: &resourcev1.Container{Image: "ghcr.io/acme/tool:1.0"}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer running a container on a server alone: %v, want ErrNeedsRole", err)
		}
		// Changing whether an environment is protected is an admin's, either way.
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_PROTECTED}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer protecting staging: %v, want ErrNeedsRole", err)
		}
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "production"}, Spec: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_OPEN}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer opening production: %v, want ErrNeedsRole", err)
		}
		// A person's change with no caller is refused; Dockwell's own isn't checked.
		if _, err := plan(ctx, "user:usr_ada", value(stagingID, "D")); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a person's change with no caller: %v, want ErrNeedsRole", err)
		}
		if _, err := plan(ctx, SystemActor, value(stagingID, "D")); err != nil {
			t.Errorf("Dockwell's own change: %v", err)
		}
	})
}
