// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func ptr(s string) *string { return &s }

// writeToken makes p a read-write token, confirmed with the owner's passkey at the domain.
func (f *invited) writeToken(t *testing.T, p Principal) NewToken {
	t.Helper()
	proof := f.stepUpAt(t, onDomain, p, f.keys, StepUpCreateToken, "").Proof
	made, err := f.svc.CreateToken(t.Context(), onDomain, p, "Profile script", store.ScopeReadWrite, nil, nil, proof)
	if err != nil {
		t.Fatal(err)
	}
	return made
}

// A name changes with no passkey, from a session or a token that makes changes,
// tidied as sign-up tidies it, and the audit log says what it was and what it is.
func TestANameChangesWithNoPasskey(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		u, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("  Mira   Okafor "), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if u.Name != "Mira Okafor" || u.Email != "mira@acme.dev" || f.user(t, f.owner).Name != "Mira Okafor" {
			t.Fatalf("after a change of name: %q <%s>, stored %q", u.Name, u.Email, f.user(t, f.owner).Name)
		}
		if got := f.auditDetails(t, "account.name"); len(got) != 1 || got[0] != `from "Mira Chen" to "Mira Okafor"` {
			t.Errorf("the audit log's name changes = %q", got)
		}
		// The same name again changes nothing, and isn't on record twice.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("Mira Okafor"), nil, ""); err != nil {
			t.Fatal(err)
		}
		if got := f.auditDetails(t, "account.name"); len(got) != 1 {
			t.Errorf("%d name changes on record after saving the same name", len(got))
		}
		// A token acting for the person changes the name too, and is on record as itself.
		tok := f.writeToken(t, f.owner)
		withToken := Principal{User: f.user(t, f.owner), Token: &tok.Token}
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, withToken, ptr("Mira"), nil, ""); err != nil {
			t.Fatalf("a read-write token changing the name: %v", err)
		}
		if ev := f.auditEvents(t, "account.name"); len(ev) != 2 || ev[1].Actor != withToken.Actor() {
			t.Errorf("the token's change is on record as %+v", ev)
		}
	})
}

// A new email takes a passkey just now, for that email in lowercase, every time:
// the hour of cover the person chose doesn't stand in, no proof for another email
// or action does, and no API token can change it. Each change is audited.
func TestAnEmailChangeAsksForAPasskeyEachTime(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		claimed := SignedIn{Token: f.ownerSecret}
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpSetPolicy, "").Proof
		if _, err := f.svc.SetStepUpPolicy(t.Context(), onDomain, f.owner, AskHourly, proof); err != nil {
			t.Fatal(err)
		}
		p := f.current(t, onDomain, claimed)
		// Within the hour, what doesn't always ask needs no passkey...
		if err := f.svc.UseStepUp(t.Context(), onDomain, p, "", StepUpRevealSecret, "sec_1"); err != nil {
			t.Fatalf("revealing within the hour: %v", err)
		}
		// ...and a new email still does.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira.chen@example.com"), ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Fatalf("a new email within the hour, with no proof: %v, want a passkey asked for", err)
		}
		wrong := f.stepUpAt(t, onDomain, p, f.keys, StepUpChangeEmail, "lee@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira.chen@example.com"), wrong); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a proof for another email: %v, want it refused", err)
		}
		other := f.stepUpAt(t, onDomain, p, f.keys, StepUpRevealSecret, "mira.chen@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira.chen@example.com"), other); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a proof for another action: %v, want it refused", err)
		}
		// Names and emails mixed in one save: neither changes when the email is refused.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, ptr("Mira Okafor"), ptr("mira.chen@example.com"), ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Fatalf("a new name and email with no proof: %v", err)
		}
		if u := f.user(t, p); u.Name != "Mira Chen" || u.Email != "mira@acme.dev" {
			t.Fatalf("after refused changes: %q <%s>", u.Name, u.Email)
		}
		// The proof is for the email as stored, in lowercase, whatever case was typed.
		good := f.stepUpAt(t, onDomain, p, f.keys, StepUpChangeEmail, "mira.chen@example.com").Proof
		u, err := f.svc.UpdateProfile(t.Context(), onDomain, p, ptr("Mira Okafor"), ptr(" Mira.Chen@Example.COM "), good)
		if err != nil {
			t.Fatal(err)
		}
		if u.Email != "mira.chen@example.com" || u.Name != "Mira Okafor" {
			t.Fatalf("after the change: %q <%s>", u.Name, u.Email)
		}
		if got := f.auditDetails(t, "account.email"); len(got) != 1 || got[0] != "from mira@acme.dev to mira.chen@example.com" {
			t.Errorf("the audit log's email changes = %q", got)
		}
		if got := f.auditDetails(t, "account.name"); len(got) != 1 {
			t.Errorf("the audit log's name changes = %q", got)
		}
		// A proof works once.
		p = f.current(t, onDomain, claimed)
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira@example.org"), good); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("the proof used again: %v", err)
		}
		// dockwell recover finds them by the new email, and nobody by the old.
		if _, _, err := f.svc.CreateSignInLink(t.Context(), "mira@acme.dev", "test"); !errors.Is(err, ErrNotFound) {
			t.Errorf("recover by the old email: %v, want nobody found", err)
		}
		if _, who, err := f.svc.CreateSignInLink(t.Context(), "Mira.Chen@example.com", "test"); err != nil || who.ID != p.User.ID {
			t.Errorf("recover by the new email: %s, %v", who.ID, err)
		}
		// The session goes on, as the person it belongs to.
		if p.User.Email != "mira.chen@example.com" || p.Session.ID == "" {
			t.Errorf("the session after the change is %s's", p.User.Email)
		}
		// No API token changes an email, nor learns anything of it.
		tok := f.writeToken(t, p)
		withToken := Principal{User: p.User, Token: &tok.Token}
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, withToken, nil, ptr("mira@example.org"), ""); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("a token changing the email: %v, want a session needed", err)
		}
	})
}

