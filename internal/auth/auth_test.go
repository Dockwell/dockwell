// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// clock is a test clock that only moves when told to.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	svc   *Service
	db    *store.DB
	key   *seal.Key
	clock *clock
}

func newFixture(t *testing.T, domain string) *fixture {
	t.Helper()
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Mid-step, so ±1 step tests aren't at a boundary.
	c := &clock{t: time.Date(2026, 11, 4, 9, 30, 15, 0, time.UTC)}
	svc, err := New(Config{
		DB:         db,
		Key:        key,
		Logger:     slog.New(slog.DiscardHandler),
		Sites:      Sites{Domain: domain},
		ServerName: func() string { return "hz-1" },
		Now:        c.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{svc: svc, db: db, key: key, clock: c}
}

var (
	localhost = Client{Site: Site{RPID: "localhost", Origin: "http://localhost:7600"}, Addr: "127.0.0.1:50000", UserAgent: "Mozilla/5.0 Chrome/141"}
	onDomain  = Client{Site: Site{RPID: "panel.acme.dev", Origin: "https://panel.acme.dev", Secure: true}, Addr: "198.51.100.23:4000", UserAgent: "Mozilla/5.0 Safari/18"}
)

// claim claims the install as Mira with a new passkey on client's site.
func (f *fixture) claim(t *testing.T, client Client, a *authtest.Authenticator) SignedIn {
	t.Helper()
	link, err := f.svc.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	cer, err := f.svc.BeginClaim(t.Context(), client, link.Token, "Mira Chen", "Mira@Acme.dev")
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := a.Create(string(cer.Options), client.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	signedIn, err := f.svc.FinishClaim(t.Context(), client, cer.ID, link.Token, []byte(cred), "Chrome on macOS")
	if err != nil {
		t.Fatal(err)
	}
	return signedIn
}

func (f *fixture) passkeySignIn(t *testing.T, client Client, a *authtest.Authenticator, cred *authtest.Credential) (SignedIn, error) {
	t.Helper()
	cer, err := f.svc.BeginSignIn(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Get(string(cer.Options), client.Site.Origin, cred)
	if err != nil {
		t.Fatal(err)
	}
	return f.svc.FinishSignIn(t.Context(), client, cer.ID, []byte(resp))
}

func (f *fixture) events(t *testing.T) []string {
	t.Helper()
	var out []string
	err := f.db.View(t.Context(), func(tx *store.Tx) error {
		evs, err := tx.ListEvents(t.Context(), 1000)
		for i := len(evs) - 1; i >= 0; i-- {
			out = append(out, evs[i].Action)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestClaimThenSignInWithAPasskey(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{}
	if claimed, _ := f.svc.Claimed(t.Context()); claimed {
		t.Fatal("a new install is claimed")
	}
	signedIn := f.claim(t, localhost, a)
	if signedIn.User.Email != "mira@acme.dev" || signedIn.User.Role != store.RoleOwner {
		t.Errorf("owner = %+v", signedIn.User)
	}
	if signedIn.Session.Method != MethodClaim || signedIn.Session.RPID != "localhost" {
		t.Errorf("session = %+v", signedIn.Session)
	}
	if p, err := f.svc.Authenticate(t.Context(), localhost, signedIn.Token); err != nil || p.User.ID != signedIn.User.ID {
		t.Fatalf("Authenticate after claim = %v, %v", p.User.ID, err)
	}

	again, err := f.passkeySignIn(t, localhost, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.User.ID != signedIn.User.ID || again.Session.Method != MethodPasskey || again.Token == signedIn.Token {
		t.Errorf("passkey sign-in = %+v", again.Session)
	}
	keys, err := f.svc.ListPasskeys(t.Context(), Principal{User: signedIn.User})
	if err != nil || len(keys) != 1 || keys[0].Name != "Chrome on macOS" || keys[0].LastUsedAt == nil {
		t.Fatalf("passkeys = %+v, %v", keys, err)
	}
	evs := f.events(t)
	for _, want := range []string{"claim_link.create", "owner.claim", "passkey.add", "auth.sign_in"} {
		if !contains(evs, want) {
			t.Errorf("audit log %v is missing %s", evs, want)
		}
	}
}

func TestClaimLinkWorksOnce(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{}
	link, err := f.svc.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	cer, err := f.svc.BeginClaim(t.Context(), localhost, link.Token, "Mira", "mira@acme.dev")
	if err != nil {
		t.Fatal(err)
	}
	// Begin again with the same link: allowed, nothing is spent yet.
	second, err := f.svc.BeginClaim(t.Context(), localhost, link.Token, "Eve", "eve@evil.test")
	if err != nil {
		t.Fatal(err)
	}
	cred, _, _ := a.Create(string(cer.Options), localhost.Site.Origin)
	if _, err := f.svc.FinishClaim(t.Context(), localhost, cer.ID, link.Token, []byte(cred), ""); err != nil {
		t.Fatal(err)
	}
	cred2, _, _ := a.Create(string(second.Options), localhost.Site.Origin)
	if _, err := f.svc.FinishClaim(t.Context(), localhost, second.ID, link.Token, []byte(cred2), ""); !errors.Is(err, ErrClaimed) {
		t.Errorf("second FinishClaim = %v, want ErrClaimed", err)
	}
	if _, err := f.svc.CheckClaim(t.Context(), localhost, link.Token); !errors.Is(err, ErrClaimed) {
		t.Errorf("CheckClaim after claiming = %v, want ErrClaimed", err)
	}
	if _, err := f.svc.CreateClaimLink(t.Context(), "test"); !errors.Is(err, ErrClaimed) {
		t.Errorf("a new claim link after claiming = %v, want ErrClaimed", err)
	}
}

func TestClaimLinkRejections(t *testing.T) {
	f := newFixture(t, "")
	old, err := f.svc.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	link, err := f.svc.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"replaced by a newer link": old.Token,
		"wrong":                    strings.Repeat("A", 43),
		"empty":                    "",
	} {
		if _, err := f.svc.CheckClaim(t.Context(), localhost, token); !errors.Is(err, ErrClaimLinkRejected) {
			t.Errorf("%s: CheckClaim = %v, want ErrClaimLinkRejected", name, err)
		}
	}
	if _, err := f.svc.CheckClaim(t.Context(), localhost, link.Token); err != nil {
		t.Errorf("the newest link: %v", err)
	}
	f.clock.advance(ClaimLinkTTL)
	if _, err := f.svc.CheckClaim(t.Context(), localhost, link.Token); !errors.Is(err, ErrClaimLinkRejected) {
		t.Errorf("an expired link: CheckClaim = %v, want ErrClaimLinkRejected", err)
	}
	if _, err := f.svc.BeginClaim(t.Context(), localhost, link.Token, "Mira", "mira@acme.dev"); !errors.Is(err, ErrClaimLinkRejected) {
		t.Errorf("an expired link: BeginClaim = %v, want ErrClaimLinkRejected", err)
	}
}

func TestClaimExpiresBetweenBeginAndFinish(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{}
	link, _ := f.svc.CreateClaimLink(t.Context(), "test")
	f.clock.advance(ClaimLinkTTL - time.Minute)
	cer, err := f.svc.BeginClaim(t.Context(), localhost, link.Token, "Mira", "mira@acme.dev")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Minute)
	cred, _, _ := a.Create(string(cer.Options), localhost.Site.Origin)
	if _, err := f.svc.FinishClaim(t.Context(), localhost, cer.ID, link.Token, []byte(cred), ""); !errors.Is(err, ErrClaimLinkRejected) {
		t.Errorf("FinishClaim after the link expired = %v, want ErrClaimLinkRejected", err)
	}
	if claimed, _ := f.svc.Claimed(t.Context()); claimed {
		t.Error("the install was claimed with an expired link")
	}
}

func TestRacingClaimsMakeOneOwner(t *testing.T) {
	f := newFixture(t, "")
	link, err := f.svc.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	const racers = 12
	type ready struct {
		id, cred string
	}
	var started []ready
	for i := range racers {
		a := &authtest.Authenticator{}
		client := localhost
		client.Addr = "127.0.0." + string(rune('1'+i%9)) + ":1"
		cer, err := f.svc.BeginClaim(t.Context(), client, link.Token, "Racer", "racer"+string(rune('a'+i))+"@acme.dev")
		if err != nil {
			t.Fatal(err)
		}
		cred, _, err := a.Create(string(cer.Options), localhost.Site.Origin)
		if err != nil {
			t.Fatal(err)
		}
		started = append(started, ready{cer.ID, cred})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for _, r := range started {
		wg.Go(func() {
			_, err := f.svc.FinishClaim(context.Background(), localhost, r.id, link.Token, []byte(r.cred), "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrClaimed), errors.Is(err, ErrClaimLinkRejected):
				lost++
			default:
				t.Errorf("FinishClaim: %v", err)
			}
		})
	}
	wg.Wait()
	if won != 1 || lost != racers-1 {
		t.Errorf("%d claims won and %d lost, want 1 and %d", won, lost, racers-1)
	}
	var n int
	_ = f.db.View(t.Context(), func(tx *store.Tx) (err error) {
		n, err = tx.CountUsers(t.Context())
		return err
	})
	if n != 1 {
		t.Errorf("%d users after racing claims, want 1", n)
	}
}

func TestTheSchemaAllowsOneOwner(t *testing.T) {
	f := newFixture(t, "")
	err := f.db.Update(t.Context(), func(tx *store.Tx) error {
		for i, email := range []string{"a@acme.dev", "b@acme.dev"} {
			if err := tx.CreateUser(t.Context(), store.User{
				ID: "usr_" + email, Email: email, Name: "x", Role: store.RoleOwner,
				WebAuthnHandle: []byte{byte(i)}, CreatedAt: f.clock.now(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second owner = %v, want ErrConflict", err)
	}
}

func TestClaimNeedsTheSameSiteAndToken(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	a := &authtest.Authenticator{}
	link, _ := f.svc.CreateClaimLink(t.Context(), "test")
	other, _ := f.svc.CreateClaimLink(t.Context(), "test") // replaces link
	cer, err := f.svc.BeginClaim(t.Context(), onDomain, other.Token, "Mira", "mira@acme.dev")
	if err != nil {
		t.Fatal(err)
	}
	cred, _, _ := a.Create(string(cer.Options), onDomain.Site.Origin)
	if _, err := f.svc.FinishClaim(t.Context(), localhost, cer.ID, other.Token, []byte(cred), ""); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing on another site = %v, want ErrCeremonyExpired", err)
	}
	cer, _ = f.svc.BeginClaim(t.Context(), onDomain, other.Token, "Mira", "mira@acme.dev")
	cred, _, _ = a.Create(string(cer.Options), onDomain.Site.Origin)
	if _, err := f.svc.FinishClaim(t.Context(), onDomain, cer.ID, link.Token, []byte(cred), ""); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing with another token = %v, want ErrCeremonyExpired", err)
	}
	// A browser on another origin signs a different origin into clientData.
	cer, _ = f.svc.BeginClaim(t.Context(), onDomain, other.Token, "Mira", "mira@acme.dev")
	cred, _, _ = a.Create(string(cer.Options), "https://evil.test")
	if _, err := f.svc.FinishClaim(t.Context(), onDomain, cer.ID, other.Token, []byte(cred), ""); !errors.Is(err, ErrPasskeyRejected) {
		t.Errorf("a response from another origin = %v, want ErrPasskeyRejected", err)
	}
	// The ceremony is gone after one try, whatever happened.
	if _, err := f.svc.FinishClaim(t.Context(), onDomain, cer.ID, other.Token, []byte(cred), ""); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("reusing a ceremony = %v, want ErrCeremonyExpired", err)
	}
}

func TestUserVerificationIsRequired(t *testing.T) {
	f := newFixture(t, "")
	link, _ := f.svc.CreateClaimLink(t.Context(), "test")
	cer, _ := f.svc.BeginClaim(t.Context(), localhost, link.Token, "Mira", "mira@acme.dev")
	lazy := &authtest.Authenticator{SkipUserVerification: true}
	cred, _, _ := lazy.Create(string(cer.Options), localhost.Site.Origin)
	if _, err := f.svc.FinishClaim(t.Context(), localhost, cer.ID, link.Token, []byte(cred), ""); !errors.Is(err, ErrPasskeyRejected) {
		t.Fatalf("a passkey made without user verification = %v, want ErrPasskeyRejected", err)
	}

	a := &authtest.Authenticator{}
	f.claim(t, localhost, a)
	a.SkipUserVerification = true
	if _, err := f.passkeySignIn(t, localhost, a, nil); !errors.Is(err, ErrPasskeyRejected) {
		t.Errorf("a sign-in without user verification = %v, want ErrPasskeyRejected", err)
	}
}

func TestPasskeysWorkOnlyOnTheirAddress(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	a := &authtest.Authenticator{}
	f.claim(t, localhost, a)
	local := a.Credentials()[0]

	// The browser would refuse to use a localhost passkey on the domain; a
	// forged response signed for localhost fails the RP ID hash check.
	if _, err := f.passkeySignIn(t, onDomain, a, local); !errors.Is(err, ErrPasskeyRejected) {
		t.Errorf("a localhost passkey on the domain = %v, want ErrPasskeyRejected", err)
	}
	if _, err := f.passkeySignIn(t, localhost, a, local); err != nil {
		t.Errorf("the same passkey on localhost: %v", err)
	}
}

func TestAddAPasskeyForANewAddress(t *testing.T) {
	f := newFixture(t, "panel.acme.dev")
	a := &authtest.Authenticator{}
	owner := f.claim(t, localhost, a)

	// Signed in on the domain some other way (here, a link from dockwell
	// recover), the owner adds a passkey there.
	link, _, err := f.svc.CreateSignInLink(t.Context(), "", "cli:root@hz-1")
	if err != nil {
		t.Fatal(err)
	}
	there, err := f.svc.SignInWithLink(t.Context(), onDomain, link.Token)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{User: there.User, Session: there.Session}
	cer, err := f.svc.BeginAddPasskey(t.Context(), onDomain, p)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := a.Create(string(cer.Options), onDomain.Site.Origin)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := f.svc.FinishAddPasskey(t.Context(), onDomain, p, cer.ID, []byte(cred), "  iPhone  ")
	if err != nil {
		t.Fatal(err)
	}
	if pk.RPID != "panel.acme.dev" || pk.Name != "iPhone" {
		t.Errorf("new passkey = %+v", pk.Passkey)
	}
	if _, err := f.passkeySignIn(t, onDomain, a, nil); err != nil {
		t.Errorf("signing in on the domain with its new passkey: %v", err)
	}

	// The same authenticator can't add a second passkey for the domain.
	cer, _ = f.svc.BeginAddPasskey(t.Context(), onDomain, p)
	if _, _, err := a.Create(string(cer.Options), onDomain.Site.Origin); !errors.Is(err, authtest.ErrExcluded) {
		t.Errorf("a second passkey from one authenticator = %v, want it excluded", err)
	}
	// Someone else's session can't finish the ceremony.
	other := Principal{User: store.User{ID: "usr_other"}, Session: owner.Session}
	if _, err := f.svc.FinishAddPasskey(t.Context(), onDomain, other, cer.ID, []byte(cred), ""); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing another user's ceremony = %v, want ErrCeremonyExpired", err)
	}
}

func TestAPasskeyThatCountsBackwardsIsTreatedAsCopied(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{Counter: true}
	f.claim(t, localhost, a)
	if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil {
		t.Fatal(err)
	}
	cred := a.Credentials()[0]
	cred.Count = 0 // a copy that missed the last use; Get adds one
	if _, err := f.passkeySignIn(t, localhost, a, nil); !errors.Is(err, ErrPasskeyCopied) {
		t.Fatalf("a sign count that went backwards = %v, want ErrPasskeyCopied", err)
	}
	cred.Count = 100 // even a count that moves on again is refused now
	if _, err := f.passkeySignIn(t, localhost, a, nil); !errors.Is(err, ErrPasskeyCopied) {
		t.Errorf("a flagged passkey = %v, want ErrPasskeyCopied", err)
	}
	if !contains(f.events(t), "passkey.clone_suspected") {
		t.Error("the suspected copy isn't in the audit log")
	}
}

func TestSyncedPasskeysCountZero(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{Synced: true}
	owner := f.claim(t, localhost, a)
	for range 3 {
		if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := f.svc.ListPasskeys(t.Context(), Principal{User: owner.User})
	if len(keys) != 1 || !keys[0].Synced() {
		t.Errorf("passkeys = %+v, want one synced", keys)
	}
}

func TestUnknownPasskeysAreRefused(t *testing.T) {
	f := newFixture(t, "")
	f.claim(t, localhost, &authtest.Authenticator{})
	stranger := &authtest.Authenticator{}
	cer, _ := f.svc.BeginSignIn(t.Context(), localhost)
	// A passkey the stranger made for localhost elsewhere.
	_, cred, _ := stranger.Create(`{"rp":{"id":"localhost"},"user":{"id":"AAAA"},"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}`, localhost.Site.Origin)
	resp, _ := stranger.Get(string(cer.Options), localhost.Site.Origin, cred)
	if _, err := f.svc.FinishSignIn(t.Context(), localhost, cer.ID, []byte(resp)); !errors.Is(err, ErrPasskeyRejected) {
		t.Errorf("an unknown passkey = %v, want ErrPasskeyRejected", err)
	}
	if _, err := f.svc.FinishSignIn(t.Context(), localhost, "nope", []byte(resp)); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("an unknown ceremony = %v, want ErrCeremonyExpired", err)
	}
}

func TestCeremoniesExpire(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{}
	f.claim(t, localhost, a)
	cer, _ := f.svc.BeginSignIn(t.Context(), localhost)
	resp, _ := a.Get(string(cer.Options), localhost.Site.Origin, nil)
	f.clock.advance(ceremonyTTL + time.Second)
	if _, err := f.svc.FinishSignIn(t.Context(), localhost, cer.ID, []byte(resp)); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("finishing after 5 min = %v, want ErrCeremonyExpired", err)
	}
}

func TestSignInNeedsAHostname(t *testing.T) {
	f := newFixture(t, "")
	ip := Client{Site: Site{Origin: "http://127.0.0.1:7600"}, Addr: "127.0.0.1:1"}
	if _, err := f.svc.BeginSignIn(t.Context(), ip); !errors.Is(err, ErrNeedsHostname) {
		t.Errorf("BeginSignIn on an IP = %v, want ErrNeedsHostname", err)
	}
	if _, err := f.svc.SignInWithCode(t.Context(), ip, "mira@acme.dev", "123456"); !errors.Is(err, ErrNeedsHostname) {
		t.Errorf("SignInWithCode on an IP = %v, want ErrNeedsHostname", err)
	}
}
