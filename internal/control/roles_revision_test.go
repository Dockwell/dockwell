// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// variableEdit sets a variable called name in env.
func variableEdit(env, name, value string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: value}}},
	}
}

// byTheOwner proposes a change as the install's owner, someone other than the
// signed-in person, as the API would carry them.
func byTheOwner(t *testing.T, s *reachStand, env, name string) *revision.Plan {
	t.Helper()
	ctx := place.WithCaller(t.Context(), place.Caller{UserID: "usr_keeper", Grants: []store.Grant{{UserID: "usr_keeper", ProjectID: store.Install, Role: store.RoleOwner}}})
	plan, err := s.f.engine.Propose(ctx, revision.Proposal{Summary: "Set " + name, Actor: "user:usr_keeper", Via: revision.ViaPanel, Edits: []revision.Edit{{
		Ref: resource.Ref{Kind: "variable", Scope: env, Name: name}, Spec: &resourcev1.Variable{Value: "1"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// Every item of a change is checked where it is, however the change came:
// nested in ProposeChange's edits, made by an undo, or making a project. Out of
// reach is not found, a project is an admin's to make, and a developer's change
// in a protected environment plans but waits for an admin (ADR 0048, check 2).
func TestTheEngineChecksEachItem(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		s.hold(in(s.blogID, store.RoleDeveloper))

		_, err := s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Reach default", Edits: []*dockwellv1.Edit{variableEdit(s.production, "X", "1")}}))
		notFound(t, "an edit in default's production, by its ID", err)
		_, err = s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Reach both", Edits: []*dockwellv1.Edit{variableEdit(s.staging, "X", "1"), variableEdit(s.production, "X", "1")}}))
		notFound(t, "a change with one edit in blog and one in default", err)
		_, err = s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Make shop", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "shop"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}}}))
		needsRole(t, "a developer making a project", err)

		// blog's production is protected: a developer plans there, and waits.
		s.hold(onInstall(store.RoleAdmin))
		blogProduction := resourceIn(t, apply(t, s.c, propose(t, s.c, "Name blog's production", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: s.blogID, Name: "production"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{DisplayName: "Live"}}},
		})), "environment")
		s.hold(in(s.blogID, store.RoleDeveloper))
		mine := propose(t, s.c, "Set MODE", variableEdit(blogProduction, "MODE", "fast"))
		_, err = s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: mine.GetId(), Fingerprint: mine.GetPlan().GetFingerprint()}))
		needsRole(t, "a developer applying in blog's protected production", err)
		if err != nil && !strings.Contains(err.Error(), "protected environment") {
			t.Errorf("the refusal says %q, want that it waits for an admin", err)
		}
		if !mine.GetProtected() {
			t.Errorf("a plan in protected production reads open: %v", mine)
		}
		// In staging, which is open, theirs applies.
		inStaging := propose(t, s.c, "Set MODE in staging", variableEdit(s.staging, "MODE", "fast"))
		if inStaging.GetProtected() {
			t.Errorf("a plan in open staging reads protected")
		}
		apply(t, s.c, inStaging)

		// Making staging protected, or a new environment other than its name has it, is an admin's.
		_, err = s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Protect staging", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: s.blogID, Name: "staging"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_PROTECTED}}},
		}}}))
		needsRole(t, "a developer protecting staging", err)
		_, err = s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add an open production", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: s.blogID, Name: "production-2"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_PROTECTED}}},
		}}}))
		needsRole(t, "a developer making an environment protected that its name would leave open", err)
		if _, err := s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add qa", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: s.blogID, Name: "qa"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		}}})); err != nil {
			t.Errorf("a developer adding qa, open as its name has it: %v", err)
		}

		// A change of the owner's in protected production: a developer can't
		// discard it; one in open staging, they can. Their own, they always can.
		theirs := byTheOwner(t, s, blogProduction, "A")
		_, err = s.c.DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: theirs.Change}))
		needsRole(t, "a developer discarding the owner's plan in protected production", err)
		open := byTheOwner(t, s, s.staging, "B")
		if _, err := s.c.DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: open.Change})); err != nil {
			t.Errorf("a developer discarding the owner's plan in open staging: %v", err)
		}
		if _, err := s.c.DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: mine.GetId()})); err != nil {
			t.Errorf("a developer discarding their own plan in protected production: %v", err)
		}

		// A viewer plans nothing.
		s.hold(in(s.blogID, store.RoleViewer))
		_, err = s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Set Z", Edits: []*dockwellv1.Edit{variableEdit(s.staging, "Z", "1")}}))
		needsRole(t, "a viewer planning", err)
	})
}

