// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// joined is someone who accepted an invite, with their passkey and their session's secret.
type joined struct {
	p      Principal
	keys   *authtest.Authenticator
	secret string
}

// join invites email to the whole install as role and accepts, from addr.
func (f *invited) join(t *testing.T, email, role, addr string) joined {
	t.Helper()
	_, secret := f.invite(t, email, role)
	client := onDomain
	client.Addr = addr
	cer, err := f.svc.BeginAcceptInvite(t.Context(), client, secret, strings.ToUpper(email[:1])+email[1:strings.Index(email, "@")])
	if err != nil {
		t.Fatal(err)
	}
	a := &authtest.Authenticator{Counter: true}
	cred, _, err := a.Create(string(cer.Options), onDomain.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.svc.FinishAcceptInvite(t.Context(), client, cer.ID, secret, []byte(cred), "")
	if err != nil {
		t.Fatal(err)
	}
	return joined{p: Principal{User: in.User, Session: in.Session}, keys: a, secret: in.Token}
}

// token makes an API token for someone, with their own passkey.
func (f *invited) token(t *testing.T, who joined) NewToken {
	t.Helper()
	proof := f.stepUpAt(t, onDomain, who.p, who.keys, StepUpCreateToken, "").Proof
	made, err := f.svc.CreateToken(t.Context(), onDomain, who.p, "deploys", store.ScopeReadWrite, nil, nil, proof)
	if err != nil {
		t.Fatal(err)
	}
	return made
}

// setRole changes someone's role on the whole install as the owner, with a passkey for exactly that.
func (f *invited) setRole(t *testing.T, userID, role string) (RoleChange, error) {
	t.Helper()
	proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(userID, role, store.Install)).Proof
	return f.svc.SetMemberRole(t.Context(), onDomain, f.owner, userID, role, store.Install, proof)
}

// signedIn reports whether a session's secret still signs in.
func (f *invited) signedIn(t *testing.T, secret string) bool {
	t.Helper()
	_, err := f.svc.Authenticate(t.Context(), onDomain, secret)
	if err != nil && !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
	return err == nil
}

// works reports whether an API token still works for a read.
func (f *invited) works(t *testing.T, tok NewToken) bool {
	t.Helper()
	_, err := f.svc.AuthenticateToken(t.Context(), onDomain, tok.Secret, AccessRead, "/dockwell.v1.ServiceService/ListServices")
	if err != nil && !errors.Is(err, ErrTokenRejected) {
		t.Fatal(err)
	}
	return err == nil
}

