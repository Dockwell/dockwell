// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/oidc/oidctest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// providerStand is a fixture on the panel's domain, claimed there, with a fake
// provider whose saved state the test changes.
type providerStand struct {
	*fixture
	idp   *oidctest.Provider
	owner Principal
	mu    sync.Mutex
	saved []Provider
}

func newProviderStand(t *testing.T, backend store.Backend) *providerStand {
	t.Helper()
	f := newFixture(t, backend, onDomain.Site.RPID)
	signedIn := f.claim(t, onDomain, &authtest.Authenticator{})
	owner, err := f.svc.Authenticate(t.Context(), onDomain, signedIn.Token)
	if err != nil {
		t.Fatal(err)
	}
	s := &providerStand{fixture: f, idp: oidctest.NewProvider(t), owner: owner}
	s.saved = []Provider{{
		ID: "sip_ours", Name: "ours", DisplayName: "Our Authentik", Issuer: s.idp.Issuer(),
		ClientID: oidctest.Client, ClientSecret: oidctest.Secret, Panel: true,
	}}
	f.svc.UseProviders(func(context.Context) ([]Provider, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]Provider(nil), s.saved...), nil
	}, s.idp.Srv.Client())
	return s
}

// edit changes the saved provider, as an admin's applied change does, and lets the
// sign-in's few seconds of keeping it pass.
func (s *providerStand) edit(edit func(p *Provider)) {
	s.mu.Lock()
	if edit == nil {
		s.saved = nil
	} else {
		edit(&s.saved[0])
	}
	s.mu.Unlock()
	s.clock.advance(providersFor + time.Second)
}

// through goes to the provider at target, as an account with subject, and back:
// the query the provider brings back to the panel.
func (s *providerStand) through(t *testing.T, target, subject string) url.Values {
	t.Helper()
	to, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	q := to.Query()
	if q.Get("redirect_uri") != onDomain.Site.Origin+ProviderCallbackPath || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != oidctest.Client {
		t.Fatalf("it goes to the provider with %v", q)
	}
	now := s.clock.now()
	s.idp.ID = func(url.Values) map[string]any {
		c := oidctest.GoodClaims()
		c["sub"], c["nonce"], c["iat"], c["exp"] = subject, q.Get("nonce"), now.Unix(), now.Add(time.Hour).Unix()
		return c
	}
	return url.Values{"code": {"abc"}, "state": {q.Get("state")}}
}

// signIn signs in with the provider as the account Mira linked, or tries to.
func (s *providerStand) signIn(t *testing.T) (ProviderDone, error) {
	t.Helper()
	target, flow, err := s.svc.BeginProviderSignIn(t.Context(), onDomain, "sip_ours")
	if err != nil {
		t.Fatal(err)
	}
	return s.svc.FinishProvider(t.Context(), onDomain, flow, s.through(t, target, "248289761001"))
}

// link links the owner's account with subject, confirmed with their passkey.
func (s *providerStand) link(t *testing.T, subject string) (ProviderDone, error) {
	t.Helper()
	proof := s.stepUpAt(t, onDomain, s.owner, s.passkeys, StepUpLinkProvider, "sip_ours").Proof
	target, flow, err := s.svc.BeginLinkProvider(t.Context(), onDomain, s.owner, "sip_ours", proof)
	if err != nil {
		t.Fatal(err)
	}
	return s.svc.FinishProvider(t.Context(), onDomain, flow, s.through(t, target, subject))
}

func TestAProviderSignsInOnlyALinkedMember(t *testing.T) {
	storetest.Each(t, testAProviderSignsInOnlyALinkedMember)
}

