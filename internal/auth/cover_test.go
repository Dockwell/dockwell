// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// current is the principal behind s as a request has it: its user and session as
// the store has them now.
func (f *fixture) current(t *testing.T, client Client, s SignedIn) Principal {
	t.Helper()
	p, err := f.svc.Authenticate(t.Context(), client, s.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// choose has s's person choose policy, with a passkey just now, and returns the
// principal as the next request has it.
func (f *fixture) choose(t *testing.T, client Client, s SignedIn, policy StepUpPolicy) Principal {
	t.Helper()
	p := f.current(t, client, s)
	proof := f.stepUpAt(t, client, p, f.passkeys, StepUpSetPolicy, "").Proof
	if _, err := f.svc.SetStepUpPolicy(t.Context(), client, p, policy, proof); err != nil {
		t.Fatal(err)
	}
	return f.current(t, client, s)
}

// setFloor stores the floor as the owner's setting would.
func (f *fixture) setFloor(t *testing.T, floor string) {
	t.Helper()
	if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.PutSetting(t.Context(), store.SettingStepUpFloor, []byte(floor))
	}); err != nil {
		t.Fatal(err)
	}
}

// codeSession signs the owner in again with an authenticator code from secret.
func (f *fixture) codeSession(t *testing.T, client Client, secret []byte) SignedIn {
	t.Helper()
	f.clock.advance(totpPeriod * time.Second)
	got, err := f.svc.SignInWithCode(t.Context(), client, "mira@acme.dev", totpCode(secret, totpStep(f.clock.now())))
	if err != nil {
		t.Fatal(err)
	}
	return got.SignedIn
}

// Each time, the default, asks for a proof for every action, however fresh the
// session's passkey, as before.
func TestEachTimeAsksForEveryAction(t *testing.T) { storetest.Each(t, testEachTimeAsksForEveryAction) }

func testEachTimeAsksForEveryAction(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	p := f.current(t, localhost, owner)
	if p.User.StepUpPolicy != store.StepUpEachTime {
		t.Fatalf("a new person's choice = %q, want each time", p.User.StepUpPolicy)
	}
	if p.Session.PasskeyAt == nil || !p.Session.PasskeyAt.Equal(f.clock.now()) {
		t.Errorf("claiming left the session's passkey at %v, want now", p.Session.PasskeyAt)
	}
	for _, action := range []StepUpAction{StepUpRevealSecret, StepUpOpenTerminal, StepUpApplyRootChange, StepUpCreateToken} {
		if err := f.svc.UseStepUp(t.Context(), localhost, p, "", action, "x_1"); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("%s with no proof, each time: %v, want needs a step-up", action, err)
		}
	}
	got, err := f.svc.GetStepUpPolicy(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy != AskEachTime || got.Floor != AskAtSignIn || got.Effective != AskEachTime || got.Cover != nil {
		t.Errorf("GetStepUpPolicy = %+v, want each time, a floor that allows all, and no cover", got)
	}
	if n := len(f.auditEvents(t, "auth.step_up_hour")) + len(f.auditEvents(t, "auth.step_up_sign_in")); n != 0 {
		t.Errorf("%d cover events, want none", n)
	}
}

// Once an hour, a passkey covers its session for the hour after it, and use doesn't
// stretch it; the next passkey opens another. Each cover is audited, with the action,
// its target, the passkey's time and the address.
func TestOnceAnHourCoversThePasskeysHour(t *testing.T) {
	storetest.Each(t, testOnceAnHourCoversThePasskeysHour)
}

