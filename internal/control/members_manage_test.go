// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// joins invites email to the whole install as role, and accepts in a browser of its own.
func (s *inviteStand) joins(t *testing.T, email, name string, role dockwellv1.Role) (*browser, string) {
	t.Helper()
	made, err := s.invite(t, email, role, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b := s.newcomer()
	in, err := accept(t, b, secretOf(t, made.GetLink()), name)
	if err != nil {
		t.Fatal(err)
	}
	return b, in.GetUser().GetId()
}

// The first invite waits for the protections to be confirmed, over the API as
// in the service, and Members says when they were and by whom.
func TestTheAPIHoldsTheFirstInviteToTheProtections(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		cert, roots := selfSigned(t, "panel.example.com")
		srv := startFull(t, backend, &https.Static{Name: "panel.example.com", Cert: cert})
		_, port, _ := net.SplitHostPort(srv.https)
		origin := "https://panel.example.com:" + port
		owner := newBrowser(t, origin, srv.https, roots)
		link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
		owner.claim(t, link.Token)
		members := dockwellv1connect.NewMemberServiceClient(owner, origin, connect.WithProtoJSON())
		ctx := t.Context()

		list, err := members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil || list.Msg.GetProtectionsConfirmTime() != nil {
			t.Fatalf("Members before anyone confirmed: %v, %v", list.Msg.GetProtectionsConfirmTime(), err)
		}
		proof := stepUpFor(t, owner, dockwellv1.StepUpAction_STEP_UP_ACTION_INVITE, auth.InviteTarget("ada@example.com", store.RoleViewer, store.Install))
		_, err = members.CreateInvite(ctx, connect.NewRequest(&dockwellv1.CreateInviteRequest{Email: "ada@example.com", Role: dockwellv1.Role_ROLE_VIEWER, Proof: proof}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "protections_unconfirmed" {
			t.Fatalf("the first invite before the protections were confirmed: %v, want protections_unconfirmed", err)
		}
		if _, err := members.ConfirmProtections(ctx, connect.NewRequest(&dockwellv1.ConfirmProtectionsRequest{})); err != nil {
			t.Fatal(err)
		}
		list, err = members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil || list.Msg.GetProtectionsConfirmTime() == nil || list.Msg.GetProtectionsConfirmedBy() != "Mira Chen" {
			t.Errorf("Members once confirmed: %v by %q, %v", list.Msg.GetProtectionsConfirmTime(), list.Msg.GetProtectionsConfirmedBy(), err)
		}
		// The same proof still makes the invite: the refusal spent nothing.
		if _, err = members.CreateInvite(ctx, connect.NewRequest(&dockwellv1.CreateInviteRequest{Email: "ada@example.com", Role: dockwellv1.Role_ROLE_VIEWER, Proof: proof})); err != nil {
			t.Errorf("the first invite once confirmed, with the passkey from before: %v", err)
		}
		// The audit log says how each environment stood.
		err = srv.store.View(ctx, func(tx *store.Tx) error {
			evs, err := tx.ListEvents(ctx, 1_000)
			found := false
			for _, e := range evs {
				if e.Action == "member.protections_confirm" {
					found = true
					if !strings.Contains(e.Detail, "default/production protected") {
						t.Errorf("the confirmation records %q, want how production stood", e.Detail)
					}
				}
			}
			if !found {
				t.Error("the confirmation isn't in the audit log")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// Managing members over the API: who may, what each ends, and what Members
// then says. A removed person's planned changes are discarded by Dockwell.
func TestMembersAreManagedOverTheAPI(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newInviteStand(t, backend)
		ctx := t.Context()
		ada, adaID := s.joins(t, "ada@example.com", "Ada Lovelace", dockwellv1.Role_ROLE_DEVELOPER)
		sam, samID := s.joins(t, "sam@example.com", "Sam Park", dockwellv1.Role_ROLE_VIEWER)
		adaMembers := dockwellv1connect.NewMemberServiceClient(ada, s.origin, connect.WithProtoJSON())

		// Members says who invited each, when and from where.
		list, err := s.members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range list.Msg.GetMembers() {
			joined := m.GetUser().GetId() != adaID && m.GetUser().GetId() != samID
			if joined != (m.GetJoinedFrom() == "") || joined != (m.GetInvitedBy() == "") {
				t.Errorf("%s reads invited by %q, joined from %q", m.GetUser().GetEmail(), m.GetInvitedBy(), m.GetJoinedFrom())
			}
			if !joined && (m.GetInvitedBy() != "Mira Chen" || m.GetJoinedFrom() != "127.0.0.1" || m.GetJoinTime() == nil) {
				t.Errorf("%s reads invited by %q, joined from %q at %v", m.GetUser().GetEmail(), m.GetInvitedBy(), m.GetJoinedFrom(), m.GetJoinTime())
			}
		}

		// Below an admin, each call is refused for want of a role, over Connect and REST, whatever passkey comes.
		for what, call := range map[string]func() error{
			"setting a role": func() error {
				_, err := adaMembers.SetMemberRole(ctx, connect.NewRequest(&dockwellv1.SetMemberRoleRequest{UserId: samID, Role: dockwellv1.Role_ROLE_ADMIN}))
				return err
			},
			"planning a removal": func() error {
				_, err := adaMembers.PlanMemberRemoval(ctx, connect.NewRequest(&dockwellv1.PlanMemberRemovalRequest{UserId: samID}))
				return err
			},
			"removing someone": func() error {
				_, err := adaMembers.RemoveMember(ctx, connect.NewRequest(&dockwellv1.RemoveMemberRequest{UserId: samID}))
				return err
			},
			"transferring ownership": func() error {
				_, err := adaMembers.TransferOwnership(ctx, connect.NewRequest(&dockwellv1.TransferOwnershipRequest{UserId: adaID}))
				return err
			},
			"confirming the protections": func() error {
				_, err := adaMembers.ConfirmProtections(ctx, connect.NewRequest(&dockwellv1.ConfirmProtectionsRequest{}))
				return err
			},
		} {
			needsRole(t, "a developer "+what, call())
		}
		if code, reason, _ := callREST(t, ada, http.MethodPost, "/v1/members/"+samID+":remove", nil, "{}"); code != http.StatusForbidden || reason != "needs_role" {
			t.Errorf("a developer removing someone over REST: %d %s, want 403 needs_role", code, reason)
		}
		// No API token manages members, an admin's included.
		token := createToken(t, s.owner, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		script := dockwellv1connect.NewMemberServiceClient(bearer{token: token, rt: s.owner.client.Transport}, s.origin, connect.WithProtoJSON())
		if _, err := script.RemoveMember(ctx, connect.NewRequest(&dockwellv1.RemoveMemberRequest{UserId: samID})); reasonOf(err) != "needs_session" {
			t.Errorf("an API token removing someone: %v, want needs_session", err)
		}
		// Without its passkey, an admin's call says which it takes.
		if _, err := s.members.RemoveMember(ctx, connect.NewRequest(&dockwellv1.RemoveMemberRequest{UserId: samID})); reasonOf(err) != "needs_step_up" {
			t.Errorf("removing someone with no passkey: %v, want needs_step_up", err)
		}

		// A role in a project, by its name.
		proof := stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_ROLE, auth.RoleTarget(samID, store.RoleDeveloper, s.shopID(t)))
		set, err := s.members.SetMemberRole(ctx, connect.NewRequest(&dockwellv1.SetMemberRoleRequest{UserId: samID, Role: dockwellv1.Role_ROLE_DEVELOPER, Project: "shop", Proof: proof}))
		if err != nil || set.Msg.GetLowered() || len(set.Msg.GetMember().GetGrants()) != 1 || set.Msg.GetMember().GetGrants()[0].GetProjectName() != "shop" {
			t.Fatalf("making sam a developer of shop: %v, %v", set, err)
		}
		// Taking back the install, which leaves them shop: a lowering, which signs them out.
		proof = stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_ROLE, auth.RoleTarget(samID, "", store.Install))
		set, err = s.members.SetMemberRole(ctx, connect.NewRequest(&dockwellv1.SetMemberRoleRequest{UserId: samID, Proof: proof}))
		if err != nil || !set.Msg.GetLowered() || set.Msg.GetSessionsEnded() != 1 || set.Msg.GetMember().GetUser().GetRole() != dockwellv1.Role_ROLE_UNSPECIFIED {
			t.Fatalf("taking back what sam holds on the install: %v, %v", set, err)
		}
		if _, err := sam.authClient().ListSessions(ctx, connect.NewRequest(&dockwellv1.ListSessionsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("sam's session after the lowering: %v, want it ended", err)
		}

		// Ada plans a change, and is then removed: the plan says what goes, and it goes.
		adaChanges := changeClient(ada, s.origin)
		var env string
		_ = s.srv.store.View(ctx, func(tx *store.Tx) error {
			all, err := tx.ListKind(ctx, "environment")
			for _, e := range all {
				if e.Scope != s.shopID(t) {
					env = e.ID
				}
			}
			return err
		})
		planned := propose(t, adaChanges, "Set MODE", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: env, Name: "MODE"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "fast"}}},
		})
		// Deploy on push acts for whoever turned it on, so a hook of ada's goes with
		// her, as the plan and the answer say (#253's re-check, N3).
		shop := s.shopID(t)
		if err := s.srv.store.Update(ctx, func(tx *store.Tx) error {
			now := time.Now()
			return tx.CreateGitHook(ctx, store.GitHook{ID: "ghk_ada", ResourceID: shop, UserID: adaID, Secret: []byte("x"), CreatedAt: now, SecretAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		// And has a token for shop alone, as a viewer at most, which the plan says where it acts (ADR 0071).
		_, ci := narrowedToken(t, ada, &dockwellv1.CreateTokenRequest{Projects: []string{"shop"}, Role: dockwellv1.TokenRole_TOKEN_ROLE_VIEWER})
		plan, err := s.members.PlanMemberRemoval(ctx, connect.NewRequest(&dockwellv1.PlanMemberRemovalRequest{UserId: adaID}))
		if err != nil || plan.Msg.GetSessions() != 1 || len(plan.Msg.GetChanges()) != 1 || plan.Msg.GetChanges()[0].GetId() != planned.GetId() || plan.Msg.GetMember().GetUser().GetEmail() != "ada@example.com" {
			t.Fatalf("the plan to remove ada: %v, %v", plan, err)
		}
		if hooks := plan.Msg.GetHooks(); len(hooks) != 1 || hooks[0].GetServiceId() != shop || hooks[0].GetServiceName() != "shop" {
			t.Errorf("the plan to remove ada lists hooks %v, want hers, by name", hooks)
		}
		var listed *dockwellv1.RemovalToken
		for _, tok := range plan.Msg.GetTokens() {
			if tok.GetId() == ci.GetId() {
				listed = tok
			}
		}
		if got := listed.GetProjects(); len(got) != 1 || got[0].GetName() != "shop" || listed.GetRole() != dockwellv1.TokenRole_TOKEN_ROLE_VIEWER {
			t.Errorf("the plan lists ada's token as %v, want it in shop, as a viewer at most", listed)
		}
		proof = stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_REMOVE_MEMBER, adaID)
		gone, err := s.members.RemoveMember(ctx, connect.NewRequest(&dockwellv1.RemoveMemberRequest{UserId: adaID, Proof: proof}))
		if err != nil || gone.Msg.GetSessionsEnded() != 1 || gone.Msg.GetChangesDiscarded() != 1 || len(gone.Msg.GetChangesLeft()) != 0 || gone.Msg.GetHooksDeleted() != 1 {
			t.Fatalf("removing ada: %v, %v", gone, err)
		}
		if _, err := ada.authClient().ListSessions(ctx, connect.NewRequest(&dockwellv1.ListSessionsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("ada's session after she was removed: %v, want it ended", err)
		}
		got, err := changeClient(s.owner, s.origin).GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: planned.GetId()}))
		if err != nil || got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_DISCARDED {
			t.Errorf("ada's planned change after she was removed: %v, %v; want it discarded", got.Msg.GetChange().GetState(), err)
		}
		list, err = s.members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil || len(list.Msg.GetMembers()) != 2 {
			t.Errorf("Members after ada was removed lists %d, %v; want the owner and sam", len(list.Msg.GetMembers()), err)
		}

		// Ownership: an admin can't move it, the owner does, and is an admin after.
		proof = stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_TRANSFER_OWNERSHIP, samID)
		moved, err := s.members.TransferOwnership(ctx, connect.NewRequest(&dockwellv1.TransferOwnershipRequest{UserId: samID, Proof: proof}))
		if err != nil || moved.Msg.GetMember().GetUser().GetRole() != dockwellv1.Role_ROLE_OWNER || len(moved.Msg.GetMember().GetGrants()) != 0 {
			t.Fatalf("transferring ownership to sam: %v, %v; want the owner, holding no project of their own", moved, err)
		}
		state, err := s.owner.authClient().GetAuthState(ctx, connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
		if err != nil || state.Msg.GetUser().GetRole() != dockwellv1.Role_ROLE_ADMIN {
			t.Errorf("whoever was the owner reads %v, %v; want an admin, still signed in", state.Msg.GetUser().GetRole(), err)
		}
		_, err = s.members.TransferOwnership(ctx, connect.NewRequest(&dockwellv1.TransferOwnershipRequest{UserId: samID}))
		needsRole(t, "an admin transferring ownership", err)

		// Activity has each, with who and what went.
		events, err := eventClient(s.owner, s.origin).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}}))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]*dockwellv1.EventMember{}
		transferred := false
		for _, e := range events.Msg.GetEvents() {
			if e.GetAction() == "member.remove" || e.GetAction() == "member.role" && seen["member.role"] == nil {
				seen[e.GetAction()] = e.GetMember()
			}
			transferred = transferred || e.GetAction() == "owner.transfer"
		}
		if !transferred {
			t.Error("Activity doesn't say ownership moved")
		}
		if r := seen["member.remove"]; r.GetEmail() != "ada@example.com" || r.GetSessions() != 1 || r.GetChanges() != 1 {
			t.Errorf("Activity's member.remove reads %v", r)
		}
		if r := seen["member.role"]; r.GetEmail() != "sam@example.com" || r.GetPreviousRole() != dockwellv1.Role_ROLE_VIEWER || r.GetRole() != dockwellv1.Role_ROLE_UNSPECIFIED || r.GetSessions() != 1 {
			t.Errorf("Activity's latest member.role reads %v", r)
		}
	})
}
