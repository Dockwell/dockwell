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
	cer, err := f.svc.BeginStepUp(t.Context(), localhost, p, StepUpRevealSecret, "sec_1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Get(string(cer.Options), localhost.Site.Origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	up, err := f.svc.FinishStepUp(t.Context(), localhost, p, cer.ID, []byte(resp))
	if err != nil {
		t.Fatal(err)
	}
	return up
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