func testOnceAnHourCoversThePasskeysHour(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	f.clock.advance(2 * time.Hour) // the claim's passkey is past its hour
	confirmedAt := f.clock.now()
	p := f.choose(t, localhost, owner, AskHourly)
	if !p.Session.PasskeyAt.Equal(confirmedAt) {
		t.Fatalf("the step-up that chose left the session's passkey at %v, want %v", p.Session.PasskeyAt, confirmedAt)
	}

	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
		t.Fatalf("revealing within the hour, with no proof: %v", err)
	}
	if _, err := f.svc.CreateToken(t.Context(), localhost, p, "CI", store.ScopeReadOnly, nil, nil, ""); err != nil {
		t.Fatalf("making a token within the hour, with no proof: %v", err)
	}
	events := f.auditEvents(t, "auth.step_up_hour")
	if len(events) != 2 || events[0].Target != "sec_1" || events[1].Target != p.User.ID {
		t.Fatalf("cover events = %+v", events)
	}
	want := "confirmed revealing sec_1 within an hour of the passkey at " + confirmedAt.Format(time.RFC3339) + ", session " + p.Session.ID + ", from 127.0.0.1"
	if events[0].Detail != want || events[0].Actor != "user:"+p.User.ID {
		t.Errorf("cover event = %q by %s, want %q", events[0].Detail, events[0].Actor, want)
	}

	got, err := f.svc.GetStepUpPolicy(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective != AskHourly || got.Cover == nil || !got.Cover.Since.Equal(confirmedAt) || !got.Cover.Until.Equal(confirmedAt.Add(StepUpHour)) || got.Cover.AtSignIn {
		t.Errorf("GetStepUpPolicy = %+v, cover %+v, want the hour from %v", got, got.Cover, confirmedAt)
	}

	f.clock.advance(StepUpHour - time.Minute)
	p = f.current(t, localhost, owner)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpOpenTerminal, "srv_1"); err != nil {
		t.Fatalf("a minute before the hour ends: %v", err)
	}
	f.clock.advance(time.Minute)
	p = f.current(t, localhost, owner)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpOpenTerminal, "srv_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Fatalf("once the hour has ended, though it was used in it: %v, want needs a step-up", err)
	}
	if _, err := f.svc.HoldStepUp(t.Context(), localhost, p, "", StepUpApplyRootChange, "chg_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("holding once the hour has ended: %v, want needs a step-up", err)
	}
	if got, _ := f.svc.GetStepUpPolicy(t.Context(), p); got.Cover != nil {
		t.Errorf("a cover %+v past the hour", got.Cover)
	}

	// A proof works as ever, and opens the next hour.
	if err := f.svc.UseStepUp(t.Context(), localhost, p, f.proof(t, p, StepUpOpenTerminal, "srv_1"), StepUpOpenTerminal, "srv_1"); err != nil {
		t.Fatal(err)
	}
	p = f.current(t, localhost, owner)
	back, err := f.svc.HoldStepUp(t.Context(), localhost, p, "", StepUpApplyRootChange, "chg_1")
	if err != nil {
		t.Fatalf("the hour a step-up opens: %v", err)
	}
	back() // nothing to give back

	// The hour is its session's: another session of the same person has its own.
	other := f.current(t, localhost, owner)
	other.Session.ID, other.Session.PasskeyAt = "ses_other", nil
	if err := f.svc.UseStepUp(t.Context(), localhost, other, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a session with no passkey of its own: %v, want needs a step-up", err)
	}
}

// Only when I sign in, a sign-in with a passkey or an authenticator code covers the
// session for its life. One with a recovery code covers nothing until a passkey in
// it opens the hour.
func TestOnlyWhenISignInCoversTheSession(t *testing.T) {
	storetest.Each(t, testOnlyWhenISignInCoversTheSession)
}

