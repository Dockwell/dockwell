// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
)

func TestTOTPMatchesRFC6238(t *testing.T) {
	// RFC 6238 appendix B, SHA-1, with the 8-digit codes cut to the 6 every
	// authenticator app shows.
	secret := []byte("12345678901234567890")
	for _, tt := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		if got := totpCode(secret, totpStep(time.Unix(tt.unix, 0))); got != tt.want {
			t.Errorf("code at %d = %s, want %s", tt.unix, got, tt.want)
		}
	}
}

func TestTOTPWindow(t *testing.T) {
	secret := newTOTPSecret()
	now := time.Unix(1_800_000_015, 0)
	step := totpStep(now)
	for d, ok := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		got, matched := matchTOTP(secret, totpCode(secret, step+d), now)
		if matched != ok || (ok && got != step+d) {
			t.Errorf("code for step %+d: matched %v at %d, want %v", d, matched, got, ok)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want codeKind
	}{
		{"123456", codeTOTP},
		{"\t123 456\t", codeTOTP},
		{"12345", codeInvalid},
		{"12345a", codeInvalid},
		{"7KQ2-M9XD-4TPA-ZR3C", codeRecovery},
		{"7kq2m9xd4tpazr3c", codeRecovery},
		{"7KQ2-M9XD-4TPA-ZR3U", codeInvalid}, // U isn't in the alphabet
		{"7KQ2-M9XD-4TPA-ZR3CC", codeInvalid},
	} {
		if _, got := normalizeCode(tt.in); got != tt.want {
			t.Errorf("normalizeCode(%q) kind = %v, want %v", tt.in, got, tt.want)
		}
	}
	if got, _ := normalizeCode("ilo0-ILO0-abcd-efgh"); got != "1100110"+"0ABCDEFGH" {
		t.Errorf("normalizeCode maps look-alikes to %q", got)
	}
	codes, hashes := newRecoveryCodes()
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatalf("got %d codes", len(codes))
	}
	for _, c := range codes {
		n, kind := normalizeCode(c)
		if kind != codeRecovery || len(c) != 19 || strings.Count(c, "-") != 3 {
			t.Errorf("recovery code %q doesn't read back (%v)", c, kind)
		}
		if !bytes.Equal(hashRecoveryCode(n), hashRecoveryCode(strings.ReplaceAll(c, "-", ""))) {
			t.Errorf("%q hashes differently with and without dashes", c)
		}
	}
}