func (f *invited) grants(t *testing.T, userID string) []string {
	t.Helper()
	var out []string
	err := f.db.View(t.Context(), func(tx *store.Tx) error {
		gs, err := tx.Grants(t.Context(), userID)
		for _, g := range gs {
			out = append(out, g.ProjectID+"="+g.Role)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sessionsOf are the sessions a person has open.
func (f *invited) sessionsOf(t *testing.T, userID string) []store.Session {
	t.Helper()
	var out []store.Session
	if err := f.db.View(t.Context(), func(tx *store.Tx) (err error) {
		out, err = tx.ListActiveSessions(t.Context(), userID, f.clock.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *invited) openInvites(t *testing.T) int {
	t.Helper()
	n := 0
	if err := f.db.View(t.Context(), func(tx *store.Tx) error {
		open, err := tx.ListOpenInvites(t.Context(), f.clock.now())
		n = len(open)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// A role change takes a passkey for exactly that person, role and place. A
// raise ends nothing; lowering what someone holds ends their sessions and
// revokes their tokens, and the invites they made once they're no longer an admin.
func TestLoweringARoleEndsWhatItGave(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleDeveloper, "203.0.113.5:1")
		tok := f.token(t, ada)

		// A passkey for another person, role, place or action changes nothing.
		sam := f.join(t, "sam@example.com", store.RoleViewer, "203.0.113.6:1")
		for what, proof := range map[string]string{
			"another person": f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(sam.p.User.ID, store.RoleAdmin, store.Install)).Proof,
			"another role":   f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(ada.p.User.ID, store.RoleViewer, store.Install)).Proof,
			"another place":  f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(ada.p.User.ID, store.RoleAdmin, "prj_1")).Proof,
			"another action": f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof,
			"no passkey":     "",
		} {
			if _, err := f.svc.SetMemberRole(t.Context(), onDomain, f.owner, ada.p.User.ID, store.RoleAdmin, store.Install, proof); !errors.Is(err, ErrNeedsStepUp) {
				t.Errorf("making ada an admin with a passkey for %s: %v, want it to need its own", what, err)
			}
		}

		raised, err := f.setRole(t, ada.p.User.ID, store.RoleAdmin)
		if err != nil || raised.Lowered || raised.Previous != store.RoleDeveloper {
			t.Fatalf("raising ada to admin: %+v, %v", raised, err)
		}
		if !f.signedIn(t, ada.secret) || !f.works(t, tok) {
			t.Error("a raise ended ada's session or her token")
		}
		// As an admin she invites someone.
		adaProof := f.stepUpAt(t, onDomain, ada.p, ada.keys, StepUpInvite, InviteTarget("lin@example.com", store.RoleViewer, store.Install)).Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, ada.p, "lin@example.com", store.RoleViewer, store.Install, adaProof); err != nil {
			t.Fatal(err)
		}
		f.invite(t, "kim@example.com", store.RoleViewer) // the owner's own, which stays
		if n := f.openInvites(t); n != 2 {
			t.Fatalf("%d open invites, want ada's and the owner's", n)
		}

		lowered, err := f.setRole(t, ada.p.User.ID, store.RoleViewer)
		if err != nil || !lowered.Lowered || lowered.Previous != store.RoleAdmin || lowered.Ended.Sessions != 1 || lowered.Ended.Tokens != 1 || lowered.Invites != 1 {
			t.Fatalf("lowering ada to viewer: %+v, %v; want her session, her token and her invite ended", lowered, err)
		}
		if f.signedIn(t, ada.secret) {
			t.Error("ada's session goes on as an admin's after she was lowered")
		}
		if f.works(t, tok) {
			t.Error("ada's token still works after she was lowered")
		}
		if n := f.openInvites(t); n != 1 {
			t.Errorf("%d open invites after ada was lowered, want the owner's alone", n)
		}
		details := f.auditDetails(t, "member.role")
		if last := details[len(details)-1]; !strings.Contains(last, `"role":"viewer"`) || !strings.Contains(last, `"previous":"admin"`) ||
			!strings.Contains(last, `"sessions":1`) || !strings.Contains(last, `"tokens":1`) || !strings.Contains(last, `"invites":1`) {
			t.Errorf("the audit log says of the lowering: %s", last)
		}
	})
}

// Nobody changes or removes themselves or the owner, nobody below an admin
// manages anyone, and the last thing someone holds is taken back only by
// removing them. Each is refused before any passkey is spent.
func TestWhoMayManageWhom(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleDeveloper, "203.0.113.6:1")
		owner := f.owner.User.ID

		// On yourself: the owner, and an admin.
		if _, err := f.setRole(t, owner, store.RoleAdmin); !errors.Is(err, ErrOnYourself) {
			t.Errorf("the owner demoting themselves: %v", err)
		}
		adaOn := func(action StepUpAction, target string) string {
			return f.stepUpAt(t, onDomain, ada.p, ada.keys, action, target).Proof
		}
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, ada.p, ada.p.User.ID, store.RoleViewer, store.Install, adaOn(StepUpChangeRole, RoleTarget(ada.p.User.ID, store.RoleViewer, store.Install))); !errors.Is(err, ErrOnYourself) {
			t.Errorf("an admin lowering themselves: %v", err)
		}
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, ada.p, ada.p.User.ID, adaOn(StepUpRemoveMember, ada.p.User.ID)); !errors.Is(err, ErrOnYourself) {
			t.Errorf("an admin removing themselves: %v", err)
		}
		// On the owner, by an admin.
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, ada.p, owner, store.RoleViewer, store.Install, adaOn(StepUpChangeRole, RoleTarget(owner, store.RoleViewer, store.Install))); !errors.Is(err, ErrTheOwner) {
			t.Errorf("an admin demoting the owner: %v", err)
		}
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, ada.p, owner, adaOn(StepUpRemoveMember, owner)); !errors.Is(err, ErrTheOwner) {
			t.Errorf("an admin removing the owner: %v", err)
		}
		if _, err := f.svc.PlanMemberRemoval(t.Context(), ada.p, owner); !errors.Is(err, ErrTheOwner) {
			t.Errorf("an admin planning the owner's removal: %v", err)
		}
		// Nobody is made the owner by a role change.
		if _, err := f.setRole(t, sam.p.User.ID, store.RoleOwner); !errors.Is(err, ErrInvalid) {
			t.Errorf("making sam the owner by a role change: %v", err)
		}
		// Below an admin, with a passkey of their own for it.
		samOn := func(action StepUpAction, target string) string {
			return f.stepUpAt(t, onDomain, sam.p, sam.keys, action, target).Proof
		}
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, sam.p, ada.p.User.ID, store.RoleViewer, store.Install, samOn(StepUpChangeRole, RoleTarget(ada.p.User.ID, store.RoleViewer, store.Install))); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("a developer lowering an admin: %v", err)
		}
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, sam.p, ada.p.User.ID, samOn(StepUpRemoveMember, ada.p.User.ID)); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("a developer removing an admin: %v", err)
		}
		if _, err := f.svc.PlanMemberRemoval(t.Context(), sam.p, ada.p.User.ID); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("a developer planning a removal: %v", err)
		}
		if _, err := f.svc.ConfirmProtections(t.Context(), onDomain, sam.p, "x"); !errors.Is(err, ErrAdminOnly) {
			t.Errorf("a developer confirming the protections: %v", err)
		}
		if _, err := f.svc.TransferOwnership(t.Context(), onDomain, ada.p, sam.p.User.ID, adaOn(StepUpTransferOwnership, sam.p.User.ID)); !errors.Is(err, ErrOwnerOnly) {
			t.Errorf("an admin transferring ownership: %v", err)
		}
		// The last thing someone holds is taken back by removing them, and a refusal spends no passkey.
		target := RoleTarget(sam.p.User.ID, "", store.Install)
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, target).Proof
		if _, err := f.svc.SetMemberRole(t.Context(), onDomain, f.owner, sam.p.User.ID, "", store.Install, proof); !errors.Is(err, ErrLastGrant) {
			t.Errorf("taking back the last thing sam holds: %v, want to be told to remove them", err)
		}
		if err := f.svc.UseStepUp(t.Context(), onDomain, f.owner, proof, StepUpChangeRole, target); err != nil {
			t.Errorf("the proof was spent on a change that was refused: %v", err)
		}
		// Nothing above changed anything.
		if got := f.grants(t, owner); len(got) != 1 || got[0] != "=owner" {
			t.Errorf("the owner holds %v", got)
		}
		if got := f.grants(t, ada.p.User.ID); len(got) != 1 || got[0] != "=admin" {
			t.Errorf("ada holds %v", got)
		}
		if !f.signedIn(t, ada.secret) || !f.signedIn(t, sam.secret) {
			t.Error("a refused call ended someone's session")
		}
	})
}