func testOnlyWhenISignInCoversTheSession(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "panel.acme.dev")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, codes := f.enableTOTP(t, ownerPrincipal(owner))
	f.choose(t, localhost, owner, AskAtSignIn)

	f.clock.advance(5 * time.Hour)
	p := f.current(t, localhost, owner)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
		t.Fatalf("five hours after claiming: %v", err)
	}
	events := f.auditEvents(t, "auth.step_up_sign_in")
	if len(events) != 1 || !strings.HasPrefix(events[0].Detail, "confirmed revealing sec_1 by signing in by claiming the install at "+owner.Session.CreatedAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("cover events = %+v", events)
	}
	got, err := f.svc.GetStepUpPolicy(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cover == nil || !got.Cover.AtSignIn || !got.Cover.Until.Equal(SessionEnd(p.Session)) {
		t.Errorf("cover = %+v, want the sign-in's, to the session's end", got.Cover)
	}

	coded := f.codeSession(t, onDomain, secret)
	p = f.current(t, onDomain, coded)
	if p.Session.PasskeyAt != nil {
		t.Errorf("a code sign-in left the session's passkey at %v, want none", p.Session.PasskeyAt)
	}
	if err := f.svc.UseStepUp(t.Context(), onDomain, p, "", StepUpOpenService, "svc_1"); err != nil {
		t.Fatalf("a session begun with an authenticator code: %v", err)
	}
	if d := f.auditDetails(t, "auth.step_up_sign_in"); len(d) != 2 || !strings.Contains(d[1], "by signing in with an authenticator code") || !strings.HasSuffix(d[1], "from 198.51.100.23") {
		t.Errorf("cover details = %q", d)
	}

	recovered, err := f.svc.SignInWithCode(t.Context(), localhost, "mira@acme.dev", codes[0])
	if err != nil {
		t.Fatal(err)
	}
	p = f.current(t, localhost, recovered.SignedIn)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Fatalf("a session begun with a recovery code: %v, want needs a step-up", err)
	}
	f.proof(t, p, StepUpRevealSecret, "sec_2")
	p = f.current(t, localhost, recovered.SignedIn)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
		t.Fatalf("a recovery code's session after a passkey in it: %v", err)
	}
	if n := len(f.auditEvents(t, "auth.step_up_hour")); n != 1 {
		t.Errorf("%d hour covers, want the recovery session's one", n)
	}
}

// Whatever anyone chose, how someone signs in, the choice and the floor, and
// approving someone else's change ask for a proof each time.
func TestSomeActionsAlwaysAsk(t *testing.T) { storetest.Each(t, testSomeActionsAlwaysAsk) }

func testSomeActionsAlwaysAsk(t *testing.T, backend store.Backend) {
	always := []StepUpAction{
		StepUpAddPasskey, StepUpRemovePasskey, StepUpSetUpCodes, StepUpDisableCodes, StepUpRecoveryCodes,
		StepUpSetPolicy, StepUpSetFloor, StepUpApproveChange,
	}
	if got := slices.Sorted(maps.Keys(alwaysAsks)); !slices.Equal(got, slices.Sorted(slices.Values(always))) {
		t.Fatalf("what always asks = %v, want %v", got, always)
	}
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	p := f.choose(t, localhost, owner, AskAtSignIn)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
		t.Fatalf("the session is covered for what doesn't always ask: %v", err)
	}
	for _, action := range always {
		if err := f.svc.UseStepUp(t.Context(), localhost, p, "", action, "x_1"); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("%s with no proof, in a covered session: %v, want needs a step-up", action, err)
		}
		if _, err := f.svc.HoldStepUp(t.Context(), localhost, p, "", action, "x_1"); !errors.Is(err, ErrNeedsStepUp) {
			t.Errorf("holding %s with no proof: %v, want needs a step-up", action, err)
		}
	}
	// Through the methods themselves, too.
	if _, err := f.svc.BeginTOTP(t.Context(), p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("setting up codes with no proof: %v", err)
	}
	if _, err := f.svc.BeginAddPasskey(t.Context(), localhost, p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("adding a passkey with no proof: %v", err)
	}
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, AskHourly, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("changing the choice with no proof: %v", err)
	}
	if got := f.user(t, p).StepUpPolicy; got != store.StepUpAtSignIn {
		t.Errorf("the choice is %q after a refused change", got)
	}
}

