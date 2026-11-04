// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// stepUp confirms revealing sec_1 with a's passkey, in p's session.
func (f *fixture) stepUp(t *testing.T, p Principal, a *authtest.Authenticator) StepUp {
	t.Helper()
	return f.stepUpAt(t, localhost, p, a, StepUpRevealSecret, "sec_1")
}

// stepUpAt confirms action on target with a's passkey for client's
// address, in p's session.
func (f *fixture) stepUpAt(t *testing.T, client Client, p Principal, a *authtest.Authenticator, action StepUpAction, target string) StepUp {
	t.Helper()
	cer, err := f.svc.BeginStepUp(t.Context(), client, p, action, target)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Get(string(cer.Options), client.Site.Origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	up, err := f.svc.FinishStepUp(t.Context(), client, p, cer.ID, []byte(resp))
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// proof confirms action on target with the owner's passkey for localhost,
// in p's session, and returns the proof.
func (f *fixture) proof(t *testing.T, p Principal, action StepUpAction, target string) string {
	t.Helper()
	return f.stepUpAt(t, localhost, p, f.passkeys, action, target).Proof
}

// A step-up's proof works once, within a minute, in the session that made
// it, for the action and target it confirmed; every step-up is audited,
// and no API token can make or use one.
func TestAStepUpProvesOneActionOnce(t *testing.T) { storetest.Each(t, testAStepUpProvesOneActionOnce) }

func testAStepUpProvesOneActionOnce(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	a := &authtest.Authenticator{Counter: true}
	owner := f.claim(t, localhost, a)
	p := ownerPrincipal(owner)

	up := f.stepUp(t, p, a)
	if up.Proof == "" || !up.Expires.Equal(f.clock.now().Add(StepUpTTL)) {
		t.Fatalf("a step-up gave %+v", up)
	}
	if err := f.svc.UseStepUp(p, up.Proof, StepUpRevealSecret, "sec_1"); err != nil {
		t.Fatalf("using a fresh proof: %v", err)
	}
	if err := f.svc.UseStepUp(p, up.Proof, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("using a proof twice: %v, want it refused", err)
	}

	up = f.stepUp(t, p, a)
	if err := f.svc.UseStepUp(p, up.Proof, StepUpRevealSecret, "sec_2"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a proof for one secret used for another: %v, want it refused", err)
	}
	if err := f.svc.UseStepUp(p, up.Proof, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a proof spent on the wrong secret still works: %v", err)
	}

	up = f.stepUp(t, p, a)
	f.clock.advance(StepUpTTL + time.Second)
	if err := f.svc.UseStepUp(p, up.Proof, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a stale proof: %v, want it refused", err)
	}

	up = f.stepUp(t, p, a)
	other := p
	other.Session.ID = "ses_other"
	if err := f.svc.UseStepUp(other, up.Proof, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a proof used from another session: %v, want it refused", err)
	}

	token := Principal{User: p.User, Token: &store.APIToken{ID: "tok_1", UserID: p.User.ID}}
	if _, err := f.svc.BeginStepUp(t.Context(), localhost, token, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsSession) {
		t.Errorf("a token beginning a step-up: %v, want it refused", err)
	}
	up = f.stepUp(t, p, a)
	if err := f.svc.UseStepUp(token, up.Proof, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsSession) {
		t.Errorf("a token using a proof: %v, want it refused", err)
	}
	if err := f.svc.UseStepUp(p, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("no proof: %v, want it refused", err)
	}

	events := f.auditEvents(t, "auth.step_up")
	if len(events) != 5 || events[0].Target != "sec_1" || !strings.Contains(events[0].Detail, "confirmed revealing sec_1 with passkey") {
		t.Errorf("step-up events = %+v", events)
	}
}

// A step-up takes the person's own passkey for this address, freshly
// signed for this ceremony; anything else is refused and audited.
func TestAStepUpTakesAFreshAssertion(t *testing.T) {
	storetest.Each(t, testAStepUpTakesAFreshAssertion)
}

func testAStepUpTakesAFreshAssertion(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "panel.acme.dev")
	a := &authtest.Authenticator{Counter: true}
	owner := f.claim(t, localhost, a)
	p := ownerPrincipal(owner)

	// An answer signed for another ceremony doesn't finish this one.
	first, err := f.svc.BeginStepUp(t.Context(), localhost, p, StepUpRevealSecret, "sec_1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.BeginStepUp(t.Context(), localhost, p, StepUpRevealSecret, "sec_1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Get(string(first.Options), localhost.Site.Origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FinishStepUp(t.Context(), localhost, p, second.ID, []byte(resp)); !errors.Is(err, ErrPasskeyRejected) {
		t.Errorf("finishing with another ceremony's answer: %v, want it refused", err)
	}
	if got := f.auditEvents(t, "auth.step_up_failed"); len(got) != 1 {
		t.Errorf("refused step-ups audited: %+v", got)
	}
	// A ceremony works once, and only in the session that began it.
	if _, err := f.svc.FinishStepUp(t.Context(), localhost, p, first.ID, []byte(resp)); err != nil {
		t.Errorf("finishing the ceremony the answer was for: %v", err)
	}
	if _, err := f.svc.FinishStepUp(t.Context(), localhost, p, first.ID, []byte(resp)); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing a ceremony twice: %v, want it refused", err)
	}
	third, err := f.svc.BeginStepUp(t.Context(), localhost, p, StepUpRevealSecret, "sec_1")
	if err != nil {
		t.Fatal(err)
	}
	other := p
	other.Session.ID = "ses_other"
	resp, _ = a.Get(string(third.Options), localhost.Site.Origin, nil)
	if _, err := f.svc.FinishStepUp(t.Context(), localhost, other, third.ID, []byte(resp)); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing from another session: %v, want it refused", err)
	}
	// With no passkey for the domain, there is nothing to confirm with.
	if _, err := f.svc.BeginStepUp(t.Context(), onDomain, p, StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNoPasskeyHere) {
		t.Errorf("a step-up on an address with no passkey: %v, want ErrNoPasskeyHere", err)
	}
	if _, err := f.svc.BeginStepUp(t.Context(), localhost, p, StepUpRevealSecret, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("a step-up for nothing: %v, want it refused", err)
	}
}

// Changing how someone signs in, or making a token, takes a step-up for
// that action, as revealing a secret does: so a stolen session can't add a
// passkey of its own, make codes or a token to keep, or remove the
// person's own ways in. A proof works only for the action it confirmed,
// in its session, and a refusal for another reason leaves it unspent.
func TestChangingHowYouSignInTakesAStepUp(t *testing.T) {
	storetest.Each(t, testChangingHowYouSignInTakesAStepUp)
}

func testChangingHowYouSignInTakesAStepUp(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	a := &authtest.Authenticator{Counter: true}
	p := ownerPrincipal(f.claim(t, localhost, a))
	ctx := t.Context()
	keys, err := f.svc.ListPasskeys(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	first := keys[0].ID

	// Without a proof, or with one for something else, each is refused.
	wrong := func() string { return f.proof(t, p, StepUpRevealSecret, "sec_1") }
	for name, try := range map[string]func(proof string) error{
		"adding a passkey": func(proof string) error { _, err := f.svc.BeginAddPasskey(ctx, localhost, p, proof); return err },
		"setting up codes": func(proof string) error { _, err := f.svc.BeginTOTP(ctx, p, proof); return err },
		"making a token": func(proof string) error {
			_, err := f.svc.CreateToken(ctx, p, "CI", store.ScopeReadOnly, nil, proof)
			return err
		},
	} {
		for _, proof := range []string{"", wrong()} {
			if err := try(proof); !errors.Is(err, ErrNeedsStepUp) {
				t.Errorf("%s with proof %q = %v, want ErrNeedsStepUp", name, proof, err)
			}
		}
	}
	// A proof from another session doesn't work in this one.
	other := ownerPrincipal(must(f.passkeySignIn(t, localhost, a, nil)))
	if _, err := f.svc.BeginAddPasskey(ctx, localhost, p, f.proof(t, other, StepUpAddPasskey, "")); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("adding a passkey with another session's proof = %v, want ErrNeedsStepUp", err)
	}

	// With its own proof, adding a passkey works, in the session that
	// began it alone, and the audit log says what confirmed it.
	b := &authtest.Authenticator{}
	cer, err := f.svc.BeginAddPasskey(ctx, localhost, p, f.proof(t, p, StepUpAddPasskey, ""))
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := b.Create(string(cer.Options), localhost.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FinishAddPasskey(ctx, localhost, other, cer.ID, []byte(cred), "stolen"); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing the ceremony in another session = %v, want ErrCeremonyExpired", err)
	}
	cer, _ = f.svc.BeginAddPasskey(ctx, localhost, p, f.proof(t, p, StepUpAddPasskey, ""))
	cred, _, _ = b.Create(string(cer.Options), localhost.Site.Origin)
	if _, err := f.svc.FinishAddPasskey(ctx, localhost, p, cer.ID, []byte(cred), "YubiKey"); err != nil {
		t.Fatalf("adding a passkey with a step-up: %v", err)
	}
	// The claim added the first; the step-up confirmed the second.
	if added := f.auditDetails(t, "passkey.add"); len(added) != 2 || !strings.HasSuffix(added[1], "confirmed with a passkey") {
		t.Errorf("passkey.add events = %q", added)
	}
	// Either passkey here confirms a step-up now.
	for _, with := range []*authtest.Authenticator{a, b} {
		if up := f.stepUpAt(t, localhost, p, with, StepUpCreateToken, ""); up.Proof == "" {
			t.Error("no proof")
		}
	}

	// Codes: set up, new recovery codes and turned off, each with its own.
	f.enableTOTP(t, p)
	if _, err := f.svc.RegenerateRecoveryCodes(ctx, p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("new recovery codes without a step-up = %v, want ErrNeedsStepUp", err)
	}
	if _, err := f.svc.RegenerateRecoveryCodes(ctx, p, f.proof(t, p, StepUpRecoveryCodes, "")); err != nil {
		t.Errorf("new recovery codes with a step-up: %v", err)
	}
	if err := f.svc.DisableTOTP(ctx, p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("turning codes off without a step-up = %v, want ErrNeedsStepUp", err)
	}
	if err := f.svc.DisableTOTP(ctx, p, f.proof(t, p, StepUpDisableCodes, "")); err != nil {
		t.Errorf("turning codes off with a step-up: %v", err)
	}

	// Removing a passkey takes a proof for that passkey.
	if err := f.svc.RemovePasskey(ctx, p, first, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("removing a passkey without a step-up = %v, want ErrNeedsStepUp", err)
	}
	if err := f.svc.RemovePasskey(ctx, p, first, f.proof(t, p, StepUpRemovePasskey, "pky_other")); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("removing a passkey with a proof for another = %v, want ErrNeedsStepUp", err)
	}
	if err := f.svc.RemovePasskey(ctx, p, first, f.proof(t, p, StepUpRemovePasskey, first)); err != nil {
		t.Errorf("removing a passkey with a step-up: %v", err)
	}

	// No token makes, or uses, any of it.
	token := Principal{User: p.User, Token: &store.APIToken{ID: "tok_1", UserID: p.User.ID}}
	if _, err := f.svc.BeginAddPasskey(ctx, localhost, token, f.stepUpAt(t, localhost, p, b, StepUpAddPasskey, "").Proof); !errors.Is(err, ErrNeedsSession) {
		t.Errorf("a token adding a passkey = %v, want ErrNeedsSession", err)
	}
	if _, err := f.svc.BeginStepUp(ctx, localhost, token, StepUpCreateToken, ""); !errors.Is(err, ErrNeedsSession) {
		t.Errorf("a token beginning a step-up to make a token = %v, want ErrNeedsSession", err)
	}
}