// A provider signs in to the panel only on its own domain, over HTTPS, and only
// someone who linked the account while signed in, with their passkey. Such a
// session confirms nothing a passkey does, and a flow is used once, on its site,
// with its state (ADR 0095).
func testAProviderSignsInOnlyALinkedMember(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	ctx := t.Context()
	if got, _ := s.svc.SignInProviders(ctx, localhost); len(got) != 0 {
		t.Errorf("on localhost it offers %+v", got)
	}
	if got, _ := s.svc.SignInProviders(ctx, onDomain); len(got) != 1 || got[0].Label() != "Our Authentik" {
		t.Errorf("on the domain it offers %+v", got)
	}
	if _, _, err := s.svc.BeginProviderSignIn(ctx, localhost, "sip_ours"); !errors.Is(err, ErrNoProviderHere) {
		t.Errorf("on localhost a sign-in begins: %v", err)
	}

	// Nobody joins by signing in: the account is linked to no one.
	if _, err := s.signIn(t); !errors.Is(err, ErrProviderNotLinked) {
		t.Fatalf("an account linked to no one signs in: %v", err)
	}
	if got := s.auditDetails(t, "auth.sign_in_failed"); len(got) != 1 {
		t.Errorf("the refusal is audited as %v", got)
	}

	// Linking takes a passkey, each time.
	if _, _, err := s.svc.BeginLinkProvider(ctx, onDomain, s.owner, "sip_ours", ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("linking with no passkey begins: %v", err)
	}
	linked, err := s.link(t, "248289761001")
	if err != nil || linked.Linked == nil || linked.Linked.Account != "mira@example.com" {
		t.Fatalf("linking gives %+v, %v", linked, err)
	}
	if _, err := s.link(t, "another-account"); !errors.Is(err, ErrProviderLinkedTwice) {
		t.Errorf("a second account at the same provider links: %v", err)
	}

	done, err := s.signIn(t)
	if err != nil || done.SignedIn == nil {
		t.Fatalf("the linked account signs in: %+v, %v", done, err)
	}
	if sess := done.SignedIn.Session; sess.Method != MethodProvider || sess.ProviderLinkID != linked.Linked.ID || sess.PasskeyAt != nil {
		t.Errorf("its session is %+v", sess)
	}
	p, err := s.svc.Authenticate(ctx, onDomain, done.SignedIn.Token)
	if err != nil {
		t.Fatal(err)
	}
	// What a passkey confirms, it still asks for: a risky action, and adding a passkey.
	if err := s.svc.UseStepUp(ctx, onDomain, p, "", StepUpRevealSecret, "sec_x"); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a provider's session reveals a secret with no passkey: %v", err)
	}
	if _, err := s.svc.BeginAddPasskey(ctx, onDomain, p, ""); !errors.Is(err, ErrNeedsStepUp) {
		t.Errorf("a provider's session adds a passkey with no passkey: %v", err)
	}

	// A flow is used once, on its own site, with its own state.
	target, flow, err := s.svc.BeginProviderSignIn(ctx, onDomain, "sip_ours")
	if err != nil {
		t.Fatal(err)
	}
	back := s.through(t, target, "248289761001")
	forged := url.Values{"code": {"abc"}, "state": {"forged"}}
	if _, err := s.svc.FinishProvider(ctx, onDomain, flow, forged); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("a forged state signs in: %v", err)
	}
	if _, err := s.svc.FinishProvider(ctx, onDomain, flow, back); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("a flow refused once signs in after: %v", err)
	}
	other := onDomain
	other.Site.Origin = "https://elsewhere.example"
	if target, flow, err = s.svc.BeginProviderSignIn(ctx, onDomain, "sip_ours"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.FinishProvider(ctx, other, flow, s.through(t, target, "248289761001")); !errors.Is(err, ErrCeremonyExpired) {
		t.Errorf("a flow finished on another site signs in: %v", err)
	}

	// Unlinking ends what the link began, and nothing else.
	if err := s.svc.DeleteProviderLink(ctx, s.owner, linked.Linked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.Authenticate(ctx, onDomain, done.SignedIn.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after unlinking, the provider's session works: %v", err)
	}
	if _, err := s.signIn(t); !errors.Is(err, ErrProviderNotLinked) {
		t.Errorf("an unlinked account signs in: %v", err)
	}
}

func TestAProvidersEditStopsItsLinks(t *testing.T) {
	storetest.Each(t, testAProvidersEditStopsItsLinks)
}

// A provider turned off for the panel, or moved to another issuer, stops its links
// signing anyone in, and ends the sessions they began; turned back, they work again.
// A deleted provider's links are deleted (ADR 0095).
func testAProvidersEditStopsItsLinks(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	ctx := t.Context()
	if _, err := s.link(t, "248289761001"); err != nil {
		t.Fatal(err)
	}
	signedIn := func() string {
		t.Helper()
		done, err := s.signIn(t)
		if err != nil {
			t.Fatal(err)
		}
		return done.SignedIn.Token
	}
	state := func() LinkState {
		t.Helper()
		links, err := s.svc.ListProviderLinks(ctx, s.owner)
		if err != nil || len(links) != 1 {
			t.Fatalf("the links are %+v, %v", links, err)
		}
		return links[0].State
	}
	token := signedIn()
	s.edit(func(p *Provider) { p.Panel = false })
	if _, err := s.svc.Authenticate(ctx, onDomain, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("turned off for the panel, its session works: %v", err)
	}
	if got := s.auditDetails(t, "auth.sessions_ended"); len(got) != 1 {
		t.Errorf("the sessions it ended are audited as %v", got)
	}
	if got := state(); got != LinkProviderOff {
		t.Errorf("turned off, the link reads %v", got)
	}
	if _, err := s.svc.Authenticate(ctx, onDomain, s.ownerToken(t)); err != nil {
		t.Errorf("the owner's passkey session ends with the provider's: %v", err)
	}

	s.edit(func(p *Provider) { p.Panel = true })
	token = signedIn()
	s.edit(func(p *Provider) { p.Issuer = "https://moved.example.com/" })
	if _, err := s.svc.Authenticate(ctx, onDomain, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("moved to another issuer, its session works: %v", err)
	}
	if got := state(); got != LinkIssuerMoved {
		t.Errorf("moved, the link reads %v", got)
	}

	s.edit(func(p *Provider) { p.Issuer = s.idp.Issuer() })
	token = signedIn()
	s.edit(nil)
	if _, err := s.svc.Authenticate(ctx, onDomain, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("its provider deleted, its session works: %v", err)
	}
	if links, err := s.svc.ListProviderLinks(ctx, s.owner); err != nil || len(links) != 0 {
		t.Errorf("its provider deleted, the links are %+v, %v", links, err)
	}
}

// ownerToken signs the owner in again with their passkey, for a token to check.
func (s *providerStand) ownerToken(t *testing.T) string {
	t.Helper()
	done, err := s.passkeySignIn(t, onDomain, s.passkeys, nil)
	if err != nil {
		t.Fatal(err)
	}
	return done.Token
}

func TestStartingAProviderSignInIsBounded(t *testing.T) {
	storetest.Each(t, testStartingAProviderSignInIsBounded)
}

// Starting a sign-in with a provider spends from the same allowance for an address
// as starting one with a passkey (ADR 0095).
func testStartingAProviderSignInIsBounded(t *testing.T, backend store.Backend) {
	s := newProviderStand(t, backend)
	var err error
	for range 40 {
		if _, _, err = s.svc.BeginProviderSignIn(t.Context(), onDomain, "sip_ours"); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrTooManyRequests) {
		t.Errorf("40 starts from one address end with %v", err)
	}
	if _, err := s.svc.BeginSignIn(t.Context(), onDomain); !errors.Is(err, ErrTooManyRequests) {
		t.Errorf("after them, a passkey sign-in starts: %v", err)
	}
}
