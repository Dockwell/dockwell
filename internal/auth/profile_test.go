// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/auth/authtest"
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

// A name changes with no passkey, from a session alone, tidied as sign-up tidies
// it, and the audit log says what it was and what it is. No token changes it, as
// nothing acts as who someone is (#801's review, M1).
func TestANameChangesWithNoPasskey(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		u, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("  Mira   Okafor "), nil, nil, "")
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
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("Mira Okafor"), nil, nil, ""); err != nil {
			t.Fatal(err)
		}
		if got := f.auditDetails(t, "account.name"); len(got) != 1 {
			t.Errorf("%d name changes on record after saving the same name", len(got))
		}
		tok := f.writeToken(t, f.owner)
		withToken := Principal{User: f.user(t, f.owner), Token: &tok.Token}
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, withToken, ptr("Mira"), nil, nil, ""); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("a read-write token changing the name: %v, want a session needed", err)
		}
		if got := f.user(t, f.owner).Name; got != "Mira Okafor" || len(f.auditEvents(t, "account.name")) != 1 {
			t.Errorf("after a token's try the name is %q", got)
		}
	})
}

// A person's own time zone changes with no passkey, from a session alone, clears
// back to empty, rejects anything that isn't an IANA zone, and is audited (#834).
func TestATimeZoneChangesWithNoPasskey(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		if got := f.user(t, f.owner).TimeZone; got != "" {
			t.Fatalf("initial time zone = %q, want empty", got)
		}
		u, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, nil, ptr("  Asia/Tokyo  "), "")
		if err != nil {
			t.Fatal(err)
		}
		if u.TimeZone != "Asia/Tokyo" || f.user(t, f.owner).TimeZone != "Asia/Tokyo" {
			t.Fatalf("after setting time zone: %q, stored %q", u.TimeZone, f.user(t, f.owner).TimeZone)
		}
		if got := f.auditDetails(t, ActionAccountTimeZone); len(got) != 1 || got[0] != `from "" to "Asia/Tokyo"` {
			t.Errorf("audit details = %q", got)
		}
		// Saving the same zone again writes no second audit entry.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, nil, ptr("Asia/Tokyo"), ""); err != nil {
			t.Fatal(err)
		}
		if got := f.auditDetails(t, ActionAccountTimeZone); len(got) != 1 {
			t.Errorf("%d audit entries after saving the same time zone", len(got))
		}
		// Omitting time zone leaves it alone when changing name.
		u, err = f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("Mira Okafor"), nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if u.TimeZone != "Asia/Tokyo" {
			t.Errorf("omitting time zone changed it to %q", u.TimeZone)
		}
		// Invalid IANA names, Local, and filesystem paths are refused.
		for _, bad := range []string{"Local", "local", "/UTC", "/etc/localtime", "Mars/Olympus"} {
			if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, nil, ptr(bad), ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("time zone %q: %v, want ErrInvalid", bad, err)
			}
		}
		// Paired with an unproved email change, neither takes effect.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("mira.chen@example.com"), ptr("UTC"), ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("email + time zone with no proof: %v, want ErrNeedsStepUp", err)
		}
		if got := f.user(t, f.owner).TimeZone; got != "Asia/Tokyo" {
			t.Errorf("after refused email + time zone, stored %q, want Asia/Tokyo", got)
		}
		// Empty clears back to this browser's.
		u, err = f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, nil, ptr("   "), "")
		if err != nil {
			t.Fatal(err)
		}
		if u.TimeZone != "" || f.user(t, f.owner).TimeZone != "" {
			t.Fatalf("after clearing time zone: %q, stored %q", u.TimeZone, f.user(t, f.owner).TimeZone)
		}
		if got := f.auditDetails(t, ActionAccountTimeZone); len(got) != 2 || got[1] != `from "Asia/Tokyo" to ""` {
			t.Errorf("audit details after clear = %q", got)
		}
		// No API token can change it.
		tok := f.writeToken(t, f.owner)
		withToken := Principal{User: f.user(t, f.owner), Token: &tok.Token}
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, withToken, nil, nil, ptr("UTC"), ""); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("token changing time zone: %v, want ErrNeedsSession", err)
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
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira.chen@example.com"), nil, ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Fatalf("a new email within the hour, with no proof: %v, want a passkey asked for", err)
		}
		wrong := f.stepUpAt(t, onDomain, p, f.keys, StepUpChangeEmail, "lee@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira.chen@example.com"), nil, wrong); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a proof for another email: %v, want it refused", err)
		}
		other := f.stepUpAt(t, onDomain, p, f.keys, StepUpRevealSecret, "mira.chen@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira.chen@example.com"), nil, other); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a proof for another action: %v, want it refused", err)
		}
		// Names and emails mixed in one save: neither changes when the email is refused.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, ptr("Mira Okafor"), ptr("mira.chen@example.com"), nil, ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Fatalf("a new name and email with no proof: %v", err)
		}
		if u := f.user(t, p); u.Name != "Mira Chen" || u.Email != "mira@acme.dev" {
			t.Fatalf("after refused changes: %q <%s>", u.Name, u.Email)
		}
		// The proof is for the email as stored, in lowercase, whatever case was typed.
		good := f.stepUpAt(t, onDomain, p, f.keys, StepUpChangeEmail, "mira.chen@example.com").Proof
		u, err := f.svc.UpdateProfile(t.Context(), onDomain, p, ptr("Mira Okafor"), ptr(" Mira.Chen@Example.COM "), nil, good)
		if err != nil {
			t.Fatal(err)
		}
		if u.Email != "mira.chen@example.com" || u.Name != "Mira Okafor" {
			t.Fatalf("after the change: %q <%s>", u.Name, u.Email)
		}
		if got := f.auditDetails(t, "account.email"); len(got) != 1 || got[0] != `from "mira@acme.dev" to "mira.chen@example.com"` {
			t.Errorf("the audit log's email changes = %q", got)
		}
		if got := f.auditDetails(t, "account.name"); len(got) != 1 {
			t.Errorf("the audit log's name changes = %q", got)
		}
		// A proof works once.
		p = f.current(t, onDomain, claimed)
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr("mira@example.org"), nil, good); !errors.Is(err, ErrNeedsStepUp) {
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
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, withToken, nil, ptr("mira@example.org"), nil, ""); !errors.Is(err, ErrNeedsSession) {
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
			if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr(bad), nil, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("email %q: %v, want it invalid", bad, err)
			}
		}
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("  "), nil, nil, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("an empty name: %v, want it invalid", err)
		}
		// Taken, in another case: no word of it without a passkey.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("Sam@Example.com"), nil, ""); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("a member's email with no proof: %v, want a passkey asked for first", err)
		}
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeEmail, "sam@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("Sam@Example.com"), nil, proof); !errors.Is(err, ErrMemberExists) {
			t.Errorf("a member's email: %v, want it taken", err)
		}
		proof = f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpChangeEmail, "ada@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, ptr("Mira Okafor"), ptr("ada@example.com"), nil, proof); !errors.Is(err, ErrEmailInvited) {
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
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, f.owner, nil, ptr("sam@example.com"), nil, proof); err != nil {
			t.Errorf("a removed member's email: %v", err)
		}
		if got := emailOf(); got != "sam@example.com" {
			t.Errorf("the owner's email is %s", got)
		}
	})
}