// enableTOTP turns on code sign-in for the owner and returns the secret and
// recovery codes.
func (f *fixture) enableTOTP(t *testing.T, p Principal) ([]byte, []string) {
	t.Helper()
	setup, err := f.svc.BeginTOTP(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(setup.URI, "otpauth://totp/Dockwell") || !strings.Contains(setup.URI, "secret="+setup.Secret) {
		t.Errorf("URI = %s", setup.URI)
	}
	secret, err := totpEncoding.DecodeString(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmTOTP(t.Context(), p, "000000"); !errors.Is(err, ErrTOTPMismatch) && totpCode(secret, totpStep(f.clock.now())) != "000000" {
		t.Errorf("confirming with a wrong code = %v, want ErrTOTPMismatch", err)
	}
	codes, err := f.svc.ConfirmTOTP(t.Context(), p, totpCode(secret, totpStep(f.clock.now())))
	if err != nil {
		t.Fatal(err)
	}
	// The confirming code has been used.
	f.clock.advance(totpPeriod * time.Second)
	return secret, codes
}

func ownerPrincipal(s SignedIn) Principal { return Principal{User: s.User, Session: s.Session} }

func TestSignInWithAnAuthenticatorCode(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, _ := f.enableTOTP(t, ownerPrincipal(owner))

	code := totpCode(secret, totpStep(f.clock.now()))
	got, err := f.svc.SignInWithCode(t.Context(), onDomain, "MIRA@acme.dev ", code)
	if err != nil {
		t.Fatal(err)
	}
	if got.User.ID != owner.User.ID || got.Session.Method != MethodAuthenticator || got.Session.RPID != "panel.acme.dev" {
		t.Errorf("signed in as %+v", got.Session)
	}
	// The same code, a second time: refused even within its window.
	if _, err := f.svc.SignInWithCode(t.Context(), onDomain, "mira@acme.dev", code); !errors.Is(err, ErrCodeReused) {
		t.Errorf("a code used twice = %v, want ErrCodeReused", err)
	}
	// An older code than the one used is refused too.
	if _, err := f.svc.SignInWithCode(t.Context(), localhost, "mira@acme.dev", totpCode(secret, totpStep(f.clock.now())-1)); !errors.Is(err, ErrCodeReused) {
		t.Errorf("an older code = %v, want ErrCodeReused", err)
	}
	f.clock.advance(totpPeriod * time.Second)
	if _, err := f.svc.SignInWithCode(t.Context(), localhost, "mira@acme.dev", totpCode(secret, totpStep(f.clock.now()))); err != nil {
		t.Errorf("the next code: %v", err)
	}
	// Two steps late is outside the window.
	late := totpCode(secret, totpStep(f.clock.now())+3)
	f.clock.advance(3 * totpPeriod * time.Second)
	f.clock.advance(2 * totpPeriod * time.Second)
	if _, err := f.svc.SignInWithCode(t.Context(), localhost, "mira@acme.dev", late); !errors.Is(err, ErrCodeRejected) {
		t.Errorf("a code two steps old = %v, want ErrCodeRejected", err)
	}
}

func TestRacingCodeSignInsUseACodeOnce(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, _ := f.enableTOTP(t, ownerPrincipal(owner))
	code := totpCode(secret, totpStep(f.clock.now()))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 8 {
		wg.Go(func() {
			client := localhost
			client.Addr = "10.0.0." + string(rune('1'+i)) + ":1"
			if _, err := f.svc.SignInWithCode(t.Context(), client, "mira@acme.dev", code); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if ok != 1 {
		t.Errorf("one code signed in %d times, want once", ok)
	}
}

func TestWrongCodesSlowDownThenLock(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, recovery := f.enableTOTP(t, ownerPrincipal(owner))
	wrong := func() string {
		good := totpCode(secret, totpStep(f.clock.now()))
		for _, c := range []string{"111111", "222222", "333333"} {
			if c != good && c != totpCode(secret, totpStep(f.clock.now())-1) && c != totpCode(secret, totpStep(f.clock.now())+1) {
				return c
			}
		}
		t.Fatal("no wrong code")
		return ""
	}
	// Each try from its own address, so only the account's limit applies.
	client := func(i int) Client {
		c := localhost
		c.Addr = "192.0.2." + string(rune('0'+i%10)) + ":1"
		return c
	}
	var slowed bool
	for i := 1; i <= lockAfter; i++ {
		_, err := f.svc.SignInWithCode(t.Context(), client(i), "mira@acme.dev", wrong())
		var slow *SlowDown
		if errors.As(err, &slow) {
			slowed = true
			if slow.Wait <= 0 || slow.Wait > maxBackoff {
				t.Errorf("wait %s", slow.Wait)
			}
			f.clock.advance(slow.Wait)
			i--
			continue
		}
		want := ErrCodeRejected
		if i == lockAfter {
			want = ErrCodesLocked
		}
		if !errors.Is(err, want) {
			t.Fatalf("wrong code %d = %v, want %v", i, err, want)
		}
	}
	if !slowed {
		t.Error("wrong codes never slowed down")
	}
	f.clock.advance(time.Hour)
	if _, err := f.svc.SignInWithCode(t.Context(), client(0), "mira@acme.dev", totpCode(secret, totpStep(f.clock.now()))); !errors.Is(err, ErrCodesLocked) {
		t.Errorf("the right code while locked = %v, want ErrCodesLocked", err)
	}
	st, _ := f.svc.TOTP(t.Context(), ownerPrincipal(owner))
	if st.LockedAt == nil || st.FailedAttempts < lockAfter {
		t.Errorf("status while locked = %+v", st)
	}

	// A recovery code still works, and leaves the lock for the owner to lift.
	in, err := f.svc.SignInWithCode(t.Context(), client(1), "mira@acme.dev", recovery[0])
	if err != nil || !in.UsedRecoveryCode || in.RecoveryCodesLeft != 9 {
		t.Fatalf("recovery code while locked = %+v, %v", in, err)
	}
	if _, err := f.svc.SignInWithCode(t.Context(), client(2), "mira@acme.dev", recovery[0]); !errors.Is(err, ErrCodeRejected) {
		t.Errorf("a recovery code used twice = %v, want ErrCodeRejected", err)
	}
	if err := f.svc.UnlockCodes(t.Context(), ownerPrincipal(in.SignedIn)); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(time.Hour)
	if _, err := f.svc.SignInWithCode(t.Context(), client(3), "mira@acme.dev", totpCode(secret, totpStep(f.clock.now()))); err != nil {
		t.Errorf("the right code after unlocking: %v", err)
	}
	evs := f.events(t)
	for _, want := range []string{"auth.sign_in_failed", "totp.lock", "recovery_code.use", "totp.unlock"} {
		if !contains(evs, want) {
			t.Errorf("audit log is missing %s", want)
		}
	}
}

func TestWrongCodesFromOneSourceSlowDown(t *testing.T) {
	f := newFixture(t, "")
	f.claim(t, localhost, &authtest.Authenticator{})
	// Different emails, one address: the source's own limit applies.
	var slowed int
	for i := range freeSourceFailures + 3 {
		_, err := f.svc.SignInWithCode(t.Context(), localhost, "guess"+string(rune('a'+i))+"@acme.dev", "123456")
		var slow *SlowDown
		if errors.As(err, &slow) {
			slowed++
		} else if !errors.Is(err, ErrCodeRejected) {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	if slowed == 0 {
		t.Error("one address guessing many accounts never slowed down")
	}
}

func TestRecoveryCodesAreReplacedTogether(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	p := ownerPrincipal(owner)
	_, old := f.enableTOTP(t, p)
	fresh, err := f.svc.RegenerateRecoveryCodes(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SignInWithCode(t.Context(), localhost, "mira@acme.dev", old[3]); !errors.Is(err, ErrCodeRejected) {
		t.Errorf("an old recovery code = %v, want ErrCodeRejected", err)
	}
	if _, err := f.svc.SignInWithCode(t.Context(), localhost, "mira@acme.dev", strings.ToLower(fresh[3])); err != nil {
		t.Errorf("a new recovery code, typed in lowercase: %v", err)
	}
	var stored []byte
	_ = f.db.View(t.Context(), func(tx *store.Tx) error {
		u, err := tx.GetUser(t.Context(), owner.User.ID)
		stored = u.TOTPSecret
		return err
	})
	if len(stored) == 0 || strings.Contains(string(stored), "otpauth") {
		t.Error("the authenticator secret isn't stored sealed")
	}
	if _, err := f.key.Open(purposeTOTP("usr_someone_else"), stored); err == nil {
		t.Error("the sealed secret opens for another user")
	}
}

func TestNeverRemoveTheLastWayIn(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{}
	owner := f.claim(t, localhost, a)
	p := ownerPrincipal(owner)
	keys, _ := f.svc.ListPasskeys(t.Context(), p)
	if err := f.svc.RemovePasskey(t.Context(), p, keys[0].ID); !errors.Is(err, ErrLastWayIn) {
		t.Errorf("removing the only passkey = %v, want ErrLastWayIn", err)
	}
	f.enableTOTP(t, p)
	if err := f.svc.RemovePasskey(t.Context(), p, keys[0].ID); err != nil {
		t.Errorf("removing the passkey once codes work: %v", err)
	}
	if err := f.svc.DisableTOTP(t.Context(), p); !errors.Is(err, ErrLastWayIn) {
		t.Errorf("turning off codes with no passkey left = %v, want ErrLastWayIn", err)
	}
	if err := f.svc.RemovePasskey(t.Context(), p, keys[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it again = %v, want ErrNotFound", err)
	}
	evs := f.events(t)
	if !contains(evs, "passkey.remove") || !contains(evs, "totp.enable") {
		t.Errorf("audit log %v", evs)
	}
}

// Passkeys for a domain the panel no longer answers on can't sign in, so
// they don't count as a way in.
func TestPasskeysForAnOldDomainArentAWayIn(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	a := &authtest.Authenticator{}
	owner := f.claim(t, onDomain, a)
	p := ownerPrincipal(owner)
	cer, err := f.svc.BeginAddPasskey(t.Context(), localhost, p)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := a.Create(string(cer.Options), localhost.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	local, err := f.svc.FinishAddPasskey(t.Context(), localhost, p, cer.ID, []byte(cred), "")
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := f.svc.ListPasskeys(t.Context(), p)
	onOld := keys[0]
	if onOld.RPID != "panel.acme.dev" || local.RPID != "localhost" {
		t.Fatalf("passkeys for %s and %s", onOld.RPID, local.RPID)
	}

	// The panel loses its domain: only the localhost passkey still works.
	f.domain = ""
	f.svc = f.restart(t)
	if err := f.svc.RemovePasskey(t.Context(), p, local.ID); !errors.Is(err, ErrLastWayIn) {
		t.Errorf("removing the only passkey that works here = %v, want ErrLastWayIn", err)
	}
	f.enableTOTP(t, p)
	f.domain = "new.acme.dev"
	f.svc = f.restart(t)
	if err := f.svc.RemovePasskey(t.Context(), p, local.ID); err != nil {
		t.Errorf("removing it once codes work: %v", err)
	}
	if err := f.svc.DisableTOTP(t.Context(), p); !errors.Is(err, ErrLastWayIn) {
		t.Errorf("turning off codes with only a passkey for an old domain left = %v, want ErrLastWayIn", err)
	}

	// Back on the domain the passkey was made for, it counts again.
	f.domain = "panel.acme.dev"
	f.svc = f.restart(t)
	if err := f.svc.DisableTOTP(t.Context(), p); err != nil {
		t.Errorf("turning off codes with a passkey for this domain: %v", err)
	}
}

// A failed sign-in could come from anyone, so the audit log names the
// address it came from as the actor, and the account it tried as the
// subject. Only a sign-in that worked names the person.
func TestFailedSignInsAreAuditedAsTheirSource(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	a := &authtest.Authenticator{}
	owner := f.claim(t, localhost, a)
	secret, _ := f.enableTOTP(t, ownerPrincipal(owner))
	from := func(addr string) Client {
		c := onDomain
		c.Addr = addr
		return c
	}
	if _, err := f.svc.SignInWithCode(t.Context(), from("192.0.2.1:4000"), "mira@acme.dev", wrongCode(t, secret, f.clock.now())); !errors.Is(err, ErrCodeRejected) {
		t.Fatalf("a wrong code = %v", err)
	}
	if _, err := f.svc.SignInWithCode(t.Context(), from("192.0.2.2:4000"), "mira@acme.dev", "AAAA-BBBB-CCCC-DDDD"); !errors.Is(err, ErrCodeRejected) {
		t.Fatalf("a wrong recovery code = %v", err)
	}
	if _, err := f.passkeySignIn(t, from("192.0.2.3:4000"), a, a.Credentials()[0]); !errors.Is(err, ErrPasskeyRejected) {
		t.Fatalf("a passkey for another address = %v", err)
	}
	failed := f.auditEvents(t, "auth.sign_in_failed")
	if len(failed) != 3 {
		t.Fatalf("%d failures audited, want 3: %+v", len(failed), failed)
	}
	for i, e := range failed {
		if want := fmt.Sprintf("anonymous:192.0.2.%d", i+1); e.Actor != want || e.Subject != owner.User.ID {
			t.Errorf("failure %d (%s) audited as %s on %s, want %s on the owner", i+1, e.Detail, e.Actor, e.Subject, want)
		}
	}
	if _, err := f.svc.SignInWithCode(t.Context(), from("192.0.2.4:4000"), "mira@acme.dev", totpCode(secret, totpStep(f.clock.now()))); err != nil {
		t.Fatal(err)
	}
	if in := f.auditEvents(t, "auth.sign_in"); in[len(in)-1].Actor != "user:"+owner.User.ID {
		t.Errorf("a sign-in that worked is audited as %s", in[len(in)-1].Actor)
	}
}

func TestSignInLinks(t *testing.T) {
	f := newFixture(t, "")
	if _, _, err := f.svc.CreateSignInLink(t.Context(), "", "test"); err == nil {
		t.Error("a sign-in link before anyone claimed the install")
	}
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	if _, _, err := f.svc.CreateSignInLink(t.Context(), "eve@acme.dev", "test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a link for nobody = %v, want ErrNotFound", err)
	}
	link, user, err := f.svc.CreateSignInLink(t.Context(), "mira@acme.dev", "cli:root@hz-1")
	if err != nil || user.ID != owner.User.ID {
		t.Fatal(err)
	}
	in, err := f.svc.SignInWithLink(t.Context(), localhost, link.Token)
	if err != nil || in.Session.Method != MethodLink {
		t.Fatalf("SignInWithLink = %+v, %v", in.Session, err)
	}
	if _, err := f.svc.SignInWithLink(t.Context(), localhost, link.Token); !errors.Is(err, ErrSignInLinkRejected) {
		t.Errorf("a link used twice = %v, want ErrSignInLinkRejected", err)
	}
	expiring, _, _ := f.svc.CreateSignInLink(t.Context(), "", "test")
	f.clock.advance(SignInLinkTTL)
	if _, err := f.svc.SignInWithLink(t.Context(), localhost, expiring.Token); !errors.Is(err, ErrSignInLinkRejected) {
		t.Errorf("an expired link = %v, want ErrSignInLinkRejected", err)
	}
}

func TestSessionsExpire(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})

	// Idle: no request for 12 h ends it.
	f.clock.advance(SessionIdle - time.Minute)
	if _, err := f.svc.Authenticate(t.Context(), localhost, owner.Token); err != nil {
		t.Fatalf("a session used within its idle limit: %v", err)
	}
	f.clock.advance(SessionIdle - time.Minute)
	if _, err := f.svc.Authenticate(t.Context(), localhost, owner.Token); err != nil {
		t.Fatalf("each request extends the idle limit: %v", err)
	}
	f.clock.advance(SessionIdle + time.Second)
	if _, err := f.svc.Authenticate(t.Context(), localhost, owner.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an idle session = %v, want ErrUnauthenticated", err)
	}

	// Absolute: busy or not, it ends 7 days after signing in.
	link, _, _ := f.svc.CreateSignInLink(t.Context(), "", "test")
	busy, err := f.svc.SignInWithLink(t.Context(), localhost, link.Token)
	if err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed < SessionMax-time.Hour; elapsed += 6 * time.Hour {
		if _, err := f.svc.Authenticate(t.Context(), localhost, busy.Token); err != nil {
			t.Fatalf("after %s: %v", elapsed, err)
		}
		f.clock.advance(6 * time.Hour)
	}
	f.clock.advance(time.Hour)
	if _, err := f.svc.Authenticate(t.Context(), localhost, busy.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a session older than 7 days = %v, want ErrUnauthenticated", err)
	}
}

func TestSessionsWorkOnlyOnTheirAddress(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	if _, err := f.svc.Authenticate(t.Context(), onDomain, owner.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a localhost session on the domain = %v, want ErrUnauthenticated", err)
	}
	other := localhost
	other.Site.Origin = "http://localhost:8000" // the same RP through another tunnel port
	if _, err := f.svc.Authenticate(t.Context(), other, owner.Token); err != nil {
		t.Errorf("the same session through another local port: %v", err)
	}
	if _, err := f.svc.Authenticate(t.Context(), localhost, owner.Token+"x"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a wrong token = %v", err)
	}
}

func TestListAndRevokeSessions(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{}
	first := f.claim(t, localhost, a)
	f.clock.advance(time.Minute)
	phone := localhost
	phone.Addr, phone.UserAgent = "127.0.0.9:2", "Mozilla/5.0 (iPhone) Safari"
	second, err := f.passkeySignIn(t, phone, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(time.Minute)
	third, _ := f.passkeySignIn(t, localhost, a, nil)

	p := ownerPrincipal(first)
	list, err := f.svc.ListSessions(t.Context(), p)
	if err != nil || len(list) != 3 || list[0].ID != third.Session.ID || list[1].UserAgent != phone.UserAgent || list[1].LastIP != "127.0.0.9" {
		t.Fatalf("sessions = %+v, %v", list, err)
	}

	// Revoking a session ends it, and closes its streams.
	ctx, stop := f.svc.Track(t.Context(), phone, ownerPrincipal(second))
	defer stop()
	if err := f.svc.RevokeSession(t.Context(), p, second.Session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("a revoked session's stream stayed open")
	}
	if _, err := f.svc.Authenticate(t.Context(), phone, second.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a revoked session = %v", err)
	}
	if err := f.svc.RevokeSession(t.Context(), p, second.Session.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking it twice = %v, want ErrNotFound", err)
	}

	n, err := f.svc.RevokeOtherSessions(t.Context(), p)
	if err != nil || n != 1 {
		t.Errorf("RevokeOtherSessions = %d, %v, want 1", n, err)
	}
	if _, err := f.svc.Authenticate(t.Context(), localhost, first.Token); err != nil {
		t.Errorf("the current session after signing out the others: %v", err)
	}
	if err := f.svc.SignOut(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(t.Context(), localhost, first.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a signed-out session = %v", err)
	}
	evs := f.events(t)
	if !contains(evs, "session.revoke") || !contains(evs, "auth.sign_out") {
		t.Errorf("audit log %v", evs)
	}
}

func TestLimiters(t *testing.T) {
	now := time.Unix(0, 0)
	f := newFailures(2, 3)
	for i, want := range []time.Duration{0, 0, time.Second} {
		if w := f.try("a", now); w != want {
			t.Errorf("try %d waits %s, want %s", i, w, want)
		}
	}
	now = now.Add(time.Second)
	if w := f.try("a", now); w != 0 || f.keys["a"].n != 3 {
		t.Errorf("after waiting: wait %s, n %d", w, f.keys["a"].n)
	}
	if w := f.try("a", now); w != 2*time.Second {
		t.Errorf("after 3 failures with 2 free: wait %s, want 2s", w)
	}
	f.reset("a")
	if w := f.try("a", now); w != 0 {
		t.Error("reset didn't forget")
	}
	for _, k := range []string{"b", "c", "d", "e"} {
		f.try(k, now)
	}
	if len(f.keys) > 3 {
		t.Errorf("%d keys kept, want at most 3", len(f.keys))
	}

	b := newBuckets(2, time.Second, 10)
	first, second, third := b.take("x", now), b.take("x", now), b.take("x", now)
	if !first || !second || third {
		t.Error("a burst of 2 should allow two and refuse the third")
	}
	if !b.take("x", now.Add(time.Second)) {
		t.Error("a token should come back after a second")
	}
	if backoff(100, 0) != maxBackoff {
		t.Error("backoff isn't capped")
	}
	for in, want := range map[string]string{
		"203.0.113.9:443":            "src:203.0.113.9",
		"[2001:db8:1:2:3:4:5:6]:443": "src:2001:db8:1:2::/64",
		"not an address":             "src:not an address",
	} {
		if got := sourceKey(in); got != want {
			t.Errorf("sourceKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// wrongCode returns a 6-digit code that doesn't match secret around now.
func wrongCode(t *testing.T, secret []byte, now time.Time) string {
	t.Helper()
	step := totpStep(now)
	for _, c := range []string{"111111", "222222", "333333", "444444"} {
		if c != totpCode(secret, step-1) && c != totpCode(secret, step) && c != totpCode(secret, step+1) {
			return c
		}
	}
	t.Fatal("no wrong code")
	return ""
}

// fromAddress is a client on localhost from its own source address, so the
// limit per source never applies.
func fromAddress(i int) Client {
	c := localhost
	c.Addr = fmt.Sprintf("[2001:db8:%x:%x::1]:443", i/0x10000, i%0x10000)
	return c
}

// lockCodes sends wrong authenticator codes for email, each from its own
// address and waiting out every slow-down, until code sign-in locks. It
// returns how many addresses it used.
func (f *fixture) lockCodes(t *testing.T, email string, secret []byte, from int) int {
	t.Helper()
	for i := from; ; i++ {
		_, err := f.svc.SignInWithCode(t.Context(), fromAddress(i), email, wrongCode(t, secret, f.clock.now()))
		var slow *SlowDown
		switch {
		case errors.As(err, &slow):
			f.clock.advance(slow.Wait)
		case errors.Is(err, ErrCodesLocked):
			return i + 1
		case !errors.Is(err, ErrCodeRejected):
			t.Fatalf("wrong code %d = %v", i, err)
		}
	}
}

// Anyone with the owner's email can keep sending codes while code sign-in is
// locked, from as many addresses as they hold. The audit log mustn't grow
// with them: one event an hour, with the count.
func TestCodesRefusedWhileLockedAreAuditedOnceAnHour(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, _ := f.enableTOTP(t, ownerPrincipal(owner))
	next := f.lockCodes(t, "mira@acme.dev", secret, 0)
	before := len(f.auditDetails(t, "auth.sign_in_failed"))

	const tries = 300
	for i := range tries {
		_, err := f.svc.SignInWithCode(t.Context(), fromAddress(next+i), "mira@acme.dev", wrongCode(t, secret, f.clock.now()))
		if !errors.Is(err, ErrCodesLocked) {
			t.Fatalf("a code while locked = %v, want ErrCodesLocked", err)
		}
		f.clock.advance(time.Second)
	}
	failed := f.auditDetails(t, "auth.sign_in_failed")
	if got := len(failed) - before; got != 1 {
		t.Fatalf("%d codes while locked made %d audit events, want 1", tries, got)
	}
	if !strings.Contains(failed[len(failed)-1], "authenticator code while code sign-in is locked") {
		t.Errorf("the event says %q", failed[len(failed)-1])
	}

	// An hour after the last event, the next refusal gets one, with the count.
	f.clock.advance(time.Hour)
	if _, err := f.svc.SignInWithCode(t.Context(), fromAddress(next+tries), "mira@acme.dev", "123456"); !errors.Is(err, ErrCodesLocked) {
		t.Fatal(err)
	}
	failed = f.auditDetails(t, "auth.sign_in_failed")
	if got := len(failed) - before; got != 2 || !strings.Contains(failed[len(failed)-1], fmt.Sprintf("and %d more codes refused since", tries-1)) {
		t.Errorf("after an hour: %d events, the last %q", got, failed[len(failed)-1])
	}

	// Unlocking counts the ones no event has counted yet.
	for i := range 5 {
		_, _ = f.svc.SignInWithCode(t.Context(), fromAddress(next+tries+1+i), "mira@acme.dev", "123456")
	}
	link, _, _ := f.svc.CreateSignInLink(t.Context(), "", "test")
	in, err := f.svc.SignInWithLink(t.Context(), localhost, link.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.UnlockCodes(t.Context(), ownerPrincipal(in)); err != nil {
		t.Fatal(err)
	}
	if unlock := f.auditDetails(t, "totp.unlock"); len(unlock) != 1 || !strings.HasPrefix(unlock[0], "5 codes refused since") {
		t.Errorf("unlock events %q, want one that counts the 5 refused codes", unlock)
	}
}

// Nothing anyone else does can stop a right recovery code: not wrong
// authenticator codes, not the lock, and not wrong recovery codes, however
// recent.
func TestARecoveryCodeWorksWhateverOthersSend(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, recovery := f.enableTOTP(t, ownerPrincipal(owner))
	stranger, _ := newRecoveryCodes()

	// Wrong authenticator codes keep the email slowed down.
	for i := range freeAccountFailures + 2 {
		_, _ = f.svc.SignInWithCode(t.Context(), fromAddress(i), "mira@acme.dev", wrongCode(t, secret, f.clock.now()))
	}
	// Wrong recovery codes, the last a moment ago.
	for i, c := range stranger {
		if _, err := f.svc.SignInWithCode(t.Context(), fromAddress(100+i), "mira@acme.dev", c); !errors.Is(err, ErrCodeRejected) {
			t.Fatalf("a wrong recovery code = %v, want ErrCodeRejected", err)
		}
	}
	in, err := f.svc.SignInWithCode(t.Context(), fromAddress(200), "mira@acme.dev", recovery[0])
	if err != nil || !in.UsedRecoveryCode {
		t.Fatalf("the owner's recovery code while others send wrong codes = %+v, %v", in, err)
	}

	// Wrong recovery codes don't count towards the lock, and aren't each
	// audited.
	st, _ := f.svc.TOTP(t.Context(), ownerPrincipal(in.SignedIn))
	if st.LockedAt != nil {
		t.Errorf("code sign-in locked after %d wrong recovery codes", len(stranger))
	}
	f.lockCodes(t, "mira@acme.dev", secret, 300)
	for i, c := range stranger {
		_, _ = f.svc.SignInWithCode(t.Context(), fromAddress(400+i), "mira@acme.dev", c)
	}
	if in, err := f.svc.SignInWithCode(t.Context(), fromAddress(500), "mira@acme.dev", recovery[1]); err != nil || in.RecoveryCodesLeft != 8 {
		t.Errorf("a recovery code while locked and under attack = %+v, %v", in, err)
	}
	refused := 0
	for _, d := range f.auditDetails(t, "auth.sign_in_failed") {
		if strings.HasPrefix(d, "wrong recovery code") || strings.HasPrefix(d, "authenticator code while") {
			refused++
		}
	}
	if refused > 2 {
		t.Errorf("%d wrong recovery codes made %d audit events, want at most one an hour", 2*len(stranger), refused)
	}

	// The limit per source still applies to recovery codes.
	var slowed bool
	for range freeSourceFailures + 1 {
		_, err := f.svc.SignInWithCode(t.Context(), onDomain, "mira@acme.dev", stranger[0])
		var slow *SlowDown
		slowed = slowed || errors.As(err, &slow)
	}
	if !slowed {
		t.Error("one address sending wrong recovery codes never slowed down")
	}
}

// answers sends wrong authenticator codes for email, each from its own
// address, waiting out every slow-down, and returns what each answer was.
func (f *fixture) answers(t *testing.T, email string, secret []byte, n, from int) []string {
	t.Helper()
	var out []string
	for i := range n {
		_, err := f.svc.SignInWithCode(t.Context(), fromAddress(from+i), email, wrongCode(t, secret, f.clock.now()))
		var slow *SlowDown
		if errors.As(err, &slow) {
			out = append(out, "wait "+slow.Wait.String())
			f.clock.advance(slow.Wait)
			continue
		}
		out = append(out, err.Error())
	}
	return out
}

// An email with no account gets the same answers as the owner's, through
// the lock, after a restart, and however many other emails are tried.
func TestUnknownEmailsGetTheSameAnswers(t *testing.T) {
	f := newFixture(t, "")
	owner := f.claim(t, localhost, &authtest.Authenticator{})
	secret, _ := f.enableTOTP(t, ownerPrincipal(owner))

	const n = 3 * lockAfter
	owners := f.answers(t, "mira@acme.dev", secret, n, 0)
	unknown := f.answers(t, "nobody@acme.dev", secret, n, 1000)
	if strings.Join(owners, "\n") != strings.Join(unknown, "\n") {
		t.Fatalf("answers differ:\nowner:   %q\nunknown: %q", owners, unknown)
	}
	if owners[len(owners)-1] != ErrCodesLocked.Error() {
		t.Errorf("the last answer is %q, want the lock", owners[len(owners)-1])
	}

	// A restart forgets what is in memory, and a flood of other emails
	// can't push either out.
	f.svc = f.restart(t)
	for i := range 200 {
		_, _ = f.svc.SignInWithCode(t.Context(), fromAddress(2000+i), fmt.Sprintf("flood%d@acme.dev", i), "123456")
	}
	for _, email := range []string{"mira@acme.dev", "nobody@acme.dev"} {
		if _, err := f.svc.SignInWithCode(t.Context(), fromAddress(3000), email, "123456"); !errors.Is(err, ErrCodesLocked) {
			t.Errorf("%s after a restart = %v, want ErrCodesLocked", email, err)
		}
	}
	// A recovery code gets the same answer either way.
	codes, _ := newRecoveryCodes()
	for _, email := range []string{"mira@acme.dev", "nobody@acme.dev"} {
		if _, err := f.svc.SignInWithCode(t.Context(), fromAddress(4000), email, codes[0]); !errors.Is(err, ErrCodeRejected) {
			t.Errorf("a wrong recovery code for %s = %v, want ErrCodeRejected", email, err)
		}
	}
}

func TestSlowDownRoundsUp(t *testing.T) {
	for wait, want := range map[time.Duration]string{
		300 * time.Millisecond:  "Wait 1 s,",
		time.Second:             "Wait 1 s,",
		1200 * time.Millisecond: "Wait 2 s,",
		15 * time.Minute:        "Wait 15 min,",
	} {
		if got := (&SlowDown{Wait: wait}).Error(); !strings.Contains(got, want) {
			t.Errorf("SlowDown{%s} = %q, want %q", wait, got, want)
		}
	}
}