// The floor is the least anyone may choose: what applies is the stricter, a choice
// below it is refused before a passkey is asked for, and one it overtook reads as it.
func TestTheFloorIsTheLeastAnyoneMayChoose(t *testing.T) {
	storetest.Each(t, testTheFloorIsTheLeastAnyoneMayChoose)
}

func testTheFloorIsTheLeastAnyoneMayChoose(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, _ := f.enableTOTP(t, ownerPrincipal(owner))
	f.choose(t, localhost, owner, AskAtSignIn)
	coded := f.codeSession(t, localhost, secret)
	p := f.current(t, localhost, coded)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
		t.Fatalf("a code's session with no floor: %v", err)
	}

	f.setFloor(t, store.StepUpHourly)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a code's session under a floor of once an hour: %v, want needs a step-up", err)
	}
	got, err := f.svc.GetStepUpPolicy(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy != AskAtSignIn || got.Floor != AskHourly || got.Effective != AskHourly || got.Cover != nil {
		t.Errorf("GetStepUpPolicy = %+v, want only when signing in, read as once an hour, with no cover", got)
	}
	f.proof(t, p, StepUpRevealSecret, "sec_2")
	p = f.current(t, localhost, coded)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
		t.Errorf("within the hour of a passkey, under a floor of once an hour: %v", err)
	}
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, AskEachTime, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("choosing each time with no proof: %v, want needs a step-up", err)
	}
	choosing := f.proof(t, p, StepUpSetPolicy, "")
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, AskAtSignIn, choosing); err != nil {
		t.Errorf("choosing what's chosen already: %v, want nothing to change", err)
	}
	p.User.StepUpPolicy = store.StepUpHourly
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, AskAtSignIn, choosing); !errors.Is(err, ErrBelowFloor) {
		t.Errorf("choosing below the floor: %v, want ErrBelowFloor", err)
	}

	f.setFloor(t, store.StepUpEachTime)
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a fresh passkey under a floor of each time: %v, want needs a step-up", err)
	}
	f.setFloor(t, "sometimes")
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a floor Dockwell doesn't know: %v, want it read as each time", err)
	}
}

// No API token is ever covered, whatever its maker chose, and none reads or
// changes the choice.
func TestNoTokenIsCovered(t *testing.T) { storetest.Each(t, testNoTokenIsCovered) }

func testNoTokenIsCovered(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	p := f.choose(t, localhost, owner, AskAtSignIn)
	token := Principal{User: p.User, Token: &store.APIToken{ID: "tok_1", UserID: p.User.ID}}
	withBoth := token
	withBoth.Session = p.Session // a session beside a token counts for nothing
	for name, q := range map[string]*Principal{"a token": &token, "a token beside a session": &withBoth} {
		if err := f.svc.UseStepUp(t.Context(), localhost, *q, "", StepUpRevealSecret, "sec_1"); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("%s revealing: %v, want needs a session", name, err)
		}
		if _, err := f.svc.HoldStepUp(t.Context(), localhost, *q, "", StepUpApplyRootChange, "chg_1"); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("%s applying: %v, want needs a session", name, err)
		}
		if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
			return f.svc.UseStepUpIn(t.Context(), tx, localhost, *q, "", StepUpRevealSecret, "sec_1")
		}); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("%s revealing in a transaction: %v, want needs a session", name, err)
		}
		if _, err := f.svc.CreateToken(t.Context(), localhost, *q, "CI", store.ScopeReadOnly, nil, nil, ""); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("%s making a token: %v, want needs a session", name, err)
		}
		if _, err := f.svc.GetStepUpPolicy(t.Context(), *q); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("%s reading the choice: %v, want needs a session", name, err)
		}
		if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, *q, AskEachTime, ""); !errors.Is(err, ErrNeedsSession) {
			t.Errorf("%s changing the choice: %v, want needs a session", name, err)
		}
	}
	if n := len(f.auditEvents(t, "auth.step_up_sign_in")); n != 0 {
		t.Errorf("%d covers recorded for a token", n)
	}
}

