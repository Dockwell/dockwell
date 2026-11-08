// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net"
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

// inviteStand is an install served at panel.example.com, claimed there by Mira,
// with a project, shop.
type inviteStand struct {
	srv      *fullServer
	origin   string
	owner    *browser
	members  dockwellv1connect.MemberServiceClient
	newcomer func() *browser
}

func newInviteStand(t *testing.T, backend store.Backend) *inviteStand {
	t.Helper()
	cert, roots := selfSigned(t, "panel.example.com")
	srv := startFull(t, backend, &https.Static{Name: "panel.example.com", Cert: cert})
	_, port, _ := net.SplitHostPort(srv.https)
	s := &inviteStand{srv: srv, origin: "https://panel.example.com:" + port}
	s.owner = newBrowser(t, s.origin, srv.https, roots)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	s.owner.claim(t, link.Token)
	s.members = dockwellv1connect.NewMemberServiceClient(s.owner, s.origin, connect.WithProtoJSON())
	s.newcomer = func() *browser { return newBrowser(t, s.origin, srv.https, roots) }
	c := changeClient(s.owner, s.origin)
	apply(t, c, propose(t, c, "Make shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	}))
	return s
}

// invite invites email as role in project, confirming with the owner's passkey.
func (s *inviteStand) invite(t *testing.T, email string, role dockwellv1.Role, project, projectID string) (*dockwellv1.CreateInviteResponse, error) {
	t.Helper()
	place := projectID
	if place == "" {
		place = store.Install
	}
	// The target names the email as the control plane keeps it: lowercased, trimmed.
	proof := stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_INVITE, auth.InviteTarget(strings.ToLower(strings.TrimSpace(email)), roleName(role), place))
	res, err := s.members.CreateInvite(t.Context(), connect.NewRequest(&dockwellv1.CreateInviteRequest{Email: email, Role: role, Project: project, Proof: proof}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func roleName(r dockwellv1.Role) string {
	return strings.ToLower(strings.TrimPrefix(r.String(), "ROLE_"))
}

// accept accepts an invite in b as name, with a new passkey.
func accept(t *testing.T, b *browser, secret, name string) (*dockwellv1.FinishAcceptInviteResponse, error) {
	t.Helper()
	c := b.authClient()
	begin, err := c.BeginAcceptInvite(t.Context(), connect.NewRequest(&dockwellv1.BeginAcceptInviteRequest{Secret: secret, Name: name}))
	if err != nil {
		return nil, err
	}
	cred, _, err := b.auth.Create(begin.Msg.GetOptionsJson(), b.origin)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.FinishAcceptInvite(t.Context(), connect.NewRequest(&dockwellv1.FinishAcceptInviteRequest{
		CeremonyId: begin.Msg.GetCeremonyId(), Secret: secret, CredentialJson: cred, PasskeyName: "Firefox on Linux",
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func secretOf(t *testing.T, link string) string {
	t.Helper()
	_, secret, ok := strings.Cut(link, "/invite#")
	if !strings.HasPrefix(link, "https://panel.example.com/invite#") || !ok || len(secret) < 40 {
		t.Fatalf("an invite's link reads %q, want https://panel.example.com/invite# and its secret", link)
	}
	return secret
}

// The owner invites someone by email with a role in a place, with a passkey
// bound to what it's for, and sends the link it shows once. The person opens it
// at the panel's domain, sees who invited them as what and where, makes their
// passkey, and signs in, as a new person holding that role there. The invite
// then works no more (ADR 0048, ADR 0049).
func TestAnInviteMakesSomeoneNewWithItsRole(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newInviteStand(t, backend)
		ctx := t.Context()
		var shopID string
		_ = s.srv.store.View(ctx, func(tx *store.Tx) error {
			all, err := tx.ListKind(ctx, "project")
			for _, p := range all {
				if p.Name == "shop" {
					shopID = p.ID
				}
			}
			return err
		})

		// The passkey confirms this invite and no other.
		_, err := s.members.CreateInvite(ctx, connect.NewRequest(&dockwellv1.CreateInviteRequest{Email: "ada@example.com", Role: dockwellv1.Role_ROLE_DEVELOPER, Project: "shop"}))
		if reasonOf(err) != "needs_step_up" {
			t.Errorf("an invite with no passkey: %v, want needs_step_up", err)
		}
		for _, other := range []string{
			auth.InviteTarget("ada@example.com", store.RoleViewer, shopID),           // another role
			auth.InviteTarget("ada@example.com", store.RoleDeveloper, store.Install), // another place
			auth.InviteTarget("bo@example.com", store.RoleDeveloper, shopID),         // someone else
		} {
			wrong := stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_INVITE, other)
			_, err = s.members.CreateInvite(ctx, connect.NewRequest(&dockwellv1.CreateInviteRequest{Email: "ada@example.com", Role: dockwellv1.Role_ROLE_DEVELOPER, Project: "shop", Proof: wrong}))
			if reasonOf(err) != "needs_step_up" {
				t.Errorf("an invite with a passkey for %q: %v, want needs_step_up", other, err)
			}
		}

		made, err := s.invite(t, "Ada@Example.com", dockwellv1.Role_ROLE_DEVELOPER, "shop", shopID)
		if err != nil {
			t.Fatal(err)
		}
		secret := secretOf(t, made.GetLink())
		if i := made.GetInvite(); i.GetEmail() != "ada@example.com" || i.GetProjectName() != "shop" || i.GetInvitedBy() != "Mira Chen" ||
			i.GetExpireTime().AsTime().Sub(i.GetCreateTime().AsTime()) != 7*24*time.Hour {
			t.Errorf("the invite reads %v, want ada, a developer of shop, from Mira, for 7 days", i)
		}
		list, err := s.members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil || len(list.Msg.GetInvites()) != 1 || list.Msg.GetInvites()[0].GetEmail() != "ada@example.com" {
			t.Errorf("Members lists invites %v, %v, want ada's", list.Msg.GetInvites(), err)
		}

		// Opening it at the domain says what it offers, and spends nothing.
		ada := s.newcomer()
		for range 2 {
			checked, err := ada.authClient().CheckInvite(ctx, connect.NewRequest(&dockwellv1.CheckInviteRequest{Secret: secret}))
			if err != nil || checked.Msg.GetInvitedBy() != "Mira Chen" || checked.Msg.GetRole() != dockwellv1.Role_ROLE_DEVELOPER ||
				checked.Msg.GetProjectName() != "shop" || checked.Msg.GetEmail() != "ada@example.com" {
				t.Errorf("checking the invite: %v, %v", checked, err)
			}
		}
		// Not in a browser someone is signed in on.
		if _, err := accept(t, s.owner, secret, "Ada"); reasonOf(err) != "signed_in" || !strings.Contains(err.Error(), "You're signed in as Mira Chen") && !strings.Contains(err.Error(), "you're signed in as Mira Chen") {
			t.Errorf("accepting where Mira is signed in: %v, want signed_in", err)
		}
		joined, err := accept(t, ada, secret, "Ada Lovelace")
		if err != nil {
			t.Fatal(err)
		}
		if joined.GetUser().GetEmail() != "ada@example.com" || joined.GetUser().GetName() != "Ada Lovelace" || joined.GetUser().GetRole() != dockwellv1.Role_ROLE_UNSPECIFIED {
			t.Errorf("the new person reads %v, want Ada, with no role on the whole install", joined.GetUser())
		}
		state, err := ada.authClient().GetAuthState(ctx, connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
		if err != nil || state.Msg.GetUser().GetEmail() != "ada@example.com" || state.Msg.GetSignInMethod() != dockwellv1.SignInMethod_SIGN_IN_METHOD_INVITE {
			t.Errorf("Ada's sign-in reads %v, %v", state, err)
		}
		list, err = s.members.ListMembers(ctx, connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil || len(list.Msg.GetInvites()) != 0 || len(list.Msg.GetMembers()) != 2 {
			t.Fatalf("Members after Ada joined: %v, %v", list, err)
		}
		for _, m := range list.Msg.GetMembers() {
			if m.GetUser().GetEmail() == "ada@example.com" && (len(m.GetGrants()) != 1 || m.GetGrants()[0].GetProjectName() != "shop" || m.GetGrants()[0].GetRole() != dockwellv1.Role_ROLE_DEVELOPER) {
				t.Errorf("Ada holds %v, want developer of shop", m.GetGrants())
			}
		}
		// It works once.
		again := s.newcomer()
		if _, err := again.authClient().CheckInvite(ctx, connect.NewRequest(&dockwellv1.CheckInviteRequest{Secret: secret})); reasonOf(err) != "invite_rejected" {
			t.Errorf("checking a spent invite: %v, want invite_rejected", err)
		}

		// Activity says who joined, as what, where; the audit log has each step.
		events, err := eventClient(s.owner, s.origin).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}}))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]*dockwellv1.EventMember{}
		for _, e := range events.Msg.GetEvents() {
			if strings.HasPrefix(e.GetAction(), "member.") {
				seen[e.GetAction()] = e.GetMember()
			}
		}
		if j := seen["member.join"]; j.GetEmail() != "ada@example.com" || j.GetName() != "Ada Lovelace" || j.GetProjectName() != "shop" || j.GetRole() != dockwellv1.Role_ROLE_DEVELOPER {
			t.Errorf("Activity's member.join reads %v", j)
		}
		if i := seen["member.invite"]; i.GetEmail() != "ada@example.com" || i.GetProjectName() != "shop" {
			t.Errorf("Activity's member.invite reads %v", i)
		}
	})
}

// What an invite may offer, and where it works: nobody as owner, an admin to
// the whole install for a day, nobody who signs in already, a newer invite
// replacing an older, a revoked one working no more, and only at the panel's
// domain.
func TestWhatAnInviteMayOffer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newInviteStand(t, backend)
		ctx := t.Context()
		if _, err := s.invite(t, "boss@example.com", dockwellv1.Role_ROLE_OWNER, "", ""); reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "ownership moves by transfer") {
			t.Errorf("an invite as owner: %v, want invalid", err)
		}
		if _, err := s.invite(t, "mira@acme.dev", dockwellv1.Role_ROLE_VIEWER, "", ""); reasonOf(err) != "member_exists" || !strings.Contains(err.Error(), "mira@acme.dev signs in here already") {
			t.Errorf("an invite to someone who signs in: %v, want member_exists", err)
		}
		if _, err := s.invite(t, "x@example.com", dockwellv1.Role_ROLE_VIEWER, "nowhere", "nowhere"); reasonOf(err) != "not_found" {
			t.Errorf("an invite to a project that isn't there: %v, want not_found", err)
		}
		admin, err := s.invite(t, "sam@example.com", dockwellv1.Role_ROLE_ADMIN, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if i := admin.GetInvite(); i.GetExpireTime().AsTime().Sub(i.GetCreateTime().AsTime()) != 24*time.Hour || i.GetProjectId() != "" {
			t.Errorf("an admin's invite reads %v, want the whole install for 24 hours", i)
		}
		// A newer one to the same email replaces it.
		newer, err := s.invite(t, "sam@example.com", dockwellv1.Role_ROLE_VIEWER, "", "")
		if err != nil {
			t.Fatal(err)
		}
		probe := s.newcomer()
		if _, err := probe.authClient().CheckInvite(ctx, connect.NewRequest(&dockwellv1.CheckInviteRequest{Secret: secretOf(t, admin.GetLink())})); reasonOf(err) != "invite_rejected" {
			t.Errorf("the replaced invite: %v, want invite_rejected", err)
		}
		if _, err := s.members.RevokeInvite(ctx, connect.NewRequest(&dockwellv1.RevokeInviteRequest{Id: newer.GetInvite().GetId()})); err != nil {
			t.Fatal(err)
		}
		if _, err := probe.authClient().CheckInvite(ctx, connect.NewRequest(&dockwellv1.CheckInviteRequest{Secret: secretOf(t, newer.GetLink())})); reasonOf(err) != "invite_rejected" {
			t.Errorf("a revoked invite: %v, want invite_rejected", err)
		}
		if _, err := s.members.RevokeInvite(ctx, connect.NewRequest(&dockwellv1.RevokeInviteRequest{Id: newer.GetInvite().GetId()})); reasonOf(err) != "not_found" {
			t.Errorf("revoking it again: %v, want not_found", err)
		}
		// Only at the panel's domain.
		last, err := s.invite(t, "lin@example.com", dockwellv1.Role_ROLE_VIEWER, "", "")
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(s.srv.api)
		local := newBrowser(t, "http://localhost:"+port, s.srv.api, nil)
		if _, err := local.authClient().CheckInvite(ctx, connect.NewRequest(&dockwellv1.CheckInviteRequest{Secret: secretOf(t, last.GetLink())})); reasonOf(err) != "invite_elsewhere" || !strings.Contains(err.Error(), "https://panel.example.com") {
			t.Errorf("an invite opened at localhost: %v, want invite_elsewhere naming the domain", err)
		}
	})
}

// An install whose panel has no domain can't invite: the link would lead
// nowhere the invited person can reach.
func TestInvitesNeedTheDomain(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_INVITE, auth.InviteTarget("ada@example.com", store.RoleViewer, store.Install))
	_, err := dockwellv1connect.NewMemberServiceClient(b, b.origin, connect.WithProtoJSON()).CreateInvite(t.Context(),
		connect.NewRequest(&dockwellv1.CreateInviteRequest{Email: "ada@example.com", Role: dockwellv1.Role_ROLE_VIEWER, Proof: proof}))
	if reasonOf(err) != "needs_domain" || !strings.Contains(err.Error(), "DOCKWELL_DOMAIN") {
		t.Errorf("an invite without a domain: %v, want needs_domain", err)
	}
}