// Someone else's change in a protected environment is approved by an admin or
// the owner, in a session, with the fingerprint of the plan they reviewed and a
// passkey bound to that plan. Their own applies as any change does (ADR 0049).
func TestSomeoneElsesProtectedChangeIsApprovedWithAPasskey(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		theirs := byTheOwner(t, s, s.production, "A")
		applyIt := func(c interface {
			ApplyChange(context.Context, *connect.Request[dockwellv1.ApplyChangeRequest]) (*connect.Response[dockwellv1.ApplyChangeResponse], error)
		}, fp, proof string,
		) error {
			_, err := c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: theirs.Change, Fingerprint: fp, Proof: proof}))
			return err
		}
		s.hold(onInstall(store.RoleAdmin))
		if err := applyIt(s.c, "", ""); reasonOf(err) != "needs_confirmation" {
			t.Errorf("approving with no fingerprint: %v, want needs_confirmation", err)
		}
		err := applyIt(s.c, theirs.Fingerprint, "")
		if reasonOf(err) != "needs_step_up" {
			t.Errorf("approving with no passkey: %v, want needs_step_up", err)
		}
		// It says which passkey: one approving this plan.
		var asked *dockwellv1.StepUpNeeded
		if cerr := new(connect.Error); errors.As(err, &cerr) {
			for _, d := range cerr.Details() {
				if v, derr := d.Value(); derr == nil {
					asked, _ = v.(*dockwellv1.StepUpNeeded)
				}
			}
		}
		if asked.GetAction() != dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE || asked.GetTarget() != theirs.Change+":"+theirs.Fingerprint {
			t.Errorf("the refusal asks for %v, want approving %s:%s", asked, theirs.Change, theirs.Fingerprint)
		}
		got, gerr := s.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: theirs.Change}))
		if gerr != nil || !got.Msg.GetChange().GetProtected() || !got.Msg.GetChange().GetApproves() || got.Msg.GetChange().GetWaits() != "" {
			t.Errorf("the owner's plan in production reads protected %v, approves %v, waits %q, %v; want protected, approved by the admin reading it",
				got.Msg.GetChange().GetProtected(), got.Msg.GetChange().GetApproves(), got.Msg.GetChange().GetWaits(), gerr)
		}
		token := createToken(t, s.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		if err := applyIt(changeClient(bearer{token: token, rt: s.b.client.Transport}, s.b.origin), theirs.Fingerprint, ""); reasonOf(err) != "needs_session" {
			t.Errorf("approving with an API token: %v, want needs_session", err)
		}
		// A passkey for another plan, or for applying as root, doesn't approve this one.
		wrong := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, theirs.Change+":"+"some-other-fingerprint")
		if err := applyIt(s.c, theirs.Fingerprint, wrong); reasonOf(err) != "needs_step_up" {
			t.Errorf("approving with a passkey for another plan: %v, want needs_step_up", err)
		}
		root := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, theirs.Change)
		if err := applyIt(s.c, theirs.Fingerprint, root); reasonOf(err) != "needs_step_up" {
			t.Errorf("approving with a passkey for applying as root: %v, want needs_step_up", err)
		}
		proof := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, theirs.Change+":"+theirs.Fingerprint)
		if err := applyIt(s.c, theirs.Fingerprint, proof); err != nil {
			t.Errorf("approving with the fingerprint and a passkey for it: %v", err)
		}

		// The admin's own change in production applies as any change does, a token's included.
		own := propose(t, s.c, "Set B", variableEdit(s.production, "B", "1"))
		if _, err := changeClient(bearer{token: token, rt: s.b.client.Transport}, s.b.origin).ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: own.GetId()})); err != nil {
			t.Errorf("an admin's token applying their own change in production: %v", err)
		}
		// So does one they planned with their token and apply in the panel.
		mine := propose(t, changeClient(bearer{token: token, rt: s.b.client.Transport}, s.b.origin), "Set D", variableEdit(s.production, "D", "1"))
		if mine.GetApproves() {
			t.Errorf("the admin's plan with their own token reads as one they approve")
		}
		if _, err := s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: mine.GetId(), Fingerprint: mine.GetPlan().GetFingerprint()})); err != nil {
			t.Errorf("an admin applying in the panel what they planned with their token: %v", err)
		}

		// Someone else's plan made with their API token, as the CLI does, is
		// theirs as much as one made in the panel: the admin reads that applying
		// it approves it, and approves it with a passkey.
		keepers := "tok_01JD6Z3R8M9QF2K7T4XW5BNCKP"
		if err := s.f.store.Update(ctx, func(tx *store.Tx) error {
			return tx.CreateAPIToken(ctx, store.APIToken{ID: keepers, TokenHash: []byte("keeper's"), UserID: "usr_keeper", Name: "deploy", Scope: "read_write", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		keeper := place.WithCaller(ctx, place.Caller{UserID: "usr_keeper", Grants: []store.Grant{{UserID: "usr_keeper", ProjectID: store.Install, Role: store.RoleOwner}}})
		viaToken, err := s.f.engine.Propose(keeper, revision.Proposal{Summary: "Set E", Actor: "token:" + keepers, Via: revision.ViaAPI, Edits: []revision.Edit{{
			Ref: resource.Ref{Kind: "variable", Scope: s.production, Name: "E"}, Spec: &resourcev1.Variable{Value: "1"},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		read, err := s.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: viaToken.Change}))
		if err != nil || !read.Msg.GetChange().GetApproves() {
			t.Errorf("the owner's plan made with a token reads approves %v, %v, want true", read.Msg.GetChange().GetApproves(), err)
		}
		_, err = s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: viaToken.Change, Fingerprint: viaToken.Fingerprint}))
		if reasonOf(err) != "needs_step_up" {
			t.Errorf("applying the owner's token-made plan with no passkey: %v, want needs_step_up", err)
		}

		// Below an admin, it waits, and says why, there and on Activity.
		waiting := byTheOwner(t, s, s.production, "C")
		s.hold(in(s.defaultID, store.RoleDeveloper))
		read, err = s.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: waiting.Change}))
		if err != nil || read.Msg.GetChange().GetApproves() || !strings.Contains(read.Msg.GetChange().GetWaits(), "it changes C in a protected environment, an admin or the owner applies this") {
			t.Errorf("a developer reading the owner's plan in production: approves %v, waits %q, %v", read.Msg.GetChange().GetApproves(), read.Msg.GetChange().GetWaits(), err)
		}
		events, err := dockwellv1connect.NewEventServiceClient(s.b, s.b.origin, connect.WithProtoJSON()).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(events.Msg.GetEvents(), func(e *dockwellv1.Event) bool { return e.GetChange().GetId() == waiting.Change })
		if i < 0 {
			t.Fatalf("Activity doesn't list %s", waiting.Change)
		}
		if c := events.Msg.GetEvents()[i].GetChange(); !c.GetProtected() || c.GetWaits() == "" || c.GetApproves() {
			t.Errorf("Activity shows the owner's plan in production to a developer as protected %v, waits %q, approves %v", c.GetProtected(), c.GetWaits(), c.GetApproves())
		}
	})
}