// Removing someone turns off every way they had in, in one go: their passkey,
// their session, their tokens and the invites they made. Their name stays on
// what they did, and their email is free to invite again as someone new.
func TestRemovingSomeoneEndsEveryWayIn(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		tok := f.token(t, ada)
		adaProof := f.stepUpAt(t, onDomain, ada.p, ada.keys, StepUpInvite, InviteTarget("lin@example.com", store.RoleViewer, store.Install)).Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, ada.p, "lin@example.com", store.RoleViewer, store.Install, adaProof); err != nil {
			t.Fatal(err)
		}
		id := ada.p.User.ID

		// From the control server, a one-time sign-in link for her, not yet used.
		link, _, err := f.svc.CreateSignInLink(t.Context(), "ada@example.com", "cli:root@hz-1")
		if err != nil {
			t.Fatal(err)
		}

		plan, err := f.svc.PlanMemberRemoval(t.Context(), f.owner, id)
		if err != nil || plan.Sessions != 1 || len(plan.Tokens) != 1 || plan.Tokens[0].ID != tok.Token.ID || len(plan.Invites) != 1 || plan.Invites[0].Email != "lin@example.com" {
			t.Fatalf("the plan to remove ada: %+v, %v; want her session, her token and her invite", plan, err)
		}
		if !f.signedIn(t, ada.secret) || !f.works(t, tok) || f.openInvites(t) != 1 {
			t.Fatal("reading the plan changed something")
		}
		// A passkey for someone else, or for another action, removes nobody.
		for what, proof := range map[string]string{
			"another person": f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, f.owner.User.ID).Proof,
			"another action": f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeRole, RoleTarget(id, "", store.Install)).Proof,
			"no passkey":     "",
		} {
			if _, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, id, proof); !errors.Is(err, ErrNeedsStepUp) {
				t.Errorf("removing ada with a passkey for %s: %v, want it to need its own", what, err)
			}
		}

		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, id).Proof
		gone, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, id, proof)
		if err != nil || gone.Sessions != 1 || len(gone.Tokens) != 1 || len(gone.Invites) != 1 {
			t.Fatalf("removing ada: %+v, %v", gone, err)
		}
		if f.signedIn(t, ada.secret) {
			t.Error("ada's session goes on after she was removed")
		}
		if f.works(t, tok) {
			t.Error("ada's token works after she was removed")
		}
		if n := f.openInvites(t); n != 0 {
			t.Errorf("%d open invites after ada was removed, want none of hers", n)
		}
		if got := f.grants(t, id); len(got) != 0 {
			t.Errorf("ada holds %v after she was removed", got)
		}
		err = f.db.View(t.Context(), func(tx *store.Tx) error {
			u, err := tx.GetUser(t.Context(), id)
			if err != nil || u.RemovedAt == nil || u.Name != "Ada" {
				t.Errorf("ada's row after removal: %+v, %v; want it kept, marked removed, with her name", u, err)
			}
			if _, err := tx.GetUserByEmail(t.Context(), "ada@example.com"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("ada@example.com still finds someone who signs in: %v", err)
			}
			keys, err := tx.ListPasskeys(t.Context(), id)
			if err != nil || len(keys) != 0 {
				t.Errorf("ada has %d passkeys after removal, %v", len(keys), err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := f.users(t); n != 1 {
			t.Errorf("%d people sign in after ada was removed, want the owner alone", n)
		}
		// Removing her again finds nobody, and so does changing her role.
		again := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, id).Proof
		if _, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, id, again); !errors.Is(err, ErrNotFound) {
			t.Errorf("removing ada twice: %v", err)
		}
		if _, err := f.setRole(t, id, store.RoleViewer); !errors.Is(err, ErrNotFound) {
			t.Errorf("giving someone removed a role: %v", err)
		}
		last := f.auditDetails(t, "member.remove")
		if len(last) != 1 || !strings.Contains(last[0], `"email":"ada@example.com"`) || !strings.Contains(last[0], `"sessions":1`) || !strings.Contains(last[0], `"tokens":1`) || !strings.Contains(last[0], `"invites":1`) {
			t.Errorf("the audit log says of the removal: %v", last)
		}
		// A sign-in link made for her before she was removed signs nobody in after.
		if _, err := f.svc.SignInWithLink(t.Context(), onDomain, link.Token); err == nil {
			t.Error("a sign-in link made for ada before she was removed still signs her in")
		}
		if n := len(f.sessionsOf(t, id)); n != 0 {
			t.Errorf("ada has %d sessions open after she was removed", n)
		}
		// Her email is free: an invite to it makes someone new, who isn't her.
		back := f.join(t, "ada@example.com", store.RoleViewer, "203.0.113.9:1")
		if back.p.User.ID == id {
			t.Error("inviting ada again made the person who was removed")
		}
	})
}