// joined accepts an invite to email as name, with a passkey of their own, and
// returns them as their next request has them, with the passkey.
func (f *invited) joined(t *testing.T, email, name string) (Principal, *authtest.Authenticator) {
	t.Helper()
	_, secret := f.invite(t, email, store.RoleViewer)
	cer, err := f.svc.BeginAcceptInvite(t.Context(), onDomain, secret, name)
	if err != nil {
		t.Fatal(err)
	}
	keys := &authtest.Authenticator{Counter: true}
	cred, _, err := keys.Create(string(cer.Options), onDomain.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.svc.FinishAcceptInvite(t.Context(), onDomain, cer.ID, secret, []byte(cred), "")
	if err != nil {
		t.Fatal(err)
	}
	return f.current(t, onDomain, in), keys
}

// A name is kept without what prints as nothing or turns text around, and in
// NFKC; and a name or an email that reads the same as another person's here is
// refused, by case and by look, as one would pass for them in Activity, Members
// and an approval. A name chosen at an invite is held to it too (#801's review, M1).
func TestACopyOfSomeoneElsesNameOrEmailIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		sam, samKeys := f.joined(t, "sam@example.com", "Sam Rivera")
		f.invite(t, "ada@example.com", store.RoleViewer)

		// What prints as nothing goes, and full-width letters read as themselves.
		u, err := f.svc.UpdateProfile(t.Context(), onDomain, sam, ptr("\u202eSam\u200b Rivera-Ｌｅｅ\u200d\ufeff"), nil, nil, "")
		if err != nil || u.Name != "Sam Rivera-Lee" {
			t.Fatalf("a name with invisible and full-width letters: %q, %v", u.Name, err)
		}
		// Someone else's name, by case, by spaces and by look, is refused, and so is
		// one hidden behind what prints as nothing.
		for _, copied := range []string{"Mira Chen", "mira chen", "MIRA  CHEN", "Mіra Chen", "Μira Chen", "Mira Chen\u200d", "\u202dMira Chen", "Mira Chén"} {
			if _, err := f.svc.UpdateProfile(t.Context(), onDomain, sam, ptr(copied), nil, nil, ""); !errors.Is(err, ErrNameTaken) {
				t.Errorf("the name %q: %v, want it taken", copied, err)
			}
		}
		// Their own name, in another case, is theirs to keep.
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, sam, ptr("sam rivera-lee"), nil, nil, ""); err != nil {
			t.Errorf("their own name in lowercase: %v", err)
		}

		// An email that only reads as the owner's, with a Cyrillic і, as the review found.
		for _, copied := range []string{"mіra@acme.dev", "MIRA@ACME.DEV\u200b", "mira@acrne.dev", "ѕam@example.com"} {
			if copied == "MIRA@ACME.DEV\u200b" {
				// What prints as nothing isn't an email's to have, nor a passkey's to confirm.
				if _, err := f.svc.UpdateProfile(t.Context(), onDomain, sam, nil, ptr(copied), nil, ""); !errors.Is(err, ErrInvalid) {
					t.Errorf("the email %q: %v, want it invalid", copied, err)
				}
				if _, err := f.svc.BeginStepUp(t.Context(), onDomain, sam, StepUpChangeEmail, copied); !errors.Is(err, ErrInvalid) {
					t.Errorf("a passkey for %q: %v, want it invalid", copied, err)
				}
				continue
			}
			proof := f.stepUpAt(t, onDomain, sam, samKeys, StepUpChangeEmail, copied).Proof
			_, err := f.svc.UpdateProfile(t.Context(), onDomain, sam, nil, ptr(copied), nil, proof)
			if copied == "ѕam@example.com" {
				// Their own, by look, is theirs.
				if err != nil {
					t.Errorf("an email that reads as their own: %v", err)
				}
				continue
			}
			if !errors.Is(err, ErrEmailLookalike) {
				t.Errorf("the email %q: %v, want it refused as reading as the owner's", copied, err)
			}
		}
		// One that reads as an open invite's.
		proof := f.stepUpAt(t, onDomain, sam, samKeys, StepUpChangeEmail, "аda@example.com").Proof
		if _, err := f.svc.UpdateProfile(t.Context(), onDomain, sam, nil, ptr("аda@example.com"), nil, proof); !errors.Is(err, ErrEmailLookalike) {
			t.Errorf("an email that reads as an invite's: %v, want it refused", err)
		}
		if got := f.user(t, f.owner); got.Name != "Mira Chen" || got.Email != "mira@acme.dev" {
			t.Errorf("the owner is %q <%s>", got.Name, got.Email)
		}

		// A name chosen at an invite that reads as someone's here.
		_, secret := f.invite(t, "lee@example.com", store.RoleViewer)
		for _, copied := range []string{"Mira Chen", "mіra chen"} {
			if _, err := f.svc.BeginAcceptInvite(t.Context(), onDomain, secret, copied); !errors.Is(err, ErrNameTaken) {
				t.Errorf("joining as %q: %v, want it taken", copied, err)
			}
		}
		if _, err := f.svc.BeginAcceptInvite(t.Context(), onDomain, secret, "Lee Park"); err != nil {
			t.Errorf("joining as a name of their own: %v", err)
		}
	})
}