// The engine refuses a person's change that reaches it without who they are,
// as the API's never does, rather than leave it unchecked. Dockwell's own isn't
// checked.
func TestAPersonsChangeWithoutACallerIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		edit := []revision.Edit{{Ref: resource.Ref{Kind: "variable", Scope: s.staging, Name: "X"}, Spec: &resourcev1.Variable{Value: "1"}}}
		_, err := s.f.engine.Propose(t.Context(), revision.Proposal{Summary: "Set X", Actor: "user:usr_keeper", Edits: edit})
		if err == nil || !strings.Contains(err.Error(), "can't check your role") {
			t.Errorf("a person's change with no caller: %v, want refused", err)
		}
		if _, err := s.f.engine.Propose(t.Context(), revision.Proposal{Summary: "Set X", Actor: revision.SystemActor, Edits: edit}); err != nil {
			t.Errorf("Dockwell's own change: %v", err)
		}
	})
}

// A file exported from an environment names its project only to someone who
// reaches it, so checking one tells no one else a project's name.
func TestAnExportedFileNamesItsProjectOnlyInReach(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		composes := dockwellv1connect.NewComposeServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		file := "name: " + docker.Project(s.production) + "\nservices:\n  app:\n    image: nginx:1.27\n"
		named := func() string {
			t.Helper()
			res, err := composes.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg.GetName()
		}
		if got := named(); got != "default" {
			t.Fatalf("an admin checking default's export reads its name as %q, want default", got)
		}
		s.hold(in(s.blogID, store.RoleDeveloper))
		if got := named(); got == "default" {
			t.Errorf("blog's developer checking default's export reads its project's name")
		}
	})
}

// Installing from the catalog makes a project, which is an admin's, so someone
// below an admin is told that, however they name the project.
func TestACatalogInstallIsAnAdminsToMake(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		cat := catalogClient(s.b)
		for _, grant := range []store.Grant{in(s.blogID, store.RoleDeveloper), onInstall(store.RoleDeveloper)} {
			s.hold(grant)
			for _, project := range []string{"", "blog", "fresh"} {
				_, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: project}))
				needsRole(t, "a developer installing postgres as "+project, err)
				if err != nil && !strings.Contains(err.Error(), "installing from the catalog makes a project of its own, which only an admin or the owner can do") {
					t.Errorf("installing as %q says %q, want why it's an admin's", project, err)
				}
			}
		}
	})
}