// Ownership moves in one go, with the owner's passkey for exactly that person:
// the new owner holds the install, whoever was the owner is an admin, and there
// is one owner before and after. The CLI's local token that acted for the old
// owner is revoked.
func TestOwnershipMovesWholeOrNotAtAll(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleViewer, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleAdmin, "203.0.113.6:1")
		was := f.owner.User.ID
		local, err := f.svc.CreateLocalToken(t.Context(), "cli:mira@hz-1", "Local access on hz-1, read-write", store.ScopeReadWrite, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		owners := func() []string {
			t.Helper()
			var out []string
			if err := f.db.View(t.Context(), func(tx *store.Tx) error {
				all, err := tx.ListGrants(t.Context())
				for _, g := range all {
					if g.Role == store.RoleOwner {
						out = append(out, g.UserID)
					}
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		for what, proof := range map[string]string{
			"another person": f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, sam.p.User.ID).Proof,
			"another action": f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, ada.p.User.ID).Proof,
			"no passkey":     "",
		} {
			if _, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, ada.p.User.ID, proof); !errors.Is(err, ErrNeedsStepUp) {
				t.Errorf("transferring to ada with a passkey for %s: %v, want it to need its own", what, err)
			}
		}
		self := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, was).Proof
		if _, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, was, self); !errors.Is(err, ErrInvalid) {
			t.Errorf("transferring to yourself: %v", err)
		}
		nobody := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, "usr_nobody").Proof
		if _, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, "usr_nobody", nobody); !errors.Is(err, ErrNotFound) {
			t.Errorf("transferring to nobody: %v", err)
		}
		if got := owners(); len(got) != 1 || got[0] != was {
			t.Fatalf("after refused transfers the owners are %v", got)
		}

		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, ada.p.User.ID).Proof
		to, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, ada.p.User.ID, proof)
		if err != nil || to.ID != ada.p.User.ID || to.Role != store.RoleOwner {
			t.Fatalf("transferring to ada: %+v, %v", to, err)
		}
		if got := owners(); len(got) != 1 || got[0] != ada.p.User.ID {
			t.Errorf("the owners are %v, want ada alone", got)
		}
		if got := f.grants(t, was); len(got) != 1 || got[0] != "=admin" {
			t.Errorf("whoever was the owner holds %v, want admin", got)
		}
		if got := f.grants(t, ada.p.User.ID); len(got) != 1 || got[0] != "=owner" {
			t.Errorf("ada holds %v, want the owner's alone", got)
		}
		if _, err := f.svc.AuthenticateToken(t.Context(), localhost, local.Secret, AccessRead, "/dockwell.v1.ServiceService/ListServices"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("the old owner's local token after the transfer: %v, want it revoked", err)
		}
		// Whoever was the owner keeps their session, as an admin, and transfers nothing more.
		if !f.signedIn(t, f.ownerSecret) {
			t.Error("the old owner was signed out by the transfer")
		}
		back := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, was).Proof
		if _, err := f.svc.TransferOwnership(t.Context(), onDomain, f.owner, sam.p.User.ID, back); !errors.Is(err, ErrOwnerOnly) {
			t.Errorf("the old owner transferring again: %v", err)
		}
		if d := f.auditDetails(t, "owner.transfer"); len(d) != 1 || !strings.Contains(d[0], "made ada@example.com the owner, in place of mira@acme.dev") {
			t.Errorf("the audit log says of the transfer: %v", d)
		}
	})
}