// The new email is written as the server keeps emails, in the step-up's target
// and in the change, whatever the panel's lowercase made of it: İ and Σ lowercase
// one way in Go and another in a browser (#801's review, L1).
func TestAnEmailIsConfirmedAsTheServerWritesIt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		for _, tt := range []struct{ typed, kept string }{
			{"İnci@Example.com", "inci@example.com"},
			{"ΑΣ@example.com", "ασ@example.com"},
		} {
			p := f.current(t, onDomain, SignedIn{Token: f.ownerSecret})
			// The browser's own lowercase is no other email: its proof is for the same one.
			proof := f.stepUpAt(t, onDomain, p, f.keys, StepUpChangeEmail, tt.typed).Proof
			u, err := f.svc.UpdateProfile(t.Context(), onDomain, p, nil, ptr(tt.typed), nil, proof)
			if err != nil || u.Email != tt.kept {
				t.Errorf("%s: kept as %q, %v; want %q", tt.typed, u.Email, err, tt.kept)
			}
		}
		// Recover and code sign-in find it by what the person types.
		if _, who, err := f.svc.CreateSignInLink(t.Context(), "ΑΣ@example.com", "test"); err != nil || who.ID != f.owner.User.ID {
			t.Errorf("recover by the typed email: %v", err)
		}
	})
}
