// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"fmt"
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

// Every action a step-up confirms checks everything else first: a
// refusal for another reason comes before it asks for a proof, and leaves
// a proof it was given unspent, so the same one works once the reason is
// gone. Adding a passkey and setting up codes have no other reason to
// refuse; revealing has, in the API (internal/control).
func TestARefusalForAnotherReasonLeavesTheProof(t *testing.T) {
	storetest.Each(t, testARefusalForAnotherReasonLeavesTheProof)
}

func testARefusalForAnotherReasonLeavesTheProof(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	p := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	ctx := t.Context()
	keys, err := f.svc.ListPasskeys(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	only := keys[0].ID

	// Codes are off: turning them off and making recovery codes refuse
	// that first, with no proof, and with one, which works once codes do.
	disable, regenerate := f.proof(t, p, StepUpDisableCodes, ""), f.proof(t, p, StepUpRecoveryCodes, "")
	for _, proof := range []string{"", disable} {
		if err := f.svc.DisableTOTP(ctx, p, proof); !errors.Is(err, ErrNoTOTP) {
			t.Errorf("turning codes off while they're off, proof %t = %v, want ErrNoTOTP", proof != "", err)
		}
	}
	for _, proof := range []string{"", regenerate} {
		if _, err := f.svc.RegenerateRecoveryCodes(ctx, p, proof); !errors.Is(err, ErrNoTOTP) {
			t.Errorf("new recovery codes without codes, proof %t = %v, want ErrNoTOTP", proof != "", err)
		}
	}
	// The only passkey can't go, with or without a proof.
	remove := f.proof(t, p, StepUpRemovePasskey, only)
	for _, proof := range []string{"", remove} {
		if err := f.svc.RemovePasskey(ctx, p, only, proof); !errors.Is(err, ErrLastWayIn) {
			t.Errorf("removing the only passkey, proof %t = %v, want ErrLastWayIn", proof != "", err)
		}
	}
	// A token with no name, or one too many.
	create := f.proof(t, p, StepUpCreateToken, "")
	for _, proof := range []string{"", create} {
		if _, err := f.svc.CreateToken(ctx, p, " ", store.ScopeReadOnly, nil, proof); !errors.Is(err, ErrInvalid) {
			t.Errorf("a token with no name, proof %t = %v, want ErrInvalid", proof != "", err)
		}
	}
	var last string
	_ = f.db.Update(ctx, func(tx *store.Tx) error {
		for i := range maxTokens {
			tok := store.APIToken{ID: fmt.Sprintf("tok_%03d", i), TokenHash: []byte{byte(i), byte(i >> 8)}, UserID: p.User.ID, Name: "CI",
				Hint: "dwt_…", Scope: store.ScopeReadOnly, CreatedAt: f.clock.now()}
			if err := tx.CreateAPIToken(ctx, tok); err != nil {
				t.Fatal(err)
			}
			last = tok.ID
		}
		return nil
	})
	for _, proof := range []string{"", create} {
		if _, err := f.svc.CreateToken(ctx, p, "CI", store.ScopeReadOnly, nil, proof); !errors.Is(err, ErrInvalid) ||
			!strings.Contains(err.Error(), "the most you can have") {
			t.Errorf("one token too many, proof %t = %v, want it refused for the most tokens", proof != "", err)
		}
	}

	// Each reason gone, each proof still works.
	if err := f.svc.RevokeToken(ctx, p, last); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateToken(ctx, p, "CI", store.ScopeReadOnly, nil, create); err != nil {
		t.Errorf("making a token with the proof a refusal left: %v", err)
	}
	f.enableTOTP(t, p)
	if _, err := f.svc.RegenerateRecoveryCodes(ctx, p, regenerate); err != nil {
		t.Errorf("new recovery codes with the proof a refusal left: %v", err)
	}
	if err := f.svc.RemovePasskey(ctx, p, only, remove); err != nil {
		t.Errorf("removing a passkey with the proof a refusal left: %v", err)
	}
	// With no passkey left, codes can't go: that refusal comes first too.
	if err := f.svc.DisableTOTP(ctx, p, disable); !errors.Is(err, ErrLastWayIn) {
		t.Errorf("turning off the last way in = %v, want ErrLastWayIn", err)
	}
}

// Someone with no passkey for this address, or none they still have, adds
// one in the minutes after signing in some other way: that sign-in
// confirms it, once.
//   - A link from dockwell recover, and a recovery code, confirm it even
//     with a passkey here, which they may have lost; a recovery code's is
//     a security event of its own.
//   - An authenticator code confirms it only with no usable passkey here,
//     as a code is phishable.
//   - A passkey sign-in confirms nothing: the passkey is at hand.
//
// A session made that way confirms nothing else, nothing once a passkey is
// added, and nothing once the minutes are up, checked again as the
// passkey is stored.
func TestASignInJustNowConfirmsAddingAPasskey(t *testing.T) {
	storetest.Each(t, testASignInJustNowConfirmsAddingAPasskey)
}

func testASignInJustNowConfirmsAddingAPasskey(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "panel.acme.dev")
	owner := ownerPrincipal(f.claim(t, localhost, &authtest.Authenticator{}))
	secret, recovery := f.enableTOTP(t, owner)
	ctx := t.Context()
	add := func(client Client, p Principal) (Passkey, error) {
		t.Helper()
		cer, err := f.svc.BeginAddPasskey(ctx, client, p, "")
		if err != nil {
			return Passkey{}, err
		}
		cred, _, err := (&authtest.Authenticator{}).Create(string(cer.Options), client.Site.Origin)
		if err != nil {
			t.Fatal(err)
		}
		return f.svc.FinishAddPasskey(ctx, client, p, cer.ID, []byte(cred), "new")
	}
	codeSignIn := func(client Client) Principal {
		t.Helper()
		f.clock.advance(totpPeriod * time.Second)
		return ownerPrincipal(must(f.svc.SignInWithCode(ctx, client, "mira@acme.dev", totpCode(secret, totpStep(f.clock.now())))).SignedIn)
	}

	// An authenticator code, with the owner's passkey here: no.
	p := codeSignIn(localhost)
	if _, err := add(localhost, p); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("adding a passkey after a code sign-in, with a passkey here = %v, want ErrNeedsStepUp", err)
	}
	if _, err := f.svc.CreateToken(ctx, p, "CI", store.ScopeReadOnly, nil, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("making a token just after signing in with a code = %v, want ErrNeedsStepUp", err)
	}

	// On the domain, where there is none: once, and checked at both ends.
	p = codeSignIn(onDomain)
	if _, err := f.svc.BeginStepUp(ctx, onDomain, p, StepUpAddPasskey, ""); !errors.Is(err, ErrNeedsFreshSignIn) {
		t.Errorf("a step-up to add a passkey with none here = %v, want ErrNeedsFreshSignIn", err)
	}
	f.clock.advance(RecentSignIn - 2*time.Second)
	late, err := f.svc.BeginAddPasskey(ctx, onDomain, p, "")
	if err != nil {
		t.Fatalf("adding a passkey just after signing in with a code, with none here: %v", err)
	}
	first, err := f.svc.BeginAddPasskey(ctx, onDomain, p, "")
	if err != nil {
		t.Fatal(err)
	}
	cred, _, _ := (&authtest.Authenticator{}).Create(string(first.Options), onDomain.Site.Origin)
	if _, err := f.svc.FinishAddPasskey(ctx, onDomain, p, first.ID, []byte(cred), "iPhone"); err != nil {
		t.Fatal(err)
	}
	if added := f.auditDetails(t, "passkey.add"); !strings.HasSuffix(added[len(added)-1], "confirmed by signing in with an authenticator code, with no passkey for this address") {
		t.Errorf("passkey.add events = %q", added)
	}
	// A second ceremony begun meanwhile, finished: the first used it.
	cred, _, _ = (&authtest.Authenticator{}).Create(string(late.Options), onDomain.Site.Origin)
	if _, err := f.svc.FinishAddPasskey(ctx, onDomain, p, late.ID, []byte(cred), "another"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a second passkey from one sign-in = %v, want ErrNeedsStepUp", err)
	}

	// A ceremony begun in the minutes, finished after them.
	p = codeSignIn(onDomain)
	onIt := f.storedOn(t, owner.User.ID, "panel.acme.dev")
	if err := f.svc.RemovePasskey(ctx, owner, onIt, f.proof(t, owner, StepUpRemovePasskey, onIt)); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(RecentSignIn - time.Second)
	cer, err := f.svc.BeginAddPasskey(ctx, onDomain, p, "")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Second)
	cred, _, _ = (&authtest.Authenticator{}).Create(string(cer.Options), onDomain.Site.Origin)
	if _, err := f.svc.FinishAddPasskey(ctx, onDomain, p, cer.ID, []byte(cred), "late"); !errors.Is(err, ErrNeedsFreshSignIn) {
		t.Errorf("finishing a passkey %s after signing in = %v, want ErrNeedsFreshSignIn", RecentSignIn+time.Second, err)
	}

	// A recovery code, with the owner's passkey here: yes, once, on the
	// timeline as a security event naming the passkey.
	got, err := f.svc.SignInWithCode(ctx, localhost, "mira@acme.dev", recovery[0])
	if err != nil {
		t.Fatal(err)
	}
	p = ownerPrincipal(got.SignedIn)
	pk, err := add(localhost, p)
	if err != nil {
		t.Fatalf("adding a passkey after a recovery code: %v", err)
	}
	if ev := f.auditEvents(t, "passkey.add_with_recovery_code"); len(ev) != 1 || ev[0].Target != pk.ID ||
		!strings.HasSuffix(ev[0].Detail, "confirmed by signing in with a recovery code") {
		t.Errorf("passkey.add_with_recovery_code events = %+v, want one for %s", ev, pk.ID)
	}
	f.clock.advance(time.Second)
	if _, err := add(localhost, p); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a second passkey after one recovery code = %v, want ErrNeedsStepUp", err)
	}

	// A link from dockwell recover, with passkeys here: yes, once.
	link, _, err := f.svc.CreateSignInLink(ctx, "", "cli:root@hz-1")
	if err != nil {
		t.Fatal(err)
	}
	signedIn, err := f.svc.SignInWithLink(ctx, localhost, link.Token)
	if err != nil {
		t.Fatal(err)
	}
	p = Principal{User: signedIn.User, Session: signedIn.Session}
	if _, err := add(localhost, p); err != nil {
		t.Errorf("adding a passkey after a link from dockwell recover: %v", err)
	}
	f.clock.advance(time.Second)
	if _, err := add(localhost, p); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a second passkey after one link = %v, want ErrNeedsStepUp", err)
	}

	// A passkey sign-in is no such confirmation: whoever holds the session
	// confirms with the passkey.
	again := ownerPrincipal(must(f.passkeySignIn(t, localhost, f.passkeys, nil)))
	if _, err := add(localhost, again); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("adding a passkey just after a passkey sign-in, without a step-up = %v, want ErrNeedsStepUp", err)
	}
}

// storedOn is the ID of a user's passkey for rpID.
func (f *fixture) storedOn(t *testing.T, userID, rpID string) string {
	t.Helper()
	var id string
	_ = f.db.View(t.Context(), func(tx *store.Tx) error {
		all, err := tx.ListPasskeys(t.Context(), userID)
		if err != nil {
			t.Fatal(err)
		}
		for i := range all {
			if all[i].RPID == rpID {
				id = all[i].ID
			}
		}
		return nil
	})
	if id == "" {
		t.Fatalf("the owner has no passkey for %s", rpID)
	}
	return id
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