// The first invite waits until someone confirmed which environments are
// protected, whoever asks and however: it's refused with the passkey unspent.
// The confirmation is on record with how each environment stood. An install
// where a second person already signs in needs none.
func TestTheFirstInviteWaitsForTheProtections(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newFixture(t, backend, onDomain.Site.RPID)
		a := &authtest.Authenticator{Counter: true}
		owner := ownerPrincipal(f.claim(t, onDomain, a))
		target := InviteTarget("ada@example.com", store.RoleViewer, store.Install)
		proof := f.stepUpAt(t, onDomain, owner, a, StepUpInvite, target).Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, owner, "ada@example.com", store.RoleViewer, store.Install, proof); !errors.Is(err, ErrProtectionsUnconfirmed) {
			t.Fatalf("the first invite before anyone confirmed the protections: %v", err)
		}
		if err := f.svc.UseStepUp(t.Context(), onDomain, owner, proof, StepUpInvite, target); err != nil {
			t.Errorf("the proof was spent on an invite that was refused: %v", err)
		}
		done, err := f.svc.ConfirmProtections(t.Context(), onDomain, owner, "default/production protected, default/staging open")
		if err != nil || done.By != owner.Actor() {
			t.Fatalf("confirming: %+v, %v", done, err)
		}
		if d := f.auditDetails(t, "member.protections_confirm"); len(d) != 1 || d[0] != "default/production protected, default/staging open" {
			t.Errorf("the audit log says of the confirmation: %v", d)
		}
		proof = f.stepUpAt(t, onDomain, owner, a, StepUpInvite, target).Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, owner, "ada@example.com", store.RoleViewer, store.Install, proof); err != nil {
			t.Errorf("the first invite once confirmed: %v", err)
		}
		// It's kept across a restart.
		f.svc = f.restart(t)
		if err := f.db.View(t.Context(), func(tx *store.Tx) error {
			if _, ok, err := f.svc.ProtectionsConfirmed(t.Context(), tx); err != nil || !ok {
				t.Errorf("after a restart the confirmation reads %v, %v", ok, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// A second person already signs in, from before the confirmation existed:
// nothing is asked before an invite.
func TestAnInstallWithTwoPeopleNeedsNoConfirmation(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newFixture(t, backend, onDomain.Site.RPID)
		a := &authtest.Authenticator{Counter: true}
		owner := ownerPrincipal(f.claim(t, onDomain, a))
		err := f.db.Update(t.Context(), func(tx *store.Tx) error {
			if err := tx.CreateUser(t.Context(), store.User{ID: "usr_sam", Email: "sam@example.com", Name: "Sam", WebAuthnHandle: []byte("sam's handle"), CreatedAt: f.clock.now()}); err != nil {
				return err
			}
			return tx.SetGrant(t.Context(), store.Grant{UserID: "usr_sam", ProjectID: store.Install, Role: store.RoleViewer, CreatedAt: f.clock.now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		proof := f.stepUpAt(t, onDomain, owner, a, StepUpInvite, InviteTarget("ada@example.com", store.RoleViewer, store.Install)).Proof
		if _, err := f.svc.CreateInvite(t.Context(), onDomain, owner, "ada@example.com", store.RoleViewer, store.Install, proof); err != nil {
			t.Errorf("an invite where two people sign in already: %v", err)
		}
	})
}

// Where someone joined from is kept with the invite they accepted.
func TestAJoinKeepsWhereItCameFrom(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleViewer, "203.0.113.5:4411")
		err := f.db.View(t.Context(), func(tx *store.Tx) error {
			inv, err := tx.AcceptedInvite(t.Context(), ada.p.User.ID)
			if err != nil || inv.AcceptedFrom != "203.0.113.5" || inv.CreatedBy != f.owner.Actor() {
				t.Errorf("ada's invite reads %+v, %v; want it accepted from 203.0.113.5, made by the owner", inv, err)
			}
			if _, err := tx.AcceptedInvite(t.Context(), f.owner.User.ID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the owner, who claimed the install, has an invite: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// Two transfers racing leave one owner: whichever lands first, the other finds
// its caller is no longer the owner.
func TestRacingTransfersLeaveOneOwner(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleViewer, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleViewer, "203.0.113.6:1")
		proofs := map[string]string{}
		for _, to := range []string{ada.p.User.ID, sam.p.User.ID} {
			proofs[to] = f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpTransferOwnership, to).Proof
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for to, proof := range proofs {
			wg.Go(func() {
				_, err := f.svc.TransferOwnership(context.Background(), onDomain, f.owner, to, proof)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, ErrOwnerOnly):
				default:
					t.Errorf("TransferOwnership: %v", err)
				}
			})
		}
		wg.Wait()
		owners := 0
		if err := f.db.View(t.Context(), func(tx *store.Tx) error {
			all, err := tx.ListGrants(t.Context())
			for _, g := range all {
				if g.Role == store.RoleOwner {
					owners++
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if won != 1 || owners != 1 {
			t.Errorf("%d transfers won and %d people own the install, want one and one", won, owners)
		}
	})
}