// A cover used in a transaction is recorded in it, so an action that doesn't
// happen leaves no record of what confirmed it, and one that does always has one.
func TestACoverIsRecordedWithItsAction(t *testing.T) {
	storetest.Each(t, testACoverIsRecordedWithItsAction)
}

func testACoverIsRecordedWithItsAction(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	p := f.choose(t, localhost, owner, AskHourly)
	failed := errors.New("the reveal failed")
	if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
		if err := f.svc.UseStepUpIn(t.Context(), tx, localhost, p, "", StepUpRevealSecret, "sec_1"); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if n := len(f.auditEvents(t, "auth.step_up_hour")); n != 0 {
		t.Fatalf("%d covers recorded for an action that rolled back", n)
	}
	if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
		return f.svc.UseStepUpIn(t.Context(), tx, localhost, p, "", StepUpRevealSecret, "sec_1")
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.auditEvents(t, "auth.step_up_hour")); n != 1 {
		t.Errorf("%d covers recorded, want 1", n)
	}
}

// Changing the choice takes a passkey whichever way it goes, says what it was and is
// in the audit log, and refuses what isn't a choice.
func TestChangingTheChoice(t *testing.T) { storetest.Each(t, testChangingTheChoice) }

func testChangingTheChoice(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	p := f.current(t, localhost, owner)
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, "sometimes", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown choice: %v, want ErrInvalid", err)
	}
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, AskEachTime, ""); err != nil {
		t.Errorf("choosing what's chosen already, with no proof: %v", err)
	}
	if _, err := f.svc.SetStepUpPolicy(t.Context(), localhost, p, AskHourly, f.proof(t, p, StepUpRevealSecret, "sec_1")); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a proof for something else: %v, want needs a step-up", err)
	}
	p = f.choose(t, localhost, owner, AskHourly)
	got, err := f.svc.GetStepUpPolicy(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy != AskHourly || got.Cover == nil {
		t.Errorf("after choosing once an hour: %+v, want it, with the hour the choosing passkey opened", got)
	}
	f.choose(t, localhost, owner, AskEachTime)
	if d := f.auditDetails(t, "auth.step_up_policy"); !slices.Equal(d, []string{"from each time to once an hour", "from once an hour to each time"}) {
		t.Errorf("choice events = %q", d)
	}
	if got := f.user(t, p).StepUpPolicy; got != store.StepUpEachTime {
		t.Errorf("the stored choice = %q", got)
	}
}

// A passkey sign-in opens its session's hour, and a code sign-in doesn't.
func TestAPasskeySignInOpensTheHour(t *testing.T) { storetest.Each(t, testAPasskeySignInOpensTheHour) }

func testAPasskeySignInOpensTheHour(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend, "")
	a := &authtest.Authenticator{}
	owner := f.claim(t, localhost, a)
	f.choose(t, localhost, owner, AskHourly)
	f.clock.advance(3 * time.Hour)
	signedIn, err := f.passkeySignIn(t, localhost, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := f.current(t, localhost, signedIn)
	if p.Session.PasskeyAt == nil || !p.Session.PasskeyAt.Equal(signedIn.Session.CreatedAt) {
		t.Fatalf("a passkey sign-in left the session's passkey at %v", p.Session.PasskeyAt)
	}
	if err := f.svc.UseStepUp(t.Context(), localhost, p, "", StepUpAddServer, ""); err != nil {
		t.Errorf("just after a passkey sign-in, once an hour: %v", err)
	}
	if d := f.auditEvents(t, "auth.step_up_hour"); len(d) != 1 || d[0].Target != p.User.ID || !strings.HasPrefix(d[0].Detail, "confirmed adding a server within an hour") {
		t.Errorf("cover events = %+v", d)
	}
}