// An email someone else signs in with, one an open invite is for, and one that
// isn't an email are refused, and nothing changes or is on record. Taken and
// invited emails are told only after the passkey, so nobody learns who's here
// without it; a removed member's email is free.
func TestAnEmailChangeIsRefusedWhereItWouldClash(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		_, secret := f.invite(t, "sam@example.com", store.RoleViewer)
		id, cred := f.begun(t, secret, "203.0.113.5:1")
		sam, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, id, secret, []byte(cred), "")
		if err != nil {
			t.Fatal(err)
		}
		f.invite(t, "ada@example.com", store.RoleViewer)
		emailOf := func() string { return f.user(t, f.owner).Email }

		for _, bad := range []string{"", "   ", "mira", "@example.com", "mira@", "mira chen@example.com"} {
			if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr(bad), ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("email %q: %v, want it invalid", bad, err)
			}
		}
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("  "), nil, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("an empty name: %v, want it invalid", err)
		}
		// Taken, in another case: no word of it without a passkey.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("Sam@Example.com"), ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a member's email with no proof: %v, want a passkey asked for first", err)
		}
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeEmail, "sam@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("Sam@Example.com"), proof); !errors.Is(err, ErrMemberExists) {
			t.Errorf("a member's email: %v, want it taken", err)
		}
		proof = f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeEmail, "ada@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("Mira Okafor"), ptr("ada@example.com"), proof); !errors.Is(err, ErrEmailInvited) {
			t.Errorf("an invited email: %v, want it refused for the invite", err)
		}
		if u := f.user(t, f.owner); u.Email != "mira@acme.dev" || u.Name != "Mira Chen" {
			t.Errorf("after refusals: %q <%s>", u.Name, u.Email)
		}
		if n := len(f.auditEvents(t, "account.email")) + len(f.auditEvents(t, "account.name")); n != 0 {
			t.Errorf("%d changes on record after refusals", n)
		}
		// Sam keeps theirs.
		if got := f.user(t, Principal{User: sam.User}).Email; got != "sam@example.com" {
			t.Errorf("Sam's email is %s", got)
		}
		// Once Sam is removed, the email is free, as it is for an invite.
		if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
			_, err := tx.RemoveUser(t.Context(), sam.User.ID, f.owner.Actor(), f.clock.now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		proof = f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeEmail, "sam@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("sam@example.com"), proof); err != nil {
			t.Errorf("a removed member's email: %v", err)
		}
		if got := emailOf(); got != "sam@example.com" {
			t.Errorf("the owner's email is %s", got)
		}
	})
}