// Someone with no passkey for this address, or none they still have, adds
// one in the minutes after signing in with a code, a recovery code or a
// link from dockwell recover: that sign-in confirms it. A session made
// that way confirms nothing else, and nothing once the minutes are up,
// when signing in again is the way.
func TestASignInJustNowConfirmsAddingAPasskey(t *testing.T) {
	storetest.Each(t, testASignInJustNowConfirmsAddingAPasskey)
}

func testASignInJustNowConfirmsAddingAPasskey(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "panel.acme.dev")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	_, recovery := f.enableTOTP(t, owner)
	ctx := t.Context()

	// On the domain, where there is no passkey, after a recovery code.
	got, err := f.svc.SignInWithCode(ctx, onDomain, "mira@acme.dev", recovery[0])
	if err != nil {
		t.Fatal(err)
	}
	p := ownerPrincipal(got.SignedIn)
	if _, err := f.svc.BeginStepUp(ctx, onDomain, p, StepUpAddPasskey, ""); !errors.Is(err, ErrNeedsFreshSignIn) {
		t.Errorf("a step-up to add a passkey with none here = %v, want ErrNeedsFreshSignIn", err)
	}
	if _, err := f.svc.CreateToken(ctx, p, "CI", store.ScopeReadOnly, nil, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("making a token just after signing in with a code = %v, want ErrNeedsStepUp", err)
	}
	f.clock.advance(RecentSignIn - time.Second)
	b := &authtest.Authenticator{}
	cer, err := f.svc.BeginAddPasskey(ctx, onDomain, p, "")
	if err != nil {
		t.Fatalf("adding a passkey just after signing in with a recovery code: %v", err)
	}
	cred, _, _ := b.Create(string(cer.Options), onDomain.Site.Origin)
	if _, err := f.svc.FinishAddPasskey(ctx, onDomain, p, cer.ID, []byte(cred), "iPhone"); err != nil {
		t.Fatal(err)
	}
	if added := f.auditDetails(t, "passkey.add"); len(added) != 2 || !strings.HasSuffix(added[1], "confirmed by signing in just now") {
		t.Errorf("passkey.add events = %q", added)
	}
	f.clock.advance(2 * time.Second)
	if _, err := f.svc.BeginAddPasskey(ctx, onDomain, p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("adding a passkey %s after signing in = %v, want ErrNeedsStepUp", RecentSignIn, err)
	}

	// A passkey sign-in is no such confirmation: whoever holds the session
	// confirms with the passkey.
	again := ownerPrincipal(must(f.passkeySignIn(t, onDomain, b, nil)))
	if _, err := f.svc.BeginAddPasskey(ctx, onDomain, again, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("adding a passkey just after a passkey sign-in, without a step-up = %v, want ErrNeedsStepUp", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
